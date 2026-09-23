# Agent images

A repository can name the image its agent runs in, so the agent has the repository's own toolchain: a one-line
`.patchy/agent.yaml` (`image: <ref>`), or the top-level `image` of an existing `.devcontainer/devcontainer.json`. The
operator turns this on with `agent.repositoryImages` and decides which registries are allowed; the
[design](../design/repository-runner-images.md) covers resolution, signing and the threat model. This page covers
building such an image: the base image patchy publishes for it, the sandbox the image runs in, and worked examples.

## The base image

`ghcr.io/devthenet-labs/patchy/agent-base` is published with every patchy release, tagged `v<version>` and `latest`, for
`linux/amd64` and `linux/arm64`, and cosign-signed like the other patchy images. It is built from
[`Dockerfile.agent-base`](https://github.com/devthenet-labs/patchy/blob/main/Dockerfile.agent-base) and contains no
patchy binary: at pod start a trusted init container copies `agent-runner` and the claude CLI into a read-only
`/patchy/bin`, so the image never has to carry or update them.

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

## The sandbox your image runs in

Whatever the image says, the agent container:

- runs as uid 65532 with a read-only root filesystem, all capabilities dropped and no privilege escalation; the image's
  `USER` and `ENTRYPOINT` are ignored, and `/patchy/bin/agent-runner` is started by absolute path;
- can write only to `/workspace` (`HOME`, with the repository at `/workspace/repo`) and `/tmp`, both emptyDirs that hide
  whatever the image has at those paths and allow executing what is written there;
- has no network except the egress broker, so nothing can be downloaded at run time;
- runs with `PATH=/patchy/bin:<the image's PATH>`, `HOME=/workspace` and `GIT_CONFIG_NOSYSTEM=1` (the image's
  `/etc/gitconfig` is ignored), and with `BASH_ENV`, `ENV`, `SHELLOPTS`, `PROMPT_COMMAND`, `LD_PRELOAD`,
  `LD_LIBRARY_PATH`, `LD_AUDIT`, `NODE_OPTIONS`, `PYTHONSTARTUP`, `GIT_CONFIG_GLOBAL`, `GIT_CONFIG_SYSTEM`,
  `GIT_EXEC_PATH`, `GIT_SSH_COMMAND` and the proxy variables set empty, so an image that relies on any of them breaks.

## Building FROM it

- **Install as root, run as 65532.** Switch to `USER 0:0` for `apk add` and back to `USER 65532:65532` at the end.
- **Bake every dependency in.** Go modules, Python packages and `node_modules` must be in the image, and the tools
  configured not to reach for the network, so a missing dependency fails fast instead of hanging.
- **Keep baked content out of `/workspace` and `/tmp`.** Install toolchains and dependencies under paths such as `/opt`
  or `/usr`, readable by uid 65532; the emptyDirs would hide anything placed at those two paths.
- **Keep writable caches under `/tmp` or `HOME`.** The base's `XDG_CACHE_HOME` already covers Go's build cache and pip;
  point other tools there too.
- **Leave reserved names out of `ENV`.** source-controller rejects an image whose `ENV` sets `HOME`; any name starting
  `PATCHY_`, `ANTHROPIC_` or `CLAUDE_`; a model or forge credential name (`OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`); `AWS_REGION` or `CLOUD_ML_REGION`; a proxy
  variable in either case; or one of git's repository redirections (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`,
  `GIT_OBJECT_DIRECTORY`, `GIT_COMMON_DIR`).
- **No `VOLUME`.** An image that declares one is rejected: only patchy's emptyDirs are writable.
- **Prepend to `PATH`, keep it absolute.** Empty and relative entries are dropped, and a `PATH` with no absolute entry
  is rejected.
- **Stay on glibc.** Rebasing the image onto Alpine breaks the claude CLI; the agent's preflight fails the run with
  `image_incompatible`.
- **Label your own image.** Labels are inherited, so set `org.opencontainers.image.source` and `description` to yours.

## Examples

[`examples/agent-images/`](https://github.com/devthenet-labs/patchy/tree/main/examples/agent-images) has one small,
commented Dockerfile per toolchain. Each copies only the dependency manifest from the repository, so the image is
rebuilt when dependencies change and never contains the source.

| Example                                                                                              | Toolchain       | Dependencies baked into      | Offline switch                     |
| ---------------------------------------------------------------------------------------------------- | --------------- | ---------------------------- | ---------------------------------- |
| [Go](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/go/Dockerfile)         | Go 1.24         | `GOMODCACHE=/opt/go/pkg/mod` | `GOPROXY=off`, `GOTOOLCHAIN=local` |
| [Python](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/python/Dockerfile) | Python 3.13     | virtualenv at `/opt/venv`    | `PIP_NO_INDEX=1`                   |
| [Node.js](https://github.com/devthenet-labs/patchy/blob/main/examples/agent-images/node/Dockerfile)  | Node.js 24, npm | `/node_modules`              | `npm_config_offline=true`          |

The Node example installs `node_modules` at the filesystem root because Node resolves packages by walking up from the
importing file, and `/` is the only ancestor of `/workspace/repo` the pod does not replace with an emptyDir; it avoids
`NODE_PATH`, which ES modules ignore, and `NODE_OPTIONS`, which the pod empties.

## Trying an image locally

`patchy check image` runs the checks source-controller runs, and with `--run` the agent's own preflight in a local
docker container shaped like the pod, including the injected claude CLI:

```sh
patchy check image ghcr.io/your-org/your-repo-agent:1 --run
```

It prints one PASS, FAIL or SKIP line per check. The registry checks need the image pushed (a scratch tag or a local
registry will do); `--run` also works on an image that is only in your local docker. See the
[CLI tour](../cli.md#checking-an-agent-image) for the flags.

To go further, run the image the way the pod does before pushing it. Docker mounts a `--tmpfs` `noexec` by default,
while the pod's emptyDirs allow execution (`go test` runs its test binaries from `/tmp`), hence `exec`:

```sh
docker run --rm --user 65532:65532 --read-only --network none --cap-drop ALL \
  --security-opt no-new-privileges --tmpfs /tmp:exec,mode=1777 --tmpfs /workspace:exec,mode=1777 \
  -v "$PWD":/workspace/repo -w /workspace/repo -e HOME=/workspace \
  ghcr.io/your-org/your-repo-agent:1 bash -c 'go build ./... && go test ./...'
```

To check that the injected claude CLI runs on your image, copy it out of the runner image for the same platform as your
image and mount it where the pod does:

```sh
mkdir -p patchy-bin && id=$(docker create ghcr.io/devthenet-labs/patchy/claude-agent-runner:latest)
docker cp "$id":/usr/local/bin/claude patchy-bin/claude && docker rm "$id"
docker run --rm --user 65532:65532 --read-only --network none \
  -v "$PWD/patchy-bin":/patchy/bin:ro ghcr.io/your-org/your-repo-agent:1 /patchy/bin/claude --version
```
