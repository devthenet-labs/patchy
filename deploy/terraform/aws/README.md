<!--
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
-->

# patchy on AWS: the reference Terraform module

The AWS side of a patchy install with intent-driven development and previews on an **EKS Auto Mode** cluster with
**ECR**. It creates the identities, image repositories, certificates and DNS records the chart's values point at, and
emits those values. Kubernetes objects stay the chart's.

| Created                                                                                                                                                    | When                                         |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------- |
| The GitHub Actions OIDC provider                                                                                                                           | `github_oidc_provider_arn` is null (default) |
| source-controller's Pod Identity role (ECR read on `<agent_path_prefix>/*`), pinned to this cluster, namespace and ServiceAccount, and its association     | always                                       |
| Per app ([`modules/app`](modules/app/README.md)): an agent toolchain repository and, with previews, a runtime repository, each with its own publisher role | one per `apps` entry                         |
| The preview node role (joins this cluster, pulls `<preview_path_prefix>/*` only), its EKS access entry and the Auto Mode node policy                       | `previews` is set                            |
| The `*.<host_suffix>` ACM certificate and its DNS validation record                                                                                        | `previews` is set                            |
| The webhook and status edge ACM certificate and its DNS validation records                                                                                 | `edge` is set                                |
| Alias records for the edge hosts to the edge ALB                                                                                                           | `create_edge_alias_records = true`           |
| The `*.<host_suffix>` alias record to the preview ALB                                                                                                      | `create_preview_alias_record = true`         |

The module has no provider and no backend block: configure both in your root. Only ECR on EKS Auto Mode is supported,
because the preview edge depends on Auto Mode's ALB IngressClassParams and the chart accepts only an ECR preview
registry. source-controller's identity is EKS Pod Identity, which Auto Mode runs natively. The commands below read the
module's outputs with `terraform output`, which sees only the root's: re-export the ones you read
(`output "helm_values" { value = module.patchy.helm_values }`, and likewise `github_variables_dotenv` and
`preview_node_class`). The operator guide, [Deploying intents and previews](../../../docs/intents/deploying.md), walks
the whole path.

## Example

```hcl
module "patchy" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws?ref=vX.Y.Z"

  cluster_name = "acme-prod"

  # Null creates the provider. An account holds one provider per issuer, so pass its ARN when it already exists.
  github_oidc_provider_arn = null

  apps = {
    # The key is the image slug: lowercase, independent of the GitHub name.
    hello-web = {
      # Every value exactly as GitHub returns it: IAM compares them case-sensitively.
      github = {
        owner            = "acme"      # gh api repos/acme/Hello.Web --jq .owner.login
        name             = "Hello.Web" # --jq .name
        repository_id    = "123456789" # --jq .id
        owner_id         = "987654"    # --jq .owner.id
        default_branch   = "main"      # --jq .default_branch
        sub_claim_prefix = "repo:acme@987654/Hello.Web@123456789"
        # gh api repos/acme/Hello.Web/actions/oidc/customization/sub --jq .sub_claim_prefix
      }
      preview = true # false: an agent image only, no runtime repository or publisher
    }
  }

  previews = {
    host_suffix     = "preview.acme-apps.dev" # a separate registrable domain is recommended
    zone_id         = "Z0123456789PREVIEW"
    alb_subnet_ids  = ["subnet-0public0a", "subnet-0public0b"]   # one per zone, /20 or narrower; the ALB is pinned to these
    node_subnet_ids = ["subnet-0private0a", "subnet-0private0b"] # where preview nodes run, in the zones above
    inbound_cidrs   = ["203.0.113.7/32"]                         # who may open previews
  }

  edge = {
    zone_id      = "Z0123456789EDGE"
    webhook_host = "patchy.acme.dev"
    status_host  = "status.patchy.acme.dev"
    alb_name     = "acme-prod-patchy" # = the chart's edgeIngressClass.loadBalancerName; not the preview ALB
  }

  create_edge_alias_records   = false # true once Helm stage 1 has created the edge ALB
  create_preview_alias_record = false # true once Helm stage 2 has created the preview ALB
}
```

