# The patchy CLI

`patchy` works with the pipeline's custom resources from a terminal: list findings, read what an agent concluded, and
approve or suspend work.

It talks to the Kubernetes API with **your** kubeconfig — never through a controller, and never through the status
server. There is no patchy-specific auth, no separate endpoint to expose, and no service account acting on your behalf:
what you can do is exactly what your RBAC allows.

This page is the tour. For every command, flag and default, generated from the binary itself, see the
[command reference](cli/patchy.md).

## Install

On macOS and linux, from the Homebrew tap:

```sh
brew install bitwise-media-group/tap/patchy
```

Otherwise, binaries ship with each release, cosign-signed, for linux, macOS and windows:

```sh
# from a release archive
tar -xzf patchy-cli_<version>_<os>_<arch>.tar.gz
install -m 0755 patchy /usr/local/bin/patchy

# or from source
go install github.com/bitwise-media-group/patchy/cmd/patchy@latest
```

The cask and the archive both carry `kubectl-patchy` — brew puts it on your `PATH` for you; from an archive, put it
there yourself. Either way every command below also works as `kubectl patchy …`:

```sh
kubectl patchy get findings
```

## Shell completion

Completion covers verbs, nouns, and the enumerated flag values (phases, severities, output formats). The cask installs
all of it; from an archive, install what your shell reads — the scripts ship pre-generated under `completions/`:

```sh
install -m 0644 completions/patchy.zsh "${fpath[1]}/_patchy"                     # zsh
install -m 0644 completions/patchy.bash /usr/local/etc/bash_completion.d/patchy  # bash
install -m 0644 completions/patchy.fish ~/.config/fish/completions/patchy.fish   # fish
```

`patchy completion <shell>` prints the same script, and adds `powershell`.

`kubectl patchy …` completes through a different mechanism, and needs one more file. kubectl never reads a plugin's own
completion script: it strips its own global flags, then looks for an executable named `kubectl_complete-<plugin>` on
your `PATH` and asks that for candidates. So the hook belongs in `bin`, not in a completion directory:

```sh
install -m 0755 completions/kubectl_complete-patchy /usr/local/bin/kubectl_complete-patchy
```

It is a one-line forward to `kubectl-patchy __complete`, so both spellings complete from the same command tree and
cannot drift. Requires kubectl 1.26 or newer; the cask installs it for you.

## Grammar

```text
patchy <verb> <noun> [name...] [flags]
```

Verbs and nouns are separate axes: every verb accepts any noun it makes sense for, so learning one verb teaches you all
of them. Nouns take the same short names the CRDs declare, which means `patchy get fnd` and `kubectl get fnd` always
mean the same thing.

| Noun            | Also accepts                                |
| --------------- | ------------------------------------------- |
| `finding`       | `findings`, `fnd`                           |
| `investigation` | `investigations`, `inv`                     |
| `remediation`   | `remediations`, `rem`                       |
| `findingrollup` | `findingrollups`, `fr`, `rollup`, `rollups` |
| `repository`    | `repositories`, `repo`, `repos`             |
| `intent`        | `intents`                                   |
| `intentrun`     | `intentruns`, `irun`                        |
| `integration`   | `integrations`                              |
| `forge`         | `forges`                                    |
| `project`       | `projects`, `proj`                          |

`get` takes one more: `all`, meaning every kind in the table above. It is the CLI's spelling of `kubectl get patchy`
(every CRD declares the `patchy` category), and no other verb accepts it — there is nothing sensible to describe, review
or approve collectively.

The intent nouns (`intent`, `intentrun`, `project`) are for reading only: intents are driven from their GitHub issue
(labels and `/patchy` commands, see [intent-controller](configuration/intent-controller.md)), and no CLI verb acts on
them.

## Global flags

```text
    --kubeconfig string    path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)
    --context string       kubeconfig context to use
-n, --namespace string     namespace to work in (default: the context's, exactly as kubectl resolves it)
-A, --all-namespaces       work across every namespace
-o, --output string        table | wide | json | yaml | name | markdown   (default table)
    --no-color             disable colour and styling
    --request-timeout dur  timeout for a single API call (default 30s)
-v, --verbose              log what the CLI is doing to stderr
```

