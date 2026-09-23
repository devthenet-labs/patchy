# Agent image examples

Worked examples of an agent image a repository can declare in `.patchy/agent.yaml`, each built `FROM` patchy's
[`agent-base`](../../Dockerfile.agent-base) image (`ghcr.io/devthenet-labs/patchy/agent-base`). The base supplies glibc,
bash, git and the core build tools, the uid 65532 `patchy` account and caches under `/tmp`; each example adds one
toolchain and bakes the project's dependencies in, because the agent pod has no network.

| Example                        | Toolchain       | Dependencies baked into      | Offline switch                     |
| ------------------------------ | --------------- | ---------------------------- | ---------------------------------- |
| [`go/`](go/Dockerfile)         | Go 1.24 (wolfi) | `GOMODCACHE=/opt/go/pkg/mod` | `GOPROXY=off`, `GOTOOLCHAIN=local` |
| [`python/`](python/Dockerfile) | Python 3.13     | virtualenv at `/opt/venv`    | `PIP_NO_INDEX=1`                   |
| [`node/`](node/Dockerfile)     | Node.js 24, npm | `/node_modules`              | `npm_config_offline=true`          |

Build an example with your repository root as the context, since each copies only the dependency manifest (`go.mod` and
`go.sum`, `requirements.txt`, or `package.json` and `package-lock.json`) from it, never the source:

```sh
docker build -f examples/agent-images/go/Dockerfile -t ghcr.io/your-org/your-repo-agent:1 .
```

Then push it to a registry the operator has allowlisted and declare it:

```yaml
# .patchy/agent.yaml
image: ghcr.io/your-org/your-repo-agent:1
```

Rebuild the image whenever the dependency manifest changes. Set your own `org.opencontainers.image.source` label (the
examples carry a placeholder), because labels are inherited from the base.

Before pushing, run the image the way the pod does: uid 65532, read-only root filesystem, no network, no capabilities,
and only `/workspace` (`HOME`) and `/tmp` writable. Docker mounts a `--tmpfs` `noexec` by default, whereas the pod's
emptyDirs allow execution (`go test` runs its binaries from `/tmp`), so pass `exec`:

```sh
docker run --rm --user 65532:65532 --read-only --network none --cap-drop ALL \
  --security-opt no-new-privileges --tmpfs /tmp:exec,mode=1777 --tmpfs /workspace:exec,mode=1777 \
  -v "$PWD":/workspace/repo -w /workspace/repo -e HOME=/workspace \
  ghcr.io/your-org/your-repo-agent:1 bash -c 'go build ./... && go test ./...'
```

See [Agent images](../../docs/integrations/agent-images.md) for the rules an agent image has to follow.