## The phases

The module applies in phases around the chart's Helm stages, because Auto Mode creates both load balancers from
Ingresses and terraform can only look them up afterwards. The operator guide,
[Deploying intents and previews](../../../docs/intents/deploying.md), has every command and values file; in order:

1. **Terraform, phase 1.** By default the apply waits until ACM has issued the certificates
   (`wait_for_certificate_validation = true`), so the zones must already be delegated. Set it false to apply before
   delegation; ACM then issues the certificates on its own once the records resolve.
2. **The namespace**, created with the `restricted` Pod Security labels (not `helm --create-namespace`, which creates it
   without them), and its Secrets.
3. **Helm, stage 1** (controllers and intents, previews off). `helm_values` carries only infrastructure-derived keys.
   Pass it **last**, after your own values file, so it wins for the keys it owns:

   ```sh
   helm upgrade --install patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy --version X.Y.Z \
     --namespace patchy -f patchy-values.yaml -f <(terraform output -raw helm_values)
   ```

   It sets `sourceController.serviceAccount.name` (the name the Pod Identity association binds),
   `agent.repositoryImages.registries`, and with `previews` the `preview.*` infrastructure keys (`albSubnetIDs`, which
   pins the preview ALB to `alb_subnet_ids`, among them) and `previewController.config.apiServerCIDR`. With `edge` it
   sets `webhook.host`, `statusServer.host`, and the certificate, listen-ports and ssl-redirect annotations of both
   Ingresses. Your file keeps every decision that is yours: `agent.repositoryImages.enabled`, `ephemeralStorage` and
   `cosignPublicKey` (or the explicit opt-out `allowUnsigned: true`); `agent.networkPolicy.broadEgress: never`; the
   runner and the egress broker, its `limits` included; `intentController.enabled` and its `forgeSecrets`; the edge
   class (`edgeIngressClass.loadBalancerName` equal to `edge.alb_name`) and the Ingresses' `enabled`; and later
   `preview.enabled`, `preview.placeholder.enabled`, `preview.nodeIsolation` and `previewController.enabled`.

4. **Edge DNS.** Once Helm has created the edge Ingresses, and so the edge ALB, set `create_edge_alias_records = true`
   and apply again. The lookup fails the plan while the edge ALB is missing; it never needs the preview ALB.
5. **Each app repository's agent image, and its Project.** Set the repository's Actions variables, then its agent gate,
   and publish the agent image; the Project follows once the tag is in ECR, and intents work from here on:

   ```sh
   terraform output -json github_variables_dotenv | jq -r '."hello-web"' | gh variable set --repo acme/Hello.Web -f -
   gh variable set AGENT_PUBLISH_ENABLED --repo acme/Hello.Web --body true
   gh workflow run "agent image" --repo acme/Hello.Web  # then confirm the toolchain tag is in ECR
   ```

6. **Helm, stage 2** (the preview foundation). The isolated preview NodeClass and NodePool come first: with the chart's
   `preview.nodeIsolation.create: true`, set `preview.nodeIsolation.role`, `subnetIDs` and `securityGroupIDs` from the
   `preview_node_class` output, or apply your own NodeClass and NodePool with those values. Enabling the placeholder
   creates the preview ALB, and charges start there.
7. **Preview DNS.** Set `create_preview_alias_record = true` and apply again. The lookup fails the plan while the
   preview ALB is missing.
8. **The isolation probe.** Turn on the runtime publisher, which the probe's disposable pull request needs, and prove
   the slots' isolation with `hack/preview-isolation-probe/run.sh` before any Project previews:

   ```sh
   gh variable set PREVIEW_PUBLISH_ENABLED --repo acme/Hello.Web --body true
   ```

9. **Helm, stage 3** (`previewController.enabled`), then the Project's `preview` block.

## The app repository's variables

The generated publish workflows read these repository variables. Nothing here is secret: the role trust, not the values,
decides who can publish.

