# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Offline, deterministic: the AWS provider is mocked, so these run with no
# credentials and no network beyond the provider download for its schema.

mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "eu-west-2"
    }
  }
  mock_resource "aws_ecr_repository" {
    defaults = {
      arn            = "arn:aws:ecr:eu-west-2:111122223333:repository/mocked"
      repository_url = "111122223333.dkr.ecr.eu-west-2.amazonaws.com/mocked"
    }
  }
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::111122223333:role/mocked"
    }
  }
  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::111122223333:policy/mocked"
    }
  }
}

# A mixed-case, punctuated GitHub name on purpose: IAM compares the claims
# case-sensitively, so the module must carry the name through verbatim.
variables {
  slug = "hello-web"
  github = {
    owner            = "Acme-Org"
    name             = "Hello.Web"
    repository_id    = "123456789"
    owner_id         = "987654"
    sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
  }
  github_oidc_provider_arn = "arn:aws:iam::111122223333:oidc-provider/token.actions.githubusercontent.com"
}

run "publisher_trust_pins_every_claim" {
  command = plan

  assert {
    condition = jsondecode(aws_iam_role.publisher["agent"].assume_role_policy).Statement[0].Condition.StringEquals == {
      "token.actions.githubusercontent.com:aud"                 = "sts.amazonaws.com"
      "token.actions.githubusercontent.com:sub"                 = "repo:Acme-Org@987654/Hello.Web@123456789:ref:refs/heads/main"
      "token.actions.githubusercontent.com:repository_id"       = "123456789"
      "token.actions.githubusercontent.com:repository_owner_id" = "987654"
      "token.actions.githubusercontent.com:ref"                 = "refs/heads/main"
      "token.actions.githubusercontent.com:job_workflow_ref"    = "Acme-Org/Hello.Web/.github/workflows/publish-agent.yml@refs/heads/main"
    }
    error_message = "The agent publisher's trust must pin exactly aud, sub, repository_id, repository_owner_id, ref and job_workflow_ref."
  }

  assert {
    condition = jsondecode(aws_iam_role.publisher["runtime"].assume_role_policy).Statement[0].Condition.StringEquals == {
      "token.actions.githubusercontent.com:aud"                 = "sts.amazonaws.com"
      "token.actions.githubusercontent.com:sub"                 = "repo:Acme-Org@987654/Hello.Web@123456789:ref:refs/heads/main"
      "token.actions.githubusercontent.com:repository_id"       = "123456789"
      "token.actions.githubusercontent.com:repository_owner_id" = "987654"
      "token.actions.githubusercontent.com:ref"                 = "refs/heads/main"
      "token.actions.githubusercontent.com:job_workflow_ref"    = "Acme-Org/Hello.Web/.github/workflows/publish-runtime.yml@refs/heads/main"
    }
    error_message = "The runtime publisher's trust must pin the same claims, with its own workflow."
  }

  assert {
    condition = alltrue([for kind, role in aws_iam_role.publisher : (
      jsondecode(role.assume_role_policy).Statement[0].Principal.Federated == var.github_oidc_provider_arn &&
      jsondecode(role.assume_role_policy).Statement[0].Action == "sts:AssumeRoleWithWebIdentity" &&
      length(jsondecode(role.assume_role_policy).Statement) == 1 &&
      length(jsondecode(role.assume_role_policy).Statement[0].Condition) == 1 &&
      role.max_session_duration == 3600
    )])
    error_message = "Each publisher role must have one statement, trusting only the GitHub OIDC provider, with StringEquals as its only condition operator."
  }

  assert {
    condition = (
      aws_iam_role.publisher["agent"].name == "patchy-app-hello-web-agent-push" &&
      aws_iam_role.publisher["runtime"].name == "patchy-app-hello-web-runtime-push" &&
      alltrue([for role in aws_iam_role.publisher : role.path == null])
    )
    error_message = "Role names are <role_name_prefix><slug>-<kind>-push, with no IAM path (the provider's default /), so the role ARNs the app repository stores are arn:aws:iam::<account>:role/<name>."
  }

  assert {
    condition     = aws_ecr_repository.this["agent"].name == "patchy/app-envs/hello-web" && aws_ecr_repository.this["runtime"].name == "patchy/previews/hello-web"
    error_message = "Repositories are <agent_path_prefix>/<slug> and patchy/previews/<slug>."
  }

  assert {
    condition = alltrue([for repo in aws_ecr_repository.this : (
      repo.image_tag_mutability == "IMMUTABLE" && repo.force_delete == false && repo.image_scanning_configuration[0].scan_on_push
    )])
    error_message = "Every repository has immutable tags, scan on push and no force delete."
  }
}

