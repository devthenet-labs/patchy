# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# One app's patchy images. The slug, not the GitHub name, names every AWS
# resource:
#   - an agent toolchain repository, <agent_path_prefix>/<slug>. The app's
#     .patchy/agent.yaml names it, and source-controller may read it (the
#     platform module grants read on the whole agent prefix);
#   - with previews, a runtime repository, patchy/previews/<slug>. Only the
#     isolated preview nodes pull from it;
#   - one trusted publisher role per repository. Each trusts only the app's
#     own default-branch publisher workflow for that kind, so the runtime
#     publisher can never write an agent image, and no app can write another
#     app's repositories.

data "aws_region" "current" {}

locals {
  # Fixed in this release: the chart's preview admission policy and the
  # preview-controller require exactly this path under preview.imageRegistry.
  preview_path_prefix = "patchy/previews"

  # One image repository and one publisher per kind. Each kind has its own
  # trusted reusable workflow in the app repository, named in the role's
  # trust as job_workflow_ref. That workflow is a workflow_call callee of the
  # publish-images.yml dispatcher: GitHub sets job_workflow_ref to the callee
  # and workflow_ref to the dispatcher, so the trust never pins workflow_ref.
  images = merge(
    {
      agent = {
        repository = "${var.agent_path_prefix}/${var.slug}"
        workflow   = "publish-agent.yml"
      }
    },
    var.preview ? {
      runtime = {
        repository = "${local.preview_path_prefix}/${var.slug}"
        workflow   = "publish-runtime.yml"
      }
    } : {},
  )

  default_ref = "refs/heads/${var.github.default_branch}"

  # GitHub's immutable subject for a job on the default branch, where
  # workflow_run publishers always run:
  #   repo:<owner>@<owner_id>/<name>@<repository_id>:ref:refs/heads/<branch>
  subject = "${var.github.sub_claim_prefix}:ref:${local.default_ref}"
}

# Immutable tags: a tag patchy resolved to a digest keeps meaning the same
# bits. force_delete stays off, so destroying a repository that still holds
# images fails instead of deleting them.
resource "aws_ecr_repository" "this" {
  for_each = local.images

  name                 = each.value.repository
  image_tag_mutability = "IMMUTABLE"
  force_delete         = false

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = merge(var.tags, {
    Name = each.value.repository
  })
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each = aws_ecr_repository.this

  repository = each.value.name
  # Untagged images are superseded pushes and build leftovers. Runtime images:
  # a PR head's sha- image outlives its preview (72 hours) by far; default
  # branch commits are also tagged main-<sha>, and the newest 20 of those are
  # kept past the sha- expiry, because a preview runs the default branch's
  # image for a repository the intent did not change. A rule's tag match
  # shields an image from every lower-priority rule.
  #
  # Agent repositories get the untagged rule only: their toolchain-v<N> tag
  # is pinned in .patchy/agent.yaml and must never expire.
  policy = jsonencode({
    rules = concat(
      [{
        rulePriority = 1
        description  = "Expire untagged build leftovers after 14 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 14
        }
        action = { type = "expire" }
      }],
      # Runtime repositories only (a filter: the two rules differ in shape).
      [for rule in [
        {
          rulePriority = 2
          description  = "Keep the newest 20 main-branch images"
          selection = {
            tagStatus     = "tagged"
            tagPrefixList = ["main-"]
            countType     = "imageCountMoreThan"
            countNumber   = 20
          }
          action = { type = "expire" }
        },
        {
          rulePriority = 3
          description  = "Expire PR-head preview images after 30 days"
          selection = {
            tagStatus     = "tagged"
            tagPrefixList = ["sha-"]
            countType     = "sinceImagePushed"
            countUnit     = "days"
            countNumber   = 30
          }
          action = { type = "expire" }
        },
      ] : rule if each.key == "runtime"],
    )
  })
}

resource "aws_iam_role" "publisher" {
  for_each = local.images

  name                 = "${var.role_name_prefix}${var.slug}-${each.key}-push"
  description          = "Trusted ${var.github.default_branch}-branch ${each.key} image publisher for ${var.github.owner}/${var.github.name} only"
  max_session_duration = 3600
  # The publisher runs on workflow_run, from the default branch's trusted
  # workflow, never the PR's. Every claim is pinned: the subject, the numeric
  # repository and owner IDs (a repository later created under a reused name
  # cannot assume it), the default branch, and this kind's own reusable
  # workflow, which keeps the runtime publisher out of the agent role.
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "GitHubActionsTrustedPublisher"
      Effect    = "Allow"
      Action    = "sts:AssumeRoleWithWebIdentity"
      Principal = { Federated = var.github_oidc_provider_arn }
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud"                 = "sts.amazonaws.com"
          "token.actions.githubusercontent.com:sub"                 = local.subject
          "token.actions.githubusercontent.com:repository_id"       = var.github.repository_id
          "token.actions.githubusercontent.com:repository_owner_id" = var.github.owner_id
          "token.actions.githubusercontent.com:ref"                 = local.default_ref
          "token.actions.githubusercontent.com:job_workflow_ref"    = "${var.github.owner}/${var.github.name}/.github/workflows/${each.value.workflow}@${local.default_ref}"
        }
      }
    }]
  })

  tags = merge(var.tags, {
    Name = "${var.role_name_prefix}${var.slug}-${each.key}-push"
  })
}

resource "aws_iam_policy" "publisher" {
  for_each = local.images

  name        = "${var.role_name_prefix}${var.slug}-${each.key}-push"
  description = "Publish only ${each.value.repository}; no delete, other repository or cluster access"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Registry-wide by design; it grants no repository access on its own.
        Sid      = "RegistryAuthentication"
        Effect   = "Allow"
        Action   = "ecr:GetAuthorizationToken"
        Resource = "*"
      },
      {
        Sid    = "PublishOwnImageOnly"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:InitiateLayerUpload",
          "ecr:UploadLayerPart",
          "ecr:CompleteLayerUpload",
          "ecr:PutImage",
          "ecr:BatchGetImage",
          "ecr:DescribeImages",
        ]
        Resource = aws_ecr_repository.this[each.key].arn
      },
    ]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "publisher" {
  for_each = local.images

  role       = aws_iam_role.publisher[each.key].name
  policy_arn = aws_iam_policy.publisher[each.key].arn
}