| Variable                   | Value                                                                   | Set by                      |
| -------------------------- | ----------------------------------------------------------------------- | --------------------------- |
| `AWS_REGION`               | The registry's region                                                   | the module, always          |
| `ECR_REGISTRY`             | `<account>.dkr.ecr.<region>.amazonaws.com`                              | the module, always          |
| `PUBLISH_REPOSITORY_ID`    | The numeric repository ID, which the publisher's guard compares         | the module, always          |
| `PUBLISH_OWNER_ID`         | The numeric owner ID, which the publisher's guard compares              | the module, always          |
| `AGENT_IMAGE_REPOSITORY`   | `<agent_path_prefix>/<slug>`                                            | the module, always          |
| `AGENT_ROLE_ARN`           | `arn:aws:iam::<account>:role/<app_role_name_prefix><slug>-agent-push`   | the module, always          |
| `RUNTIME_IMAGE_REPOSITORY` | `<preview_path_prefix>/<slug>`                                          | the module, with `preview`  |
| `RUNTIME_ROLE_ARN`         | `arn:aws:iam::<account>:role/<app_role_name_prefix><slug>-runtime-push` | the module, with `preview`  |
| `AGENT_PUBLISH_ENABLED`    | `true` to let the agent image publish                                   | the operator, never emitted |
| `PREVIEW_PUBLISH_ENABLED`  | `true` to let runtime (preview) images publish                          | the operator, never emitted |

Two outputs carry them, keyed by app slug:

- `github_variables`: a map of maps, `{ "<slug>" = { AWS_REGION = "…", … } }`. Each inner map holds exactly the six
  names above that the module always sets, plus `RUNTIME_IMAGE_REPOSITORY` and `RUNTIME_ROLE_ARN` when that app has
  `preview = true`.
- `github_variables_dotenv`: the same per app as one sorted `NAME=value` line per variable, which `gh variable set -f -`
  reads from standard input.

## Trusted publisher workflows

Each app gets one publisher role per image kind. A role trusts exactly one workflow file in exactly one repository, on
its default branch:

| Role                                        | Trusted workflow (`job_workflow_ref`)                                      | Pushes to                      |
| ------------------------------------------- | -------------------------------------------------------------------------- | ------------------------------ |
| `<app_role_name_prefix><slug>-agent-push`   | `<owner>/<name>/.github/workflows/publish-agent.yml@refs/heads/<branch>`   | `<agent_path_prefix>/<slug>`   |
| `<app_role_name_prefix><slug>-runtime-push` | `<owner>/<name>/.github/workflows/publish-runtime.yml@refs/heads/<branch>` | `<preview_path_prefix>/<slug>` |

`publish-agent.yml` and `publish-runtime.yml` are `workflow_call` callees of the `.github/workflows/publish-images.yml`
dispatcher, which runs on `workflow_run`. GitHub sets `job_workflow_ref` to the callee and `workflow_ref` to the
dispatcher, so the trust pins `job_workflow_ref` and never `workflow_ref`. Every condition is `StringEquals`:

- `aud` = `sts.amazonaws.com`;
- `sub` = `<sub_claim_prefix>:ref:refs/heads/<default_branch>`, in GitHub's immutable form only:
  `repo:<owner>@<owner_id>/<name>@<repository_id>:ref:refs/heads/<default_branch>`. `sub_claim_prefix` is required and
  must equal `repo:<owner>@<owner_id>/<name>@<repository_id>` built from the app's own values, so a prefix copied from
  another repository fails the plan. A repository still on the classic `repo:<owner>/<name>` subject (the API reports
  `use_immutable_subject: false`), or on a custom `include_claim_keys` template, fails the plan until it switches to
  immutable subject claims
  ([how, and what that changes for its other workflows](../../../docs/intents/deploying.md#4-terraform-phase-1));
- `repository_id` and `repository_owner_id`, the numeric IDs, so a repository later created under a reused name cannot
  assume the role;
- `ref` = `refs/heads/<default_branch>`;
- `job_workflow_ref`, as in the table.

The role's policy allows `ecr:GetAuthorizationToken` (registry-wide by design; it grants no repository access) and, on
that one repository only, `BatchCheckLayerAvailability`, `InitiateLayerUpload`, `UploadLayerPart`,
`CompleteLayerUpload`, `PutImage`, `BatchGetImage` and `DescribeImages`. There is no delete. Role ARNs carry no IAM
path. If a first publish fails with `Not authorized to perform sts:AssumeRoleWithWebIdentity`, compare the `apps`
output's `oidc_subject` with the repository's real subject.

## Image repositories

Every repository has immutable tags, scans on push and no `force_delete`, so destroying one that still holds images
fails instead of deleting them. Untagged images expire after 14 days. Runtime repositories also keep the newest 20
`main-` images (the default-branch publishes, whatever the branch is called) and expire `sha-` images after 30 days.
Agent repositories never expire a tagged image, so a pinned `toolchain-v<N>` tag is never removed; because tags are
immutable, a toolchain change publishes under a new tag.

## Security notes

- **Disjoint prefixes.** The agent and preview image prefixes must be disjoint, and variable validation enforces it. A
  runtime image is built from an unreviewed pull request head, so it must never be admissible as an agent sandbox image,
  and preview nodes must never be able to pull a toolchain image. The two are compared on path segment boundaries
  (`patchy/previews-agents` is beside `patchy/previews`, not under it). `preview_path_prefix` (default
  `patchy/previews`) reaches the chart as `preview.imagePathPrefix` through `helm_values`, and the chart refuses to
  render an agent allowlist that overlaps it, so the module, the slot admission policy and source-controller agree.
- **Preview subnets.** A node subnet that assigns public IPs fails the plan, and no subnet may serve as both a node
  subnet and an ALB subnet. List 2 to 4 ALB subnets, one per Availability Zone, each in the cluster's VPC, tagged
  `kubernetes.io/role/elb` (Auto Mode requires the tag of an internet-facing load balancer's subnets) and `/20` or
  narrower (the chart's `albSubnetCIDRs` admits no wider range). Each node subnet must be in one of those zones: the
  preview ALB uses IP targets, and an ALB sends no traffic to a target in a zone it has not enabled (`Target.NotInUse`),
  so a preview on a node elsewhere would never become Ready; the plan fails otherwise. `helm_values` pins the preview
  ALB to exactly these subnets (`preview.albSubnetIDs`), and their CIDRs are the only sources preview pods admit
  (`preview.albSubnetCIDRs`), so every ALB node sits in an admitted subnet and no other subnet is admitted. Without the
  pin, Auto Mode would place the ALB in a tagged subnet of every zone that has one, and the slot NetworkPolicy would
  drop the traffic of any zone the list left out.
- **A separate preview ALB.** `previews.alb_name` (default `<cluster_name>-preview`) must differ from `edge.alb_name`.
  The preview ALB admits only `inbound_cidrs`, so sharing it would put the GitHub webhook behind those /32s.
- **What the module cannot see.** It cannot check that the cluster enforces NetworkPolicy (`kube-system/amazon-vpc-cni`
  with `enable-network-policy-controller: "true"`), and neither does `patchy check project`: read the ConfigMap
  yourself, and prove the preview slots' isolation with the cold-start isolation probe
  (`hack/preview-isolation-probe/README.md`) before any Project previews.

## Adopting existing resources

A name match is not enough to adopt a resource: where the resources already exist, `apply` fails on the name conflicts.
Adopt them with `import` blocks, and plan first; an exact match plans as imports with no changes:

```hcl
import {
  to = module.patchy.module.app["hello-web"].aws_iam_role.publisher["runtime"]
  id = "acme-prod-app-hello-web-runtime-push"
}
```

`name_prefix` and `app_role_name_prefix` reproduce existing IAM names. Two kinds of non-import line are expected and
harmless: the `aws_acm_certificate_validation` resources are created (they create nothing in AWS, they only wait for the
certificates to be issued), and adopted validation records gain `allow_overwrite = true`, a flag kept in state only.
Edge validation records are keyed by domain without its `*.` prefix, so a name and its wildcard share one record
(`aws_route53_record.edge_validation["patchy.acme.dev"]`); the preview certificate's one record is keyed by its wildcard
domain (`aws_route53_record.preview_validation["*.preview.acme-apps.dev"]`).

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

### Modules

| Name | Source | Version |
| ---- | ------ | ------- |
| app | ./modules/app | n/a |

### Resources

| Name | Type |
| ---- | ---- |
| [aws_acm_certificate.edge](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/acm_certificate) | resource |
| [aws_acm_certificate.preview](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/acm_certificate) | resource |
| [aws_acm_certificate_validation.edge](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/acm_certificate_validation) | resource |
| [aws_acm_certificate_validation.preview](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/acm_certificate_validation) | resource |
| [aws_eks_access_entry.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/eks_access_entry) | resource |
| [aws_eks_access_policy_association.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/eks_access_policy_association) | resource |
| [aws_eks_pod_identity_association.source_controller](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/eks_pod_identity_association) | resource |
| [aws_iam_openid_connect_provider.github](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_openid_connect_provider) | resource |
| [aws_iam_policy.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_policy) | resource |
| [aws_iam_policy.source_controller](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_policy) | resource |
| [aws_iam_role.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role) | resource |
| [aws_iam_role.source_controller](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role) | resource |
| [aws_iam_role_policy_attachment.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role_policy_attachment) | resource |
| [aws_iam_role_policy_attachment.source_controller](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role_policy_attachment) | resource |
| [aws_route53_record.edge](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route53_record) | resource |
| [aws_route53_record.edge_validation](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route53_record) | resource |
| [aws_route53_record.preview_validation](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route53_record) | resource |
| [aws_route53_record.preview_wildcard](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/route53_record) | resource |
| [aws_caller_identity.current](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/caller_identity) | data source |
| [aws_eks_cluster.this](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/eks_cluster) | data source |
| [aws_lb.edge](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/lb) | data source |
| [aws_lb.preview](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/lb) | data source |
| [aws_partition.current](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/partition) | data source |
| [aws_region.current](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/region) | data source |
| [aws_subnet.preview_alb](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/subnet) | data source |
| [aws_subnet.preview_node](https://registry.terraform.io/providers/hashicorp/aws/latest/docs/data-sources/subnet) | data source |

### Inputs

| Name | Description | Type | Default | Required |
| ---- | ----------- | ---- | ------- | :------: |
| cluster\_name | The EKS Auto Mode cluster patchy runs on. Its ARN, VPC, primary security group and service CIDR are read from it. | `string` | n/a | yes |
| agent\_path\_prefix | ECR path the agent toolchain images live under, with no leading or trailing slash. source-controller may read every repository under it, and helm\_values admits declared images only there. It must be disjoint from preview\_path\_prefix. | `string` | `"patchy/app-envs"` | no |
| app\_role\_name\_prefix | Prefix of every app's publisher role and policy names: `<app_role_name_prefix><slug>-agent-push` and `<app_role_name_prefix><slug>-runtime-push`. Null means `<name_prefix>-app-`. | `string` | `null` | no |
| apps | The application repositories patchy works on, keyed by slug: the image name, lowercase letters, digits and<br/>inner hyphens, independent of the GitHub name (Hello.Web -> hello-web). Each app gets an agent toolchain<br/>repository and its publisher role; with preview (default true) also a runtime repository and its publisher.<br/>- github: the repository exactly as the API reports it (IAM compares case-sensitively). owner, name,<br/>  repository\_id, owner\_id and default\_branch come from `gh api repos/<owner>/<name>`; sub\_claim\_prefix,<br/>  which is required, from `gh api repos/<owner>/<name>/actions/oidc/customization/sub --jq .sub_claim_prefix`.<br/>  It must be GitHub's immutable form, `repo:<owner>@<owner_id>/<name>@<repository_id>`. | <pre>map(object({<br/>    github = object({<br/>      owner            = string<br/>      name             = string<br/>      repository_id    = string<br/>      owner_id         = string<br/>      default_branch   = optional(string, "main")<br/>      sub_claim_prefix = string<br/>    })<br/>    preview = optional(bool, true)<br/>  }))</pre> | `{}` | no |
| create\_edge\_alias\_records | Create the Route53 alias records for edge.webhook\_host and edge.status\_host, pointing at the edge ALB looked up by edge.alb\_name. Set it once Helm stage 1 has created the edge Ingresses and so the ALB: the lookup fails the plan while the ALB is missing. It is independent of the preview alias, so the webhook resolves before previews are turned on. | `bool` | `false` | no |
| create\_preview\_alias\_record | Create the `*.<previews.host_suffix>` Route53 alias record, pointing at the preview ALB looked up by its name. Set it once Helm stage 2 (previews on, with the placeholder Ingress) has created the preview ALB: the lookup fails the plan while the ALB is missing. | `bool` | `false` | no |
| edge | An ACM certificate, and after Helm stage 1 alias records, for patchy's own public edge on an ALB: the GitHub<br/>webhook and the status page. Null (the default) creates none; the edge then works with any ingress and<br/>certificate source, cert-manager included.<br/>- zone\_id: the Route53 hosted zone that holds the hosts.<br/>- webhook\_host, status\_host: the two hostnames; status\_host is optional.<br/>- certificate\_domains: the names the certificate covers, which must cover both hosts. Null means the hosts.<br/>- alb\_name: the ALB the edge Ingresses share, looked up by name only for the alias records: the chart's<br/>  edgeIngressClass.loadBalancerName, when the chart creates the edge class. | <pre>object({<br/>    zone_id             = string<br/>    webhook_host        = string<br/>    status_host         = optional(string)<br/>    certificate_domains = optional(list(string))<br/>    alb_name            = string<br/>  })</pre> | `null` | no |
| github\_oidc\_provider\_arn | ARN of the account's existing IAM OIDC provider for token.actions.githubusercontent.com. Null creates one. An account holds at most one provider per issuer, so pass the ARN when it already exists. | `string` | `null` | no |
| name\_prefix | Prefix of the platform IAM names: `<name_prefix>-patchy-source-controller` and `<name_prefix>-patchy-preview-node`. Null means cluster\_name. IAM names are account-wide, so the prefix keeps two installs in one account apart. | `string` | `null` | no |
| namespace | The patchy Helm release namespace, where source-controller runs. | `string` | `"patchy"` | no |
| preview\_path\_prefix | ECR path the preview runtime images live under, with no leading or trailing slash: each app's runtime repository is `<preview_path_prefix>/<slug>`, the preview nodes may pull only under it, and helm\_values sets the chart's preview.imagePathPrefix to it. It must be disjoint from agent\_path\_prefix. | `string` | `"patchy/previews"` | no |
| previews | Preview infrastructure; null (the default) creates none. Set, the module creates the preview node role and its<br/>EKS access entry, and a wildcard certificate for `*.<host_suffix>` validated in zone\_id.<br/>- host\_suffix: preview hosts are `<project>-<issue>.<host_suffix>`. A separate registrable domain is recommended.<br/>- zone\_id: the Route53 hosted zone that holds host\_suffix.<br/>- alb\_name: the preview ALB, at most 32 characters. Null means `<cluster_name>-preview`. It must differ from<br/>  edge.alb\_name: previews get an ALB of their own.<br/>- alb\_subnet\_ids: the public subnets the preview ALB is placed in: 2 to 4, one per Availability Zone, each in the<br/>  cluster's VPC, tagged kubernetes.io/role/elb and /20 or narrower. helm\_values pins the ALB to exactly these<br/>  subnets (preview.albSubnetIDs), and their CIDRs become preview.albSubnetCIDRs, the only sources preview pods<br/>  admit.<br/>- node\_subnet\_ids: the private subnets preview nodes run in (the NodeClass subnetSelectorTerms), each in a zone<br/>  of one of the alb\_subnet\_ids: the ALB sends no traffic to a target in a zone it has not enabled. Untrusted<br/>  preview workloads must never get public IPs, so a subnet that assigns them fails the plan. The nodes join the<br/>  cluster and pull from ECR out of these subnets, so they need a NAT gateway or EKS, ECR and S3 endpoints.<br/>- inbound\_cidrs: who may reach previews: 1 to 8 IPv4 /32s.<br/>- prefix\_list\_ids: managed prefix lists (pl-...) the preview ALB admits beside inbound\_cidrs, at most 8. Optional<br/>  and additive: inbound\_cidrs stays required. helm\_values passes them as preview.prefixListsIDs.<br/>- dns\_cidr, api\_server\_cidr: the cluster DNS and Kubernetes API Service /32s. Null derives .10 and .1 of the<br/>  cluster's service CIDR, which is what EKS assigns. | <pre>object({<br/>    host_suffix     = string<br/>    zone_id         = string<br/>    alb_name        = optional(string)<br/>    alb_subnet_ids  = list(string)<br/>    node_subnet_ids = list(string)<br/>    inbound_cidrs   = list(string)<br/>    prefix_list_ids = optional(list(string), [])<br/>    dns_cidr        = optional(string)<br/>    api_server_cidr = optional(string)<br/>  })</pre> | `null` | no |
| source\_controller\_service\_account | The source-controller ServiceAccount the Pod Identity association binds. helm\_values sets sourceController.serviceAccount.name to it, so the chart creates exactly this name whatever the release is called. | `string` | `"patchy-source-controller"` | no |
| tags | Tags added to every taggable resource the module creates. | `map(string)` | `{}` | no |
| wait\_for\_certificate\_validation | Hold apply until ACM has issued the certificates, so helm\_values never carries a certificate the ALB cannot attach yet. Set false while the zones are not yet delegated; ACM then issues the certificates on its own once they are. | `bool` | `true` | no |

### Outputs

| Name | Description |
| ---- | ----------- |
| apps | Per app slug: its GitHub repository, image repository URLs, publisher role ARNs and the OIDC subject the roles trust. The runtime values are null for an app without previews. |
| edge\_certificate\_arn | ACM certificate for the webhook and status hosts (helm\_values edge annotations); null without edge |
| github\_oidc\_provider\_arn | ARN of the GitHub Actions OIDC provider the publisher roles trust: the one this module created, or the one it was given |
| github\_variables | Per app slug, the repository variables its publish workflows read, as a map: AWS\_REGION, ECR\_REGISTRY, PUBLISH\_REPOSITORY\_ID, PUBLISH\_OWNER\_ID, AGENT\_IMAGE\_REPOSITORY, AGENT\_ROLE\_ARN and, with previews, RUNTIME\_IMAGE\_REPOSITORY and RUNTIME\_ROLE\_ARN. |
| github\_variables\_dotenv | Per app slug, the same variables as a sorted dotenv string: `terraform output -json github_variables_dotenv`, piped through `jq -r '."<slug>"'` to `gh variable set --repo <owner>/<name> -f -` |
| helm\_values | YAML values for the patchy chart, derived from the infrastructure: pass it last, after the operator's own values file (`helm install ... -f operator-values.yaml -f <(terraform output -raw helm_values)`). It carries no enable flag and no security opt-out. |
| preview\_certificate\_arn | ACM certificate for `*.<previews.host_suffix>` (helm\_values preview.certificateARN); null without previews |
| preview\_node\_class | What the isolated preview NodeClass names: the node role (spec.role), the private subnets (subnetSelectorTerms) and the cluster's primary security group (securityGroupSelectorTerms). Null without previews. |
| registry | The account's ECR registry host (`<account>.dkr.ecr.<region>.amazonaws.com`) |
| source\_controller\_role\_arn | IAM role source-controller assumes through EKS Pod Identity (read-only on `<agent_path_prefix>/*`) |
<!-- prettier-ignore-end -->
<!-- END_TF_DOCS -->
<!-- --8<-- [end:reference] -->