run "push_policy_covers_only_its_own_repository" {
  command = apply

  assert {
    condition = alltrue([for kind, policy in aws_iam_policy.publisher : (
      jsondecode(policy.policy).Statement[0].Action == "ecr:GetAuthorizationToken" &&
      jsondecode(policy.policy).Statement[0].Resource == "*" &&
      jsondecode(policy.policy).Statement[1].Resource == aws_ecr_repository.this[kind].arn &&
      toset(jsondecode(policy.policy).Statement[1].Action) == toset([
        "ecr:BatchCheckLayerAvailability",
        "ecr:InitiateLayerUpload",
        "ecr:UploadLayerPart",
        "ecr:CompleteLayerUpload",
        "ecr:PutImage",
        "ecr:BatchGetImage",
        "ecr:DescribeImages",
      ])
    )])
    error_message = "A publisher may authenticate registry-wide, and push, read back and describe only its own repository, with no delete."
  }
}

run "agent_repository_never_expires_tagged_images" {
  command = plan

  assert {
    condition = (
      length(jsondecode(aws_ecr_lifecycle_policy.this["agent"].policy).rules) == 1 &&
      jsondecode(aws_ecr_lifecycle_policy.this["agent"].policy).rules[0].selection.tagStatus == "untagged"
    )
    error_message = "The agent repository must expire untagged images only: a sha- (or any tagged) rule would silently expire the pinned toolchain image."
  }

  assert {
    condition = [for rule in jsondecode(aws_ecr_lifecycle_policy.this["runtime"].policy).rules : {
      tag   = try(rule.selection.tagPrefixList[0], "")
      count = rule.selection.countNumber
      }] == [
      { tag = "", count = 14 },
      { tag = "main-", count = 20 },
      { tag = "sha-", count = 30 },
    ]
    error_message = "The runtime repository expires untagged images after 14 days, keeps the newest 20 main- images, and expires sha- images after 30 days."
  }
}

run "github_variables_contract" {
  command = apply

  assert {
    condition = output.github_variables == {
      AWS_REGION               = "eu-west-2"
      ECR_REGISTRY             = "111122223333.dkr.ecr.eu-west-2.amazonaws.com"
      PUBLISH_REPOSITORY_ID    = "123456789"
      PUBLISH_OWNER_ID         = "987654"
      AGENT_IMAGE_REPOSITORY   = "patchy/app-envs/hello-web"
      AGENT_ROLE_ARN           = "arn:aws:iam::111122223333:role/mocked"
      RUNTIME_IMAGE_REPOSITORY = "patchy/previews/hello-web"
      RUNTIME_ROLE_ARN         = "arn:aws:iam::111122223333:role/mocked"
    }
    error_message = "github_variables must carry exactly the eight names the generated publish workflows read."
  }

  assert {
    condition     = startswith(output.github_variables_dotenv, "AGENT_IMAGE_REPOSITORY=patchy/app-envs/hello-web\nAGENT_ROLE_ARN=") && endswith(output.github_variables_dotenv, "RUNTIME_ROLE_ARN=arn:aws:iam::111122223333:role/mocked\n") && length(split("\n", output.github_variables_dotenv)) == 9
    error_message = "github_variables_dotenv is one sorted NAME=value line per variable."
  }
}

run "agent_only_app" {
  command = apply

  variables {
    preview = false
  }

  assert {
    condition     = keys(aws_ecr_repository.this) == ["agent"] && keys(aws_iam_role.publisher) == ["agent"]
    error_message = "Without previews only the agent repository and role exist."
  }

  assert {
    condition     = sort(keys(output.github_variables)) == tolist(["AGENT_IMAGE_REPOSITORY", "AGENT_ROLE_ARN", "AWS_REGION", "ECR_REGISTRY", "PUBLISH_OWNER_ID", "PUBLISH_REPOSITORY_ID"])
    error_message = "Without previews github_variables has no RUNTIME_ names."
  }

  assert {
    condition     = output.runtime_repository_url == null && output.runtime_role_arn == null
    error_message = "Without previews the runtime outputs are null."
  }
}

