# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# The GitHub Actions OIDC provider app publishers federate through. One per
# issuer per account, so an account that already has it passes its ARN
# instead. No thumbprint_list: IAM verifies GitHub's JWKS endpoint against
# its library of trusted root CAs, so there is nothing here to rotate.
resource "aws_iam_openid_connect_provider" "github" {
  count = var.github_oidc_provider_arn == null ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]

  tags = merge(var.tags, { Name = "token.actions.githubusercontent.com" })
}

locals {
  github_oidc_provider_arn = coalesce(var.github_oidc_provider_arn, one(aws_iam_openid_connect_provider.github[*].arn))
  source_controller_name   = "${local.name_prefix}-patchy-source-controller"
}

# source-controller resolves a declared agent image's tag to a digest and
# reads its manifest and config. Trusted by the Pod Identity service principal
# and pinned by the session tags Pod Identity sets on every AssumeRole: this
# cluster, the release namespace and the source-controller ServiceAccount. An
# association pointing any other workload at this role fails to assume it.
resource "aws_iam_role" "source_controller" {
  name        = local.source_controller_name
  description = "EKS Pod Identity role for patchy's source-controller: read patchy app-env images in ECR"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowEksAuthToAssumeRoleForPodIdentity"
      Effect    = "Allow"
      Principal = { Service = "pods.eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
      Condition = {
        StringEquals = {
          "aws:RequestTag/eks-cluster-arn"            = local.cluster_arn
          "aws:RequestTag/kubernetes-namespace"       = var.namespace
          "aws:RequestTag/kubernetes-service-account" = var.source_controller_service_account
        }
      }
    }]
  })

  tags = merge(var.tags, { Name = local.source_controller_name })
}

resource "aws_iam_policy" "source_controller" {
  name = "${local.source_controller_name}-ecr-read"
  # Replacing a policy is the only way to change its description, so this
  # keeps the wording existing installs were created with.
  description = "Resolve tags and read manifests/configs of patchy app-env images (${var.agent_path_prefix}/*) only"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Registry-wide by design; it grants no repository access on its own.
        Sid      = "EcrAuthToken"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "ReadPatchyAppEnvImages"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
          "ecr:DescribeImages",
          "ecr:BatchCheckLayerAvailability",
          "ecr:ListImages",
        ]
        Resource = local.agent_repository_arns
      },
    ]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "source_controller" {
  role       = aws_iam_role.source_controller.name
  policy_arn = aws_iam_policy.source_controller.arn
}

# Auto Mode runs the Pod Identity agent natively. The webhook injects the
# credentials when a pod is created, so a running source-controller picks the
# association up only after a restart.
resource "aws_eks_pod_identity_association" "source_controller" {
  cluster_name    = var.cluster_name
  namespace       = var.namespace
  service_account = var.source_controller_service_account
  role_arn        = aws_iam_role.source_controller.arn

  tags = merge(var.tags, { Name = local.source_controller_name })
}
