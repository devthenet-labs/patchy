# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

locals {
  # Infrastructure-derived chart values only. Every enable flag and every
  # security opt-out (agent.repositoryImages.enabled, allowUnsigned or
  # cosignPublicKey, ephemeralStorage, broadEgress, the runners and the
  # broker, preview.enabled, previewController.enabled, intentController,
  # the ingress classes) stays an explicit line in the operator's own values
  # file. Each optional block is a zero-or-one element list, all of them
  # expanded into one merge(): a conditional cannot return objects of
  # different shapes.
  helm_values = merge(concat(
    [{
      # Pinned by name, so the Pod Identity association always binds the
      # ServiceAccount the chart renders, whatever the release is called.
      sourceController = {
        serviceAccount = {
          name = var.source_controller_service_account
        }
      }
      agent = {
        repositoryImages = {
          registries = ["${local.registry}/${var.agent_path_prefix}/"]
        }
      }
    }],
    [for previews in(local.previews_enabled ? [var.previews] : []) : {
      preview = merge({
        imageRegistry   = local.registry
        imagePathPrefix = var.preview_path_prefix
        dnsCIDR         = local.preview_dns_cidr
        albSubnetCIDRs  = [for id in previews.alb_subnet_ids : data.aws_subnet.preview_alb[id].cidr_block]
        # Pins the ALB to the subnets whose CIDRs the line above admits.
        albSubnetIDs   = [for id in previews.alb_subnet_ids : data.aws_subnet.preview_alb[id].id]
        inboundCIDRs   = previews.inbound_cidrs
        certificateARN = local.preview_certificate_arn
        hostSuffix     = previews.host_suffix
        albName        = local.preview_alb_name
        },
        # Only when set, so an install without prefix lists renders as before.
        length(previews.prefix_list_ids) == 0 ? {} : { prefixListsIDs = previews.prefix_list_ids },
      )
      previewController = {
        config = {
          apiServerCIDR = local.preview_api_server_cidr
        }
      }
    }],
    [for edge in(local.edge_enabled ? [var.edge] : []) : {
      webhook = {
        host    = edge.webhook_host
        ingress = { annotations = local.edge_ingress_annotations }
      }
    }],
    [for host in(local.edge_enabled ? compact([var.edge.status_host]) : []) : {
      statusServer = {
        host    = host
        ingress = { annotations = local.edge_ingress_annotations }
      }
    }],
    # The relay's host and edge Ingress only: previewAuth.enabled, its stage,
    # Dex and the viewers stay in the operator's values.
    [for auth in(local.edge_enabled && var.preview_auth != null ? [var.preview_auth] : []) : {
      previewAuth = {
        host    = auth.host
        ingress = { annotations = local.edge_ingress_annotations }
      }
    }],
  )...)

  # HTTPS on the edge ALB with the edge certificate; plain HTTP only redirects.
  # listen-ports is a literal in the annotation's usual spelling (jsonencode
  # drops the spaces), so an install that set it by hand before adopting
  # helm_values renders the same Ingress.
  edge_ingress_annotations = {
    "alb.ingress.kubernetes.io/certificate-arn" = local.edge_certificate_arn
    "alb.ingress.kubernetes.io/listen-ports"    = "[{\"HTTP\": 80}, {\"HTTPS\": 443}]"
    "alb.ingress.kubernetes.io/ssl-redirect"    = "443"
  }
}

output "helm_values" {
  description = "YAML values for the patchy chart, derived from the infrastructure: pass it last, after the operator's own values file (`helm install ... -f operator-values.yaml -f <(terraform output -raw helm_values)`). It carries no enable flag and no security opt-out."
  value       = yamlencode(local.helm_values)
}

output "github_variables" {
  description = "Per app slug, the repository variables its publish workflows read, as a map: AWS_REGION, ECR_REGISTRY, PUBLISH_REPOSITORY_ID, PUBLISH_OWNER_ID, AGENT_IMAGE_REPOSITORY, AGENT_ROLE_ARN and, with previews, RUNTIME_IMAGE_REPOSITORY and RUNTIME_ROLE_ARN."
  value       = { for slug, app in module.app : slug => app.github_variables }
}

output "github_variables_dotenv" {
  description = "Per app slug, the same variables as a sorted dotenv string: `terraform output -json github_variables_dotenv`, piped through `jq -r '.\"<slug>\"'` to `gh variable set --repo <owner>/<name> -f -`"
  value       = { for slug, app in module.app : slug => app.github_variables_dotenv }
}

output "apps" {
  description = "Per app slug: its GitHub repository, image repository URLs, publisher role ARNs and the OIDC subject the roles trust. The runtime values are null for an app without previews."
  value = {
    for slug, app in module.app : slug => {
      github_repository      = "${var.apps[slug].github.owner}/${var.apps[slug].github.name}"
      agent_repository_url   = app.agent_repository_url
      runtime_repository_url = app.runtime_repository_url
      agent_role_arn         = app.agent_role_arn
      runtime_role_arn       = app.runtime_role_arn
      oidc_subject           = app.oidc_subject
    }
  }
}

output "registry" {
  description = "The account's ECR registry host (`<account>.dkr.ecr.<region>.amazonaws.com`)"
  value       = local.registry
}

output "github_oidc_provider_arn" {
  description = "ARN of the GitHub Actions OIDC provider the publisher roles trust: the one this module created, or the one it was given"
  value       = local.github_oidc_provider_arn
}

output "source_controller_role_arn" {
  description = "IAM role source-controller assumes through EKS Pod Identity (read-only on `<agent_path_prefix>/*`)"
  value       = aws_iam_role.source_controller.arn
}

output "preview_node_class" {
  description = "What the isolated preview NodeClass names: the node role (spec.role), the private subnets (subnetSelectorTerms) and the cluster's primary security group (securityGroupSelectorTerms). Null without previews."
  value = !local.previews_enabled ? null : {
    role               = aws_iam_role.preview_node[0].name
    role_arn           = aws_iam_role.preview_node[0].arn
    subnet_ids         = [for id in var.previews.node_subnet_ids : data.aws_subnet.preview_node[id].id]
    security_group_ids = [local.cluster_security_group_id]
  }
}

output "preview_certificate_arn" {
  description = "ACM certificate for `*.<previews.host_suffix>` (helm_values preview.certificateARN); null without previews"
  value       = local.preview_certificate_arn
}

output "edge_certificate_arn" {
  description = "ACM certificate for the webhook, status and preview sign-in relay hosts (helm_values edge annotations); null without edge"
  value       = local.edge_certificate_arn
}

output "preview_auth_dex_redirect_uri" {
  description = "The one redirect URI of Dex's static client for the preview sign-in relay, https://<preview_auth.host>/dex/callback: the only per-install IdP setup preview sign-in needs. Null without preview_auth."
  value       = var.preview_auth == null ? null : "https://${var.preview_auth.host}/dex/callback"
}