run "custom_default_branch" {
  command = plan

  variables {
    github = {
      owner            = "Acme-Org"
      name             = "Hello.Web"
      repository_id    = "123456789"
      owner_id         = "987654"
      default_branch   = "trunk"
      sub_claim_prefix = "repo:Acme-Org@987654/Hello.Web@123456789"
    }
  }

  assert {
    condition = alltrue([for role in aws_iam_role.publisher : (
      jsondecode(role.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] == "repo:Acme-Org@987654/Hello.Web@123456789:ref:refs/heads/trunk" &&
      jsondecode(role.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:ref"] == "refs/heads/trunk"
    )])
    error_message = "A non-main default branch carries through to sub and ref on both roles."
  }

  assert {
    condition = (
      jsondecode(aws_iam_role.publisher["agent"].assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:job_workflow_ref"] == "Acme-Org/Hello.Web/.github/workflows/publish-agent.yml@refs/heads/trunk" &&
      jsondecode(aws_iam_role.publisher["runtime"].assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:job_workflow_ref"] == "Acme-Org/Hello.Web/.github/workflows/publish-runtime.yml@refs/heads/trunk"
    )
    error_message = "A non-main default branch carries through to each kind's job_workflow_ref."
  }
}

# The roles trust the immutable subject only: a repository still on the
# classic repo:<owner>/<name> subject must switch before it can publish, and
# finds out at plan time rather than at its first AssumeRoleWithWebIdentity.
run "classic_subject_is_refused" {
  command = plan

  variables {
    github = {
      owner            = "Acme-Org"
      name             = "Hello.Web"
      repository_id    = "123456789"
      owner_id         = "987654"
      sub_claim_prefix = "repo:Acme-Org/Hello.Web"
    }
  }

  expect_failures = [var.github]
}

run "slug_rejects_a_github_name" {
  command = plan

  variables {
    slug = "Hello.Web"
  }

  expect_failures = [var.slug]
}

run "slug_rejects_edge_hyphens" {
  command = plan

  variables {
    slug = "app-"
  }

  expect_failures = [var.slug]
}

run "role_names_stay_within_iam_limit" {
  command = plan

  variables {
    role_name_prefix = "a-very-long-organisation-name-patchy-app-"
    slug             = "hello-web-frontend"
  }

  expect_failures = [var.role_name_prefix]
}

run "agent_prefix_must_not_contain_previews" {
  command = plan

  variables {
    agent_path_prefix = "patchy"
  }

  expect_failures = [var.agent_path_prefix]
}

run "agent_prefix_must_not_sit_under_previews" {
  command = plan

  variables {
    agent_path_prefix = "patchy/previews/agents"
  }

  expect_failures = [var.agent_path_prefix]
}

run "agent_prefix_must_be_a_path" {
  command = plan

  variables {
    agent_path_prefix = "/patchy/app-envs/"
  }

  expect_failures = [var.agent_path_prefix]
}

run "subject_prefix_from_another_repository_is_refused" {
  command = plan

  variables {
    github = {
      owner            = "Acme-Org"
      name             = "Hello.Web"
      repository_id    = "123456789"
      owner_id         = "987654"
      sub_claim_prefix = "repo:Acme-Org@987654/Other@555"
    }
  }

  expect_failures = [var.github]
}

run "agent_prefix_must_not_equal_previews" {
  command = plan

  variables {
    agent_path_prefix = "patchy/previews"
  }

  expect_failures = [var.agent_path_prefix]
}

run "sibling_prefix_sharing_a_stem_is_disjoint" {
  command = plan

  # patchy/previews-agents shares a string prefix with patchy/previews but no
  # path segment, so the two stay disjoint.
  variables {
    agent_path_prefix = "patchy/previews-agents"
  }

  assert {
    condition     = aws_ecr_repository.this["agent"].name == "patchy/previews-agents/hello-web"
    error_message = "A sibling path is disjoint from patchy/previews and accepted."
  }
}

run "oidc_provider_for_another_issuer_is_refused" {
  command = plan

  variables {
    github_oidc_provider_arn = "arn:aws:iam::111122223333:oidc-provider/gitlab.com"
  }

  expect_failures = [var.github_oidc_provider_arn]
}

run "oidc_subject_output_names_the_trusted_subject" {
  command = plan

  assert {
    condition     = output.oidc_subject == "repo:Acme-Org@987654/Hello.Web@123456789:ref:refs/heads/main"
    error_message = "oidc_subject is the subject both roles trust."
  }
}

run "subject_prefix_case_must_match" {
  command = plan

  variables {
    github = {
      owner            = "Acme-Org"
      name             = "Hello.Web"
      repository_id    = "123456789"
      owner_id         = "987654"
      sub_claim_prefix = "repo:acme-org/hello.web"
    }
  }

  expect_failures = [var.github]
}
