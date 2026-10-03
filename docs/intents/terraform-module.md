# The reference Terraform module

The AWS side of intents and previews is a Terraform module in this repository, at `deploy/terraform/aws`, versioned with
the chart: the same release tag pins both, so the values the module emits for the chart always use the chart's own keys.
It creates the identities, image repositories, certificates and DNS records the chart points at, and emits the chart
values and each application's repository variables. Kubernetes objects stay the chart's.
[Deploying intents and previews](deploying.md) walks through using it; this page is the reference.

It has two layers:

- **The platform module**, `deploy/terraform/aws`: the GitHub Actions OIDC provider, source-controller's Pod Identity
  role, the preview node role and its EKS access entry, the preview and edge certificates and, in a second apply, the
  alias records; and one `modules/app` per `apps` entry.
- **The app module**, `deploy/terraform/aws/modules/app`: one application's two ECR repositories and two publisher
  roles. It works on its own, for an account whose platform half is managed some other way; `patchy init app` prints its
  block.

```hcl
module "patchy" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws?ref=vX.Y.Z"
  # ...
}

module "patchy_app_shop_web" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws/modules/app?ref=vX.Y.Z"
  # ...
}
```

Neither has a provider or a backend block: your root configures both. `terraform output` reads only the root's outputs,
so re-export the ones you read:

```hcl
output "helm_values" {
  value = module.patchy.helm_values
}
output "github_variables_dotenv" {
  value = module.patchy.github_variables_dotenv
}
output "preview_node_class" {
  value = module.patchy.preview_node_class
}
```

## What the outputs are for

| Output                    | Read by                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `helm_values`             | The `patchy` chart, passed **last** (`-f <(terraform output -raw helm_values)`). Infrastructure-derived keys only: `sourceController.serviceAccount.name`, `agent.repositoryImages.registries`, the `preview.*` infrastructure keys, `previewController.config.apiServerCIDR`, and the edge hosts with their certificate annotations. It carries no enable flag and no security opt-out. |
| `github_variables_dotenv` | Each application repository's Actions variables, keyed by slug, as dotenv: `terraform output -json github_variables_dotenv`, piped through `jq -r '."<slug>"'` to `gh variable set -f - --repo <owner>/<name>`                                                                                                                                                                           |
| `github_variables`        | The same, as a map                                                                                                                                                                                                                                                                                                                                                                       |
| `preview_node_class`      | The chart's `preview.nodeIsolation.role`, `subnetIDs` and `securityGroupIDs`, for `preview.nodeIsolation.create`                                                                                                                                                                                                                                                                         |
| `apps`                    | Per slug: the repository URLs, the publisher role ARNs and the OIDC subject the roles trust. A Project's `preview.imageRepository` is its `runtime_repository_url`                                                                                                                                                                                                                       |
| `registry`                | The ECR registry host, `patchy init app --registry`                                                                                                                                                                                                                                                                                                                                      |

## Security properties

- **Publisher trust.** Each application has one role per image kind. A role trusts one workflow file
  (`publish-agent.yml` or `publish-runtime.yml`, as `job_workflow_ref`), in one repository (by its numeric repository
  and owner IDs and GitHub's immutable OIDC subject), on its default branch, and may push to one ECR repository, with no
  delete. A repository on the classic `repo:<owner>/<name>` subject fails the plan.
- **Disjoint prefixes.** Agent images live under `agent_path_prefix` (default `patchy/app-envs`), runtime images under
  `patchy/previews`. A runtime image is built from an unreviewed pull request, so it must never be admissible as an
  agent image, and the module refuses overlapping prefixes.
- **Preview subnets.** The preview load balancer is pinned to `alb_subnet_ids`, whose CIDRs are the only sources preview
  Pods admit; a node subnet that assigns public IPs fails the plan, and so does one in a zone no load balancer subnet
  covers, where the load balancer would never send a preview traffic.
- **Immutable images.** Every repository has immutable tags and no `force_delete`. Agent repositories never expire a
  tagged image, so a pinned `toolchain-v<N>` tag never disappears; runtime repositories expire `sha-` images after 30
  days and keep the newest 20 `main-` images.
- **What it cannot see.** Whether the cluster enforces NetworkPolicy: check `kube-system/amazon-vpc-cni` yourself, and
  run the isolation probe ([Deploying, step 12](deploying.md#12-the-isolation-probe)).

Adopting existing resources with `import` blocks, and the exact trust conditions, are in the module's own
[README](https://github.com/devthenet-labs/patchy/blob/main/deploy/terraform/aws/README.md#adopting-existing-resources).

## Reference

The tables in the two sections below are generated by `terraform-docs` from the modules themselves and embedded from
their READMEs, so they always match the release these docs were built from.

## The platform module: `deploy/terraform/aws`

--8<-- "aws/README.md:reference"

## The app module: `deploy/terraform/aws/modules/app`

--8<-- "aws/modules/app/README.md:reference"