Namespace resolution follows kubectl's rules exactly, including its fallback to `default`, so the two tools never
disagree about where they are looking. If your findings live in `patchy`, either set that on your context or pass
`-n patchy`.

## Reading

```sh
patchy get findings
patchy get findings -o wide                      # adds the issue and pull-request links
patchy get findings --phase AwaitingApproval
patchy get findings --severity critical,high --sort-by severity
patchy get findings --awaiting                   # only findings you could act on right now
patchy get findings --repo billing --suspended
patchy get investigations --finding my-finding
```

Columns come from the CRDs' own print columns, so they match `kubectl get` and always will; `-o wide` adds the ones the
CRDs mark lower priority. Filters that map to labels (`--severity`, `--source`, `--finding`, `-l`) run on the API
server; the rest (`--phase`, `--verdict`, `--repo`, `--suspended`, `--awaiting`) need the object and run locally.

`patchy get all` is the whole pipeline in one screen — every kind, each in its own table, in the order the pipeline uses
them:

```text
Findings
NAME    REPO      SEVERITY   PRIORITY   PHASE    VERDICT     AGE
fnd-1   billing   high       high       Queued   remediate   4h

Investigations
NAME          FINDING   ATTEMPT   STATE      VERDICT     AGE
fnd-1-inv-1   fnd-1     1         Complete   remediate   3h

Forges
NAME   PROVIDER   READY   AGE
gh     github     True    9d
```

Kinds with nothing in them are left out rather than printed as an empty table. `all` lists, and only lists: it takes no
names, and it refuses the finding-only filters (`--phase`, `--verdict`, `--repo`, `--suspended`, `--awaiting`,
`--finding`) rather than narrow one table and leave the rest whole. The label filters (`-l`, `--severity`, `--source`)
mean the same thing on every kind, so those do apply. `-o json` and `-o yaml` give you one stream of every object;
`-o name` gives you fully-qualified references you can pipe back into `kubectl`.

```sh
patchy describe finding my-finding               # state, timeline, owners, alerts, runs, spend, runner image
patchy describe investigation my-finding-inv-1
patchy describe repository my-finding-src        # pinned commit, artifact, runner image
```

With [repository-declared agent images](integrations/agent-images.md) on, a finding and its repository snapshot also
show the runner image: the file that declared it, the digest it was pinned to, whether the runs used it (`repository`)
or the default runner image (`default`), and the reason a declaration was rejected or not applicable.

## Reviewing an agent's work

```sh
patchy review finding my-finding                     # both stages together
patchy review investigation --finding my-finding     # the latest attempt
patchy review investigation --finding my-finding --attempt 2
patchy review remediation my-finding-rem-1
```

On a terminal the report is rendered for reading. Piped, or with `-o markdown`, you get the markdown the agent actually
wrote — so pasting it into a ticket loses nothing:

```sh
patchy review finding my-finding -o markdown > report.md
```

`--raw` keeps the machine frontmatter, which is a contract between the investigate and remediate stages and is stripped
by default.

## Opening the human-facing page

```sh
patchy browse finding my-finding          # the tracking issue
patchy browse remediation my-finding-rem-1 # the pull request
patchy browse finding my-finding --print-url
```

The verb is `browse` rather than `open` because `Opened` is a real phase — `patchy open finding` would read like a state
transition. `patchy review … --web` is the same behaviour without leaving the review.

## Acting on a finding

```sh
patchy approve  finding my-finding [--note "shipping despite the break"]
patchy suspend  finding my-finding
patchy resume   finding my-finding
patchy retry    finding my-finding
patchy expedite finding my-finding
```

Every action writes to the finding's **spec only**. A controller observes the change and moves the phase — the CLI never
writes status and never transitions a finding itself, which is what keeps each phase edge single-writer.

Actions are idempotent. Approving an already-approved finding succeeds and changes nothing, so re-running a script is
safe:

```sh
patchy suspend finding my-finding --dry-run                              # report, write nothing
patchy suspend finding -l patchy.bitwisemedia.uk/severity=critical -y    # bulk, no prompt
```

Bulk operations prompt above one finding unless you pass `-y`, report each finding individually, and exit non-zero if
any failed — one unavailable finding never stops the rest.

