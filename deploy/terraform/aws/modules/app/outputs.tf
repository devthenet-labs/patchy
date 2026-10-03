# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

locals {
  registry = split("/", aws_ecr_repository.this["agent"].repository_url)[0]

  # The repository variables the app's publish workflows read. Nothing here is
  # secret: the role trust, not these values, decides who can publish. The
  # operator-flipped gates AGENT_PUBLISH_ENABLED and PREVIEW_PUBLISH_ENABLED
  # are deliberately absent.
  github_variables = merge(
    {
      AWS_REGION             = data.aws_region.current.region
      ECR_REGISTRY           = local.registry
      PUBLISH_REPOSITORY_ID  = var.github.repository_id
      PUBLISH_OWNER_ID       = var.github.owner_id
      AGENT_IMAGE_REPOSITORY = aws_ecr_repository.this["agent"].name
      AGENT_ROLE_ARN         = aws_iam_role.publisher["agent"].arn
    },
    var.preview ? {
      RUNTIME_IMAGE_REPOSITORY = aws_ecr_repository.this["runtime"].name
      RUNTIME_ROLE_ARN         = aws_iam_role.publisher["runtime"].arn
    } : {},
  )
}

output "slug" {
  description = "The app's slug"
  value       = var.slug
}

output "registry" {
  description = "ECR registry host (<account>.dkr.ecr.<region>.amazonaws.com)"
  value       = local.registry
}

output "agent_repository_url" {
  description = "Agent image repository URL. .patchy/agent.yaml names <this>:toolchain-v<N>; tags are immutable, so a toolchain change bumps N"
  value       = aws_ecr_repository.this["agent"].repository_url
}

output "runtime_repository_url" {
  description = "Runtime (preview) image repository URL, the Project's preview imageRepository; null without previews"
  value       = var.preview ? aws_ecr_repository.this["runtime"].repository_url : null
}

output "agent_role_arn" {
  description = "IAM role the agent publisher (.github/workflows/publish-agent.yml) assumes"
  value       = aws_iam_role.publisher["agent"].arn
}

output "runtime_role_arn" {
  description = "IAM role the runtime publisher (.github/workflows/publish-runtime.yml) assumes; null without previews"
  value       = var.preview ? aws_iam_role.publisher["runtime"].arn : null
}

output "oidc_subject" {
  description = "The OIDC subject both publisher roles trust. A first publish failing with \"Not authorized to perform sts:AssumeRoleWithWebIdentity\" usually means it differs from the repository's real subject"
  value       = local.subject
}

output "github_variables" {
  description = "The app repository's Actions variables, as a map"
  value       = local.github_variables
}

output "github_variables_dotenv" {
  description = "The same variables as a sorted dotenv string, for: gh variable set --repo <owner>/<name> -f <file>"
  value       = join("", [for name in sort(keys(local.github_variables)) : "${name}=${local.github_variables[name]}\n"])
}
