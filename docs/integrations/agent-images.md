# Bring your own agent image

The agent that investigates and fixes a finding runs your repository's build and tests. The default runner image carries
no language toolchain, so on its own the agent cannot run `go test`, `pytest` or `npm test`. A repository can name the
image its agent runs in instead, with one committed file and no cluster access. patchy adds its own two binaries to that
image when the pod starts and runs it in the same sandbox as the default image.

This page is for repository owners: how to declare an image, how to build one, where to push it, how to check it, and
what each failure means. Your operator turns the feature on and chooses which registries are allowed (`--repository-*`
flags on the [source-controller](../configuration/source-controller.md#repository-runner-images), or
`agent.repositoryImages` in the Helm chart); the [design](../design/repository-runner-images.md) covers resolution,
signing and the threat model.

## Declaring the image

### `.patchy/agent.yaml`

At the root of the repository, on the branch patchy watches (the default branch unless the operator pins another):

```yaml
image: ghcr.io/your-org/your-repo-agent:1
```

`image` is the only key. patchy rejects a file with any other key (so a newer schema fails closed rather than being
half-read), a `build` key (patchy does not build images), more than one YAML document, or more than 64 KiB. A tag is
resolved to a digest once, when the finding's repository snapshot is taken; `name@sha256:<64 hex>` pins the digest
yourself and goes through the same checks.

### The `.devcontainer/devcontainer.json` fallback

When the repository has no `.patchy/agent.yaml`, patchy reads `.devcontainer/devcontainer.json` and honours its
top-level `image` string, so a repository that already names a prebuilt editor image needs no new file:

```jsonc
{
  // the agent runs in this prebuilt image too
  "image": "ghcr.io/your-org/your-repo-dev:2",
  "customizations": { "vscode": { "extensions": ["golang.go"] } },
}
```

Comments and trailing commas are accepted. A devcontainer.json that patchy cannot honour is **not applicable**: it is
treated as no declaration, the agent runs in the default runner image, the finding carries on as normal, and the
tracking issue says why. That covers a file which:

- builds its image (`build`, `dockerFile` or `dockerComposeFile`);
- adds `features` (patchy would have to build them);
- has no `image`, or one that is not a string, is empty, or uses `${...}` variable substitution;
- cannot be parsed, or is over 64 KiB.

To use such a repository's toolchain, publish the image and set `image`, or declare it in `.patchy/agent.yaml`.

### Precedence

| `.patchy/agent.yaml` | `.devcontainer/devcontainer.json` | The agent runs in                                                   |
| -------------------- | --------------------------------- | ------------------------------------------------------------------- |
| valid                | anything or absent                | the `.patchy/agent.yaml` image; devcontainer.json is not read       |
| invalid              | anything or absent                | nothing from either file: a rejection (see below), never a fallback |
| absent               | usable `image`                    | the devcontainer.json image                                         |
| absent               | not applicable                    | the default runner image, with the reason on the tracking issue     |
| absent               | absent                            | the default runner image, and nothing is posted                     |

A broken `.patchy/agent.yaml` never falls through to devcontainer.json: an explicit declaration that silently became a
different image would be worse than none.

### What is not honoured

From devcontainer.json, only the top-level `image` counts. Lifecycle commands (`postCreateCommand` and the rest),
`containerEnv`, `remoteEnv`, `remoteUser`, `mounts`, `customizations`, `features` and `build` are ignored, and
devcontainer.json files at other paths (`.devcontainer.json`, `.devcontainer/<name>/devcontainer.json`) are not read.

From the image itself, `USER`, `ENTRYPOINT` and `CMD` are ignored (see [the sandbox](#the-sandbox-your-image-runs-in)),
and an image that declares a `VOLUME` or sets a reserved `ENV` name is rejected. Everything else in the image's `ENV` is
honoured, including its `PATH`.

### When a change takes effect

patchy reads the declaration once per finding, from the same commit the agent works on, and pins the image to a digest
beside that commit. The investigation and the remediation of one finding therefore always run the same image, even if
the tag moves between them, and a fix to the declaration or the image applies to the next finding on the repository.

## The sandbox your image runs in

Whatever the image says, the agent container:

- runs as uid 65532 with a read-only root filesystem, all capabilities dropped, no privilege escalation and the
  `RuntimeDefault` seccomp profile; the image's `USER` and `ENTRYPOINT` are ignored, and `/patchy/bin/agent-runner` is
  started by absolute path;
- gets patchy's binaries (`agent-runner` and the claude CLI) copied in by a trusted init container, read-only, under
  `/patchy/bin`, so the image never carries or updates them;
- can write only to `/workspace` (`HOME`, with the repository at `/workspace/repo`) and `/tmp`, both emptyDirs that hide
  whatever the image has at those paths and allow executing what is written there, bounded by the operator's
  ephemeral-storage limit;
- has no network: the pod reaches DNS, patchy's artifact server and the egress broker (the model API) and nothing else,
  so nothing can be downloaded at run time;
- holds no credential of any kind: no forge token, no model key, no Kubernetes API access;
- runs with `PATH=/patchy/bin:<the image's PATH>`, `HOME=/workspace` and `GIT_CONFIG_NOSYSTEM=1` (the image's
  `/etc/gitconfig` is ignored), and with `BASH_ENV`, `ENV`, `SHELLOPTS`, `PROMPT_COMMAND`, `LD_PRELOAD`,
  `LD_LIBRARY_PATH`, `LD_AUDIT`, `NODE_OPTIONS`, `PYTHONSTARTUP`, `GIT_CONFIG_GLOBAL`, `GIT_CONFIG_SYSTEM`,
  `GIT_EXEC_PATH`, `GIT_SSH_COMMAND`, the proxy variables and every unused `PATCHY_*` setting set empty, so an image
  that relies on any of them breaks.

The image's `ENV` may not set any of these names; source-controller rejects the image (`ReservedEnv`) if it does:

- `HOME`;
- any name starting `PATCHY_`, `ANTHROPIC_` or `CLAUDE_`;
- a model or forge credential name: `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`, `COPILOT_GITHUB_TOKEN`,
  `GH_TOKEN`, `GITHUB_TOKEN`;
- a model gateway name: `AWS_REGION`, `CLOUD_ML_REGION`;
- a proxy variable in any case: `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY`;
- one of git's repository redirections: `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`,
  `GIT_COMMON_DIR`.

Before the agent's first model call, `agent-runner` runs a preflight in your image: `claude --version`, `git --version`
and `bash -c true`. A failure ends the run as `image_incompatible` within seconds, with the error on the tracking issue.
The claude CLI is a glibc binary, so the image must be glibc-based (not Alpine or another musl distribution) and have
`bash`, `sh` and `git` on its `PATH`, readable and runnable by uid 65532.

## The base image

The simplest way to meet all of that is to build `FROM` the base image patchy publishes.
`ghcr.io/devthenet-labs/patchy/agent-base` is published with every patchy release, tagged `v<version>` and `latest`, for
`linux/amd64` and `linux/arm64`, and cosign-signed like the other patchy images. It is built from
[`Dockerfile.agent-base`](https://github.com/devthenet-labs/patchy/blob/main/Dockerfile.agent-base) and contains no
patchy binary.

| Setting          | Value                                                                                                            | Why                                                                                                     |
| ---------------- | ---------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| Base             | `wolfi-base`, pinned by digest                                                                                   | glibc: the injected claude CLI is a glibc binary and cannot run on musl (Alpine) images                 |
| Packages         | `bash`, `git`, `ca-certificates`, `coreutils`, `diffutils`, `findutils`, `gnutar`, `grep`, `gzip`, `make`, `sed` | bash for claude's shell tool, git for the changeset diff, GNU tools that build scripts assume           |
| User             | `patchy`, uid and gid 65532, home `/workspace`                                                                   | the pod runs as 65532 regardless; the home matches the `HOME` the Job sets                              |
| `PATH`           | `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`                                                   | explicit and absolute; the pod runs with `/patchy/bin` in front of it                                   |
| `XDG_CACHE_HOME` | `/tmp/.cache`                                                                                                    | writable in the pod whatever `HOME` is, and outside the repository, so caches never reach the changeset |
| `TMPDIR`         | `/tmp`                                                                                                           | temporary files go to the writable emptyDir                                                             |
| `LANG`           | `C.UTF-8`                                                                                                        | UTF-8 for tools that consult the locale                                                                 |

It declares no `VOLUME` and no `ENTRYPOINT`, and ends as `USER 65532:65532`. Pin a release tag and its digest in your
`FROM` rather than `latest`.

## Building FROM it

- **Install as root, run as 65532.** Switch to `USER 0:0` for `apk add` and back to `USER 65532:65532` at the end.
- **Bake every dependency in**, and configure the tools not to reach for the network, so a missing dependency fails fast
  instead of hanging (see [offline dependency caches](#offline-dependency-caches)).
- **Keep baked content out of `/workspace` and `/tmp`.** Install toolchains and dependencies under paths such as `/opt`
  or `/usr`, readable by uid 65532; the emptyDirs would hide anything placed at those two paths.
- **Keep writable caches under `/tmp` or `HOME`.** The base's `XDG_CACHE_HOME` already covers Go's build cache and pip;
  point other tools there too.
- **Leave [reserved names](#the-sandbox-your-image-runs-in) out of `ENV`**, and declare no `VOLUME`.
- **Prepend to `PATH`, keep it absolute.** Empty and relative entries are dropped, and a `PATH` with no absolute entry
  is rejected. A multi-platform image must set the same `PATH` on every platform.
- **Stay on glibc.** Rebasing the image onto Alpine breaks the claude CLI, and the preflight fails the run.
- **Label your own image.** Labels are inherited, so set `org.opencontainers.image.source` and `description` to yours.

### Examples

[`examples/agent-images/`](https://github.com/devthenet-labs/patchy/tree/main/examples/agent-images) has one small,
commented Dockerfile per toolchain. Each copies only the dependency manifest from the repository, so the image is
rebuilt when dependencies change and never contains the source. Build one with your repository root as the context:

```sh
docker build -f examples/agent-images/go/Dockerfile -t ghcr.io/your-org/your-repo-agent:1 .
```

| Example                                                                                              | Toolchain       | Dependencies baked into      | Offline switch                     |
| ---------------------------------------------------------------------------------------------------- | --------------- | ---------------------------- | ---------------------------------- |
| [Go](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/go/Dockerfile)         | Go 1.24         | `GOMODCACHE=/opt/go/pkg/mod` | `GOPROXY=off`, `GOTOOLCHAIN=local` |
| [Python](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/python/Dockerfile) | Python 3.13     | virtualenv at `/opt/venv`    | `PIP_NO_INDEX=1`                   |
| [Node.js](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/node/Dockerfile)  | Node.js 24, npm | `/node_modules`              | `npm_config_offline=true`          |

### Offline dependency caches

The pod has no network, so every dependency the build and tests need must already be in the image, and each tool should
be told so, or it spends the run timing out against a registry it cannot reach.

- **Go.** `go mod download` from `go.mod` and `go.sum` into a `GOMODCACHE` outside `/workspace` and `/tmp` (the example
  uses `/opt/go/pkg/mod`), then `GOPROXY=off` and `GOTOOLCHAIN=local`. `GOFLAGS=-mod=mod` lets the agent add a
  requirement the cache already holds. The build cache lands in `/tmp/.cache/go-build` through the base's
  `XDG_CACHE_HOME`. A vendored module (`GOFLAGS=-mod=vendor`) needs no cache at all.
- **Python.** Install `requirements.txt` into a virtualenv under `/opt/venv`, put its `bin` first on `PATH`, then
  `PIP_NO_INDEX=1`. `PYTHONDONTWRITEBYTECODE=1` keeps `.pyc` files out of the repository, and so out of the changeset.
- **Node.js.** `npm ci` from `package.json` and `package-lock.json`, moved to `/node_modules`, then
  `npm_config_offline=true` and `npm_config_cache=/tmp/.cache/npm`. The example installs at the filesystem root because
  Node resolves packages by walking up from the importing file, and `/` is the only ancestor of `/workspace/repo` the
  pod does not replace with an emptyDir; it avoids `NODE_PATH`, which ES modules ignore, and `NODE_OPTIONS`, which the
  pod empties.

Rebuild the image whenever the dependency manifest changes: a dependency missing from the baked cache fails the agent's
build (for Go, with `module lookup disabled by GOPROXY=off`).

## Publishing the image

The image must sit under a registry path your operator has allowlisted, and by default it must be signed with the
operator's cosign key (the operator may allow unsigned images instead). Ask your operator for both. Use immutable tags
(a version, or the commit SHA); patchy resolves a tag once per finding, but a tag you move changes what the next finding
runs.

### To Amazon ECR from GitHub Actions

Push with GitHub's OIDC token, so the workflow holds no long-lived AWS key. You need an IAM role that trusts GitHub's
OIDC provider for your repository and may push to the ECR repository, and the workflow needs `id-token: write`:

```yaml
# .github/workflows/agent-image.yaml
name: agent image
on:
  push:
    branches: [main]
    paths: [go.mod, go.sum, .patchy/Dockerfile]
permissions:
  contents: read
  id-token: write # the OIDC token configure-aws-credentials exchanges for the role
jobs:
  push:
    runs-on: ubuntu-latest
    env:
      IMAGE: <account>.dkr.ecr.<region>.amazonaws.com/<repository>
    steps:
      - uses: actions/checkout@v5
      - uses: aws-actions/configure-aws-credentials@v5
        with:
          role-to-assume: arn:aws:iam::<account>:role/<push-role>
          aws-region: <region>
      - uses: aws-actions/amazon-ecr-login@v2
      - uses: docker/setup-buildx-action@v3
      - uses: docker/build-push-action@v6
        id: build
        with:
          context: .
          file: .patchy/Dockerfile
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ env.IMAGE }}:${{ github.sha }}
      - uses: sigstore/cosign-installer@v3
      # Only when the operator requires signatures: sign the pushed digest with the key they gave you.
      - run: cosign sign --yes --key "$COSIGN_KEY" "$IMAGE@${{ steps.build.outputs.digest }}"
        env:
          COSIGN_KEY: ${{ vars.COSIGN_KEY_REF }} # a KMS reference, e.g. awskms:///alias/<signing-key>
```

Then declare `image: <account>.dkr.ecr.<region>.amazonaws.com/<repository>:<sha>`. The push role needs
`ecr:GetAuthorizationToken` (on `*`) and, on the repository's ARN only, `ecr:BatchCheckLayerAvailability`,
`ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`, `ecr:CompleteLayerUpload`, `ecr:PutImage` and `ecr:BatchGetImage`;
scope its trust policy's `token.actions.githubusercontent.com:sub` condition to your repository and branch (for example
`repo:your-org/your-repo:ref:refs/heads/main`). With a KMS signing key, also `kms:Sign` and `kms:GetPublicKey` on that
key.

On the devthenet deployment, for example, the values are account `377946145366`, region `us-east-1` and a repository
under the allowlisted `patchy/` prefix, one per application (`patchy/app-envs/<app>`):

```yaml
role-to-assume: arn:aws:iam::377946145366:role/<push-role>
aws-region: us-east-1
# IMAGE: 377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/app-envs/patchy-target
```

In the same account source-controller resolves the image through its own AWS identity and the nodes pull it with theirs,
so no pull secret is needed.

### To a public registry

Any registry the operator allowlists works. On `ghcr.io`, push with the workflow's own token (`permissions:`
`packages: write`, `docker/login-action` with `registry: ghcr.io` and `password: ${{ secrets.GITHUB_TOKEN }}`), sign the
digest the same way, and make the package public; a private package also works when the operator configures a pull
secret for it.

## Checking an image before you declare it

`patchy check image` runs source-controller's own checks, through the same code, against your local docker credentials,
and reports every verdict rather than the first failure. `--allow` stands in for the operator's registry allowlist and
`--cosign-key` for their public key; `--run` also runs the agent's preflight in a local docker container shaped like the
pod, with the claude CLI patchy injects, on every platform the image serves:

```sh
patchy check image ghcr.io/your-org/your-repo-agent:1
patchy check image ghcr.io/your-org/your-repo-agent:1 --allow ghcr.io/your-org/ --cosign-key cosign.pub
patchy check image ghcr.io/your-org/your-repo-agent:1 --run
```

Each check prints one line: PASS, FAIL or SKIP, the check, the platform for a `--run` check, and the reason. The exit
status is non-zero when any check fails. The registry checks need the image pushed (a scratch tag or a local registry
will do); `--run` also works on an image that is only in your local docker. See the
[CLI tour](../cli.md#checking-an-agent-image) and the [reference](../cli/patchy_check_image.md) for every flag.

To go further, run your build and tests the way the pod does. Docker mounts a `--tmpfs` `noexec` by default, while the
pod's emptyDirs allow execution (`go test` runs its test binaries from `/tmp`), hence `exec`:

```sh
docker run --rm --user 65532:65532 --read-only --network none --cap-drop ALL \
  --security-opt no-new-privileges --tmpfs /tmp:exec,mode=1777 --tmpfs /workspace:exec,mode=1777 \
  -v "$PWD":/workspace/repo -w /workspace/repo -e HOME=/workspace \
  ghcr.io/your-org/your-repo-agent:1 bash -c 'go build ./... && go test ./...'
```

## What you see on the tracking issue

Once the finding's repository snapshot is taken, patchy keeps one comment on the finding's tracking issue, headed
**Agent runner image** and edited in place, whenever the repository declared anything:

- **Used** — which file declared which image, the digest it was pinned to, and whether its signature was verified.
- **Not applicable** — the devcontainer.json was not used, the exact reason, and that the default runner image ran.
- **Rejected** — the declared image, the reason label (see [troubleshooting](#troubleshooting)) and the exact message,
  and what patchy did: ran the default runner image (the default), or parked the finding for a human because the
  operator set `onReject: handoff` (patchy does not investigate a parked finding, and approving it does not revive it).
  It ends with the `patchy check image` command to run.
- **Failed in a run** — a run on the accepted image ended `image_incompatible` (with the preflight's error), or was
  refused with `SandboxUnenforced` because the cluster does not enforce the network isolation patchy requires; the
  second is for the operator to fix, not you.

A repository that declares nothing gets no comment. Separately, when a run on your image recommends `ignore`, patchy
hands the finding to a human instead of dismissing the alert, and says so in the hand-off notice: a verdict produced
inside an image the repository controls is not trusted to close an alert on its own.

## Gate CI on `patchy/**` branches

patchy pushes its fixes to `patchy/<finding>` branches in your repository and opens a pull request for a human to
review. A changeset from a run on your image may not touch `.github/workflows/**` or `.github/actions/**`, but your
workflows still run on the branch as soon as it is pushed. Treat a patchy branch as untrusted input until a human has
read the pull request: keep secrets out of workflows that run on `patchy/**` branches (branch filters, or environments
with required reviewers).

## Troubleshooting

Each rejection's label appears on the tracking issue and in `status.runnerImage.rejected` on the finding's Repository
(`patchy describe repository <finding>-src`). A rejected declaration runs the default runner image, or parks the finding
under `onReject: handoff`; the fix applies to the next finding on the repository.

| Reason                | Meaning                                                                                                                                                        | Fix                                                                                                                                                            |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `InvalidDeclaration`  | `.patchy/agent.yaml` is not a single YAML mapping with a non-empty string `image` and nothing else (unknown key, `build`, several documents, over 64 KiB)      | Reduce the file to `image: <ref>`                                                                                                                              |
| `InvalidReference`    | The declared reference is not a valid OCI image reference (whitespace, bad characters)                                                                         | Declare `registry/path/name:tag` or `registry/path/name@sha256:<digest>`                                                                                       |
| `InvalidDigest`       | A `@sha256:` pin is not `sha256:` followed by 64 hex characters                                                                                                | Copy the digest exactly, for example from `docker buildx imagetools inspect`                                                                                   |
| `NotAllowlisted`      | The image is not under any registry path the operator allows                                                                                                   | Push it under an allowlisted path (ask the operator), or ask for the path to be added                                                                          |
| `NotFound`            | The registry has no such image or tag                                                                                                                          | Push it, and check the reference's spelling and tag                                                                                                            |
| `AccessDenied`        | The registry refused source-controller (401 or 403)                                                                                                            | Make the image public, or ask the operator for a pull secret or cloud credential that covers it                                                                |
| `Oversized`           | The compressed layers of one platform exceed the operator's size cap, or the image config is too large                                                         | Slim the image: multi-stage builds, fewer toolchains, no caches you do not need; or ask the operator to raise the cap                                          |
| `UnsupportedPlatform` | The image is not for `linux/amd64` or `linux/arm64`, or its index leaves a node a fallback patchy did not check (`linux/386` without `linux/amd64`, and so on) | Build for `linux/amd64` and/or `linux/arm64` only (`docker buildx build --platform linux/amd64,linux/arm64`)                                                   |
| `Unsupported`         | The reference is not a runnable image: an attestation or other artifact, a nested index, a manifest with no config size, or too many manifests to check        | Declare the image itself (the index or manifest `docker push` printed), not a signature or attestation                                                         |
| `PathMismatch`        | The platforms of a multi-platform image set different `PATH`s                                                                                                  | Set the same `ENV PATH` for every platform                                                                                                                     |
| `EmptyPath`           | The image's `PATH` has no absolute entry                                                                                                                       | Set an absolute `PATH`, or none (patchy then uses the standard default)                                                                                        |
| `ReservedEnv`         | The image's `ENV` sets a [reserved name](#the-sandbox-your-image-runs-in)                                                                                      | Remove it from `ENV` (set it in your build scripts instead if a tool needs it)                                                                                 |
| `Volume`              | The image declares a `VOLUME`                                                                                                                                  | Remove the `VOLUME` instruction; only `/workspace` and `/tmp` are writable                                                                                     |
| `Unsigned`            | No signature by the operator's cosign key was found on the image's digest                                                                                      | Sign the pushed digest with the operator's key (`cosign sign --key <key> <image>@<digest>`), both the bundle form (cosign v3) and the legacy `.sig` tag verify |
| `SignatureInvalid`    | Signatures exist, but none verifies with the operator's key                                                                                                    | Re-sign the digest you declared, with the operator's key; a signature on another digest (an old tag) does not count                                            |
| `Resolve`             | Any other deterministic refusal from the registry checks                                                                                                       | Read the message; `patchy check image <ref>` reports the failing check                                                                                         |

Other outcomes you may see:

| Outcome                                | Meaning                                                                                            | Fix                                                                                                                     |
| -------------------------------------- | -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| devcontainer.json not applicable       | The devcontainer.json builds its image, adds features, or names no usable `image`; not a rejection | Publish the image and set `image`, or declare one in `.patchy/agent.yaml`                                               |
| `image_incompatible`                   | The preflight failed in your image: musl libc, no `bash`, `sh` or `git`, or not runnable by 65532  | Build `FROM` the agent base, or install bash and git on a glibc base; `patchy check image <ref> --run` reproduces it    |
| `agent image pull failed`              | The node could not pull the pinned digest (deleted, or the node lacks pull access)                 | Keep pushed digests, and make sure the operator's nodes (or pull secret) can pull from the registry                     |
| `SandboxUnenforced`                    | The cluster does not enforce NetworkPolicy, so patchy refused to run a repository image            | Nothing in your repository; the operator must enable NetworkPolicy enforcement. Later runs use the default runner image |
| `budget_exceeded` from `per-pod limit` | The run hit the egress broker's per-pod request or token limit                                     | Usually a runaway loop in the build or tests; if the work is legitimate, ask the operator to raise the broker limits    |
