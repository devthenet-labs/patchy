# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Every check here is a variable validation, so a bad input fails the plan
# before anything is created (a check block would only warn).

variable "slug" {
  description = "Short name for the app. It is the leaf of both ECR repository names and part of both IAM role names, and it is independent of the GitHub repository name (Hello.Web -> hello-web). A rename on GitHub never changes it."
  type        = string

  validation {
    # The preview-controller's own grammar for an image leaf (one DNS-label
    # segment), which is also a valid ECR path segment.
    condition     = length(var.slug) <= 63 && can(regex("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", var.slug))
    error_message = "slug must be lowercase letters, digits and hyphens, starting and ending with a letter or digit, at most 63 characters (for example \"hello-web\" for a repository named Hello.Web)."
  }
}

variable "github" {
  description = <<-EOT
    The app's GitHub repository. Take every value from the API, exactly as it is returned (IAM compares them
    case-sensitively):
    - owner, name, repository_id, owner_id, default_branch: from `gh api repos/<owner>/<name>`, as `.owner.login`,
      `.name`, `.id`, `.owner.id` and `.default_branch`
    - sub_claim_prefix: `gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix`
  EOT
  type = object({
    owner            = string
    name             = string
    repository_id    = string
    owner_id         = string
    default_branch   = optional(string, "main")
    sub_claim_prefix = string
  })

  validation {
    condition     = can(regex("^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$", var.github.owner))
    error_message = "github.owner must be a GitHub user or organization login."
  }
  validation {
    condition     = can(regex("^[A-Za-z0-9._-]{1,100}$", var.github.name)) && !contains([".", ".."], var.github.name)
    error_message = "github.name must be a GitHub repository name: 1-100 letters, digits, '.', '_' or '-'."
  }
  validation {
    condition     = can(regex("^[1-9][0-9]{0,18}$", var.github.repository_id)) && can(regex("^[1-9][0-9]{0,18}$", var.github.owner_id))
    error_message = "github.repository_id and github.owner_id must be the numeric IDs from the API (.id and .owner.id), not names."
  }
  validation {
    # A ref name usable verbatim in sub, ref and job_workflow_ref. IAM's
    # StringEquals has no wildcards, so a '*' here could never match.
    condition     = can(regex("^[A-Za-z0-9._/-]{1,255}$", var.github.default_branch)) && !strcontains(var.github.default_branch, "..") && !startswith(var.github.default_branch, "/") && !endswith(var.github.default_branch, "/")
    error_message = "github.default_branch must be a plain branch name such as \"main\"."
  }
  validation {
    # The roles trust GitHub's immutable subject only, whose numeric IDs a
    # renamed or re-created repository cannot reproduce; the module appends
    # ":ref:refs/heads/<default_branch>". The value is required, and must
    # equal the form built from the other inputs, so that the operator reads
    # it from the API: a repository still on the classic repo:<owner>/<name>
    # subject (or a custom include_claim_keys template) fails here, at plan
    # time, instead of at its first AssumeRoleWithWebIdentity. It also
    # catches a prefix copied from another repository.
    condition     = var.github.sub_claim_prefix == "repo:${var.github.owner}@${var.github.owner_id}/${var.github.name}@${var.github.repository_id}"
    error_message = "github.sub_claim_prefix must be the repository's immutable OIDC subject prefix, repo:<owner>@<owner_id>/<name>@<repository_id>, built from this repository's own values. Read it with: gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix. A repository whose prefix is the classic repo:<owner>/<name> (use_immutable_subject false), or that uses a custom include_claim_keys template, must switch to immutable subject claims first."
  }
}

variable "github_oidc_provider_arn" {
  description = "ARN of the account's IAM OIDC provider for token.actions.githubusercontent.com. The platform module creates one, or passes on the existing one it was given."
  type        = string

  validation {
    condition     = can(regex("^arn:aws[a-z-]*:iam::[0-9]{12}:oidc-provider/token\\.actions\\.githubusercontent\\.com$", var.github_oidc_provider_arn))
    error_message = "github_oidc_provider_arn must be the ARN of an IAM OIDC provider for token.actions.githubusercontent.com."
  }
}

variable "preview" {
  description = "Whether the app has previews: a runtime ECR repository under patchy/previews/ and its own runtime publisher role. The agent repository and role are always created."
  type        = bool
  default     = true
}

variable "agent_path_prefix" {
  description = "ECR path the agent image repository goes under (no leading or trailing slash). Must match the platform module's agent_path_prefix, and must never overlap patchy/previews."
  type        = string
  default     = "patchy/app-envs"

  validation {
    condition     = can(regex("^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$", var.agent_path_prefix))
    error_message = "agent_path_prefix must be one or more ECR path segments separated by '/', with no leading or trailing slash (for example \"patchy/app-envs\")."
  }
  validation {
    # A runtime image is built from an unreviewed same-repository PR head. It
    # must never be admissible as an agent sandbox image, and preview nodes
    # must never be able to pull agent toolchain images, so neither prefix may
    # contain the other.
    condition     = !startswith("${var.agent_path_prefix}/", "patchy/previews/") && !startswith("patchy/previews/", "${var.agent_path_prefix}/")
    error_message = "agent_path_prefix must not be patchy/previews, contain it or sit under it: agent images and preview images must have disjoint prefixes."
  }
}

variable "role_name_prefix" {
  description = "Prefix of both IAM role and policy names: `<role_name_prefix><slug>-agent-push` and `<role_name_prefix><slug>-runtime-push`. IAM names are account-wide, so the prefix keeps two installs in one account apart."
  type        = string
  default     = "patchy-app-"

  validation {
    condition     = can(regex("^[A-Za-z0-9+=,.@_-]*$", var.role_name_prefix))
    error_message = "role_name_prefix may only contain the characters IAM allows in a role name: letters, digits and +=,.@_-"
  }
  validation {
    # IAM role names are at most 64 characters; "-runtime-push" is 13.
    condition     = length("${var.role_name_prefix}${var.slug}-runtime-push") <= 64
    error_message = "The IAM role name ${var.role_name_prefix}${var.slug}-runtime-push is longer than IAM's 64 characters: shorten role_name_prefix or the slug."
  }
}

variable "tags" {
  description = "Tags added to every taggable resource."
  type        = map(string)
  default     = {}
}
