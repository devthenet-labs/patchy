# Onboarding an application

An application repository is ready for intents when patchy can build in it and, with previews, run it. Three things make
that so: an **agent image**, the toolchain patchy's coding agent builds and tests in, declared in `.patchy/agent.yaml`;
a **runtime image** of every pull request head, which a preview runs; and **trusted publishers** that push both to ECR
from the repository's default branch, never from pull request code. `patchy init app` writes all of it, the reference
terraform module creates the registry repositories and the publisher roles, and a `Project` points patchy at the
repository.

This page uses the example site of [Deploying intents and previews](deploying.md): the application `acme/Shop.Web`, the
registry `123456789012.dkr.ecr.us-west-2.amazonaws.com`, and the Project `shop-web`. The platform (the cluster, the
chart and the module's platform half) is that page's; this one is per application, and repeats for each.

## The runtime contract

A preview runs the runtime image inside a locked-down slot. An image that breaks one of these rules fails its rollout,
and the Preview says why:

| Rule                              | What it means for the image                                                                                                                                                                                                             |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **uid 65532**, no capabilities    | The Pod runs as a non-root user with every capability dropped and no privilege escalation (Pod Security `restricted`). Declare `USER 65532:65532`.                                                                                      |
| **Read-only root filesystem**     | Nothing on the image's filesystem is writable, and there is no writable `/tmp`. Write nothing to disk: an app that writes temp files, caches or PID files at startup crashes.                                                           |
| **One port**, plain HTTP          | One container listening on one port, the Project's `preview.port`. TLS ends at the load balancer; the app speaks HTTP.                                                                                                                  |
| **Readiness path**                | The Project's `preview.readinessPath` must answer 200 once the app is ready. It is both the Pod's readiness check and the load balancer's health check, so a Preview is `Ready` only once the load balancer reports the target healthy. |
| **Immutable `sha-<40 hex>` tags** | A preview runs exactly `<imageRepository>:sha-<full head SHA>` of the pull request. Only the trusted runtime publisher pushes those tags, and ECR's immutable tags keep them from moving.                                               |
| **The NodePool's architecture**   | The preview nodes are `amd64` unless the operator chose otherwise (`preview.nodeIsolation.arch`). The generated publishers build and accept a single `linux/amd64` image.                                                               |
| **Small**                         | The publisher refuses an OCI archive over 128 MiB. A preview container runs with fixed resources: 25m CPU and 32 MiB requested, limited to 250m CPU and **256 MiB of memory**, above which it is killed.                                |
| **No egress**                     | A preview Pod reaches DNS and nothing else: no other service, no database, no internet. An app that needs a backend to start does not start.                                                                                            |

The generated Go service meets all of it: a static binary on distroless, uid 65532, port 8080 and `/healthz`.

## Naming: the repository name is yours

Nothing in patchy constrains a repository's name: `acme/Shop.Web`, `acme/billing_api` and `acme/patchy-anything` all
work. Three short, DNS-safe names are derived beside it, each chosen once:

| Name               | Example    | Where it shows                                                                                          | Rule                                        |
| ------------------ | ---------- | ------------------------------------------------------------------------------------------------------- | ------------------------------------------- |
| **Image slug**     | `shop-web` | The ECR repositories `patchy/app-envs/shop-web` and `patchy/previews/shop-web`, the publisher roles     | Lowercase letters, digits and inner hyphens |
| **Project name**   | `shop-web` | Intent names `shop-web-<issue>`, the trigger label `patchy:shop-web`, preview hosts `shop-web-<issue>.` | A DNS label of at most 25 characters        |
| **Repository key** | `shop-web` | The Project's `repositories[].name`: run and Repository names                                           | A DNS label of at most 16 characters        |

`patchy init app` derives the slug from the repository name (`Shop.Web` becomes `shop-web`; `--image-name` sets
another), and the module keys the app by it. A GitHub rename later changes only the module's `github.name` and the
Project's `url`: the slug, and every AWS name with it, stays. Keep the slug short: it is part of the publisher role
names, which IAM caps at 64 characters.

## Scaffold the repository

Run `patchy init app` in a checkout of the repository, with the CLI of the release you deploy (it pins the agent base
image and the module to its own release):

```sh
gh repo create acme/Shop.Web --private --clone && cd Shop.Web
patchy init app --registry 123456789012.dkr.ecr.us-west-2.amazonaws.com
git add -A && git commit -m "chore: scaffold with patchy init app" && git branch -M main && git push -u origin main
```

The repository and its default branch are read from `origin`. A new application gets a small Go service besides the
patchy files; [the CLI page](../cli.md#scaffolding-an-application-repository) lists every file and what it does. Its CI
(`test`) runs on the push; both publishers skip, because none of their variables is set yet. Then follow the **next
steps** `init app` prints on stderr: they are the rest of this page, with your names filled in, including the terraform
block for this application.

### An application that already exists

`--existing` writes only `.patchy/` and the CI publishers, builds the runtime image in a workflow of its own
(`runtime-image.yml`, named `runtime image`) so your CI is untouched, and ends its next steps with what to adapt:

```sh
patchy init app --existing --registry 123456789012.dkr.ecr.us-west-2.amazonaws.com
```

- **`./Dockerfile`**, at the repository root, is the runtime image `runtime-image.yml` builds, with the repository root
  as its context, for `linux/amd64` only, with a `BUILD_SHA` build argument. It must meet the runtime contract above
  (`USER 65532:65532`, nothing written to disk, one `EXPOSE`d port, the readiness path) and stay under 128 MiB as an OCI
  archive. Add one if the repository has none.
- **`.patchy/Dockerfile`** builds the agent's Go module cache from the root `go.mod` and `go.sum`: keep both out of
  `.dockerignore`'s exclusions. It pins Go 1.26.6 with `GOTOOLCHAIN=local`, so a `go.mod` that asks for a newer Go fails
  the agent image's build until you change its toolchain stage. Without a root `go.mod`, adapt it to where the module
  lives.
- **Workflow names.** The dispatcher, `publish-images.yml`, follows the builds by the names `runtime image` and
  `agent image`: rename any workflow of yours that already has one.
- **Your CI** keeps testing the application; `runtime-image.yml` only builds the runtime image (its job is `build`), and
  the publishers' own tests write `__pycache__` under `.github/actions/publish` when run locally: add `__pycache__/` to
  `.gitignore`.

There is then no `test` check from patchy: a Project's `checks.fix` names your own CI's check runs
([The Project](#the-project)).

`--existing` is for an application `init app` did not scaffold. Over a full scaffold (a copy of a template repository,
say) it leaves that scaffold's `ci.yml` building a runtime image nothing publishes any more, and that `ci.yml`'s
actionlint list leaves out `runtime-image.yml`. Retarget such a repository instead, as below.

### A template repository

A template repository saves running `init app` from scratch for every new application. Make it the pure output of
`init app` under a neutral image name, plus a README section on how to use it:

```sh
gh repo create acme/app-template --private --clone && cd app-template
patchy init app --image-name app-template --registry 123456789012.dkr.ecr.us-west-2.amazonaws.com
git add -A && git commit -m "chore: scaffold with patchy init app" && git branch -M main && git push -u origin main
gh repo edit acme/app-template --template
```

Never set the publish variables on the template, and never at the organization level: the template then publishes
nothing, and neither does a copy until its own variables are set.

A repository created from the template still carries the template's names: `app-template` in `go.mod`, the page, the
READMEs and the image `.patchy/agent.yaml` declares. **Retarget** it with a full `init app --force`, after removing the
two toolchain files, which `--force` keeps and which still name the template's agent image:

```sh
gh repo create acme/Shop.Web --private --template acme/app-template --clone && cd Shop.Web
rm .patchy/agent.yaml .patchy/Dockerfile
patchy init app --force --registry 123456789012.dkr.ecr.us-west-2.amazonaws.com
git add -A && git commit -m "chore: retarget the template to Shop.Web" && git push
```

Not `--existing`: that would keep the template's names and its `ci.yml` runtime build beside a second one. `--force`
rewrites the application's files too, which in a fresh copy are the template's. The template is a snapshot of one CLI
release; regenerate it with `init app --force` when you upgrade patchy.

## The registry repositories and publisher roles

Each application gets two ECR repositories with immutable tags, `patchy/app-envs/<slug>` (the agent image) and
`patchy/previews/<slug>` (the runtime images), and one publisher role for each. A role trusts only this repository (by
its numeric IDs and GitHub's immutable OIDC subject), only its default branch, and only its own publisher workflow,
`.github/workflows/publish-agent.yml` or `publish-runtime.yml`. No other workflow can assume these roles, and neither
role can write the other's repository. Any other IAM principal with ECR write on these repositories can still push to
them, though: account administrators, broad CI roles, anything holding `ecr:*`. Under `allowUnsigned`, that write access
decides which image an agent runs, so keep it narrow, with an ECR repository policy that denies `ecr:PutImage` to all
but the publisher role, or an SCP.

With the platform module ([Deploying, step 4](deploying.md#4-terraform-phase-1)), add an `apps` entry keyed by the slug.
Where the platform half is managed elsewhere, use the app module on its own. `init app` prints this block, pinned to its
release, with the values only GitHub knows left as placeholders beside the `gh` command that reads each (0.12.16's
`init app` prints no block, and a `gh variable set` command per variable):

```hcl
module "patchy_app_shop_web" {
  source = "git::https://github.com/devthenet-labs/patchy.git//deploy/terraform/aws/modules/app?ref=vX.Y.Z"

  slug = "shop-web"
  github = {
    owner            = "acme"
    name             = "Shop.Web"
    repository_id    = "123456789"                            # gh api repos/acme/Shop.Web --jq .id
    owner_id         = "987654"                               # gh api repos/acme/Shop.Web --jq .owner.id
    default_branch   = "main"
    sub_claim_prefix = "repo:acme@987654/Shop.Web@123456789" # gh api repos/acme/Shop.Web/actions/oidc/customization/sub --jq .sub_claim_prefix
  }
  # The account's GitHub Actions OIDC provider, created once per account.
  github_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"
}
output "patchy_app_shop_web_variables" {
  value = module.patchy_app_shop_web.github_variables_dotenv
}
```

IAM compares the repository's values case-sensitively, so take each one from the API exactly as returned (`Shop.Web`,
not `shop.web`). A repository still on GitHub's classic OIDC subject fails the plan: Deploying's step 4 shows how to
[switch it to immutable subject claims](deploying.md#4-terraform-phase-1), and what that changes for its other
workflows. If the first publish fails with `Not authorized to perform sts:AssumeRoleWithWebIdentity`, compare the
module's `oidc_subject` output with the subject GitHub reports. See [the module reference](terraform-module.md) for
every input.

## Publishing the images

### The repository variables

The publishers read their configuration from the repository's Actions variables, and nothing in it is secret: the role
trust, not the values, decides who can publish. The module emits all of them as one dotenv string per application:

```sh
# The app module on its own, through the root output above:
terraform output -raw patchy_app_shop_web_variables | gh variable set -f - --repo acme/Shop.Web
# The platform module, through a root output that re-exports github_variables_dotenv:
terraform output -json github_variables_dotenv | jq -r '."shop-web"' | gh variable set -f - --repo acme/Shop.Web
```

| Variable                   | Value                                                             |
| -------------------------- | ----------------------------------------------------------------- |
| `AWS_REGION`               | `us-west-2`                                                       |
| `ECR_REGISTRY`             | `123456789012.dkr.ecr.us-west-2.amazonaws.com`                    |
| `PUBLISH_REPOSITORY_ID`    | The repository's numeric ID, which the publisher's guard compares |
| `PUBLISH_OWNER_ID`         | The owner's numeric ID                                            |
| `AGENT_IMAGE_REPOSITORY`   | `patchy/app-envs/shop-web`                                        |
| `AGENT_ROLE_ARN`           | The agent publisher role                                          |
| `RUNTIME_IMAGE_REPOSITORY` | `patchy/previews/shop-web` (with previews)                        |
| `RUNTIME_ROLE_ARN`         | The runtime publisher role (with previews)                        |
| `AGENT_PUBLISH_ENABLED`    | `true` to publish the agent image: yours to set, never emitted    |
| `PREVIEW_PUBLISH_ENABLED`  | `true` to publish runtime images: yours to set, never emitted     |

!!! warning "Per repository, never per organization"

    Set these variables, and above all the two `*_PUBLISH_ENABLED` gates, on each repository. An organization variable
    reaches every repository that has these workflows: a template repository and every copy of it would start
    publishing, each with the wrong configuration or none.

### The gates, in order

1. **The configuration**, from the module's dotenv, first: a publisher fails closed on any variable it reads unset or
   malformed.
2. **`AGENT_PUBLISH_ENABLED=true`**, then build the agent image. A push to the default branch that changes `.patchy/`,
   `go.mod` or `go.sum` builds it, and the dispatcher then publishes it; a push made before the gate was set published
   nothing, so run the build by hand. Confirm the tag before the Project exists: a Project whose application has no
   accepted agent image blocks every build with `ImageRequired`.

   ```sh
   gh variable set AGENT_PUBLISH_ENABLED --repo acme/Shop.Web --body true
   gh workflow run "agent image" --repo acme/Shop.Web
   # once "agent image" and then "publish images" have succeeded:
   aws ecr describe-images --region us-west-2 --repository-name patchy/app-envs/shop-web --image-ids imageTag=toolchain-v1
   ```

3. **`PREVIEW_PUBLISH_ENABLED=true`**, last, once the Project previews this repository (or for the operator's isolation
   probe). From then on the runtime image of every same-repository pull request head and every default-branch commit is
   published as `sha-<commit>` (and `main-<commit>` for the default branch). Fork pull requests never publish.

A skipped publisher is not a publication: to stop publishing, set a gate to anything but `true`.

### Check the agent image before the Project

`patchy check image` judges an image the way source-controller will, and takes an image reference, not a repository.
Read the reference from the repository's declaration, in a checkout or from GitHub:

```sh
image=$(awk '$1 == "image:" {print $2}' .patchy/agent.yaml)
# without a checkout:
image=$(gh api -H 'Accept: application/vnd.github.raw' repos/acme/Shop.Web/contents/.patchy/agent.yaml \
  | awk '$1 == "image:" {print $2}')
patchy check image "$image" --allow 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/app-envs/ --run
```

`--allow` stands in for the operator's allowlist (`agent.repositoryImages.registries`), and `--run` runs the agent's
preflight in a local docker shaped like the agent pod. The signature line is a SKIP without `--cosign-key`, as it should
be under `allowUnsigned`. `check image` reads the registry with **your docker credentials**, which for ECR means one of:

- the ECR credential helper: install `docker-credential-ecr-login` and name it for the registry in
  `~/.docker/config.json`, after which it uses the AWS SDK's default chain (`AWS_PROFILE` works):

  ```json
  { "credHelpers": { "123456789012.dkr.ecr.us-west-2.amazonaws.com": "ecr-login" } }
  ```

- or a twelve-hour login:

  ```sh
  aws ecr get-login-password --region us-west-2 \
    | docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-west-2.amazonaws.com
  ```

`patchy check project`, below, reads ECR through the AWS SDK directly instead, so `AWS_PROFILE` is enough there.

### Changing the toolchain

Published tags are immutable. Edit `.patchy/Dockerfile` (or the dependencies) and bump the tag in `.patchy/agent.yaml`
to `toolchain-v2`, `toolchain-v3` and so on, in the same commit; the agent publisher reads the tag from that file and
refuses a changed image at a published tag. A dependency change needs a bump too: the agent has no network, so a module
missing from the baked cache fails its build. Intents can never change `.patchy/` or `.github/` (their changesets refuse
both), so a toolchain change is always a human's commit. If the registry moves, change `ECR_REGISTRY`,
`AGENT_IMAGE_REPOSITORY` and the image in `.patchy/agent.yaml` together.

## The Project

The Project is the operator's: it lives in the `patchy-config` chart's `projects` array, and writing one is admin-only,
because a Project is the power to point agents at repositories.

```yaml
projects:
  - name: shop-web
    spec:
      intentRepository: https://github.com/acme/intents
      approvers:
        logins: [octocat] # the only accounts whose labels and commands count
      repositories:
        - name: shop-web
          url: https://github.com/acme/Shop.Web
      preview: # leave out for intents without previews
        imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/patchy/previews/shop-web
        port: 8080
        readinessPath: /healthz
      checks:
        fix: [test] # failing checks on patchy's own PR that start a fix round
      # labels: {trigger: patchy:shop-web, approve: patchy:approved}   the defaults
      # limits: {maxActiveIntents: 2, maxCostMicroUSD: 10000000}        $10 per intent
```

`checks.fix` names check runs or commit statuses (`test` is the generated CI's job); the App then needs checks, statuses
and actions read on the repository. Under `--existing` there is no `test` check from patchy (`runtime-image.yml`'s job
is `build`): name your own CI's check runs, as GitHub lists them for the default branch's head
(`gh api repos/acme/Shop.Web/commits/main/check-runs --jq '.check_runs[].name'`). The App must be installed on the
repository before the Project is `Ready`. Upgrade `patchy-config`, then run the preflight:

```sh
helm upgrade patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config --version X.Y.Z \
  --namespace patchy -f patchy-config-values.yaml
GH_TOKEN=$(gh auth token) patchy check project shop-web -n patchy
```

Every line should be PASS, except two expected SKIPs: `preview-tls` from an address the preview load balancer does not
admit, and `preview-image` until a default-branch commit is published after `PREVIEW_PUBLISH_ENABLED` was set (push one,
or re-run the default branch's latest `test` run; with one previewed repository it is never a FAIL).
[Deploying, verification](deploying.md#verification) lists what each check proves and what none of them can. An issue in
the intent repository with the label `patchy:shop-web`, opened or labelled by an approver with write access to it, is
then the first intent. A label an issue form applies as the issue is created counts as the issue author's, so a form
with the trigger label starts an intent only for an approver.

An application that spans several repositories, a front end and its API say, is one Project over all of them: onboard
each repository as on this page, then follow
[Several repositories in one Project](deploying.md#several-repositories-in-one-project).
