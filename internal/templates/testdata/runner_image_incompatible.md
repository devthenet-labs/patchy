<!-- patchy:runner-image -->
### Agent runner image

`.patchy/agent.yaml` declares `ghcr.io/acme/go-env:1.26`. patchy pinned it to `ghcr.io/acme/go-env@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd`, and runs the agent in it with patchy's own tools added under `/patchy/bin`.

The investigation (attempt 2) stopped before the agent started: the image failed the agent's preflight (`image_incompatible`):
```text
preflight: /patchy/bin/claude --version: exit status 127: /lib/ld-linux-aarch64.so.1: not found (a musl image cannot run the claude CLI)
```

The image must be glibc-based, have `bash`, `sh` and `git` on its `PATH`, and let uid 65532 run them. `patchy check image ghcr.io/acme/go-env:1.26 --run` runs the same preflight in a local container.

To change the image, edit `.patchy/agent.yaml`; the [agent image guide](https://devthenet-labs.github.io/patchy/integrations/agent-images/) covers what the image needs.