## Backfilling an integration

One action acts on an Integration rather than a finding: the
[manual backfill](integrations/sources/github.md#the-manual-backfill), which lists the provider's open alerts and
ingests the ones that predate webhook coverage.

```sh
patchy backfill gh                                      # the credential's full scope
patchy backfill gh --repo acme/                         # one owner
patchy backfill gh --repo acme/shop --repo acme/billing # exact repositories (required with a PAT)
```

Like every action it writes spec only (`spec.backfill`); the integration-controller runs the walk on its next reconcile
and reports on `status.backfill` — watch for `truncated`, which means the page budget ran out and a narrower `--repo`
prefix is needed to reach the rest.

## Testing a generic integration

The `dev` commands are the one part of the CLI that never touches a cluster: a local test harness for authors of
[generic integrations](integrations/sources/generic.md).

```sh
patchy dev generic --secret dev-secret --enhance-url http://127.0.0.1:9000/enhance
```

hosts the real generic webhook receiver on your workstation — the same HMAC authentication, deduplication, and
validation the integration-controller runs — retains ingested findings in memory, and drives the enhancer call and
resolver write-back at your endpoints, so one signed POST tests every exchange of the contract. For a process that is
only an enhancer or resolver, `patchy dev enhance` / `patchy dev resolve` fire a single signed exchange from a findings
payload file, no server involved.

Uniquely in the CLI, every `dev` flag also resolves from `PATCHY_DEV_*` environment variables and an optional
`.patchy.yaml` in the working directory (flag beats environment beats file). See the
[generic integration guide](integrations/sources/generic.md#testing-your-integration-locally) for the full walkthrough.

## Maintaining a chart and image mirror

The `mirror` commands are the other cluster-free group: they maintain a vendored mirror store — a git repository where
`mirror.yaml` holds global defaults and every `charts/<name>/` or `artifacts/<name>/` directory pins one upstream helm
chart or OCI artifact, vendored for PR review, digest-locked, provenance-verified, scanned, and published signed to one
or more platform registries. `mirror.yaml` lists the registries; every entry publishes to all of them, each signed with
that registry's own `signing` block when it has one (a KMS key, say) and the global default otherwise.

```sh
patchy mirror upgrade --check -o json   # what would move, as data
patchy mirror upgrade --all             # move pins, regenerate everything derived
patchy mirror validate --all            # the CI gate: current, verified, clean
patchy mirror sync --all                # converge every registry, idempotently
patchy mirror sync --all --registry ghcr   # just the named registry
```

Signing, verification, and image scanning shell out to external binaries rather than linking them: `sync` and `validate`
need `cosign` (v3 or later) on `PATH`, and the image scanners are all opt-in shell-outs — enable `scan.scanners.osv`
(`osv-scanner` v2) or `scan.scanners.grype` (`grype`) in `mirror.yaml`, since an image scan with neither enabled is an
error; `scan.enabled: false` turns image scanning off deliberately. `kubescape` remains the optional configuration
scanner. Existing stores that relied on the old built-in default should add `scan.scanners.osv.enabled: true` and
install `osv-scanner` (mise/brew).

`upgrade` mutates files and nothing else — patchy never runs git, so branches, commits, and pull requests belong to the
calling pipeline. Entries that share a `lockstep` group bump together, holding at the lowest version every member has
published. `sync` never replaces an existing chart tag and skips anything already current and signed, so re-runs are
safe. `validate` regenerates the derived state out-of-tree and byte-compares it, which is why upgrade is the only verb
allowed to consult the wall clock (tracked-tag cooldowns, allowlist expiry stamping).

Like `dev`, the `mirror` flags also resolve from `PATCHY_MIRROR_*` environment variables and `.patchy.yaml` (`mirror:`
block); `-C` points at a store checkout from anywhere.

## Checking an agent image

`patchy check image` is the third cluster-free command, for repository owners about to declare an
[agent image](integrations/agent-images.md) in `.patchy/agent.yaml` or `.devcontainer/devcontainer.json`. It runs
source-controller's own checks, through the same code, and reports every verdict rather than the first failure:

```sh
patchy check image ghcr.io/acme/shop-agent:1                                   # platform, size, VOLUME, ENV, PATH
patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --cosign-key cosign.pub
patchy check image ghcr.io/acme/shop-agent:1 --run                             # plus the agent's preflight in docker
```

`--allow` stands in for the operator's registry allowlist and `--cosign-key` for their signing key; without the key the
signature line is skipped, because source-controller admits an unverified image only when the operator allows unsigned
images. Registry credentials are your own docker credentials. `--run` needs a local docker: it copies `agent-runner` and
the claude CLI out of the claude runner image released with this CLI (for a development build, which has none, the
newest `vX.Y.Z` release in the registry, never `latest`), pinned to the digest its tag names in the registry so a stale
local copy of the tag never stands in for it, and reports the image and digest on a `runner-image` line of their own.
The registry is the one the CLI's release published its images to, stamped in when it was built; a plain `go build`
stamps none (`hack/build.sh` does when `PATCHY_IMAGE_REGISTRY` is set). `--runner-image` overrides the choice and is
used as given; when no image can be chosen, because the registry is unreachable or the CLI knows none, the line fails
and says to pass one. It then runs the image the way the agent pod does, with uid 65532, a read-only root filesystem, no
network, no capabilities and bounded processes, memory and CPU, then runs the preflight a stage runs before its first
model call, followed by `bash -c true` and `git --version`. A pod may land on a node of any platform the image serves,
so this runs once per platform: the docker host's own natively, any other under docker's emulation (Docker Desktop has
it; on Linux, binfmt_misc with QEMU), and a platform the host cannot emulate is reported as SKIP rather than passed
over. Each check prints one line (PASS, FAIL or SKIP, the platform for a `--run` check, then the reason); `-o json`
prints the report as data, and the exit status is non-zero when any check fails.

## Creating the GitHub App

`patchy setup github-app` is the fourth cluster-free command. It creates the GitHub App patchy authenticates as through
GitHub's
[App manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest), and
writes its credentials as the Secret manifest a `Forge` and an `Integration` read
([what the App needs, and why](getting-started/github-app.md)):

```sh
patchy setup github-app --org acme --intents --checks                    # intents, with check-fix rounds
patchy setup github-app --org acme --security --webhook-url https://patchy.acme.dev/github/webhooks
patchy setup github-app --org acme --intents --dry-run                   # print the manifest; create nothing
patchy setup github-app --org acme --intents -o - | sops --encrypt --input-type yaml --output-type yaml /dev/stdin
```

Choose what the App is for. It asks for the least each chosen feature uses, and nothing else:

| Flag                          | Permissions                                                         | Webhook events                                                   |
| ----------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `--security`                  | Code scanning alerts, Issues, Contents, Pull requests: read & write | `code_scanning_alert`, `issues`, `issue_comment`, `pull_request` |
| `--intents`                   | Issues, Contents, Pull requests: read & write                       | none: intent-controller polls GitHub                             |
| `--checks` (with `--intents`) | Checks, Commit statuses, Actions: read                              | none                                                             |

Metadata read comes with every App. The table is `internal/intentperm`, the one intent-controller proves a `Project`'s
grants against before it is Ready, so an App created with `--intents` (and `--checks` for a Project with
`spec.checks.fix`) passes that check once it is installed. `--security` needs `--webhook-url`, the
integration-controller's `https://<host>/github/webhooks`; an App without it has no webhook.

The flow: patchy listens on a random `127.0.0.1` port and opens a page in your browser that posts the manifest to
GitHub's "create a GitHub App" form, for `--org` (you must be an owner of it) or, with `--user`, your own account. Check
the form and click **Create GitHub App**. GitHub sends the browser back with a one-time code, which patchy accepts only
with the random state it started the flow with; it exchanges the code for the App's ID, private key and webhook secret
(`POST /app-manifests/{code}/conversions`, which needs no credential) and stops listening. The page carries that state,
so it is served once: if your browser says it was served already, something else on the machine read it first, so stop
patchy and run it again. A code for an App that an account other than `--org` owns is refused, and nothing is written.
On a machine without a browser, `--no-browser` writes the page to a file you open anywhere, and you paste back the
address GitHub sends you to (it asks again until it gets one from this run, and after a bare code GitHub does not
accept). A code works once, within an hour; if the run ends without one, the error says where to delete the App or how
to finish it.

The Secret (`appID`, `privateKey`, and `webhookSecret` for an App with a webhook; `--secret-name`, default
`patchy-github`, in `-n`, default `patchy`) is written to `<secret-name>.secret.yaml` with mode 0600, and an existing
file is never replaced without `--force`; a file patchy could not write, in a directory it cannot write to, is refused
before anything is created. `-o -` writes it to stdout for a pipe, and refuses a stdout that is a terminal or a file
other users can read, as a shell's `> file` is under the usual umask: name the file with `-o <file>` instead. The
private key appears nowhere else, and GitHub keeps no copy: apply or encrypt the file, then delete it. Finally, install
the App with the link printed on stderr, on the repositories patchy works on: for intents, the intent repository and
every application repository. Only github.com is supported, and nothing here talks to a cluster.

## Checking a Project

`patchy check project <name>` is the preflight for a [Project](configuration/intent-controller.md) before its first
intent. Unlike `check image` it reads the cluster, with your kubeconfig, and it reports the same way: one line per
check, PASS, FAIL or SKIP, then what the check is about (a repository's key, `intent-repository`, or `-` for the Project
as a whole) and the reason. It looks at every repository the Project lists, and every previewed one, never just the
first:

```sh
patchy check project shop -n patchy
GH_TOKEN=$(gh auth token) AWS_PROFILE=prod patchy check project shop -n patchy
patchy check project shop -o json | jq '.checks[] | select(.status == "FAIL")'
```

| Check           | What it reads, and what it says                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ready`         | The Project's `Ready` condition, with its reason and message: intent-controller's own verdict, which it reaches by minting the App's scoped tokens in-cluster. A condition older than the spec fails.                                                                                                                                                                                                                                                                                                                |
| `intent-names`  | The `IntentNameConflict` condition.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `forge`         | Each repository, the intent repository included, resolved over the namespace's Forge CRs by the controllers' own `forge.Resolve`, and that Forge's `Ready` condition.                                                                                                                                                                                                                                                                                                                                                |
| `labels`        | The trigger and approve labels, from `Ready`: intent-controller ensures them last, so they exist exactly when it is True.                                                                                                                                                                                                                                                                                                                                                                                            |
| `agent-image`   | Each repository's `.patchy/agent.yaml` (or `.devcontainer/devcontainer.json`) at its default-branch head, judged by `check image`'s checks under source-controller's live allowlist, size cap and signing key. Only a regular file counts, as for source-controller: a symlink is not a declaration. When source-controller requires a signature but its key is not in a ConfigMap labelled for it (the chart's layout), an image that passes everything else is a SKIP, and `check image --cosign-key` verifies it. |
| `previews`      | That intent-controller writes Previews and preview-controller is configured, from their ConfigMaps.                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `preview-image` | Each previewed repository's image repository: under preview-controller's prefix, one valid leaf, reachable, and holding `sha-<default-branch head>`, the tag a preview runs for a component an intent leaves be. With one previewed repository no preview runs that tag (only the pull request's head), so its absence is a SKIP.                                                                                                                                                                                    |
| `preview-dns`   | That `<project>-0.<host suffix>` resolves to the preview load balancer, the placeholder Ingress's address. No intent is numbered 0, so the wildcard covers that name and no Preview owns it. It fails when the name does not exist (NXDOMAIN) or resolves elsewhere, and when the placeholder Ingress has no load balancer address; a lookup that times out, or a placeholder you cannot read, is a SKIP.                                                                                                            |
| `preview-tls`   | That the name serves a certificate trusted for it. The preview load balancer admits only `preview.inboundCIDRs`, so from any other address the handshake times out, and that is a SKIP, not a FAIL.                                                                                                                                                                                                                                                                                                                  |

The check never reads a Secret, so it never holds the App's private key: whether the App is installed with the
permissions intents use, and whether the labels exist, come from the conditions the controller computed in the cluster.
What only your own identity can see, it reads with that identity and says so: GitHub with `GH_TOKEN` (else
`GITHUB_TOKEN`, else anonymously, which sees public repositories only), a GitHub Enterprise Server repository with
`GH_ENTERPRISE_TOKEN` (else `GITHUB_ENTERPRISE_TOKEN`) only when `GH_HOST` names its host and anonymously otherwise, and
registries with your cloud and docker credentials (ECR through the AWS SDK's default chain, so `AWS_PROFILE` works). The
Project, which someone else may have written, names the repositories' hosts, so a github.com token never leaves
github.com and an enterprise token never leaves `GH_HOST`. A PASS there means _you_ can read the image; whether
source-controller's and the preview nodes' own credentials can is shown by a Repository's `status.runnerImage` and a
Preview's status. The exit status is 1 when any check fails, 3 when the Project does not exist and 4 when you may not
read it.

## Scaffolding an application repository

`patchy init app` is the fifth cluster-free command. It writes the files an application repository needs before patchy
can build intents in it and preview its pull requests, from templates built into the CLI, and makes no GitHub call:

```sh
patchy init app hello-web --repo acme/Hello.Web --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com
patchy init app --existing --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com   # in a checkout, beside its code
```

| Path                                                                         | What it is                                                                                                                                                      |
| ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.patchy/agent.yaml`                                                         | the [agent image](integrations/agent-images.md) the repository declares: `<registry>/patchy/app-envs/<image-name>:toolchain-v1`                                 |
| `.patchy/Dockerfile`                                                         | that image's recipe: patchy's agent base, pinned by digest, plus the toolchain and the dependencies, offline                                                    |
| `.github/workflows/ci.yml`                                                   | `test`: tests the exact head and builds the runtime image as an artifact, with no credential and no OIDC                                                        |
| `.github/workflows/agent-image.yml`                                          | `agent image`: builds the agent image from the default branch, reproducibly, also uncredentialed                                                                |
| `.github/workflows/publish-images.yml`                                       | the `workflow_run` dispatcher: holds no role and runs no step, and calls each kind's publisher behind its own gate variable                                     |
| `.github/workflows/publish-runtime.yml`                                      | the trusted runtime publisher (`workflow_call`), the only workflow that assumes `RUNTIME_ROLE_ARN`                                                              |
| `.github/workflows/publish-agent.yml`                                        | the trusted agent publisher (`workflow_call`), the only workflow that assumes `AGENT_ROLE_ARN`                                                                  |
| `.github/actions/publish/`                                                   | the publishers' scripts (`guard.cjs`, `validate_oci.py`, `check-config.sh`, `copy-image.sh`), their tests, and a README listing the variables and the workflows |
| `Dockerfile`, `.dockerignore`, `README.md`, `.gitignore`, a small Go service | for a new application only: a runtime image that meets the preview contract (uid 65532, a read-only root filesystem, one port, a readiness path)                |

No generated workflow or script names an account, region, role or repository ID: the publishers read them from
repository variables, and AWS role trust (the repository's numeric IDs, the default branch and the publisher's own
`job_workflow_ref`) is the boundary. Only `.patchy/agent.yaml` names the registry, because source-controller reads the
image from it. The variables, which `init app` prints as `gh variable set` commands, are `PUBLISH_REPOSITORY_ID`,
`PUBLISH_OWNER_ID`, `AWS_REGION`, `ECR_REGISTRY`, `AGENT_IMAGE_REPOSITORY`, `AGENT_ROLE_ARN`, `RUNTIME_IMAGE_REPOSITORY`
and `RUNTIME_ROLE_ARN`, then the two gates: `AGENT_PUBLISH_ENABLED` publishes the agent image (a repository without
previews still needs it), and `PREVIEW_PUBLISH_ENABLED`, set last, publishes runtime images.

The repository defaults to the checkout's `origin` remote and the default branch to the one `origin`'s HEAD names, both
read from `.git` with no git binary. `--image-name`, the leaf of both registry repositories, defaults to the repository
name made image-safe (`Hello.Web` becomes `hello-web`). The agent base is the one released with this CLI, pinned to the
digest its tag names in the registry now; `--agent-base` overrides it (a digest-pinned reference is used as given), and
a development build, which has no agent base of its own, needs it.

An existing file is never overwritten without `--force`, and a symbolic link never is: every path is checked before any
is written. `--existing` writes only `.patchy/` and the CI publishers, builds the runtime image in a workflow of its own
(`runtime-image.yml`, named `runtime image`) so the application's CI is untouched, and ends its next steps with what the
application must be adapted to.

Published tags are immutable. To change the agent toolchain, edit `.patchy/Dockerfile` (or the dependencies) and bump
the tag in `.patchy/agent.yaml` to `toolchain-v2`, `toolchain-v3` and so on, in the same commit: the agent publisher
reads the tag from that file, and refuses a changed image at a published tag with a message saying to bump it.

## Permissions

Each action is a **custom RBAC verb**, granted independently: holding `approve` says nothing about `suspend`. The
per-finding verbs (`approve`, `retry`, `expedite`, `suspend`, `resume`) live on `findings.patchy.bitwisemedia.uk`; the
integration-scoped ones (`backfill`, `replay`, `reset`) on `integrations.patchy.bitwisemedia.uk`. To see yours:

```sh
patchy can-i                # the whole matrix, findings and integrations
patchy can-i approve        # one verb; exit code answers, for shell conditionals
patchy can-i backfill       # integration-scoped verbs resolve on integrations
```

Two things enforce those verbs, and only one of them matters:

- The CLI runs a `SelfSubjectAccessReview` before writing. This is **ergonomics** — a fast, clear failure naming the
  verb you lack instead of an opaque server rejection. It carries no security weight and is trivially bypassed by not
  using the CLI.
- A **`ValidatingAdmissionPolicy`** in the cluster binds each spec field to its verb. This runs inside the API server's
  admission chain, so it applies identically to `patchy`, `kubectl edit`, `kubectl patch`, server-side apply and raw
  `curl`. This is the actual enforcement. There are two policies: one on findings, one on integrations (gating
  `spec.backfill`/`spec.replay`/`spec.reset` while leaving the operator's configuration surface freely updatable).

That second piece exists because Kubernetes RBAC has no notion of a field: `update` on findings grants the whole object.
Without the policy, letting a developer suspend a finding would also let them rewrite its severity or forge an approval.
See [Kustomize](deployment/kustomize.md) for the manifest and the role ladder.

!!! warning "Requires Kubernetes 1.30+"

    `ValidatingAdmissionPolicy` reached GA in 1.30. On an older cluster the policy will not install and
    enforcement degrades silently to "whoever holds `update` owns the whole resource". Check with
    `kubectl api-resources | grep validatingadmissionpolic`.

Because the policy enumerates the spec fields it freezes, contributors adding a field to `FindingSpec` must extend it —
see [Extending](extending.md#adding-a-field-to-findingspec).

## Exit codes

| Code | Meaning                                                                   |
| ---- | ------------------------------------------------------------------------- |
| `0`  | success                                                                   |
| `1`  | runtime failure — unreachable cluster, bad response                       |
| `2`  | usage error                                                               |
| `3`  | the named resource does not exist                                         |
| `4`  | RBAC refused, or the action is unavailable in the finding's current phase |

## A triage session

```sh
patchy get findings --awaiting -o wide      # what needs a human
patchy review finding <name>                # what the agent concluded, and why
patchy browse finding <name>                # the tracking issue, if you want the thread
patchy approve finding <name>               # release the hold
```

## Output and piping

Rendered reports use the dark theme by default. On a light terminal, set the same variable `glow` and the other charm
tools read:

```sh
export GLAMOUR_STYLE=light     # or dark, dracula, tokyo-night, notty, ascii
```

There is no automatic detection: probing a terminal's background needs a query/response round trip that only an
interactive event loop can service, and `patchy` is a one-shot command. Tables are unaffected — they use the ANSI 0-15
palette, so they inherit whatever theme your terminal already has.

`stdout` carries data, `stderr` carries narration — so `-v` never corrupts a pipe, and "no findings found" goes to
stderr rather than into your `jq`. Styling turns itself off whenever stdout is not a terminal, and also honours
`--no-color`, `NO_COLOR`, and `TERM=dumb`.

```sh
patchy get findings -o json | jq '.items[] | select(.status.priority == "critical") | .metadata.name'
patchy get findings -o name | xargs -n1 patchy describe finding
```
