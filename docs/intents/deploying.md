# Deploying intents and previews

Intent-driven development turns a GitHub issue into a reviewed pull request: an approver labels an issue in an intent
repository, patchy plans the change in a read-only agent Job and posts the plan, the approver approves it, and patchy
builds exactly that plan in the application's own toolchain image and opens the pull request. With previews on, every
such pull request is also deployed to its own URL, `<project>-<issue>.<host suffix>`, until it merges or closes.

This guide takes an operator from an empty AWS account and GitHub organization to a merged intent with a working
preview. It is written against one fictional site throughout, so every command and value file is complete; substitute
your own values:

| What                   | Example                                                          |
| ---------------------- | ---------------------------------------------------------------- |
| GitHub organization    | `acme`                                                           |
| Intent repository      | `acme/intents`                                                   |
| Application repository | `acme/Shop.Web`, image slug `shop-web`, Project `shop-web`       |
| AWS account and region | `123456789012`, `us-west-2`                                      |
| ECR registry           | `123456789012.dkr.ecr.us-west-2.amazonaws.com`                   |
| EKS Auto Mode cluster  | `acme-prod`, service CIDR `172.20.0.0/16`                        |
| patchy's own edge      | `patchy.acme.dev` (GitHub webhook) and `status.patchy.acme.dev`  |
| Preview hosts          | `*.preview.acme-apps.dev`, in a registrable domain of their own  |
| Who may open previews  | `203.0.113.10/32`                                                |
| patchy release         | `X.Y.Z`, **0.12.16 or later**: the chart, the CLI and the module |

