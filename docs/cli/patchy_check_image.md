## patchy check image

Check an agent image a repository means to declare

### Synopsis

Check an image before declaring it in .patchy/agent.yaml or as the image of
.devcontainer/devcontainer.json, with the checks patchy itself runs.

Without --run, the checks are source-controller's own, run by the same code: the
reference is canonicalised, checked against --allow (the operator's
--repository-image-registries) when given, pinned to a digest, and every
linux/amd64 and linux/arm64 manifest is judged for platform, compressed size,
VOLUME, reserved ENV and PATH; with --cosign-key the signature is verified as
well. Registry credentials are your local docker credentials
(~/.docker/config.json and its credential helpers), so an image you can pull
is an image this can check. Every check is reported, not just the first
failure.

With --run, the image is also run the way the agent pod runs it, on your local
docker: agent-runner and the claude CLI are copied out of the claude runner
image released with this CLI (--runner-image to override), and the image runs
as uid 65532 with a read-only root filesystem, no network, no capabilities, no
privilege escalation, bounded processes, memory and CPU, sized executable tmpfs
mounts at /tmp and /workspace, the two binaries read-only at /patchy/bin,
PATH=/patchy/bin:<the image's PATH> and the rest of the pod's environment; each
container is removed when its run ends, even an interrupted one. In it,
agent-runner's own preflight (the check a stage runs before its first model
call: claude --version, git --version and bash -c true) runs, then bash -c true
and git --version on their own. Without a docker CLI the run is skipped, not
failed. An image that exists only in your local docker store fails the registry
checks but still runs; to check both before publishing, push it to a scratch
tag or a local registry.

Each check prints one line: PASS, FAIL or SKIP, the check, and the reason.
-o json or -o yaml prints the whole report as data instead. The exit status is
non-zero when any check fails.

```
patchy check image <reference> [flags]
```

### Examples

```
  patchy check image ghcr.io/acme/shop-agent:1
  patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --cosign-key cosign.pub
  patchy check image ghcr.io/acme/shop-agent:1 --run
  patchy check image ghcr.io/acme/shop-agent:1 --run -o json | jq '.checks[] | select(.status == "FAIL")'
```

### Options

```
      --allow stringArray     registry path prefix the image must sit under, as the operator's --repository-image-registries entries (repeatable)
      --cosign-key string     operator's PEM cosign public key; verify the image's signature with it
  -h, --help                  help for image
      --max-bytes int         largest compressed layer total per platform, as the operator's --repository-image-max-bytes (default 4294967296)
      --run                   also run the image the way the agent pod does, on the local docker
      --runner-image string   claude runner image to take agent-runner and claude from with --run (default: the one released with this CLI)
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
  -o, --output string              output format: table, wide, json, yaml, name, or markdown (default "table")
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [patchy check](patchy_check.md)	 - Check an artifact the way patchy will judge it, without a cluster

