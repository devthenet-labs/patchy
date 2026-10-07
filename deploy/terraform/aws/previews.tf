# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Preview infrastructure, all of it behind var.previews:
#   - a dedicated Auto Mode node identity for the isolated preview NodeClass,
#     which can join only this cluster and pull only <preview_path_prefix>/*;
#   - the subnets the preview NodeClass and the preview ALB use, checked;
#   - the *.<host_suffix> certificate the preview ALB terminates TLS with;
#   - after Helm stage 2: the wildcard alias to the preview ALB, which Auto
#     Mode creates from the chart's placeholder Ingress, so Terraform only
#     looks it up.

locals {
  previews_enabled = var.previews != null

  preview_node_role_name = "${local.name_prefix}-patchy-preview-node"
  preview_alb_name       = !local.previews_enabled ? null : (var.previews.alb_name != null ? var.previews.alb_name : "${var.cluster_name}-preview")
  preview_domain         = local.previews_enabled ? "*.${var.previews.host_suffix}" : null

  # EKS gives the cluster DNS Service .10 and the kubernetes Service .1 of the
  # service CIDR. An IPv6 cluster has no IPv4 service CIDR: both must then be
  # set, which the preview node role's precondition enforces.
  preview_dns_cidr = !local.previews_enabled ? null : (
    var.previews.dns_cidr != null ? var.previews.dns_cidr : try("${cidrhost(local.service_cidr, 10)}/32", null)
  )
  preview_api_server_cidr = !local.previews_enabled ? null : (
    var.previews.api_server_cidr != null ? var.previews.api_server_cidr : try("${cidrhost(local.service_cidr, 1)}/32", null)
  )

  preview_certificate_arn = !local.previews_enabled ? null : (
    var.wait_for_certificate_validation ? aws_acm_certificate_validation.preview[0].certificate_arn : aws_acm_certificate.preview[0].arn
  )
}

# The preview ALB's subnets. helm_values pins the ALB to exactly these
# (preview.albSubnetIDs, the IngressClassParams subnets.ids), and their CIDRs
# are the only sources preview pods admit (preview.albSubnetCIDRs). Left to
# discovery, Auto Mode would place the ALB in a kubernetes.io/role/elb subnet
# of every zone it finds one in, and the slot NetworkPolicy would drop
# traffic through any of them the list leaves out.
data "aws_subnet" "preview_alb" {
  for_each = toset(local.previews_enabled ? var.previews.alb_subnet_ids : [])

  id = each.value

  lifecycle {
    postcondition {
      condition     = self.vpc_id == local.vpc_id
      error_message = "Preview ALB subnet ${self.id} is not in the cluster's VPC."
    }
    postcondition {
      condition     = contains(keys(self.tags), "kubernetes.io/role/elb")
      error_message = "Preview ALB subnet ${self.id} is not tagged kubernetes.io/role/elb, which Auto Mode requires of an internet-facing load balancer's subnets."
    }
    postcondition {
      # The chart admits /20 to /32 source ranges only.
      condition     = tonumber(split("/", self.cidr_block)[1]) >= 20
      error_message = "Preview ALB subnet ${self.id} (${self.cidr_block}) is wider than the /20 the chart's albSubnetCIDRs accepts."
    }
  }
}

# The preview nodes' subnets: untrusted workloads must never get public IPs.
data "aws_subnet" "preview_node" {
  for_each = toset(local.previews_enabled ? var.previews.node_subnet_ids : [])

  id = each.value

  lifecycle {
    postcondition {
      condition     = self.vpc_id == local.vpc_id
      error_message = "Preview node subnet ${self.id} is not in the cluster's VPC."
    }
    postcondition {
      condition     = !self.map_public_ip_on_launch
      error_message = "Preview node subnet ${self.id} assigns public IPs on launch: untrusted preview nodes belong in private subnets."
    }
  }
}

