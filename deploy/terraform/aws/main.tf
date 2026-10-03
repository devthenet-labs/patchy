# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# The AWS side of a patchy install on an EKS Auto Mode cluster with intents
# and previews: the identities, image repositories, certificates and DNS the
# chart's values point at. It configures no provider and no backend; the
# caller does both. Kubernetes objects stay the chart's.
#
#   identity.tf  the GitHub Actions OIDC provider and source-controller's Pod
#                Identity role (ECR read on the agent image prefix)
#   apps.tf      one modules/app per application repository
#   previews.tf  the preview node role and access entry, the wildcard
#                certificate and, in phase 2, the wildcard alias
#   edge.tf      the optional webhook/status certificate and aliases
#   outputs.tf   helm_values, the per-app GitHub variables and the rest

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "aws_region" "current" {}

data "aws_eks_cluster" "this" {
  name = var.cluster_name
}

locals {
  # Only null selects a default: coalesce() would also replace "", hiding an
  # explicitly empty prefix.
  name_prefix          = var.name_prefix != null ? var.name_prefix : var.cluster_name
  app_role_name_prefix = var.app_role_name_prefix != null ? var.app_role_name_prefix : "${local.name_prefix}-app-"

  partition  = data.aws_partition.current.partition
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region
  registry   = "${local.account_id}.dkr.ecr.${local.region}.${data.aws_partition.current.dns_suffix}"

  # Fixed until the chart makes the preview image prefix configurable: its
  # admission policy and the preview-controller pin
  # <preview.imageRegistry>/patchy/previews/.
  preview_path_prefix = "patchy/previews"

  # Every repository under each prefix, including ones added later.
  agent_repository_arns   = "arn:${local.partition}:ecr:${local.region}:${local.account_id}:repository/${var.agent_path_prefix}/*"
  preview_repository_arns = "arn:${local.partition}:ecr:${local.region}:${local.account_id}:repository/${local.preview_path_prefix}/*"

  cluster_arn               = data.aws_eks_cluster.this.arn
  vpc_id                    = data.aws_eks_cluster.this.vpc_config[0].vpc_id
  cluster_security_group_id = data.aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
  service_cidr              = try(data.aws_eks_cluster.this.kubernetes_network_config[0].service_ipv4_cidr, null)
}