`X.Y.Z` stands for one release of `devthenet-labs/patchy` throughout: the chart, the CLI and the terraform module all
come from it. Use the newest, and at least 0.12.16, the first release whose chart has the values this guide sets
(`clusterDNSCIDR`, `edgeIngressClass`, `preview.nodeIsolation.create`) and the first with
[multi-repository intents](#several-repositories-in-one-project). 0.12.15 and older lack those keys, and their values
schema refuses them. Find the newest release, and once the CLI is installed (step 1), check it:

```sh
gh release view --repo devthenet-labs/patchy --json tagName --jq .tagName   # v0.12.16, say: X.Y.Z is 0.12.16
patchy --version                                                            # patchy version 0.12.16 (...)
```

0.12.16's `patchy init app` prints an older form of its next steps, a `gh variable set` command per variable instead of
the terraform module block and its one-command dotenv, and its READMEs say the same; the workflows, scripts and
toolchain files it writes are those these pages describe. Follow these pages rather than that output.

The patchy chart and the reference terraform module are the supported path, and this guide follows it. Onboarding each
application repository has a page of its own, [Onboarding an application](onboarding-app.md), and the module's inputs
and outputs are in [the module reference](terraform-module.md).

## Prerequisites

Previews are built on EKS Auto Mode's load balancer and node pools, whose object kinds the chart renders directly, so
they do not port to other clusters. (Intents without previews run on any cluster the chart supports; this guide covers
the Auto Mode path, previews included.) Check every item below before you start; a missing one costs more to discover
halfway through.

| Prerequisite                                   | Why, and how to check                                                                                                                                                                                                                                                                                                                               |
| ---------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **EKS Auto Mode**, Kubernetes 1.34 or later    | The preview edge is an Auto Mode `IngressClassParams` (`eks.amazonaws.com/v1`), the preview nodes an Auto Mode `NodeClass` and `NodePool`, and the target-health readiness gate an Auto Mode label. A cluster without Auto Mode, running the AWS Load Balancer Controller or Karpenter itself, is not supported.                                    |
| **NetworkPolicy enforced** by the VPC CNI      | Auto Mode programs NetworkPolicies only once the cluster's network policy controller is on: the `kube-system/amazon-vpc-cni` ConfigMap with `enable-network-policy-controller: "true"`. Without it every patchy policy, the agent sandbox's and the preview slots' included, is silently inert. The ConfigMap is yours, not the chart's.            |
| **ECR** in the cluster's account               | Agent and preview images live in ECR. source-controller resolves agent images through its own Pod Identity role, and nodes pull with their node roles, so the images belong in the cluster's account.                                                                                                                                               |
| **ACM** certificates and **Route53** zones     | Both load balancers terminate TLS with ACM certificates, which the module requests and validates by DNS in your Route53 zones. Delegate the zones before the first apply. See [TLS: ACM only, for previews](#tls-acm-only-for-previews).                                                                                                            |
| Subnets                                        | Public subnets tagged `kubernetes.io/role/elb` for the preview load balancer: 2 to 4, one per Availability Zone, each `/20` or narrower. Private subnets for the preview nodes, which must never get public IPs, each in one of those zones and with a way out to EKS, ECR and S3. [Why, and how to find them](#find-the-sites-values).             |
| **Public** charts and images                   | Nothing to log in to: the charts (`oci://ghcr.io/devthenet-labs/patchy/charts/*`), the controller and agent runner images and the agent base image `init app` pins are public on `ghcr.io/devthenet-labs/patchy`. No `helm registry login` and no `image.pullSecrets`.                                                                              |
| **Helm**, not kustomize, for previews          | Previews exist only in the Helm chart; the kustomize tree has an intent-controller component and no preview one.                                                                                                                                                                                                                                    |
| **One preview-enabled release per cluster**    | The preview admission policies, the `alb-preview` class and the slot namespaces (`patchy-preview-0`, `-1`, ...) have fixed, cluster-wide names. A second release with `preview.enabled` would fight the first over them.                                                                                                                            |
| A **separate registrable domain** for previews | Recommended. A preview runs code from an unreviewed pull request; under its own eTLD+1 (`acme-apps.dev` beside `acme.dev`) it is cross-site to patchy's status page and everything else you run, so no cookie or same-site request crosses over. Keep that zone out of any external-dns `--domain-filter` too.                                      |
| Budget for **load balancers**                  | The preview edge is a load balancer of its own, separate from patchy's webhook and status edge: each costs roughly $20 to $35 a month before traffic (hourly charge plus public IPv4 addresses). The preview load balancer bills from the moment the chart creates it, whether or not a preview runs; preview nodes bill only while a preview runs. |
| A **model credential**                         | An Anthropic API key or a `claude setup-token` OAuth token for the egress broker, or Bedrock, Vertex or Foundry through the broker's workload identity (see [the provider recipes](../deployment/helm.md#model-providers-brokered-claude)).                                                                                                         |
| Rights                                         | Cluster admin (the chart installs CRDs, admission policies and cluster-scoped preview objects), IAM and Route53 rights for terraform, **owner** of the GitHub organization (to create an organization App), and admin on each application repository (to set its Actions variables).                                                                |
| Workstation tools                              | `kubectl`, `helm` 3.8 or later, `terraform` 1.9 or later, `aws`, `gh` (signed in), `jq`, and the patchy CLI at the release you deploy (see [Install](../cli.md#install)). `docker` for `patchy check image --run`; `cosign` to verify the CLI.                                                                                                      |

Check the cluster before anything else:

```sh
aws eks update-kubeconfig --name acme-prod --region us-west-2
aws eks describe-cluster --name acme-prod --region us-west-2 --query '{version: cluster.version,
  autoMode: cluster.computeConfig.enabled, serviceCIDR: cluster.kubernetesNetworkConfig.serviceIpv4Cidr,
  vpc: cluster.resourcesVpcConfig.vpcId}'
kubectl -n kube-system get configmap amazon-vpc-cni -o jsonpath='{.data}'; echo
```

The first should print a `version` of 1.34 or later, `autoMode: true`, the service CIDR and the VPC. The second should
include `"enable-network-policy-controller":"true"`; set it before installing patchy, and `clusterDNSCIDR` as below in
the same change (once policies are enforced, every patchy pod needs the DNS rule that value adds):

```sh
# The ConfigMap does not exist (NotFound):
kubectl -n kube-system create configmap amazon-vpc-cni --from-literal=enable-network-policy-controller=true
# It exists without the key, or with another value:
kubectl -n kube-system patch configmap amazon-vpc-cni --type merge \
  -p '{"data":{"enable-network-policy-controller":"true"}}'
```

The cluster DNS address is the tenth address of the service CIDR: `172.20.0.10/32` for `172.20.0.0/16`. Auto Mode runs
CoreDNS on every node at that address rather than as pods in `kube-system`, so the chart's usual "DNS to kube-system"
rule matches nothing there, and `clusterDNSCIDR` adds the one that does.

Agent Jobs carry no node selector, and the generated publishers publish agent images for `linux/amd64` only, so no
NodePool without a taint may launch arm64 nodes. List each pool's architectures and taints: every pool with no taints
should print `["amd64"]`, as Auto Mode's built-in `general-purpose` pool does.

```sh
kubectl get nodepools -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.requirements[?(@.key=="kubernetes.io/arch")].values}{"\t"}{.spec.template.spec.taints}{"\n"}{end}'
```

### Find the site's values

The terraform root in step 4 names zones, subnets and an OIDC provider by ID. The subnets have rules the module checks
at plan time:

- **Load balancer subnets**: public and tagged `kubernetes.io/role/elb`, 2 to 4 in distinct zones, each `/20` or
  narrower, since the chart admits `/20` to `/32` source ranges for them.
- **Node subnets**: private, and each in one of the load balancer subnets' zones. The load balancer sends nothing to a
  target in a zone it has not enabled (target health `Target.NotInUse`), so a preview on a node elsewhere would never
  become Ready. Best are the private subnets the cluster's Auto Mode nodes already use: preview nodes join the cluster
  and pull from ECR out of them, so they need a NAT gateway or VPC endpoints for EKS, ECR (`api` and `dkr`) and S3, and
  a wrong choice shows only at the isolation probe (step 12) or the first preview (step 14).

Read each value from the account:

```sh
# The hosted zones (public, delegated): the zone ID is the part after /hostedzone/.
aws route53 list-hosted-zones-by-name --dns-name acme.dev --max-items 1 \
  --query 'HostedZones[].[Name, Id, Config.PrivateZone]' --output text
aws route53 list-hosted-zones-by-name --dns-name acme-apps.dev --max-items 1 \
  --query 'HostedZones[].[Name, Id, Config.PrivateZone]' --output text

# The account's GitHub Actions OIDC provider. An account holds one per issuer: if this prints an
# ARN, pass it as github_oidc_provider_arn; if it prints nothing, leave that null and the module creates it.
aws iam list-open-id-connect-providers --output text \
  --query "OpenIDConnectProviderList[?ends_with(Arn, 'token.actions.githubusercontent.com')].Arn"

vpc=$(aws eks describe-cluster --name acme-prod --region us-west-2 --query cluster.resourcesVpcConfig.vpcId --output text)
# The preview load balancer's subnets: public, tagged kubernetes.io/role/elb, one per zone, /20 or narrower.
aws ec2 describe-subnets --region us-west-2 \
  --filters "Name=vpc-id,Values=$vpc" Name=tag-key,Values=kubernetes.io/role/elb \
  --query 'Subnets[].[SubnetId, AvailabilityZone, CidrBlock, MapPublicIpOnLaunch]' --output table
# The preview nodes' subnets: the cluster's private subnets, in the zones chosen above.
aws ec2 describe-subnets --region us-west-2 \
  --subnet-ids $(aws eks describe-cluster --name acme-prod --region us-west-2 \
    --query cluster.resourcesVpcConfig.subnetIds --output text) \
  --query 'Subnets[?MapPublicIpOnLaunch==`false`].[SubnetId, AvailabilityZone, CidrBlock]' --output table

# Your own address, for previews.inbound_cidrs: the curls in steps 11 and 14 come from it.
echo "$(curl -s https://checkip.amazonaws.com)/32"
```

## The path

Each row is one step. The sections after the table give the commands and the values files.

| #   | Step                                     | What does it                                                                                                                                                 |
| --- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 0   | Prerequisites                            | [Above](#prerequisites): Auto Mode, NetworkPolicy enforcement, zones, subnets, a model credential                                                            |
| 1   | Install the CLI                          | The release archive from `devthenet-labs/patchy`, verified with cosign ([Install](../cli.md#install))                                                        |
| 2   | Create the intent repository and the App | `gh repo create acme/intents`; `patchy setup github-app --org acme --intents --checks` writes the App's Secret manifest; install the App on the repositories |
| 3   | Scaffold the application repository      | `patchy init app` ([Onboarding an application](onboarding-app.md)); push it                                                                                  |
| 4   | Terraform, phase 1                       | The reference module: OIDC provider, source-controller identity, preview node role, certificates, each app's ECR repositories and publisher roles            |
| 5   | Namespace and Secrets                    | The `patchy` namespace with the `restricted` Pod Security labels; `patchy-github` from step 2; `patchy-anthropic`                                            |
| 6   | Helm stage 1: controllers and intents    | `patchy` with your values and `helm_values`, previews off; `patchy-config` with the Forge                                                                    |
| 7   | Edge DNS                                 | `create_edge_alias_records = true`, apply again                                                                                                              |
| 8   | Publish the application's agent image    | The module's dotenv into the repository's variables; `AGENT_PUBLISH_ENABLED`; run `agent image`; confirm the tag                                             |
| 9   | Project, and the first intent            | A `projects` entry in `patchy-config`; `patchy check project shop-web`; an issue with the trigger label. Intents work from here on, without previews         |
| 10  | Helm stage 2: the preview foundation     | `preview.enabled`, `preview.nodeIsolation.create`, the placeholder. The preview load balancer is created and starts billing                                  |
| 11  | Preview DNS                              | `create_preview_alias_record = true`, apply again                                                                                                            |
| 12  | The isolation probe                      | `PREVIEW_PUBLISH_ENABLED`; a disposable pull request's image; `hack/preview-isolation-probe/run.sh`                                                          |
| 13  | Helm stage 3: the preview controller     | `previewController.enabled`                                                                                                                                  |
| 14  | Preview the Project                      | `preview` on the Project; `patchy check project shop-web` again; the next intent's pull request gets a URL                                                   |

Record a rollback point before every Helm step: the current revision of both releases.

```sh
helm history patchy -n patchy --max 1; helm history patchy-config -n patchy --max 1
```

## 1. Install the CLI

The supported install is the `patchy-cli` release archive attached to each release of `devthenet-labs/patchy`, verified
against its keyless cosign signature. The steps are on [the CLI page](../cli.md#install). Use the CLI of the release you
deploy: `patchy init app` pins the agent base image and the terraform module to its own release.

## 2. The intent repository and the GitHub App

Intents are issues in a repository of their own, which every Project names as its `intentRepository`. Any repository
works; a dedicated one keeps intents apart from code:

```sh
gh repo create acme/intents --private --description "Intents for patchy"
```

Create the App with the CLI. `--intents` asks for exactly what intent-controller uses, `--checks` adds the reads that
automatic check-fix rounds need (a Project with `spec.checks.fix`), `--rerun-failed` adds the Actions write that
re-running a failed check before a round needs (a Project with `spec.checks.rerunFailed`, off by default), and none
needs a webhook, because intent-controller polls GitHub:

```sh
patchy setup github-app --org acme --intents --checks
# With the security-findings pipeline too, which does need the webhook:
patchy setup github-app --org acme --intents --checks --security --webhook-url https://patchy.acme.dev/github/webhooks
```

You must be an owner of the organization. A browser page opens and posts the App manifest to GitHub; check the form and
click **Create GitHub App**. The CLI writes the App's credentials as a Secret manifest, `patchy-github.secret.yaml`,
mode 0600; it holds the only copy of the private key, so apply it in step 5 and then delete it (or encrypt it, with
`-o - | sops ...`). [Creating the GitHub App](../cli.md#creating-the-github-app) has every flag, and
[Create the GitHub App](../getting-started/github-app.md#intents) the permissions and why each is needed:

| Repository                     | Permission                       | Access       |
| ------------------------------ | -------------------------------- | ------------ |
| The intent repository          | Issues                           | Read & write |
| Each app repository            | Contents, Pull requests          | Read & write |
| Each app repository            | Issues                           | Read         |
| With `spec.checks.fix`         | Checks, Commit statuses, Actions | Read         |
| With `spec.checks.rerunFailed` | Actions                          | Read & write |

Then install the App from the link the CLI prints. Choose **Only select repositories** and pick the intent repository
and every application repository: an App installed on all repositories can act on all of them. Add each new application
repository to the installation as you onboard it; until then its Project reports `AppNotInstalled`.

If you name the Secret something other than `patchy-github` (`--secret-name`), name it in the Forge's `secretRef` and in
the chart's `intentController.forgeSecrets` too: intent-controller may read only the Secrets listed there.

## 3. Scaffold the application repository

Follow [Onboarding an application](onboarding-app.md) up to its terraform step: create `acme/Shop.Web` (from a template
repository, or empty), run `patchy init app` in it, and push. Its CI runs, and both publishers skip, since no variable
is set yet. The page also covers an application that already exists.

A repository created now did not exist when you installed the App in step 2: add `acme/Shop.Web` to the installation
(the organization's **Settings > GitHub Apps > Configure > Repository access**), or its Project fails `ready` with
`AppNotInstalled` in step 9.

## 4. Terraform, phase 1

The reference module lives in this repository at `deploy/terraform/aws`, versioned with the chart: use the same release
tag for both, so the values it emits always match the chart's keys. It has no provider or backend block; your root
supplies both, and re-exports the outputs you read with `terraform output`:

```hcl
# infra/patchy.tf
terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0" }
  }
  # backend "s3" { ... }   # any state backend works: use the one the rest of your infrastructure uses
}

provider "aws" {
  region = "us-west-2"
}

module "patchy" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws?ref=vX.Y.Z"

  cluster_name = "acme-prod" # the service CIDR, VPC and cluster security group are read from it
  namespace    = "patchy"    # the Helm release namespace

  # Null creates the GitHub Actions OIDC provider. An account holds one per issuer:
  # pass its ARN when it already exists (step 0 prints it).
  github_oidc_provider_arn = null

  apps = {
    shop-web = { # the image slug, independent of the GitHub name
      github = {
        # gh api repos/acme/Shop.Web --jq '{owner: .owner.login, name: .name, repository_id: .id,
        #   owner_id: .owner.id, default_branch: .default_branch}'
        owner          = "acme"
        name           = "Shop.Web"
        repository_id  = "123456789"
        owner_id       = "987654"
        default_branch = "main"
        # gh api repos/acme/Shop.Web/actions/oidc/customization/sub --jq .sub_claim_prefix
        sub_claim_prefix = "repo:acme@987654/Shop.Web@123456789"
      }
    }
  }

  edge = {
    zone_id      = "Z0123456789EDGE" # the acme.dev hosted zone
    webhook_host = "patchy.acme.dev"
    status_host  = "status.patchy.acme.dev"
    alb_name     = "acme-prod-patchy" # = edgeIngressClass.loadBalancerName below
  }

  previews = {
    host_suffix     = "preview.acme-apps.dev"
    zone_id         = "Z0123456789PREVIEW"                                     # the acme-apps.dev hosted zone
    alb_subnet_ids  = ["subnet-0a1b2c3d4e5f60001", "subnet-0a1b2c3d4e5f60002"] # public, tagged kubernetes.io/role/elb
    node_subnet_ids = ["subnet-0a1b2c3d4e5f60011", "subnet-0a1b2c3d4e5f60012"] # private, in the same zones
    inbound_cidrs   = ["203.0.113.10/32"]                                     # your own address, and each reviewer's
  }

  create_edge_alias_records   = false # step 7
  create_preview_alias_record = false # step 11
}

output "helm_values" {
  value = module.patchy.helm_values
}
output "github_variables_dotenv" {
  value = module.patchy.github_variables_dotenv
}
output "preview_node_class" {
  value = module.patchy.preview_node_class
}
output "apps" {
  value = module.patchy.apps
}
```

```sh
terraform -chdir=infra init
terraform -chdir=infra plan -out=phase1.tfplan
terraform -chdir=infra apply phase1.tfplan
```

The apply waits until ACM has issued both certificates, so the zones must already resolve. The plan checks the subnets:
load balancer subnets tagged, in distinct zones and `/20` or narrower, node subnets private and each in one of the load
balancer subnets' zones, all in the cluster's VPC. `edge` is optional: leave it out and no edge certificate or alias is
made, and the edge works with any ingress and certificate source, cert-manager included. Intents need no edge at all
(they poll GitHub); the edge serves the security pipeline's webhook and the status page. The `sub_claim_prefix` must be
GitHub's immutable form, `repo:<owner>@<owner_id>/<name>@<repository_id>`; a repository still on the classic
`repo:<owner>/<name>` subject fails the plan until it is switched to immutable subject claims, as below. The module
reference lists [every input and output](terraform-module.md#reference) and what each resource is.

!!! note "Immutable subject claims"

    GitHub gives a repository created on or after 15 July 2026 the immutable subject; an older repository keeps the
    classic one until it opts in. Read the setting first:

    ```sh
    gh api repos/acme/Shop.Web/actions/oidc/customization/sub
    ```

    `"use_immutable_subject": true` and a `sub_claim_prefix` of the form above mean nothing is to do. Otherwise opt the
    repository in, which takes a repository admin (or the repository's OIDC settings page; an organization owner can
    opt in every repository at once with `PUT orgs/acme/actions/oidc/customization/sub`, see
    [GitHub's OIDC reference](https://docs.github.com/en/actions/reference/security/oidc#immutable-subject-claims)):

    ```sh
    gh api -X PUT repos/acme/Shop.Web/actions/oidc/customization/sub \
      -F use_default=true -F use_immutable_subject=true
    ```

    `use_default=true` also drops a custom `include_claim_keys` template, which the module refuses too. **The switch
    changes the `sub` claim of every workflow in the repository**: a cloud trust policy that matches the classic
    `repo:acme/Shop.Web:*` stops matching. Let each such policy accept both forms before you switch, and drop the
    classic one after.

## 5. Namespace and Secrets

```sh
kubectl create namespace patchy
kubectl label namespace patchy \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

kubectl apply -f patchy-github.secret.yaml && rm patchy-github.secret.yaml
kubectl -n patchy create secret generic patchy-anthropic --from-literal=api-key="$ANTHROPIC_API_KEY"
```

The model credential lives in the release namespace with the egress broker, its only reader: no agent pod ever holds it.
A `claude setup-token` OAuth token works too, with `egressBroker.anthropicAuth: token`.

## 6. Helm stage 1: controllers and intents

Two values files go to the `patchy` chart. Yours holds every decision and every security opt-out, as explicit lines; the
module's `helm_values` holds only what the infrastructure decides (the source-controller ServiceAccount name the Pod
Identity association binds, the agent registry allowlist, the preview infrastructure keys, the edge hosts and their
certificate annotations). Pass `helm_values` **last**, so it wins for the keys it owns.

```yaml
# patchy-values.yaml: the operator's decisions
config:
  logLevel: info

# Auto Mode's node-local CoreDNS: the tenth address of the service CIDR.
clusterDNSCIDR: 172.20.0.10/32

# The webhook and status edge: one ALB, named as the module's edge.alb_name.
edgeIngressClass:
  create: true
  loadBalancerName: acme-prod-patchy
webhook:
  ingress:
    enabled: true # an empty className selects the edge class above
    annotations:
      alb.ingress.kubernetes.io/target-type: ip
      alb.ingress.kubernetes.io/healthcheck-port: "8081"
      alb.ingress.kubernetes.io/healthcheck-path: /healthz
statusServer:
  ingress:
    enabled: true
    annotations:
      alb.ingress.kubernetes.io/target-type: ip
      alb.ingress.kubernetes.io/healthcheck-port: "8081"
      alb.ingress.kubernetes.io/healthcheck-path: /healthz

# Brokered claude, the only runner intents use.
egressBroker:
  anthropicSecret: patchy-anthropic
  anthropicAuth: key
  # The enforced bound on what an agent pod spends once repositoryImages is on. Every limit
  # is off here: an explicit opt-out until they are sized (see below).
  limits: {}

agent:
  # Builds run in the application's own toolchain image, which .patchy/agent.yaml declares.
  # registries comes from helm_values.
  repositoryImages:
    enabled: true
    # The generated publishers do not sign, so this is an explicit opt-out of signature
    # checks: registry write access alone decides what an agent runs, that is the publisher
    # roles and any other principal with ECR push on patchy/app-envs/*. Set cosignPublicKey
    # instead to require signatures.
    allowUnsigned: true
    onReject: default
    ephemeralStorage: 8Gi
  networkPolicy:
    mode: none # Auto Mode enforces plain NetworkPolicy, no hostname dialect
    broadEgress: never # brokered claude needs no 443 to anywhere; repositoryImages requires it

intentController:
  enabled: true
  forgeSecrets:
    - patchy-github
```

```sh
helm upgrade --install patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy --version X.Y.Z \
  --namespace patchy -f patchy-values.yaml -f <(terraform -chdir=infra output -raw helm_values)
```

**Spend limits.** A build or revise run executes in the application's own image, so its in-pod token budget, and the
cost a Project's `limits.maxCostMicroUSD` checks, are reported from inside that image and only advisory.
[`egressBroker.limits`](../deployment/helm.md#egress-broker-limits) is the enforced bound, checked by the broker before
any model call; every limit is off by default, and this file leaves them off as an explicit opt-out. Off, what bounds a
build pod is time: its Job deadline (`intentController.config.jobDeadline`, 90 minutes) at whatever rate the model
provider allows the key, so set a spend limit on the key at the provider too. To size the broker's limits, read the
per-pod totals on its audit line after the first intents (`kubectl -n patchy logs deploy/patchy-egress-broker`); note
that `tokensPerPod` counts input, cache and output tokens, while a stage's `tokenBudget` counts output tokens only, so
it must sit far above the build stage's 800000. A run that ends early (its stage timeout, a token budget, a crash) still
records what it spent, as its stream reported it call by call: the input and cache tokens exactly, but output tokens
only as counted when each call started, so the cost recorded for such a run is a floor.

**The Ingresses.** Intents need neither: an App created with only `--intents --checks` has no webhook, the webhook
Ingress serves only the security pipeline (`--security`), and the status Ingress the status page. Keeping both, as here,
is harmless and leaves the webhook ready for `--security` later. Without them, set both `ingress.enabled: false`, leave
`edgeIngressClass` and the module's `edge` out, and skip step 7.

Then the `patchy-config` release, with the Forge that lets the controllers reach `acme`'s repositories (and, for the
security pipeline, an Integration as in
[Install with Helm](../getting-started/install.md#switch-the-pipeline-on-the-patchy-config-chart)):

```yaml
# patchy-config-values.yaml
forges:
  - name: github
    spec:
      provider: github
      secretRef:
        name: patchy-github # keys appID and privateKey
      orgs: [acme]
      interval: 10m
projects: [] # step 9
```

```sh
helm upgrade --install patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config --version X.Y.Z \
  --namespace patchy -f patchy-config-values.yaml
kubectl -n patchy get deploy,forges
```

Every Deployment should be available and the Forge `Ready`. The status page starts in its rollups-only posture (public
statistics, no findings, no sign-in), the one posture safe to expose without an identity provider; see
[the status page](../status-ui.md#access-model) before you configure sign-in. **Rollback point:** none before the first
install (`helm uninstall` removes it, keeping the CRDs); record the revisions now.

## 7. Edge DNS

Once the edge Ingresses exist, Auto Mode creates the `acme-prod-patchy` load balancer. Point the hosts at it:

```hcl
  create_edge_alias_records = true
```

```sh
terraform -chdir=infra apply
curl -sS -o /dev/null -w '%{http_code}\n' https://status.patchy.acme.dev/
```

The plan fails while the load balancer does not exist yet; wait for `kubectl -n patchy get ingress` to show an address.
With `--security`, the App's webhook now reaches the integration-controller.

## 8. Publish the application's agent image

Set the repository's variables from the module's dotenv output, on the repository, never on the organization: an
organization variable reaches every repository with these workflows, a template repository and all its copies included.
Then turn on the agent publisher and build the image:

```sh
terraform -chdir=infra output -json github_variables_dotenv | jq -r '."shop-web"' \
  | gh variable set -f - --repo acme/Shop.Web
gh variable set AGENT_PUBLISH_ENABLED --repo acme/Shop.Web --body true
gh workflow run "agent image" --repo acme/Shop.Web
# when "agent image" and then "publish images" have succeeded:
aws ecr describe-images --region us-west-2 --repository-name patchy/app-envs/shop-web --image-ids imageTag=toolchain-v1
```

[Onboarding an application](onboarding-app.md#publishing-the-images) explains the two gates and their order.
`PREVIEW_PUBLISH_ENABLED` waits for step 12.

## 9. The Project, and the first intent

A `Project` ties an intent repository, its approvers and the application repository together. Add it to
`patchy-config-values.yaml` and upgrade the release:

```yaml
projects:
  - name: shop-web # at most 25 characters; its intents are shop-web-<issue>
    spec:
      intentRepository: https://github.com/acme/intents
      approvers:
        # The only accounts whose labels and commands count. Include your own login to open
        # the first intents yourself.
        logins: [octocat]
      repositories:
        - name: shop-web # the key: a DNS label of at most 16 characters
          url: https://github.com/acme/Shop.Web
      checks:
        fix: [test] # a failing `test` check on patchy's PR starts a fix round
        # rerunFailed: true # re-run a failed Actions check once first; needs Actions read & write
```

`checks.fix` names check runs: `test` is the job of the CI `init app` generates for a new application. A repository
scaffolded with `--existing` has no `test` check from patchy (its `runtime image` workflow's job is `build`), so name
your own CI's check runs instead, as GitHub lists them for the default branch's head:

```sh
gh api repos/acme/Shop.Web/commits/main/check-runs --jq '.check_runs[].name'
```

Each approver also needs **write** access to the intent repository (Write, not Triage): a label or command from an
approver without it is refused. Grant it to each, or to a team:

```sh
gh api -X PUT repos/acme/intents/collaborators/octocat -f permission=push
gh api -X PUT orgs/acme/teams/intent-approvers/repos/acme/intents -f permission=push   # or a team
```

```sh
helm upgrade patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config --version X.Y.Z \
  --namespace patchy -f patchy-config-values.yaml
GH_TOKEN=$(gh auth token) patchy check project shop-web -n patchy
```

The repositories are private, so the check reads them with your GitHub token; without `GH_TOKEN` it reads anonymously
and `agent-image` is a SKIP. It reads ECR with the AWS SDK's default chain: add `AWS_PROFILE=<profile>` when that is not
the cluster's account. Every line should be PASS; the preview checks are SKIP until step 14.

Then open an issue in `acme/intents` with the label `patchy:shop-web` (the trigger label; intent-controller creates it).
patchy posts a plan on the issue; an approver adds `patchy:approved` or comments `/patchy approve`, and the pull request
follows. An issue form per Project in the intent repository (`.github/ISSUE_TEMPLATE/shop-web.yml` with
`labels: ["patchy:shop-web"]`) saves typing the label. A label applied as the issue is created, by a form or by
`gh issue create --label patchy:shop-web`, is the issue author's label event, so it starts an intent only when the
author is an approver with write access; anyone else's closes the intent with one notice before it plans. Give people
who are not approvers a form without `labels:`, and let an approver add the label. What happens on the issue is in
[intent-controller](../configuration/intent-controller.md#on-the-issue).

## 10. Helm stage 2: the preview foundation

Stage 2 renders the slot namespaces and their guardrails (admission policies, NetworkPolicies, quotas), the
`alb-preview` class, the isolated preview NodeClass and NodePool, and the kept placeholder Ingress that creates the
preview load balancer. Read the node values from terraform:

```sh
terraform -chdir=infra output -json preview_node_class
```

```yaml
# patchy-previews.yaml: added to the patchy release from this stage on
preview:
  enabled: true
  slotCount: 2 # preview slots, 1 to 4: the bound on concurrent previews and their cost
  placeholder:
    enabled: true # creates the preview load balancer; it bills from here on
  nodeIsolation:
    nodePool: patchy-preview
    nodeClass: patchy-preview
    taintKey: patchy.acme.dev/preview-only # any label key; the taint is <key>=true:NoExecute
    create: true # render the NodeClass and NodePool
    role: acme-prod-patchy-preview-node # preview_node_class.role
    subnetIDs: [subnet-0a1b2c3d4e5f60011, subnet-0a1b2c3d4e5f60012] # preview_node_class.subnet_ids
    securityGroupIDs: [sg-0123456789abcdef0] # preview_node_class.security_group_ids
    instanceTypes: [t3a.medium]
    arch: amd64
    cpuLimit: "4" # the NodePool stops launching nodes at 4 CPUs
```

```sh
helm history patchy -n patchy --max 1   # the rollback point
helm upgrade patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy --version X.Y.Z --namespace patchy \
  -f patchy-values.yaml -f patchy-previews.yaml -f <(terraform -chdir=infra output -raw helm_values)
kubectl -n patchy-preview-0 get ingress patchy-preview-placeholder \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'; echo
```

The NodePool provisions nothing until a preview Pod needs a node; each node then bills until it is consolidated, 30
seconds after its last preview Pod. `preview.nodeIsolation.cpuLimit` is the bound to budget by: Auto Mode is not known
to enforce a NodePool's node limit. If the cluster already has a NodeClass, NodePool or IngressClass of the same names
that it did not create, the upgrade fails with an ownership error and changes nothing:
[EKS Auto Mode and previews](../deployment/helm.md#eks-auto-mode-and-previews) covers adopting them.

**Rollback** from stage 2 is not just `helm rollback`: every slot namespace, guardrail, the NodeClass and NodePool and
the placeholder are kept on rollback, by design, so a rollback never leaves a slot workload without its guards. Remove
the preview DNS record first (step 11 undone), then delete the placeholder
(`kubectl -n patchy-preview-0 delete ingress,service patchy-preview-placeholder`) and check the preview load balancer is
gone, then `helm rollback patchy <revision>`. The rest is [teardown](#teardown).

## 11. Preview DNS

```hcl
  create_preview_alias_record = true
```

```sh
terraform -chdir=infra apply
dig +short shop-web-0.preview.acme-apps.dev
```

The name resolves to the preview load balancer, which serves only the wildcard certificate and admits only
`203.0.113.10/32`; from there, `https://shop-web-0.preview.acme-apps.dev/` answers 404 (the placeholder has no backend).
The plan fails while the preview load balancer does not exist yet.

## 12. The isolation probe

Before any Project gets a preview, prove that a preview Pod on a cold preview node can reach nothing: not the node's
metadata, not Pod Identity, not the Kubernetes API, not any patchy Service, not the internet, and not a sibling in the
same or another slot. The probe runs a network test from a disposable pull request's image, in the slots, as a preview
would. Turn on the runtime publisher first, so the pull request's image is published:

```sh
gh variable set PREVIEW_PUBLISH_ENABLED --repo acme/Shop.Web --body true
```

The probe and its script come from a checkout of the release you deploy, never `main`: its flags and assumptions change
between releases. Its README, `hack/preview-isolation-probe/README.md` in that checkout (on GitHub at
`https://github.com/devthenet-labs/patchy/blob/vX.Y.Z/hack/preview-isolation-probe/README.md`), explains every check.
Open a disposable same-repository pull request whose image is the probe instead of the application: on a
`test/preview-*` branch from the current default branch, the probe's `cmd/netprobe/`, and its `Dockerfile` and
`.dockerignore` in place of the application's, and nothing else.

```sh
git clone --depth 1 --branch vX.Y.Z https://github.com/devthenet-labs/patchy.git patchy-vX.Y.Z
probe=$PWD/patchy-vX.Y.Z/hack/preview-isolation-probe
gh repo clone acme/Shop.Web shop-web-probe && cd shop-web-probe
git switch -c test/preview-isolation origin/main
mkdir -p cmd && cp -R "$probe/cmd/netprobe" cmd/
cp "$probe/Dockerfile" "$probe/.dockerignore" .
git add -A && git commit -m "test: preview isolation probe, never merged"
git push -u origin test/preview-isolation
gh pr create --repo acme/Shop.Web --base main --draft \
  --title "Preview isolation probe (do not merge)" --body "Disposable: closed without merging."
gh pr diff --name-only   # cmd/netprobe/*, Dockerfile and .dockerignore only
```

The probe's Dockerfile builds with Go 1.26.6 and `GOTOOLCHAIN=local` from the repository's root `go.mod`, so that file
must exist and ask for Go 1.26.6 or older (the generated service's does). Wait until the pull request's `test` run (or
`runtime image`, under `--existing`) and then `publish images` have succeeded, and its image is in ECR:

```sh
aws ecr describe-images --region us-west-2 --repository-name patchy/previews/shop-web \
  --image-ids imageTag=sha-$(gh pr view test/preview-isolation --repo acme/Shop.Web --json headRefOid --jq .headRefOid)
```

Then run the probe from the release checkout. It needs `gh` signed in with read access to the repository, `kubectl`'s
current context on the cluster, and AWS credentials (`AWS_PROFILE` works) allowed `ecr:DescribeImages` on the image's
repository. `--dry-run` checks the values and prints the site it would probe, without calling `gh`, `aws` or `kubectl`:

```sh
cd ../patchy-vX.Y.Z
pr=$(gh pr view test/preview-isolation --repo acme/Shop.Web --json number --jq .number)
bash hack/preview-isolation-probe/run.sh --dry-run \
  --repository acme/Shop.Web \
  --image 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/shop-web \
  --taint-key patchy.acme.dev/preview-only \
  "$pr"
# the same without --dry-run
```

`--node-pool` and `--node-class` name the preview NodePool and NodeClass when they are not the default `patchy-preview`.
The script assumes the release is named `patchy` in the namespace `patchy`, and the slots `patchy-preview-0` and
`patchy-preview-1`, so at least two. All 128 cold-start connections and the 176 of the sibling run must be blocked; any
other result stops the run, and previews stay unused until it is explained. Then close the pull request without merging
it (`gh pr close test/preview-isolation --repo acme/Shop.Web --delete-branch`).

## 13. Helm stage 3: the preview controller

```yaml
# patchy-previews.yaml, added
previewController:
  enabled: true
```

```sh
helm history patchy -n patchy --max 1   # the rollback point
helm upgrade patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy --version X.Y.Z --namespace patchy \
  -f patchy-values.yaml -f patchy-previews.yaml -f <(terraform -chdir=infra output -raw helm_values)
```

The chart refuses the controller without `preview.enabled`, the placeholder and `intentController.enabled`. Its
`apiServerCIDR` (the Kubernetes API Service address) comes from `helm_values`. On its own it does nothing: only a
Project with a `preview` block is previewed. Roll back with `helm rollback patchy <revision>` once no Preview exists
(`kubectl get previews -A`).

## 14. Preview the Project

Add the runtime contract to the Project. Only the operator writes it: the image repository, the port and the readiness
path never come from an issue or an agent, and the tag only from the pull request's observed head.

```yaml
projects:
  - name: shop-web
    spec:
      # ... as in step 9
      preview:
        imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/shop-web
        port: 8080 # the one port the runtime image listens on
        readinessPath: /healthz # also the load balancer's health check
```

```sh
helm upgrade patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config --version X.Y.Z \
  --namespace patchy -f patchy-config-values.yaml
GH_TOKEN=$(gh auth token) patchy check project shop-web -n patchy
```

Every line should now be PASS, with two expected SKIPs. `preview-tls` is a SKIP from an address the preview load
balancer does not admit. `preview-image` is a SKIP until a default-branch commit is published after
`PREVIEW_PUBLISH_ENABLED` was set; with one previewed repository it is never a FAIL, since a preview runs only the pull
request's head. To publish one, push a commit, or re-run the default branch's latest `test` run (`runtime image` under
`--existing`):

```sh
gh run rerun --repo acme/Shop.Web \
  "$(gh run list --repo acme/Shop.Web --workflow test --branch main --limit 1 --json databaseId --jq '.[0].databaseId')"
```

Every intent pull request gets a preview at `https://shop-web-<issue>.preview.acme-apps.dev` once its runtime image is
published and the load balancer reports the target healthy; `kubectl -n patchy get previews` shows its phase and URL.
That includes one already open, such as step 9's: its Preview appears within about a minute of the Project gaining
`preview`, so no new intent is needed. Its head was pushed before `PREVIEW_PUBLISH_ENABLED` was set, though, so publish
its image the same way, re-running the pull request's latest `test` run (`--branch patchy-intent/shop-web-<issue>`
above). A cold preview node takes about three minutes the first time. A new push to the pull request redeploys it, the
previous revision answering until the new one is Ready (its image published and its target healthy), and merging or
closing the pull request deletes it; the node goes soon after.

Reviewers need not ask for the URL: within a minute of a preview going live, the issue's status comment links it, and
the pull request gets one comment linking it at its head, which patchy edits as the head moves, the preview redeploys or
fails, and once the intent ends ([The preview link](../configuration/intent-controller.md#the-preview-link)).

Previews are now guarded by the load balancer's IP allowlist alone. To put them behind GitHub sign-in as well, follow
[Preview sign-in](preview-sign-in.md): one Dex redirect URI, a list of viewer teams, and two more `helm upgrade`s.

## Several repositories in one Project

An application whose changes span repositories, a web front end and its API say, can be one Project over all of them.
One intent is then planned over every repository at once and built in each one the plan changes, in parallel, with a
pull request in each that links the others; with previews, one URL serves every previewed repository, each under its own
path. It is off by default.

Onboard each repository as in steps 3, 4 and 8: `patchy init app` in it, an `apps` entry of its own in the module (its
own ECR repositories, publisher roles and variables), its agent image published, and the App installed on it. Then turn
multi-repository intents on, in step 6's `intentController` block, and upgrade the `patchy` release as there:

```yaml
# patchy-values.yaml
intentController:
  enabled: true
  forgeSecrets:
    - patchy-github
  config:
    multiRepo: true
    maxConcurrentRuns: 2 # at least the repositories one intent usually changes, or its builds run one at a time
```

List the repositories in the Project. The first is the one the plan runs in; the others are fetched beside it,
read-only. `spec.preview`, the one-repository shorthand, is refused beside a second repository, so move it onto the
first repository's entry, and give each other previewed repository a `preview` of its own, under its own `path`:

```yaml
projects:
  - name: shop-web
    spec:
      # intentRepository, approvers and checks as in step 9
      repositories:
        - name: shop-web # the first: the plan's working tree
          url: https://github.com/acme/Shop.Web
          preview: # step 14's spec.preview, moved here; path defaults to /
            imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/shop-web
            port: 8080
            readinessPath: /healthz
        - name: shop-api
          url: https://github.com/acme/Shop.Api
          preview:
            imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/shop-api
            port: 8080
            readinessPath: /api/healthz # the app serves under its path: the load balancer does not rewrite it
            path: /api
```

Upgrade `patchy-config` and run `GH_TOKEN=$(gh auth token) patchy check project shop-web -n patchy` again. What changes:

- **Without `multiRepo`**, a Project listing several repositories is not `Ready` (`UnsupportedRepositories`), and its
  intents are held where they stand. Turning it off is the supported rollback; do not roll the chart back below 0.12.16
  while such an intent is open (cancel or suspend it first).
- **One plan, one build per repository.** The approver approves one plan naming the repositories that must change; each
  gets its own build, in its own agent image, launched together as `maxConcurrentRuns` allows. The Project's
  `limits.maxCostMicroUSD` is checked once before they launch, so one intent's builds can pass it by up to one fewer
  than their number.
- **The pull requests open once every build has pushed**, each commented with links to the others. Revision and
  check-fix rounds run on each pull request's own repository, one round at a time per intent. The intent ends `Merged`
  once every pull request has merged; one closed unmerged ends it `Closed` once the others settle, and what merged stays
  merged.
- **Previews** serve at most four repositories, each at its own path on the one host. Components reach each other only
  from the browser, same-origin and by path; the slot's NetworkPolicy blocks every server-side call between them. A
  previewed repository the intent did not change runs its default branch as it was when review began, so its runtime
  image must be published for every default-branch commit. The generated publishers do that, and the module keeps the
  newest 20 `main-<SHA>` images past the PR images' expiry; never cancel or skip a default-branch build. With more than
  one previewed repository, `preview-image` is therefore a FAIL, not a SKIP, while a default branch's head image is
  missing.

0.12.16 has run this end to end on a live cluster: a plan over two repositories in about four minutes, both builds at
once, two cross-linked pull requests and one preview of both; the intent stayed in review after the first merge, ended
`Merged` after the second, and its preview was torn down. The agents cost $1.51 in all. The full rules are in
[intent-controller](../configuration/intent-controller.md#several-repositories) and, for previews,
[preview-controller](../configuration/preview-controller.md).

## Sizing agents

Agent Jobs request no CPU or memory unless you set some. They are then BestEffort pods: the scheduler puts them on any
node with room, a small one shared with patchy's controllers included, and they get only what that node has spare. That
suits plans and the security pipeline's Jobs, which mostly wait on the model. It starves a build that runs a heavy test
suite.

overdub, a browser app tested with Chromium, found this out. Its build ran the app's CI subset (`npm run test:ci`, 11
browser suites, three at a time) on the cluster's one `c6a.large`, shared with the controllers: 2 vCPU and 3.7 GiB, of
which pods get about 1.8 CPUs and 3 GiB. The command never returned, and the agent waited until the build's one-hour
timeout. The same subset takes under two minutes on a 2-vCPU CI runner running two suites at a time.

The fix is a **resource class**: a named size you define in the `patchy` chart and a Project picks for one repository.
That repository's builds, revise rounds and check-fix rounds run on it; plans, the Project's other repositories and the
security pipeline stay on the default.

```yaml
# patchy-values.yaml
agent:
  resources:
    classes:
      large:
        requests:
          cpu: 4
          memory: 8Gi
        limits:
          memory: 10Gi # above the request, and no CPU limit: see below
```

```yaml
# patchy-config-values.yaml
projects:
  - name: shop-web
    spec:
      # intentRepository, approvers and checks as in step 9
      repositories:
        - name: shop-web
          url: https://github.com/acme/Shop.Web
          agentResourceClass: large
```

Upgrade `patchy` first, then `patchy-config`. The `patchy` upgrade adds the field to the Project CRD and defines the
class, and restarts intent-controller alone; an older CRD would drop `agentResourceClass` without a word, and a Project
that picks a class intent-controller does not define holds that repository's builds (see below). Each class must set
`requests.cpu`, `requests.memory` and `limits.memory`; every one is checked when intent-controller starts (positive, at
most 64 CPUs and 512 GiB, each request at or below its limit), and a bad one stops it from starting, its log naming the
class. Check `kubectl -n patchy get pods` after the upgrade.

The classes are the spend ceiling: a Project can pick only a class you defined, so the largest is the most any one agent
Job can request, and nothing in a repository, an issue or an agent's output can pick one. To choose the numbers:

- **Requests are what you pay for.** EKS Auto Mode launches nodes to fit pods' requests, and a node gives its pods less
  than its nominal size, because the kubelet, the system and the DaemonSets take a share: a `c6a.large` offers about
  1780m of its 2 vCPU. A request of 4 CPUs therefore needs an instance with more than 4 vCPUs, in practice an 8-vCPU
  one. To fit a 4-vCPU, 8-GiB instance instead, ask for about `3500m` and `6Gi`.
- **No CPU limit.** A CPU limit throttles a build whenever it bursts, and browsers and Node test runners burst. The
  request alone reserves the CPU, and the pod may still use idle cores.
- **A memory limit somewhat above the request.** On cgroup v2 a container that reaches its memory limit is OOM-killed
  whole, the agent with the build, and the attempt is spent; the run's detail then says so, naming the limit and the
  class. A memory-backed `/dev/shm` for Chromium, if your agent image mounts one, counts against the same limit.
- **Both containers get the class.** The prepare init container runs before the agent, so the pod reserves the class
  once, not twice.
- **Start from the app's CI.** Pick a class at least as large as the CI runner its tests pass on, then judge it by real
  builds (EKS Auto Mode installs no metrics-server): their timings, and any run whose detail reports an OOM kill.

`agent.resources.default` sizes every agent Job without a class: the security pipeline's Jobs, plans, evaluation units
and the builds of repositories that pick none. Leave it `{}` unless you want each of them reserved: once it is set,
every agent Job is Burstable and may launch a node of its own. At worst, agents together request the default times the
agent Jobs that can run at once (investigations, remediations and evaluation units) plus the largest class times
`intentController.config.maxConcurrentRuns`. patchy caps neither total; a ResourceQuota on `patchy-agents` can, once a
default is set (a quota on CPU or memory refuses pods that request none).

What patchy does when something goes wrong:

| When                                                                                             | What happens                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| ------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The Project picks a class intent-controller does not define (a typo, a class removed or renamed) | That repository's run waits `Pending` with no Job, no slot and no attempt spent. The intent is `Blocked` with `ResourcesUnavailable` (`UnknownResourceClass`), naming the class and the classes defined; the Project stays `Ready` and reports `ResourceClassesResolved: False`, so plans and the other repositories carry on. Defining the class or changing the pick lifts the block, and the same run launches.                                  |
| No node can fit the pod (a class larger than any instance the cluster launches)                  | After 10 minutes `Unschedulable`, long enough for Auto Mode to add a node that fits, patchy deletes the Job and ends the run `unschedulable`, with the scheduler's message in its detail. No attempt is spent, and nothing is retried until something changes: a plan or build blocks its intent (`ResourcesUnavailable`, `Unschedulable`) until the Project changes or intent-controller restarts; a revise or check-fix round ends with a notice. |
| The build runs out of memory                                                                     | The agent container is OOM-killed and the run fails, its detail naming the kill, the memory limit and the class to raise. The attempt counts: the agent ran.                                                                                                                                                                                                                                                                                        |
| Auto Mode consolidates its nodes                                                                 | Nothing: every agent pod carries `karpenter.sh/do-not-disrupt: "true"`, so a running agent is never evicted to consolidate or replace its node. A node failure or a spot interruption can still end a run.                                                                                                                                                                                                                                          |

Auto Mode removes an empty node within a minute, so a build on a large class may start on a fresh node and pull its
agent image again; expect that in the time from the Job's creation to the agent's start.

## TLS: ACM only, for previews

The preview edge terminates TLS with ACM and nothing else. Every preview host is one label under the wildcard, served by
the one `alb-preview` load balancer with the wildcard certificate attached to its class (`IngressClassParams`
`certificateARNs`). The slot admission policy refuses an Ingress with `spec.tls`, so a certificate Secret, which is what
cert-manager produces, has nowhere to attach; and a wildcard from cert-manager would need DNS-01 credentials inside the
cluster, next to the controller that deploys unreviewed code. cert-manager is therefore not supported for previews.

patchy's own edge, the webhook and status page, is different: it works with any ingress controller and certificate
source. The module's `edge` and the chart's `edgeIngressClass` are the ACM path on Auto Mode; leave both out and use
`webhook.ingress.tls` with cert-manager, or Gateway API (`webhook.httpRoute`), as on any cluster
([Webhook exposure](../deployment/webhook.md)).

## DNS: two phases, in terraform

The load balancers are created by Auto Mode from Ingresses, not by terraform, so their DNS names exist only after the
Helm stage that creates them. The module therefore applies in phases: certificates and their validation records first,
then each alias record once its load balancer exists (`create_edge_alias_records` after stage 1,
`create_preview_alias_record` after stage 2), looked up by the fixed load balancer name. Each lookup fails the plan
while its load balancer is missing.

**external-dns is not supported yet.** The preview admission policy accepts no external-dns annotation on slot objects,
and external-dns's ingress source would also write a record per preview host and for the placeholder, which upsert-only
syncing leaves behind; none of this is tested. Use the terraform alias records. If the cluster runs external-dns for
other zones, keep the preview zone out of its `--domain-filter`.

## Verification

`patchy check project shop-web -n patchy` is the preflight for one Project. It reads the cluster with your kubeconfig,
GitHub with `GH_TOKEN`, and the registry with your own AWS credentials (`AWS_PROFILE` works), and prints one line per
check:

| Check           | Proves                                                                                                           |
| --------------- | ---------------------------------------------------------------------------------------------------------------- |
| `ready`         | intent-controller's own verdict, reached by minting the App's scoped tokens in the cluster: the App is installed |
| `intent-names`  | No two issues claim the same intent name                                                                         |
| `forge`         | Each repository resolves to one Ready Forge                                                                      |
| `labels`        | The trigger and approve labels exist                                                                             |
| `agent-image`   | The declared agent image passes source-controller's checks under its live allowlist                              |
| `previews`      | intent-controller writes Previews and preview-controller is configured                                           |
| `preview-image` | The preview image repository is under the preview prefix, with `sha-<default-branch head>` published             |
| `preview-dns`   | `shop-web-0.preview.acme-apps.dev` resolves to the preview load balancer                                         |
| `preview-tls`   | The name serves a trusted certificate; a SKIP from outside the inbound CIDRs, where the handshake times out      |

What it **cannot** prove, and where to look instead:

- **That NetworkPolicy is enforced**, and that a preview Pod is isolated: the isolation probe (step 12) is the only
  evidence. The check never reads `kube-system/amazon-vpc-cni`.
- **That source-controller's own AWS identity can read the agent image**: the check uses _your_ credentials. After the
  first intent's plan, the Repository's `status.runnerImage` (`kubectl -n patchy get repositories -o yaml`) shows what
  source-controller pinned, or why it refused.
- **That the preview nodes can pull the runtime image**: a Preview's status, and its Pod's events in the slot namespace.
- **The publisher roles' scoping**: the application repository's Actions runs show what was published; IAM is the
  boundary, and only a role's trust policy proves what it admits.
- **Who can reach previews**: only the load balancer's security group, which the chart renders from
  `preview.inboundCIDRs`.

## Upgrades

- **One release, three artifacts.** Upgrade the chart, the CLI and the module ref together: `helm_values` carries chart
  keys, and `patchy init app` scaffolds against its own release.
- **A new resource class goes into `patchy` before `patchy-config` picks it**, and a class leaves `patchy-config` before
  it leaves `patchy`; out of order, the repository's builds wait (they are never lost) until both agree. See
  [Sizing agents](#sizing-agents).
- **Record the rollback point** (both releases' revisions) before each `helm upgrade`, and read `helm status` before
  retrying one that failed: an upgrade interrupted by a network drop may have applied part of a revision.
- **Re-run the isolation probe** after every EKS, Auto Mode or VPC CNI upgrade, and before relying on previews again. An
  upgraded network policy agent is exactly what the probe exists to catch.
- **`previewController.config.targetHealth` defaults to `true` from 0.12.16**; up to 0.12.15 it was `false`. An upgrade
  of an install with previews that never set it turns it on: a Preview already Ready is not regated, but one deploying
  during the upgrade spends one rollout retry. Upgrade while no Preview is deploying, or set the value explicitly
  ([the upgrade note](../deployment/helm.md#eks-auto-mode-and-previews)).
- **Re-enabling previews over retained guardrails.** After a rollback or uninstall kept older slot admission policies,
  first upgrade with `preview.enabled: true` and `preview.placeholder.enabled: false`, which updates the guardrails and
  creates no load balancer. Check with a server-side dry run that the new policy admits the placeholder, then enable the
  placeholder in a separate revision.
- **The reviewers' addresses** are `preview.inboundCIDRs`, each a `/32`, at most 8: a change is a terraform apply
  (`previews.inbound_cidrs`) followed by a Helm upgrade.

## Teardown

In this order; each step depends on the one before it.

1. **Stop new intents and previews.** Remove each Project's `preview` block, or delete its `projects` entry, upgrade
   `patchy-config`, and wait until `kubectl get previews -A` lists nothing: each Preview's finalizer empties its slot.
   `spec.suspend: true` is not enough on its own: it stops new intents and runs, but leaves the Previews of open pull
   requests in place.
2. **Turn the publishers off**: `gh variable set PREVIEW_PUBLISH_ENABLED --repo acme/Shop.Web --body false`, and the
   same for `AGENT_PUBLISH_ENABLED`.
3. **Preview DNS**: `create_preview_alias_record = false`, apply.
4. **The preview controller**: an upgrade with `previewController.enabled: false`.
5. **The preview load balancer**: delete the kept placeholder,
   `kubectl -n patchy-preview-0 delete ingress,service patchy-preview-placeholder`, and wait until the load balancer is
   gone (`aws elbv2 describe-load-balancers --names acme-prod-preview` fails).
6. **The preview nodes**: `kubectl delete nodepool patchy-preview`, wait until its nodes are gone
   (`kubectl get nodeclaims`), then `kubectl delete nodeclass patchy-preview`.
7. **The preview foundation**: an upgrade with `preview.enabled: false`. Every slot namespace and guardrail is kept;
   once each slot is empty (`kubectl get all,ingresses -n patchy-preview-0`, and each other slot), delete the slot
   namespaces, the `patchy-preview-*` ValidatingAdmissionPolicies and bindings, and the `alb-preview` IngressClass and
   IngressClassParams, deliberately. Keep the guards while any preview workload or node remains.
8. **Edge DNS and patchy**: `create_edge_alias_records = false`, apply; `helm uninstall patchy-config -n patchy`, then
   `helm uninstall patchy -n patchy`. The edge Ingresses go with the release, and their load balancer with them; the
   kept edge IngressClass and IngressClassParams, and the CRDs (with any custom resources left), are deleted by hand if
   you mean to.
9. **Terraform**: `terraform destroy`. An ECR repository that still holds images fails to delete, on purpose (no
   `force_delete`): delete its images first if you mean to.
10. **GitHub**: uninstall and delete the App, and delete the repositories' Actions variables.

## Limitations

- **EKS Auto Mode and ECR only**, and previews only through the Helm chart.
- **Previews are reachable from an IP allowlist only**: each reviewer address is a `/32`, at most 8, and each change is
  a Helm upgrade. There is no sign-in in front of a preview; teams on dynamic addresses or a VPN range cannot use them
  realistically yet.
- **A fixed slot quota and fixed container limits.** `preview.slotCount` (1 to 4) slots, one Preview each; more wait in
  a queue. A slot holds at most 5 Services and 8 Pods. Every preview container runs with 25m CPU and 32 MiB requested,
  limited to 250m CPU and 256 MiB of memory; none of it is configurable yet.
- **One container per Pod**, one port, no volumes (a read-only root filesystem and no writable `/tmp`), and the runtime
  contract in [Onboarding an application](onboarding-app.md#the-runtime-contract).
- **A resource class sizes a pod, not where it runs.** Agent Jobs carry no node selector or toleration, so every class
  lands on Auto Mode's general-purpose pool, which has no limits of its own.
- **amd64 by default.** The generated publishers build and accept `linux/amd64` images only, the preview NodePool is
  `amd64` by default, and agent Jobs carry no node selector, so the nodes they land on must run amd64 images (Auto
  Mode's built-in general-purpose pool launches amd64 nodes).
- **At most eight repositories per Project, four of them previewed**, and more than one only with
  `intentController.config.multiRepo` ([Several repositories in one Project](#several-repositories-in-one-project)).
  Previewed components reach each other from the browser only, never server-side.
- **One preview-enabled release per cluster.**
- **The isolation probe tests observed connectivity**, not the Auto Mode network policy agent's own logs, which are not
  yet available to inspect directly.
