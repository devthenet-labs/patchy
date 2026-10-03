# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Preview infrastructure, all of it behind var.previews:
#   - a dedicated Auto Mode node identity for the isolated preview NodeClass,
#     which can join only this cluster and pull only patchy/previews/*;
#   - the subnets the preview NodeClass and the preview ALB use, checked;
#   - the *.<host_suffix> certificate the preview ALB terminates TLS with;
#   - phase 2: the wildcard alias to the preview ALB, which Auto Mode creates
#     from the chart's placeholder Ingress, so Terraform only looks it up.

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

# The preview ALB's subnets: their CIDRs are the only sources preview pods
# admit (preview.albSubnetCIDRs). The chart's IngressClassParams names no
# subnets, so Auto Mode places the ALB by the kubernetes.io/role/elb tag.
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
      error_message = "Preview ALB subnet ${self.id} is not tagged kubernetes.io/role/elb, so Auto Mode would not place the internet-facing preview ALB in it."
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
      condition     = local.preview_dns_cidr != null && local.preview_api_server_cidr != null
      error_message = "The cluster has no IPv4 service CIDR to derive the DNS and API server /32s from: set previews.dns_cidr and previews.api_server_cidr."
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

# Phase 2. The lookup fails the plan with "no matching LB" until Helm has
# created the preview ALB (preview.enabled with the placeholder Ingress on).
data "aws_lb" "preview" {
  count = local.previews_enabled && var.create_alias_records ? 1 : 0

  name = local.preview_alb_name
}

resource "aws_route53_record" "preview_wildcard" {
  count = local.previews_enabled && var.create_alias_records ? 1 : 0

  zone_id = var.previews.zone_id
  name    = local.preview_domain
  type    = "A"

  alias {
    name                   = data.aws_lb.preview[0].dns_name
    zone_id                = data.aws_lb.preview[0].zone_id
    evaluate_target_health = false
  }
}