resource "aws_iam_role" "preview_node" {
  count = local.previews_enabled ? 1 : 0

  name        = local.preview_node_role_name
  description = "EKS Auto Mode node identity for isolated patchy preview slots"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })

  tags = var.tags

  lifecycle {
    precondition {
      # Only sign-in may stand in for the allowlist, and the chart decides
      # when it may (the require stage, an explicit confirmation and a short
      # session); without preview_auth there is nothing to stand in. Here,
      # not on var.previews, because var.preview_auth's own validations
      # already read var.previews.
      condition     = length(var.previews.inbound_cidrs) >= 1 || var.preview_auth != null
      error_message = "previews.inbound_cidrs must list at least one IPv4 /32 unless preview_auth is set: without sign-in the preview ALB is never open to the internet."
    }
    precondition {
      condition     = local.preview_dns_cidr != null && local.preview_api_server_cidr != null
      error_message = "The cluster has no IPv4 service CIDR to derive the DNS and API server /32s from: set previews.dns_cidr and previews.api_server_cidr."
    }
    precondition {
      # An ALB takes one subnet per Availability Zone. Two listed subnets in
      # one zone would otherwise fail only at Helm stage 2, when Auto Mode
      # creates the ALB. (A data source's postcondition cannot compare its
      # own instances, so the check sits on the preview anchor resource.)
      condition     = length(distinct([for id in var.previews.alb_subnet_ids : data.aws_subnet.preview_alb[id].availability_zone])) == length(var.previews.alb_subnet_ids)
      error_message = "previews.alb_subnet_ids must be in distinct Availability Zones: an ALB takes one subnet per zone."
    }
    precondition {
      # The preview ALB uses IP targets, and an ALB sends no traffic to a
      # target in a zone it has not enabled (target health Target.NotInUse).
      # The NodePool may launch a node in any node subnet, so a node subnet
      # outside the ALB's zones would leave every preview scheduled there
      # unreachable, never Ready, until its rollout times out.
      condition = length(setsubtract(
        [for id in var.previews.node_subnet_ids : data.aws_subnet.preview_node[id].availability_zone],
        [for id in var.previews.alb_subnet_ids : data.aws_subnet.preview_alb[id].availability_zone],
      )) == 0
      error_message = "Every previews.node_subnet_ids subnet must be in an Availability Zone of one of the previews.alb_subnet_ids: the preview ALB sends no traffic to a node in a zone it has not enabled."
    }
  }
}

# The node identity may join this cluster's Pod Identity and pull preview
# runtime images, nothing else: no push, no agent images, no other role.
resource "aws_iam_policy" "preview_node" {
  count = local.previews_enabled ? 1 : 0

  name        = local.preview_node_role_name
  description = "Cluster-scoped node identity and pull-only access to preview runtime images"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ClusterPodIdentity"
        Effect   = "Allow"
        Action   = "eks-auth:AssumeRoleForPodIdentity"
        Resource = local.cluster_arn
      },
      {
        Sid      = "RegistryAuthentication"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid    = "PullPreviewRuntimeOnly"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
        ]
        Resource = local.preview_repository_arns
      },
    ]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "preview_node" {
  count = local.previews_enabled ? 1 : 0

  role       = aws_iam_role.preview_node[0].name
  policy_arn = aws_iam_policy.preview_node[0].arn
}

# Auto Mode nodes launched with this role join the cluster through an EC2
# access entry carrying the Auto Mode node policy.
resource "aws_eks_access_entry" "preview_node" {
  count = local.previews_enabled ? 1 : 0

  cluster_name  = var.cluster_name
  principal_arn = aws_iam_role.preview_node[0].arn
  type          = "EC2"

  tags = var.tags
}

resource "aws_eks_access_policy_association" "preview_node" {
  count = local.previews_enabled ? 1 : 0

  cluster_name  = var.cluster_name
  principal_arn = aws_iam_role.preview_node[0].arn
  policy_arn    = "arn:${local.partition}:eks::aws:cluster-access-policy/AmazonEKSAutoNodePolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.preview_node]
}

# Preview hosts are one label under host_suffix, which this wildcard covers.
resource "aws_acm_certificate" "preview" {
  count = local.previews_enabled ? 1 : 0

  domain_name       = local.preview_domain
  validation_method = "DNS"

  tags = merge(var.tags, { Name = var.previews.host_suffix })

  lifecycle {
    create_before_destroy = true
  }
}

# Keyed by the certificate's domain, which the configuration knows at plan
# time even before ACM has issued the validation record.
resource "aws_route53_record" "preview_validation" {
  for_each = toset(local.previews_enabled ? [local.preview_domain] : [])

  zone_id         = var.previews.zone_id
  name            = one([for dvo in aws_acm_certificate.preview[0].domain_validation_options : dvo.resource_record_name if dvo.domain_name == each.key])
  type            = one([for dvo in aws_acm_certificate.preview[0].domain_validation_options : dvo.resource_record_type if dvo.domain_name == each.key])
  ttl             = 300
  records         = [one([for dvo in aws_acm_certificate.preview[0].domain_validation_options : dvo.resource_record_value if dvo.domain_name == each.key])]
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "preview" {
  count = local.previews_enabled && var.wait_for_certificate_validation ? 1 : 0

  certificate_arn         = aws_acm_certificate.preview[0].arn
  validation_record_fqdns = [for record in aws_route53_record.preview_validation : record.fqdn]
}

# After Helm stage 2. The lookup fails the plan with "no matching LB" until
# Helm has created the preview ALB (preview.enabled with the placeholder
# Ingress on), so it has a flag of its own, apart from the edge aliases.
data "aws_lb" "preview" {
  count = local.previews_enabled && var.create_preview_alias_record ? 1 : 0

  name = local.preview_alb_name
}

resource "aws_route53_record" "preview_wildcard" {
  count = local.previews_enabled && var.create_preview_alias_record ? 1 : 0

  zone_id = var.previews.zone_id
  name    = local.preview_domain
  type    = "A"

  alias {
    name                   = data.aws_lb.preview[0].dns_name
    zone_id                = data.aws_lb.preview[0].zone_id
    evaluate_target_health = false
  }
}
