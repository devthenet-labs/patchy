<!--
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
-->

# modules/app: one application's patchy images

Creates, for one application repository:

- an agent toolchain ECR repository, `<agent_path_prefix>/<slug>`;
- with `preview = true` (the default), a runtime ECR repository, `patchy/previews/<slug>`;
- one trusted publisher role per repository, `<role_name_prefix><slug>-<agent|runtime>-push`, with no IAM path. The
  agent role trusts only `.github/workflows/publish-agent.yml` and the runtime role only
  `.github/workflows/publish-runtime.yml`, each on the repository's default branch.

Every repository has immutable tags, scans on push and has no `force_delete`. Untagged images expire after 14 days.
Runtime repositories also keep the newest 20 `main-` images and expire `sha-` images after 30 days. Agent repositories
never expire a tagged image, so a pinned `toolchain-v<N>` tag never disappears.

The platform module ([`deploy/terraform/aws`](../../README.md)) instantiates this module once per `apps` entry, and its
README describes the trust, the repository variables and the phases in full. The module also works on its own, which
suits an account whose platform half is managed elsewhere:

```hcl
module "app_hello_web" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws/modules/app?ref=vX.Y.Z"

  slug = "hello-web"
  github = {
    owner            = "acme"
    name             = "Hello.Web"
    repository_id    = "123456789"
    owner_id         = "987654"
    sub_claim_prefix = "repo:acme@987654/Hello.Web@123456789"
  }
  github_oidc_provider_arn = "arn:aws:iam::111122223333:oidc-provider/token.actions.githubusercontent.com"
  role_name_prefix         = "acme-prod-app-"
}
```

The app repository's publish workflows read the `github_variables` map: `AWS_REGION`, `ECR_REGISTRY`,
`PUBLISH_REPOSITORY_ID`, `PUBLISH_OWNER_ID`, `AGENT_IMAGE_REPOSITORY` and `AGENT_ROLE_ARN`, plus
`RUNTIME_IMAGE_REPOSITORY` and `RUNTIME_ROLE_ARN` with previews. Export `github_variables_dotenv` from your root (for
example as an output `hello_web_variables`) and set them from it:

```sh
terraform output -raw hello_web_variables | gh variable set --repo acme/Hello.Web -f -
```

`AGENT_PUBLISH_ENABLED` and `PREVIEW_PUBLISH_ENABLED` are the operator's gates, set by hand; the module never emits
them.

If the first publish fails with `Not authorized to perform sts:AssumeRoleWithWebIdentity`, compare `oidc_subject` with
the repository's real subject. Only GitHub's immutable subject, `repo:<owner>@<owner_id>/<name>@<repository_id>`, is
trusted. A repository still on the classic `repo:<owner>/<name>` subject (the API reports
`use_immutable_subject: false`), or on a custom `include_claim_keys` template, fails the plan until it switches to
immutable subject claims
([how, and what that changes for its other workflows](../../../../../docs/intents/deploying.md#4-terraform-phase-1)).

## Reference

<!-- The docs site embeds what lies between the two snippet markers (docs/intents/terraform-module.md). -->
<!-- --8<-- [start:reference] -->
<!-- BEGIN_TF_DOCS -->
<!-- prettier-ignore-start -->
### Requirements

| Name | Version |
| ---- | ------- |
| terraform | >= 1.9 |
| aws | >= 6.0 |

### Providers

| Name | Version |
| ---- | ------- |
| aws | >= 6.0 |

### Resources

| Name | Type |
| ---- | ---- |
| [aws_ecr_lifecycle_policy.this](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/ecr_lifecycle_policy) | resource |
| [aws_ecr_repository.this](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/ecr_repository) | resource |
| [aws_iam_policy.publisher](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_policy) | resource |
| [aws_iam_role.publisher](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role) | resource |
| [aws_iam_role_policy_attachment.publisher](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role_policy_attachment) | resource |
| [aws_region.current](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/region) | data source |

### Inputs

| Name | Description | Type | Default | Required |
| ---- | ----------- | ---- | ------- | :------: |
| github | The app's GitHub repository. Take every value from the API, exactly as it is returned (IAM compares them<br/>case-sensitively):<br/>- owner, name, repository\_id, owner\_id, default\_branch: from `gh api repos/<owner>/<name>`, as `.owner.login`,<br/>  `.name`, `.id`, `.owner.id` and `.default_branch`<br/>- sub\_claim\_prefix: `gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix` | <pre>object({<br/>    owner            = string<br/>    name             = string<br/>    repository_id    = string<br/>    owner_id         = string<br/>    default_branch   = optional(string, "main")<br/>    sub_claim_prefix = string<br/>  })</pre> | n/a | yes |
| github\_oidc\_provider\_arn | ARN of the account's IAM OIDC provider for token.actions.githubusercontent.com. The platform module creates one, or passes on the existing one it was given. | `string` | n/a | yes |
| slug | Short name for the app. It is the leaf of both ECR repository names and part of both IAM role names, and it is independent of the GitHub repository name (Hello.Web -> hello-web). A rename on GitHub never changes it. | `string` | n/a | yes |
| agent\_path\_prefix | ECR path the agent image repository goes under (no leading or trailing slash). Must match the platform module's agent\_path\_prefix, and must never overlap patchy/previews. | `string` | `"patchy/app-envs"` | no |
| preview | Whether the app has previews: a runtime ECR repository under patchy/previews/ and its own runtime publisher role. The agent repository and role are always created. | `bool` | `true` | no |
| role\_name\_prefix | Prefix of both IAM role and policy names: `<role_name_prefix><slug>-agent-push` and `<role_name_prefix><slug>-runtime-push`. IAM names are account-wide, so the prefix keeps two installs in one account apart. | `string` | `"patchy-app-"` | no |
| tags | Tags added to every taggable resource. | `map(string)` | `{}` | no |

### Outputs

| Name | Description |
| ---- | ----------- |
| agent\_repository\_url | Agent image repository URL. .patchy/agent.yaml names `<this>:toolchain-v<N>`; tags are immutable, so a toolchain change bumps N |
| agent\_role\_arn | IAM role the agent publisher (.github/workflows/publish-agent.yml) assumes |
| github\_variables | The app repository's Actions variables, as a map |
| github\_variables\_dotenv | The same variables as a sorted dotenv string, for: `gh variable set --repo <owner>/<name> -f <file>` |
| oidc\_subject | The OIDC subject both publisher roles trust. A first publish failing with "Not authorized to perform sts:AssumeRoleWithWebIdentity" usually means it differs from the repository's real subject |
| registry | ECR registry host (`<account>.dkr.ecr.<region>.amazonaws.com`) |
| runtime\_repository\_url | Runtime (preview) image repository URL, the Project's preview imageRepository; null without previews |
| runtime\_role\_arn | IAM role the runtime publisher (.github/workflows/publish-runtime.yml) assumes; null without previews |
| slug | The app's slug |
<!-- prettier-ignore-end -->
<!-- END_TF_DOCS -->
<!-- --8<-- [end:reference] -->
