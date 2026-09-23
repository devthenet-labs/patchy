# Repository-declared runner images

**Status:** Proposed, 2026-09-22. Revised the same day to apply the accepted findings of the security review; the
"Security review" subsection at the end lists each one and where the design now handles it. Revised again the same day
to record three v1 decisions (the devcontainer.json fallback, a render failure on broad egress, and the first live
verification target); the "Decisions" subsection at the end records them. Amended the same day, after implementation
began, so that a devcontainer.json patchy cannot honour is no declaration at all rather than a rejection (Decisions, 4).

## Context

The claude agent image (`Dockerfile.claude-agent-runner`: wolfi-base, the claude CLI, bash, git, curl, `agent-runner`)
ships no language toolchain. On the devthenet cluster the remediation agent for a Go repository hit
`go: command not found` and could not build or run the tests it wrote. The pod has no network by design (its
NetworkPolicy allows DNS, the artifact server on `:9790` and the egress broker, nothing else), so a toolchain and its
dependency cache cannot be fetched at run time; they must be in the image.

A fat image and per-language images were rejected: both put patchy in the business of guessing every repository's build
environment. Instead, as GitHub Actions and the Copilot coding agent do, the target repository declares the image its
agent runs in. Patchy injects its own two pieces (the harness CLI and `agent-runner`) into that image at pod start, the
isolation model stays exactly as it is, and the operator keeps policy control. A repository that declares nothing gets
the per-harness runner image (`agent.runners.<harness>.image`) as today.

## Goals

- A repository owner selects the agent's toolchain with one committed file and no cluster access: `.patchy/agent.yaml`,
  or the top-level `image` of an existing `.devcontainer/devcontainer.json`.
- The default Job shape is byte-for-byte unchanged; upgrading with the feature off changes nothing observable.
- One chart block, off by default, holds every policy knob; turning the feature off is one value.
- The image is pinned to a digest exactly once per Repository, beside `resolvedSHA`, by the same single writer, and
  every check runs against that digest, never against a tag.
- Every security question below has an enforced answer, not a documented hope. Where enforcement depends on the cluster
  (NetworkPolicy), patchy verifies it per Job instead of assuming it.

## Non-goals

- Building images. A devcontainer.json that uses `build`, `dockerFile`, `dockerComposeFile` or `features` is not
  applicable: the default image runs, the reason is recorded, nothing is built; patchy honours only a prebuilt image
  named by its top-level `image`.
- Any other devcontainer.json behaviour: lifecycle commands (`postCreateCommand` and friends), `containerEnv`,
  `remoteEnv`, `remoteUser`, mounts and `customizations` are ignored, and devcontainer.json files at other paths
  (`.devcontainer.json`, `.devcontainer/<name>/devcontainer.json`) are not read.
- Repository images for codex/copilot (they hold a real credential in-pod) or for evaluation Jobs.
- Keyless (Fulcio/Rekor) signature verification; key-mode cosign only, in both the bundle and the legacy tag form.
- A per-run-kind exemption from the broker's per-pod limits for evaluation pods (it would need a pod lookup and new RBAC
  in the broker).

## Threat model

**Who can influence the image.** Three principals, not one:

1. Anyone who can commit to the branch source-controller pins (`Repository.spec.ref.branch`, the default branch when
   empty). The declaration (`.patchy/agent.yaml` or `.devcontainer/devcontainer.json`) is read from the tarball already
   stored at `status.resolvedSHA`, never from a second forge call, so it is bound to the tree the agent works on. That
   principal already controls what the agent executes, because the remediation stage runs the repository's own build and
   tests. What the image adds is control of the process environment the harness runs in, which is why the harness
   binaries are injected from a trusted image rather than taken from the declared one.
2. Anyone who can push to an allowlisted registry path: members of a ghcr organisation holding `packages: write`,
   holders of `ecr:PutImage` on an allowlisted ECR repository, and the registry operator itself. Registry write alone is
   never image authority: a signature by the operator's key is required unless the operator explicitly sets
   `allowUnsigned: true`.
3. A tag-race attacker who can move a tag between the moment the manifest is read and the moment the image is checked or
   pulled. Closed by resolving a tag exactly once and running every check on, and recording, the digest-pinned
   reference.

**What a hostile image can do.** Everything the agent can and nothing more: run arbitrary code as uid 65532; read the
workspace, the handoff markdown and the projected broker caller token; call the broker directly with that token,
bypassing `agent-runner`'s output-token kill switch; write forged `PATCHY-EVENT:` lines to `/proc/1/fd/1` (a fabricated
verdict or changeset); wrap `git`/`bash` from its own PATH; supply the dynamic loader, libc and `/etc/ld.so.preload`
that `claude` links against, so the CLI's behaviour and everything it reports (usage, cost, turns) is image-controlled;
burn CPU, memory and ephemeral storage up to the Job limits.

**What it cannot do.** Reach anything but DNS, the artifact server and the allowlisted surface of the broker, verified
per Job by the sandbox probe below rather than assumed from the CNI; obtain a forge or model credential (none exists in
the pod; the placeholder `ANTHROPIC_AUTH_TOKEN` is public and stripped by the broker); talk to the Kubernetes API (the
projected token is audience-bound to `patchy-egress-broker`; the `patchy-agent` ServiceAccount has
`automountServiceAccountToken: false` and no Role); become root, gain capabilities or write outside the emptyDirs; alter
its labels, harness or Job deadline; replace `agent-runner` (read-only mount, absolute path, static binary); affect any
other Finding. A forged changeset lands as a pull request on the same repository, pushed by remediation-controller with
a token the pod never sees, only after the controller has validated it (entry cap, path shape, no CI definitions) and
only for human review.

Under a repository image the trust boundaries are `agent-runner` and the broker. `claude` is not one: the image supplies
the runtime it executes on, so the preflight below is a compatibility check, not an integrity check, and the usage and
cost the CLI reports from such a run are untrusted (the broker's per-pod token totals are the record).

**What patchy enforces regardless of image.** `containerSecurity()` in `internal/jobs`: `RunAsUser 65532`,
`RunAsNonRoot`, read-only rootfs, drop ALL, no privilege escalation, seccomp RuntimeDefault; the image's `USER` and
`ENTRYPOINT` are irrelevant, and an image-declared `VOLUME` is rejected at resolution so the only writable paths are
patchy's emptyDirs. The trusted `prepare` init always runs the per-harness runner image, so the digest-verified tarball
fetch, the synthetic base commit and the sandbox probe happen before untrusted code runs. `activeDeadlineSeconds` is the
wall on time; the broker's token accounting is the wall on spend on metered routes; the ephemeral-storage limit is the
wall on disk.

Residual channels are stated rather than hidden: the broker token is readable (bounded by the broker's allowlisted
surface, its per-pod and global limits, and the deadline); stdout is forgeable (bounded by changeset validation, human
PR review, and routing every `ignore` verdict from a repository-image run to a human); and the DNS channel to the
cluster resolver exists today and is unchanged. Nothing in patchy bounds DNS; a Cilium `toFQDNs`/DNS-rules allowlist
does, and the operator documentation recommends one where the CNI offers it.

## Design

### The declaration file and precedence

`.patchy/agent.yaml` at the repository root of the pinned tree:

```yaml
image: ghcr.io/devthenet-labs/go-agent-env:1.26
```

`image` is the only key; unknown keys are rejected (fail closed on a newer schema), the file is capped at 64 KiB,
`build:` forms are unsupported. Tags are accepted and pinned to a digest at resolution. A `@sha256:` pin must be
`sha256:` plus 64 hex (`imageref.Parse` accepts anything after `@`); it skips only the HEAD that resolves a tag and goes
through the identical size, platform, ENV, PATH, VOLUME and signature checks.

**devcontainer.json fallback.** When the pinned tree has no `.patchy/agent.yaml`, `.devcontainer/devcontainer.json` is
consulted, and only its top-level string `image` is honoured:

```jsonc
{
  // the agent runs in this prebuilt image
  "image": "ghcr.io/devthenet-labs/go-agent-env:1.26",
  "customizations": { "vscode": { "extensions": ["golang.go"] } },
}
```

The same 64 KiB cap applies. The file is JSONC, so it is parsed by a small tolerant pre-processor in
`internal/runnerimage/jsonc.go`, beside the `.patchy/agent.yaml` parser, rather than by a new dependency: one pass over
the bytes tracking string and escape state that blanks `//` line comments and `/* */` block comments with spaces (byte
offsets survive, so `encoding/json` error positions still point into the original file) and drops a comma whose next
non-space, non-comment byte is `}` or `]`; the result is decoded with `encoding/json` into a map of raw values. An
unterminated string or block comment, or anything `encoding/json` then refuses, makes the file not applicable (reason
"could not parse `.devcontainer/devcontainer.json`: `<error>`", see below). `github.com/tailscale/hujson` was the
alternative; the pre-processor is about eighty lines with property tests, so the dependency does not pay for itself.

The decoded file is then judged, first match wins:

1. `build`, `dockerFile` or `dockerComposeFile` present: not applicable, with the reason
   "`.devcontainer/devcontainer.json` builds its image (`<key>`); patchy does not build images. Publish the image and
   set `image`, or declare one in `.patchy/agent.yaml`, which takes precedence."
2. `features` present and non-empty: not applicable, with the same shape ("... adds features, which patchy would have to
   build ..."), because features change the image and patchy runs only what the registry serves.
3. `image` absent, not a JSON string, empty, or containing `${` (devcontainer variable substitution, which patchy does
   not perform): not applicable, with a reason naming which.
4. Otherwise `image` enters resolution exactly as a `.patchy/agent.yaml` value would, through the same allowlist,
   digest, size, platform, ENV, PATH, VOLUME and signature checks.

**Not applicable is not a rejection.** A devcontainer.json was written for the editor, not for patchy, so one that
patchy cannot honour (cases 1 to 3, an unparseable file, or one over the size cap) is treated as no declaration: the
default runner image runs, the finding is never parked, `onReject` is not consulted, and the outcome is recorded as
informational (`status.runnerImage.manifest` names the file and `message` carries the reason; `image` stays empty) so
the sticky comment can tell the owner why the file was not used. `runnerimage.Declare` expresses this in its result
type: a `Declaration` with `OutcomeNotApplicable` is a value, while an invalid `.patchy/agent.yaml` is a `*Rejection`
error, so a caller that handles the error and then switches on the outcome cannot park a finding on a build-based
devcontainer. Only an explicit `.patchy/agent.yaml` that fails validation, and a declared image (from either file,
case 4) that fails a resolution check, are rejections subject to `onReject`.

Every other key is ignored rather than judged, unlike `.patchy/agent.yaml`: the file belongs to the editor tooling and
carries keys patchy has no business judging, and a repository owner who wants fail-closed strictness writes
`.patchy/agent.yaml`.

**Precedence.** `.patchy/agent.yaml` wins whenever it exists: a valid one is used and devcontainer.json is not read at
all (so a build-based devcontainer never blocks a repository that declares its agent image explicitly); an invalid one
is a rejection and never falls through to devcontainer.json, because a broken explicit declaration must not silently
become a different image. Only when `.patchy/agent.yaml` is absent is devcontainer.json consulted, and a not-applicable
one yields the default image exactly as if it were absent, with the reason recorded; when neither exists the Repository
gets the default image. `status.runnerImage.manifest` records which file the image came from.

Image contract, documented for repository owners and checked at resolution and by the preflight below: glibc-based Linux
for `linux/amd64` or `linux/arm64` (a multi-arch index must satisfy the contract on both); `bash`, `sh` and `git` on the
image's PATH; toolchains readable by uid 65532; caches under `$HOME=/workspace` or `/tmp` (the rootfs is read-only); no
`VOLUME` instructions; no `ENV` that sets a `PATCHY_*`, `ANTHROPIC_*`, `CLAUDE_*`, `CLAUDE_CODE_*` or proxy variable;
dependencies baked in (for Go, a populated `GOMODCACHE` or `GOFLAGS=-mod=vendor`) because the pod has no network. Image
ENV is otherwise honoured, including its PATH. Repository owners are also told that CI must gate `patchy/**` branches
(branch filters, or environments with required reviewers), because a pull request from patchy is untrusted input until a
human has read it.

### Resolution and pin-once recording

`RepositoryReconciler.Reconcile` (`internal/controller/source/repository_controller.go`) gains one block after the
artifact fetch, guarded exactly like the SHA: `if r.Images != nil && repo.Status.RunnerImage == nil`. The status write
is split in two so that a transient registry failure never costs a re-download: the first update persists `ResolvedSHA`,
`Forge` and `Artifact` with `Ready=False` / `RunnerImageResolving`; the second, after resolution, writes `RunnerImage`
and `Ready=True`. The artifact store indexes tarballs in memory only (they are reproducible), so a restart between the
two re-downloads the tarball at the persisted `ResolvedSHA` (the same tree) and resumes at resolution; a restart after
the second carries the pointer through untouched, so a moved tag can never change the environment between investigation
and remediation of one finding. `RunnerImage` has one writer, and `stall(reason)` carries the artifact through so a
rejected Repository still has a tree to run on.

Steps, in a new pure package `internal/runnerimage` plus an `ocireg`-backed resolver:

1. `artifact.Store.Open(key) (io.ReadCloser, error)` streams the stored tarball; a bounded tar walk reads
   `<first component>/.patchy/agent.yaml` and `<first component>/.devcontainer/devcontainer.json` (the GitHub archive
   prefix) and nothing else, then applies the precedence above.
2. `imageref.Normalize` + `imageref.Parse`, then `Policy.Allow`. `Policy` validates its entries at construction (flag
   parse time; the chart mirrors the rules): an entry is `host/segment[/...]` with at least one path segment (a
   host-only entry fails), is normalized to a trailing `/`, is matched on segment boundaries (`ghcr.io/org/` never
   matches `ghcr.io/org-evil/x`), may not contain glob characters, and `index.docker.io` is canonicalized to `docker.io`
   on both sides. Never `GlobMatch` (its `*` spans `/`).
3. Resolve to a digest exactly once. For a tag, `ocireg.Client.Digest` (HEAD) returns the digest; for a `@sha256:` pin
   the HEAD is skipped. Either way the resolver builds `<repo>@sha256:<digest>` and passes only that reference to every
   subsequent call (`ocireg` gains `ConfigFile(ctx, ref)` beside `Manifest` and `Platforms`), so nothing after this step
   can observe a tag move. The client authenticates with a host-selected keychain: the ECR credential helper (IRSA or
   Pod Identity, the path context-controller already uses) for `*.amazonaws.com`, Application Default Credentials for
   Artifact Registry (cached only on success: ggcr's `google.Keychain` keeps anonymous for the process lifetime when the
   first lookup fails), the mounted dockerconfigjson for everything else, anonymous last. A cloud credential failure is
   transient backoff, never anonymous. A 401 or 403 here is deterministic (`RunnerImageRejected`: "registry denied
   access to `<ref>`; configure pullSecret or a cloud credential"), never retry backoff.
4. Enumerate what would actually run. If the resolved object is an image index, the `linux/amd64` and `linux/arm64`
   children are enumerated by their own digests and each is checked in step 5; the image is rejected unless every child
   passes with an identical sanitized PATH, and the recorded digest is the index digest (what cosign signs and the
   kubelet pulls). Children are judged the way containerd picks one (`containerd/platforms` normalization): every
   spelling of those platforms (`x86_64`, `aarch64`, upper case, an empty OS) is checked, and an index that leaves a
   node an unchecked fallback (`linux/386` with no `linux/amd64` entry, `linux/arm/*` with no `linux/arm64` entry, an
   entry with no platform unless both architectures have one, a nested index) is rejected. If it is a single-platform
   manifest, os/arch are read from its config and must be linux with one of those two architectures.
5. Per child: `crane.Manifest` sums compressed layers against `maxBytes`; `ConfigFile` fetches the config, whose `Env`
   is rejected if it names anything in `reservedEnv` union `provider.GatewayEnvNames`, anything with the prefix
   `PATCHY_`, `ANTHROPIC_`, `CLAUDE_` or `CLAUDE_CODE_`, or `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `ALL_PROXY` in
   either case; whose `Volumes` must be empty (`RunnerImageRejected`: "image declares VOLUME `<paths>`; only patchy's
   emptyDirs are writable", because containerd honours image volumes as writable host-backed mounts outside
   ephemeral-storage accounting); and whose `PATH` is sanitized into `status.runnerImage.searchPath`. The sanitizer
   substitutes the runc default (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`) when the config carries
   no `PATH`, drops empty and relative entries, and refuses an empty result (`RunnerImageRejected`), so the pod's PATH
   can never contain an empty component that a shell would read as the untrusted working tree.
6. Signature verification against the recorded digest (required unless `allowUnsigned: true`; the two accepted forms are
   under Operator policy).
7. The second status update writes
   `status.runnerImage{declared, manifest, image (name@sha256:...), searchPath, verified, resolvedAt}` and `Ready=True`,
   where `manifest` is the path of the declaring file.

Deterministic rejections (a bad `.patchy/agent.yaml`, not allowlisted, 404, 401/403, oversized, wrong or missing
platform, reserved ENV, VOLUME, empty PATH, missing or bad signature) go through `stall`, extended to take a reason:
`Stalled=True` / `Ready=False`, reason `RunnerImageRejected`, a human message. Transient errors go through
`fail(ctx, repo, "RunnerImageResolveFailed", err)`, which returns the error for controller-runtime backoff, like a forge
outage. Repositories are per Finding, so a 10-minute in-memory cache of check results keyed by resolved digest bounds
registry traffic without weakening pin-once: the HEAD that resolves a tag is never cached, so a moved tag cannot reuse a
stale verdict. The `ocireg` client is built with the explicit keychain above, never `authn.DefaultKeychain`. `imageref`
and `ocireg` gain a controller consumer; the "mirror is CLI-only" line in `AGENTS.md` is amended.

### Operator policy

One chart block, `agent.repositoryImages`, rendered into the controllers' ConfigMaps:

- **Registry allowlist**: required non-empty when enabled (the chart `fail`s otherwise); each entry must carry a path
  segment and is validated with the rules in step 2, in the chart and again at flag parse. NOTES and the operator page
  name pull-through-cache namespaces (`.../docker-hub/`, `.../ecr-public/`,
  `<region>-docker.pkg.dev/<project>/*-remote/`) as paths that must never be allowlisted, because anyone can populate
  them. With the signing default an over-broad entry still requires the operator's key.
- **Digest pinning**: always on, not a knob.
- **Signatures**: `cosignPublicKey` (PEM) is required when `enabled: true` unless `allowUnsigned: true` is set
  explicitly; the chart `fail`s otherwise and NOTES warns whenever unsigned images are allowed. Verification is
  in-process with stdlib crypto, no cosign binary in the distroless controller image, no Fulcio/Rekor egress, and
  accepts two forms. First the referrers bundle, which cosign v3 and `patchy mirror sign` produce: `remote.Referrers` on
  the resolved digest, filtered to artifactType `application/vnd.dev.sigstore.bundle.v0.3+json` (with the
  `sha256-<digest>` referrers-tag fallback), the bundle parsed, `messageSignature.messageDigest` required to equal the
  recorded digest, and the ECDSA P-256/SHA-256 signature verified with the configured key per bundle v0.3
  message-signature semantics. Second the legacy `sha256-<digest>.sig` tag with the simple-signing payload, whose
  `critical.image.docker-manifest-digest` must equal the recorded digest. Repository owners run `cosign sign --key`; the
  docs state that v3 writes bundles and that `patchy mirror sign` output verifies.
- **Size**: `maxBytes` (default `4Gi`) bounds compressed layers only, per platform child. Writable space is bounded
  separately: `agent.resources.ephemeralStorage` renders an ephemeral-storage request and limit onto both containers, so
  a fill is evicted by the kubelet rather than filling the node.
- **Root images**: no knob; `containerSecurity()` forces the UID.
- **Pull credentials**: `pullSecret` names a dockerconfigjson Secret in the release namespace. source-controller mounts
  it with `items: [{key: .dockerconfigjson, path: config.json}]` at `DOCKER_CONFIG=/etc/patchy/registry` (the key name
  ggcr reads). The kubelet resolves `imagePullSecrets` in the pod's namespace only, so the `patchy-agent`
  ServiceAccount's entry needs a Secret of the same name in `agent.namespace`: the chart renders it there when
  `pullSecretData` is supplied, and otherwise documents the two-namespace requirement and NOTES warns. Cloud registries
  need no Secret at all: ECR and Artifact Registry are handled by the resolver's cloud keychains and by node credentials
  at pull time. Unset means anonymous resolution.
- **Kill switch**: `enabled: false` (default) stops manifest parsing in source-controller and makes both job
  controllers' `launch()` ignore an already-pinned image, so a flip takes effect at the next Job without touching CRs.
- **Verdict hold**: not a knob. An `ignore` recommendation produced on a repository image always routes to `HandedOff`
  instead of `Dismissed` (`route()` in `investigation_controller.go`), keyed on the Investigation's own launch-time
  stamp (`inv.Status.RunnerImage.Source == "repository"`) and never on controller config at collect time, so kill-switch
  or `onReject` flips between attempts cannot change the decision. This is the one abuse no sandbox control bounds, a
  forged dismissal of a real finding, and it now needs a human.
- **Sandbox probe**: when enabled, every repository-image Job verifies that NetworkPolicy is actually enforced before
  untrusted code runs (Pod construction below), and investigation-controller runs a probe-only canary Job at startup and
  refuses repository-image Jobs until it passes.

Prerequisites the chart enforces: render fails when enabled with `agent.networkPolicy.create: false`; when enabled while
`patchy.broadEgress` resolves true (under `mode: none`, and `istio`, with the default `broadEgress: auto`, the base
policy grants TCP 443 anywhere, so even an enforcing CNI would let a hostile image reach a model API with its own key);
when enabled without `cosignPublicKey` and without `allowUnsigned: true`; and when enabled while the broker's effective
`modelAllowlist` is empty.

The broad-egress guard is decided as a render failure, not a NOTES warning (see Decisions). `_helpers.tpl` fails with:

```text
agent.repositoryImages.enabled requires narrow agent egress, but agent.networkPolicy.broadEgress ("auto") resolves to
broad under agent.networkPolicy.mode "none": the base policy would allow TCP 443 to anywhere. Set
agent.networkPolicy.broadEgress: never (brokered claude runners only), or use agent.networkPolicy.mode cilium or gke.
```

with the two quoted values filled from the render. `agent.networkPolicy.broadEgress: never` is the value it names: it
removes the 443 rule for every runner, so under `mode: none` codex and copilot runners lose their model egress and a
fleet enabling repository images is brokered-only; the operator page says so. What the chart cannot check, and NOTES
states loudly: the CNI must actually enforce NetworkPolicy. EKS Auto Mode does not by default; the probe turns that into
a `SandboxUnenforced` failure instead of a silent hole.

### Pod construction

`internal/jobs/jobs.go`: `Spec.RunnerImage` and `Spec.RunnerSearchPath` (from the Repository status),
`Runner.Inject []string` (the binaries this harness's runner image contributes; `runnercfg` sets
`{"agent-runner", "claude"}` for claude and nil for codex/copilot/fake, so "claude only" lives in config),
`Config.AllowRepositoryImages`. `buildJob` injects only when all three hold; otherwise the pod is what it is today.
`jobs.Client.Create` returns the effective image and source it wrote into the `runner-image` and `runner-image-source`
annotations, and that return value is the only thing the callers record: the source is `default` whenever injection did
not happen, for any reason (codex, copilot, the fake harness, the kill switch), so the audit trail cannot say
`repository` for a pod that ran the default image.

When injecting: `prepare` keeps `runner.Image` and its script gains a guarded tail copying each `$PATCHY_INJECT` binary
from `/usr/local/bin` (where the Dockerfile puts them) into a new emptyDir `patchy-bin`, mounted at `/patchy/bin`
read-write in `prepare` and read-only in `agent`. After the artifact fetch the same trusted script runs the sandbox
probe, a bounded negative-connectivity check: `curl -m 3 -sS -o /dev/null https://1.1.1.1/`,
`curl -m 3 -sSk https://$KUBERNETES_SERVICE_HOST:443/` and `curl -m 3 -sS http://169.254.169.254/`; if any of them
succeeds the init prints one stderr line and exits 78, and the agent container never starts. The probe is appended only
when injecting, so the default Job stays byte-identical.

The agent container runs `spec.RunnerImage` with `Command: ["/patchy/bin/agent-runner"]` (absolute: the image's
ENTRYPOINT and PATH cannot redirect it), and its env adds `PATCHY_BIN_DIR=/patchy/bin`, an explicit `PATH` that is the
`strings.Join` of `/patchy/bin` and the non-empty sanitized entries (controller-owned, never a trailing or doubled `:`,
yet the image's `/usr/local/go/bin` survives), `GIT_CONFIG_NOSYSTEM=1`, and explicit empty values for `BASH_ENV`, `ENV`,
`SHELLOPTS`, `PROMPT_COMMAND`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `NODE_OPTIONS`, `PYTHONSTARTUP`,
`GIT_CONFIG_GLOBAL`, `GIT_CONFIG_SYSTEM`, `GIT_EXEC_PATH`, `GIT_SSH_COMMAND`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`,
`ALL_PROXY` (both cases), every `provider.GatewayEnvNames` entry the runner's `Env` does not set, and every `PATCHY_*`
key that `agentrun.FromEnv` reads and the Job does not set. `agentrun` exports that key list, so the pod-side backstop
covers the whole config surface without a hand-kept list; the proxy variables join `reservedEnv`. An explicit container
env overrides image ENV, so this is the backstop for names the resolver did not know. Git's repository redirections
(`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_COMMON_DIR`) cannot be blanked, because git
reads an empty value as a broken path and Kubernetes cannot unset a variable, so they join `reservedEnv` instead and an
image that sets one is refused at resolution. `agent-runner` is `CGO_ENABLED=0` (`.goreleaser.yaml`), so `LD_PRELOAD`
cannot reach it. `agent-runner` resolves the harness CLI under `$PATCHY_BIN_DIR` alone (`harness.AvailableIn`, never
falling back to PATH) during preflight and runs the stage by that absolute path (`pinCLI`). `git`, `sh` and `bash` come
from the image by contract. Both containers carry the ephemeral-storage request and limit.

Preflight: before the stage, `agent-runner` runs `$PATCHY_BIN_DIR/claude --version`, `git --version` and `bash -c true`;
failure emits a terminal event with the new `envelope.OutcomeImageIncompatible` (`image_incompatible`) and the detail,
so a musl or bash-less image fails in seconds with a readable reason. This is a compatibility check only: the image
supplies the loader and libc the CLI runs on, so a passing preflight says nothing about the CLI's integrity.

Status: `jobs.Status` gains `Waiting` (the agent container's `waiting.reason`, read from the pod), `InitExitCode` (the
`prepare` container's termination exit code) and `RunnerImageSource` (the Job annotation), so the collectors can
distinguish a repository-image Job from a default one without a second lookup.

Audit: Job and pod annotations `patchy.bitwisemedia.uk/runner-image` (the digest ref that ran),
`patchy.bitwisemedia.uk/runner-image-source` (`repository` | `default`), `patchy.bitwisemedia.uk/tools-image` (the
trusted donor), a selectable label `patchy.bitwisemedia.uk/runner-image-source`; `Investigation.status.runnerImage` and
`Remediation.status.runnerImage` written at launch beside `JobRef` from the value `Create` returned; the "created agent
job" log line gains `runner_image`. The `harness` label (the egress policy selector) stays controller-set.

### Broker-side enforcement

Under an untrusted image the in-pod kill switch is advisory, and the caller token is readable by anything in the pod, so
the broker is the enforcement point for both what a pod may ask of the model API and how much. Enforcement runs in four
layers in `Server.handle`, before any upstream call.

**Pre-authentication.** Ahead of `authenticate`: a syntactic token check (three base64url segments, an `aud` containing
the broker audience, an `exp` in the future, a `kubernetes.io.pod` claim) rejects malformed tokens without a
TokenReview; a per-source-IP token bucket and concurrency cap (`--preauth-requests-per-second`, `--preauth-burst`); and
a global TokenReview limiter with a small queue (`--token-reviews-per-second`), so a pod varying the token header
degrades itself rather than the API server or every other brokered Job. Failed authentications count against the source
IP. An OTel counter carries pre-auth rejections.

**Authentication.** The existing TokenReview with its verdict cache. When any per-pod limit is configured, a token whose
review lacks the `pod-name` extra is rejected outright; the broker never falls back to an empty pod bucket.

**Route surface.** Each route enforces a positive method and path allowlist and returns 404 for everything else, so the
broker is a model-inference proxy rather than an open proxy to the upstream API. anthropic: `POST /v1/messages`,
`POST /v1/messages/count_tokens`, `GET /v1/models`; Files and Batches are never forwarded. bedrock:
`POST /model/{id}/invoke` and `POST /model/{id}/invoke-with-response-stream`. vertex:
`POST /v1/projects/{cfg}/locations/{cfg}/publishers/anthropic/models/{id}:streamRawPredict` and `:rawPredict`. foundry
likewise. On bedrock, vertex and foundry the model id lives in the path, so it is taken from the path and checked
against `ModelAllowlist` there (a body-level check would be a no-op). The body is buffered on every route, not only
SigV4, under a separate `MaxAnthropicRequestBytes` (2 MiB default; the existing `MaxRequestBytes` stays for signed
routes), and the request is rejected when `tools[].type` matches `web_search_*`, `web_fetch_*`, `code_execution_*` or
`mcp_toolset`, or the body carries `mcp_servers`, `container`, any `file_id`, or a content block with
`source.type == "file"`: none of the server-side tools that reach the internet or the Files API on the pod's behalf can
be invoked through the broker. The verbatim `anthropic-beta` passthrough is replaced by a broker-owned deny-list
(`--beta-denylist`; default strips `mcp-client-*`, `web-fetch-*`, `code-execution-*`, `files-api-*`, `context-1m-*`), a
deny-list rather than an allowlist because the CLI's beta set changes every release.

**Spend accounting.** Keyed on the `identity.pod` the authenticator extracts from the TokenReview `pod-name` extra,
`broker.Config` gains
`Limits{RequestsPerPod, ConcurrentPerPod, TokensPerPod, TokensPerHour, MaxTokensCeiling, ModelAllowlist}`. Request count
and concurrency are checked before proxying on every route. `TokensPerPod` counts input, cache-creation input,
cache-read input and output tokens, read from `message_start.usage` and `message_delta.usage` (SSE) or `usage` (JSON);
on a mid-stream cut or a usage-less response the pod is charged an estimate from the request body size (bytes/4), so
aborting cannot dodge the counter. `TokensPerHour` is a broker-global ceiling, the backstop against attacker-minted
findings. `ModelAllowlist` defaults to `agent.modelAllowlist` plus the CLI's helper (haiku) models, rendered by the
chart, and the chart `fail`s when repository images are enabled and the effective allowlist is empty. Over any limit
returns 429 in the anthropic error envelope before forwarding, with the fixed message prefix
`egress broker: per-pod limit`, which `agentrun.stageOutcome` maps to `budget_exceeded`. The audit line reports the
per-pod totals so operators can size the limit from observed runs; new OTel counters carry the numbers. Usage parsing is
per route and lands on the anthropic route first; until it lands on bedrock, vertex and foundry, `activeDeadlineSeconds`
is the only wall on those routes, and the docs say so. Counters are in-memory per replica, evicted after 24h; the chart
runs one replica. No new RBAC: TokenReview stays the broker's only Kubernetes access. Aggregate spend is bounded by
`maxConcurrent` x the per-pod limit x Job turnover, and the docs state the formula.

**Usage trust.** The CLI's reported `Usage`, `CostUSD` and `NumTurns` feed the rollups today. From a run whose
`runner-image-source` is `repository` they are image-controlled: such runs are excluded from estimate calibration,
flagged in the rollup delta, and the broker's per-pod token totals are the trustworthy record.

### Evaluation Jobs

`EvalSpec` gets no image field and `evalAgentContainer` never injects: evaluation Jobs keep the per-harness runner
image. A workspace bundle has no pinned tree to read a declaration from. Evaluation pods do run under the same
ServiceAccount and the same brokered runner, so the broker keys them identically and they share the per-pod limits; a
per-run-kind exemption was rejected because it would need a pod lookup and new RBAC in the broker. When evolve is
enabled, operators size `tokensPerPod` and `requestsPerPod` for the longest legitimate evaluation, and the
evaluation-controller page says so.

### Failure modes and what the human sees

- **No manifest, or feature disabled**: default image, `status.runnerImage` nil, `runner-image-source: default`.
- **Policy rejection** (an invalid `.patchy/agent.yaml`, or a declared image that fails a resolution check):
  `Stalled=True` / `RunnerImageRejected` on the Repository, artifact retained; the gate's `ensureRepository` parks the
  Finding `HandedOff` with `LastFailureReason` set to the Stalled message (today it forwards a fixed size-cap string),
  which the issue projection's phase notice renders. No attempt consumed. Pin-once stays absolute: the manifest is bound
  to the pinned SHA, so re-resolution only matters if operator policy changed. The revival path is the existing one: a
  human `approve` or `retry` on the parked Finding runs it on the default image, with `status.runnerImage.rejected`
  recorded and the sticky comment stating so; `launch()` copies the pin only when `Image != ""`, so the spawners need no
  special case beyond not blocking on `Stalled`. A fixed manifest is picked up by the next Finding on that repository.
  With `onReject: default` the run proceeds on the default image without the human step, and the rejection is still
  recorded.
- **devcontainer.json that is not applicable** (it builds its image, adds features, names no usable `image`, or cannot
  be parsed): the most common outcome once the feature is on, because many repositories carry a `build`-based
  devcontainer for their editors. It is no declaration: the run proceeds on the default image with no human step, the
  finding is never parked, `onReject` is not consulted and `runner-image-source` is `default`. The reason from the
  declaration section (the key, "patchy does not build images", and the two fixes) is recorded on `status.runnerImage`
  (`manifest` and `message`, no `image`) and posted as the sticky comment, so the owner learns why the file was not used
  and what to change. A repository that wants patchy to use its devcontainer image publishes it and sets `image`, or
  declares it in `.patchy/agent.yaml`.
- **Registry denies access**: 401/403 is a policy rejection (above) with a message naming the reference and the fix, not
  backoff.
- **Registry unreachable**: `Ready=False` / `RunnerImageResolveFailed`, backoff; the Finding waits; the artifact is not
  re-downloaded on retry.
- **Pull failure at pod start**: gated on the Job's `runner-image-source: repository` annotation, so default-image Jobs
  keep today's behaviour. While `Waiting` is set and a 2-minute grace has not elapsed, both `collect()` paths return
  `ctrl.Result{RequeueAfter: grace - elapsed}` (a pod in `ImagePullBackOff` never mutates the Job object, so the Job
  watch alone would never fire). After the grace, `InvalidImageName`, `CreateContainerConfigError`, and an
  `ErrImagePull` whose message says manifest unknown or not found are deterministic and fail the run `aborted` with
  `agent image pull failed: <reason>`; any other pull error keeps waiting for `activeDeadlineSeconds`.
- **Sandbox unenforced**: the probe exits 78; both `collect()` paths map that init exit code to `Failed` with
  `LastFailureReason` `SandboxUnenforced` (attempt not consumed), and the job controller trips an in-memory circuit
  breaker that refuses further repository-image Jobs until restart, logged and exposed as an OTel gauge.
  investigation-controller additionally refuses repository-image Jobs until its startup canary has passed.
- **Incompatible image**: preflight emits `image_incompatible`; same path to `Failed`.
- **Broker limit hit**: 429 carrying the `egress broker: per-pod limit` prefix; a cooperative run ends `budget_exceeded`
  (the CLI failure is mapped by `agentrun.stageOutcome`), a hostile one is cut off; the deadline still ends the Job.
- **Changeset rejected**: the remediation collector validates every changeset before any forge call against what a
  legitimate run always produces: the Repository's pinned `resolvedSHA` as its base, path length and shape (no `..`, no
  absolute paths, nothing under `.git/`, no NUL), and a `100644`/`100755`/`120000` mode with base64 content on every
  upsert. A repository-image run is also refused what a legitimate diff can contain: more than `--changeset-max-entries`
  (default 500) entries (upserts plus deletes), a control character in a path, and anything under `.github/workflows/**`
  or `.github/actions/**`, because a branch in the same repository triggers CI with repository secrets before any human
  has looked at it. Default-image runs keep those three as they were, so upgrading with the feature off changes nothing
  observable (a vendored dependency bump legitimately rewrites hundreds of files, and leaving workflows alone also
  avoids the 422 when the App lacks the `workflows` permission). A rejected changeset fails the attempt
  `changeset_rejected` with a `LastFailureReason` naming the limit or the path, and zero forge calls are made.
- **Ignore verdict on a repository image**: the Finding goes to `HandedOff`. The phase notice and the sticky comment
  read "the agent recommended ignore; patchy did not dismiss the alert because the run used a repository-declared
  image". Because dismissal write-back (`resolveSource` in `internal/controller/integration/project.go`) fires only on
  entry to `Dismissed` and with no human gate, a held `ignore` is never written back: the alert stays open in the
  scanner of record until a human resolves it there. A human `dismiss` verb that would route `HandedOff` to `Dismissed`
  through the existing write-back is deferred (Open questions).
- **Kill switch flipped mid-finding**: the next Job uses the default image, annotated so; the verdict-hold decision for
  an attempt already launched is unchanged because it reads the Investigation's stamp; documented as an emergency
  control.
- **source-controller restart**: artifact re-downloaded at the pinned SHA (the store's tarball index is in memory),
  manifest not re-read once pinned, image unchanged.

`onReject: handoff | default` (default `handoff`) ships together with a sticky tracking-issue comment
(`<!-- patchy:runner-image -->`, the existing `findSticky` mechanism in `internal/controller/integration/project.go`,
template `runner_image_comment.md.tmpl`) stating one of "ran on `<image>` declared in `<manifest>`" (the declaring file,
`.patchy/agent.yaml` or `.devcontainer/devcontainer.json`), "could not use `<ref>` from `<manifest>`: `<reason>`; the
default image was used" (a rejection), "`.devcontainer/devcontainer.json` was not used: `<reason>`; the default image
was used" (a not-applicable devcontainer, quoting the build/features reason verbatim), or the held-ignore sentence
above, so fallback is never silent and a repository owner without cluster access learns why. The comment and
`InvestigationSummary.RunnerImage` derive from the same `status.runnerImage` value the launch recorded.

## CRD, config and chart changes

`api/v1alpha1` (then `mise run codegen`; CI fails on drift):

- `repository_types.go`: a `RunnerImage` struct with `Declared`, `Manifest`, `Image` and `SearchPath` strings, a
  `Verified` bool, `Rejected` and `Message` strings and `ResolvedAt *metav1.Time`;
  `RepositoryStatus.RunnerImage *RunnerImage`; printcolumn `Image` on `.status.runnerImage.image`, priority 1; the stale
  ".git directory" claim on `Artifact` corrected. `Manifest` holds the repository-relative path of the declaring file,
  `.patchy/agent.yaml` or `.devcontainer/devcontainer.json`, and is empty when neither exists; it is the one record of
  which file won, so no separate source field is added.
- `common_types.go`: `RunnerImageRef struct { Image, Source, Manifest string }` (`Manifest` copied from the Repository
  at launch when `Source` is `repository`, so the sticky comment and `describe` name the file);
  `InvestigationStatus.RunnerImage`, `RemediationStatus.RunnerImage` and
  `InvestigationSummary.RunnerImage *RunnerImageRef` (the last for the projection).
- `conditions.go`: `ReasonRunnerImageResolving`, `ReasonRunnerImageRejected`, `ReasonRunnerImageResolveFailed`. No new
  condition type, no Forge or Integration field (`charts/patchy-config` untouched), no transition-table change.
- `internal/envelope`: `OutcomeImageIncompatible Outcome = "image_incompatible"` and
  `OutcomeChangesetRejected Outcome = "changeset_rejected"` (distinct from the agent-side `changeset_too_large`, which
  is the pod's own byte cap).

`internal/jobs`: `Spec.RunnerImage`, `Spec.RunnerSearchPath` (string), `Runner.Inject []string`,
`Config.AllowRepositoryImages bool`, `Config.EphemeralStorage` (request and limit), `Status.Waiting string`,
`Status.InitExitCode *int32`, `Status.RunnerImageSource string`; `Client.Create` returns the effective `RunnerImageRef`;
constants `volPatchyBin`, `patchyBinDir = "/patchy/bin"`, `ExitSandboxUnenforced = 78`, `annotationRunnerImage`,
`annotationRunnerImageSource`, `annotationToolsImage`, `labelRunnerImageSource`. `internal/agentrun`: `ConfigEnvKeys()`
(every `PATCHY_*` key `FromEnv` reads) and the 429-prefix mapping in `stageOutcome`.

Flags (`PATCHY_` prefix): source-controller `--repository-images` (bool), `--repository-image-registries` (comma list,
validated at parse), `--repository-image-max-bytes` (int64), `--repository-image-cosign-key-file` (path),
`--repository-image-allow-unsigned` (bool), `--repository-image-on-reject` (`handoff|default`); investigation- and
remediation-controller `--repository-images` (bool) and `--agent-ephemeral-storage` (quantity); remediation-controller
`--changeset-max-entries` (int, default 500); egress-broker `--requests-per-pod`, `--concurrent-per-pod`,
`--tokens-per-pod`, `--tokens-per-hour`, `--max-tokens-ceiling` (int), `--model-allowlist` (comma list),
`--beta-denylist` (comma list), `--max-anthropic-request-bytes` (int64), `--preauth-requests-per-second`,
`--preauth-burst`, `--token-reviews-per-second`.

Chart (`charts/patchy/values.yaml`):

```yaml
agent:
  resources:
    ephemeralStorage: 8Gi # quantity; request = limit, both containers
  repositoryImages:
    enabled: false # bool; the kill switch
    registries: [] # []string; host/path prefixes, validated, required when enabled
    maxBytes: 4Gi # quantity; compressed layers, per platform child
    cosignPublicKey: "" # PEM; required when enabled unless allowUnsigned
    allowUnsigned: false # bool; explicit opt-out of signature verification
    pullSecret: "" # dockerconfigjson Secret name, release namespace
    pullSecretData: "" # optional; renders the same Secret in agent.namespace
    onReject: handoff # handoff | default
egressBroker:
  limits:
    requestsPerPod: 2000
    concurrentPerPod: 4
    tokensPerPod: 0 # 0 = 25 x agent.remediate.manual.tokenBudget
    tokensPerHour: 0 # 0 = off; broker-global backstop
    maxTokensCeiling: 0 # 0 = off; the CLI sets max_tokens itself
    modelAllowlist: [] # empty = agent.modelAllowlist + the CLI helper models
    betaDenylist: [] # empty = the built-in default deny-list
    maxAnthropicRequestBytes: 2Mi
    preauthRequestsPerSecond: 50
    preauthBurst: 100
    tokenReviewsPerSecond: 20
```

`configmap.yaml` renders `PATCHY_REPOSITORY_IMAGES` into the source-, investigation- and remediation-controller data;
`PATCHY_REPOSITORY_IMAGE_REGISTRIES`, `_MAX_BYTES`, `_ON_REJECT`, `_ALLOW_UNSIGNED` and
`_COSIGN_KEY_FILE=/etc/patchy/repository-image/cosign.pub` into source-controller only; `PATCHY_AGENT_EPHEMERAL_STORAGE`
into both job controllers; `PATCHY_CHANGESET_MAX_ENTRIES` into remediation-controller. `deployments.yaml` mounts a
`<fullname>-repository-image-key` ConfigMap and, with `pullSecret`, the Secret at `DOCKER_CONFIG=/etc/patchy/registry`
with the `config.json` item mapping; `serviceaccounts.yaml` adds `imagePullSecrets` to `patchy-agent` and, with
`pullSecretData`, renders the Secret in `agent.namespace`; `egress-broker.yaml` passes the limit flags and the rendered
model allowlist; `_helpers.tpl` gains the four `fail` guards; `NOTES.txt` prints the CNI-enforcement warning, the
pull-through-cache warning, the unsigned-images warning and the two-namespace pull-secret note whenever the matching
condition holds. `deploy/kustomize/base` mirrors the ConfigMap keys with disabled defaults.

## Documentation changes

- New `docs/deployment/repository-images.md` ("Repository runner images"): repository owners first (the manifest, the
  devcontainer.json fallback with its precedence, the one honoured key, the ignored keys, the build/features
  not-applicable outcome and its two fixes, the image contract including no `VOLUME` and no reserved `ENV`, offline
  caches, uid 65532, a Go and a Node example, `cosign sign --key`, gating `patchy/**` branches in CI, the preflight, how
  outcomes appear on the issue including the held-ignore notice), then operators (the `agent.repositoryImages` block,
  allowlist precision and the pull-through-cache namespaces never to allowlist, the signing requirement and
  `allowUnsigned`, both signature forms and that `patchy mirror sign` output verifies, the size cap and ephemeral
  storage, the kill switch, `pullSecret` and the two-namespace rule versus cloud credentials, the `broadEgress` render
  failure and its brokered-only consequence under `mode: none`, `onReject: default` for estates of build-based
  devcontainers, the CNI-enforcement prerequisite with the EKS Auto Mode caveat and the probe, the DNS channel and
  Cilium `toFQDNs`, broker limits and the aggregate-spend formula, sizing for evaluations), then the security model
  (this threat model, shadowing defences, audit trail, why CLI-reported usage is untrusted). `zensical.toml`: Deployment
  group, after "Isolation model".
- `docs/design/repository-runner-images.md`: this document, under a new "Design" nav group.
- `docs/deployment/isolation.md`: new section "Repository-declared images" after "Pod security"; one sentence in "What
  leaves the pod" on forged events and changeset validation.
- `docs/how-it-works.md` section 4 "Repositories are pinned once": the image digest joins the SHA.
- `docs/configuration/source-controller.md` "Flags" and "Behavior"; `investigation-controller.md` "Agent Job flags" and
  "Verdict routing" (the held `ignore`); `remediation-controller.md` "Agent Job flags" and the changeset validator;
  `agent-runner.md` "The workspace, and how it got there" and "Credentials in the pod"; `egress-broker.md` "Flags",
  "Hardening posture" (the route surface, body checks, beta deny-list, pre-authentication), "Known limitations"
  (per-replica counters, unmetered routes); `evaluation-controller.md` one paragraph on shared broker limits;
  `docs/observability.md` the new counters and the circuit-breaker gauge.
- `docs/deployment/helm.md` "Agent sandbox" values table; `deploy/README.md`; `charts/patchy/README.md`; `README.md` one
  line; `DESIGN.md` isolation-model paragraph (hand-formatted, minimal); `AGENTS.md` and `CLAUDE.md` (jobs,
  source-controller, broker, mirror bullets); `internal/jobs/doc.go`, `internal/runnerimage/doc.go`,
  `internal/broker/doc.go`; the `Dockerfile.claude-agent-runner` header. `patchy describe finding` gains the image line,
  so `docs/cli/` and `completions/` regenerate via `mise run docs`.
  `mise run fmt-prose && mise run lint-prose && mise run docs-build` gate the lot.

## Implementation phases

Branch `feature/repository-runner-images`, one draft PR per phase, `make pr` green at each; phases 1 to 3 ship with no
observable change.

1. **Pure core.** `internal/runnerimage`: manifest extraction from a tar.gz stream (both files), the
   `.patchy/agent.yaml` parser, the JSONC pre-processor (`jsonc.go`) and the devcontainer.json judge with its build,
   features and bad-image not-applicable outcomes, the precedence function with its distinct not-applicable result,
   `Policy` entry validation and `Allow`, strict digest check, the PATH sanitizer with the runc default, the
   reserved-ENV check with prefixes and proxy names, the VOLUME check, `Resolver` interface. Tests: table tests (each
   rejection message; a valid `.patchy/agent.yaml` beside a build-based devcontainer yields the yaml image; an invalid
   yaml never falls through; comments and trailing commas in every position; `//` and `/*` inside strings and escaped
   quotes survive; an unterminated block comment is rejected) plus seeded property tests (normalization idempotence; no
   entry matches a reference on another host or a sibling path; a digest pin round-trips; the emitted PATH has no empty
   component and only absolute entries for any input; a fixture config carrying `PATCHY_CHANGESET_MAX_BYTES` is
   rejected; the pre-processor is the identity on comment-free JSON without trailing commas; for generated JSON with
   comments and trailing commas injected between tokens it decodes equal to the original; its output always has the
   input's length).
2. **API.** Types, reasons, printcolumn, `Artifact` comment fix, the two new outcomes; codegen; drift gate. Tests: the
   existing deepcopy suite.
3. **source-controller.** `artifact.Store.Open`, the ggcr resolver (single HEAD, digest-pinned reference, index
   enumeration, size per child, config, host-selected keychain, bundle and legacy signature verification), the two-phase
   status write, `RepositoryReconciler.Images`, pin-once guard, `stall(reason)`, flags, the digest-keyed cache. Tests:
   reconciler with a fake resolver and fixture tarballs (yaml only, devcontainer only with `manifest` recorded, both, a
   build-based devcontainer yielding the default image with the not-applicable reason recorded and no `Stalled`
   condition); an in-process ggcr registry that flips a tag between calls, asserting `status.runnerImage.image` is the
   object that was checked; a seeded property that the checked configs equal the runnable children; signature fixtures
   in both forms signed with a committed test key pair; a 401 fixture yielding `RunnerImageRejected`; `Open` round-trip.
4. **jobs + agent-runner.** The `Spec`/`Runner`/`Config` fields, `patchy-bin` volume and copy tail, the probe tail,
   absolute command, PATH join and the scrub env including the exported `PATCHY_*` keys, ephemeral storage, annotations
   and label, `Create`'s return value, the `Status` fields; `harness.Available` `PATCHY_BIN_DIR`, preflight, the
   429-prefix mapping. Tests: golden pod specs for the default (unchanged) and injected shapes in
   `jobs_test.go`/`brokered_test.go`, the injected golden showing the blanked keys and the probe; returned source equals
   the annotation for claude-with-Inject, codex-without-Inject and `AllowRepositoryImages=false`; preflight tests with a
   fake CLI; e2e untouched (the fake harness has no `Inject`).
5. **Controllers.** Gate forwards the Stalled message; both `launch()` copy the pinned image under the flag and record
   what `Create` returned; both `collect()` gate the pull fail-fast on the annotation, requeue through the grace, map
   exit 78 to `SandboxUnenforced` and trip the breaker; the startup canary; `route()` holds `ignore` on the
   Investigation's stamp; the remediation changeset validator (entry cap, path shape, CI paths for repository runs);
   revival on the default image; `runnercfg` sets `Inject`. Tests: fake-client reconciler tests per branch; the `route`
   table keyed on the stamp; a default-image Job in `ImagePullBackOff` is not failed while a repository-image Job with
   manifest-unknown is failed after the grace; an over-count changeset and a workflow-path changeset each yield zero
   forge calls; a webhook test that closing a held finding makes no resolver call.
6. **Broker enforcement.** Pre-authentication, the route surface per route with the path-borne model id, body buffering
   and the tool/file/MCP checks, the beta deny-list, token accounting across all usage fields with the cut-stream
   estimate, the hourly ceiling, the allowlist default, 429 with the fixed prefix, audit and OTel, flags. Tests: broker
   unit tests over the existing fake TokenReview and a fake upstream: a `web_fetch` body, an `mcp_servers` body, a
   `POST /v1/files` and a bedrock invoke of a non-allowlisted id each get 4xx before any upstream call; a streamed
   `message_start` with large `input_tokens` trips the cap before the second request; a mid-stream cut is charged; a
   token without the `pod-name` extra is rejected when limits are on; a malformed-token flood produces no TokenReview.
7. **Chart + kustomize.** Values, ConfigMap rendering, mounts with the `config.json` item mapping, `imagePullSecrets`
   and the optional agent-namespace Secret, the four `fail` guards (broad egress among them, with the message above),
   NOTES, broker flags and the rendered allowlist. Tests: `mise run helm-lint`; a render fixture per guard and per
   pull-secret case, the broad-egress fixture asserting the message names `agent.networkPolicy.broadEgress: never` and
   passing once it is set.
8. **Feedback, docs, verification.** `onReject`, the sticky comment and its template golden (goldens for a yaml source,
   a devcontainer source and a not-applicable devcontainer), CLI `describe` with the declaring file, every page above,
   and the live verification below.

### Verification and rollout

The first live target is decided (see Decisions): the `devthenet-labs/patchy-target` Go repository on the EKS cluster
`devthenet-dev` (us-east-1), running a golang-derived image hosted in ECR under
`377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/` (for example `.../patchy/go-agent-env`), with that prefix as the
only allowlist entry, resolution through the ECR keychain and pulls through node credentials, and the image signed with
the key whose public half is `cosignPublicKey`.

Prerequisite, **in progress**: NetworkPolicy enforcement on `devthenet-dev`, which EKS Auto Mode does not provide by
default, is being enabled in parallel. Until it is, the sandbox probe is expected to fail every repository-image Job
with `SandboxUnenforced`, which is itself the first check. The chart values for the cluster set
`agent.networkPolicy.broadEgress: never` (the fleet there is brokered claude only), and the render is confirmed to fail
with the message above before that value is set.

Checks, in order: the probe and startup canary trip the breaker before enforcement and pass after it; with
`.patchy/agent.yaml` naming the ECR image, `go test` runs in the remediation pod and the Job carries the annotations; a
branch of the target with only a `.devcontainer/devcontainer.json` `image` resolves with `manifest` recorded as that
file, and one with a `build`-based devcontainer runs on the default image with the not-applicable reason on
`status.runnerImage` and the tracking issue; a disallowed registry parks the finding with a readable reason and
`approve` revives it on the default image; a scripted caller inside a pod gets 4xx for a `web_fetch` request and 429
over the token limit. The feature stays `enabled: false` in every other environment until these pass.

## Security review notes

- The injected binaries are the only way the harness reaches the model; both are absolute-pathed and read-only mounted,
  and `agent-runner` is static. Shell rc hooks cannot reach them: `BASH_ENV`/`ENV`/`SHELLOPTS`/`PROMPT_COMMAND` are
  blanked and the runner execs the CLI without a shell. `claude` runs on the image's loader and libc, so it is not a
  trust boundary; the preflight is a compatibility check.
- Reserved-ENV rejection at resolve time is an early signal that an image was built to tamper; blanking in the pod,
  driven by the exported `PATCHY_*` key list, is the backstop for names the resolver did not know.
- `pullSecret` widens what the release namespace holds; node credentials on EKS can pull any ECR image in the account.
  Both make the allowlist's path precision load-bearing, which is why entries are validated rather than left to
  convention and why signatures are required by default.
- Holding `ignore` on the Investigation's stamp closes the forged-verdict channel; `manual` already hands off;
  `remediate` ends in a validated, human-reviewed PR pushed by the controller. Dismissal write-back to the scanner of
  record fires on entry to `Dismissed` with no human gate, so a held `ignore` is never written back and the alert stays
  open until a human resolves it in the scanner.
- The broker's limits are the enforced spend bound and its route surface is the enforced capability bound; the in-pod
  budget is advisory and the CLI's reported usage from a repository-image run is untrusted. The DNS channel to the
  cluster resolver exists today and is unchanged.
- Reading the manifest is a bounded tar walk over an already size-capped (1 GiB) artifact; the ENV, PATH and VOLUME
  checks read one config blob per platform child, not layers.
- The devcontainer.json fallback adds no principal: the file sits in the same pinned tree as `.patchy/agent.yaml`, and
  its `image` passes every check a yaml value does. The JSONC pre-processor runs on at most 64 KiB, allocates one buffer
  of the input's size, and never evaluates variables or executes anything the file names.

### Security review

Accepted findings of the 2026-09-22 review and where each one now lives:

- **Broker is an open proxy to the full upstream API (high).** Broker-side enforcement: a positive method and path
  surface per route with 404 elsewhere, the model id read from the path on bedrock/vertex/foundry, bodies buffered on
  every route under `MaxAnthropicRequestBytes` and rejected when they carry server-side tools, `mcp_servers`,
  `container` or file references, and a broker-owned `anthropic-beta` deny-list. The threat model says "the allowlisted
  surface of the broker".
- **Spend bound not enforced as written (high).** `TokensPerPod` over every usage field with a body-size estimate on a
  cut stream; `ModelAllowlist` defaulted from `agent.modelAllowlist` plus the helper models and required non-empty by
  the chart; tokens without the `pod-name` extra rejected when limits are on; a global `TokensPerHour`; `tokensPerPod`
  defaulted to 25 x the manual budget with per-pod totals on the audit line; the aggregate-spend formula and the
  unmetered-route caveat in the docs.
- **"Given an enforcing CNI" is a documented hope (high).** The `prepare` init's negative-connectivity probe on every
  repository-image Job (exit 78), `SandboxUnenforced` with a circuit breaker in both job controllers, the startup canary
  in investigation-controller, and the DNS channel stated as bounded only by a Cilium `toFQDNs` allowlist.
- **Dismissal write-back has no human gate; a held ignore is never written back (high).** The `holdDismissals` knob is
  gone: `route()` holds `ignore` on the Investigation's launch-time stamp; the notice and sticky comment say why; the
  notes state that the alert stays open in the scanner until a human resolves it there; a `dismiss` verb is deferred to
  Open questions.
- **Image-declared VOLUMEs become writable host-backed mounts (medium).** `internal/runnerimage` rejects a config with
  `Volumes`; the image contract says no `VOLUME`; `agent.resources.ephemeralStorage` bounds both containers; `maxBytes`
  is documented as compressed layers only.
- **A changeset of many tiny files exhausts the installation token's budget (medium).** The remediation collector
  validates every changeset before any forge call: `--changeset-max-entries` (500), path length and shape;
  `changeset_rejected` names the limit; the same validator hosts the CI-path deny.
- **"Cannot replace claude" and the preflight's integrity meaning are void (low).** Threat model and notes reworded:
  only `agent-runner` and the broker are trust boundaries; the preflight is a compatibility check; CLI-reported usage
  from repository runs is excluded from calibration and flagged, with the broker's totals as the record.
- **Tag-to-digest TOCTOU and multi-arch index handling (high).** Resolution steps 3 and 4: one HEAD, then only the
  `@sha256:` reference is passed on; `@sha256:` pins skip only the HEAD; index children enumerated by digest and each
  checked, identical sanitized PATHs required, the index digest recorded; the cosign payload digest must equal it; the
  cache is keyed by resolved digest.
- **Threat model omits the registry-path principal; a forged changeset runs CI with secrets (high).** Two new principals
  and the dropped "toolchain choice" sentence; `cosignPublicKey` required unless `allowUnsigned: true`; repository-image
  changesets touching `.github/workflows/**` or `.github/actions/**` rejected; repository owners told to gate
  `patchy/**` branches; a per-Forge path deny-list in Open questions.
- **pullSecret cannot work as specified (high).** The `config.json` item mapping under `DOCKER_CONFIG`, the
  agent-namespace Secret rendered from `pullSecretData` or the two-namespace rule documented and warned, the
  host-selected keychain with the ECR helper and `google.Keychain`, and 401/403 as a deterministic rejection.
- **Image ENV can set every unreserved `PATCHY_*`, `ANTHROPIC_*`/`CLAUDE_*` and proxy variable (medium).** Prefix
  rejection at resolve time plus the proxy names; `agentrun` exports the `PATCHY_*` keys it reads and `buildJob` blanks
  every one the Job does not set; the proxy variables join `reservedEnv`.
- **Allowlist precision left to convention (medium).** `Policy` validates entries at construction and the chart mirrors
  it: a path segment required, trailing `/`, segment-boundary matching, no globs, `index.docker.io` canonicalized; the
  pull-through-cache namespaces named in NOTES and the operator page.
- **Per-pod limits run after TokenReview; a token-header flood stalls every brokered Job (medium).** Pre-authentication:
  the syntactic token check, the per-source-IP bucket and concurrency cap, the global TokenReview limiter with a queue,
  failed authentications counted against the IP, an OTel counter.
- **PATH yields a trailing empty component when the config carries no PATH (medium).** The sanitizer substitutes the
  runc default, drops empty and relative entries, refuses an empty result, and the pod value is a join of non-empty
  entries.
- **launch() records the Repository's pin while buildJob decides injection (high).** `jobs.Client.Create` returns the
  effective image and source from the annotations; both `launch()` record that, `route()` reads it from the
  Investigation, and the sticky comment and summary derive from the same value.
- **Pull-failure fail-fast never fires from the Job watch and changes feature-off behaviour (medium).** Gated on the
  `runner-image-source: repository` annotation, requeued through the grace, only manifest-unknown, `InvalidImageName`
  and `CreateContainerConfigError` treated as deterministic; default-image Jobs unchanged.
- **Resolution before the single status update re-downloads the tarball; no artifact for revival; "new commit = new
  Finding" is wrong (medium).** The two-phase status write with `RunnerImageResolving`, `stall(reason)` retaining the
  artifact, and the revival path through `approve`/`retry` on the default image in Failure modes.
- **cosign verification targets the legacy .sig tag (medium).** The referrers bundle
  (`application/vnd.dev.sigstore.bundle.v0.3+json`, `messageDigest` equal to the recorded digest) verified first, the
  legacy tag second, fixtures in both forms, and the docs stating that `cosign sign --key` v3 output and
  `patchy mirror sign` output verify.
- **Broker per-pod limits also bound evaluation pods; a 429 ends a cooperative run as runtime_error (low).** The
  Evaluation Jobs section states the shared limits and the rejected exemption, the docs say to size for the longest
  evaluation, and the 429 envelope's fixed prefix is mapped to `budget_exceeded` by `agentrun.stageOutcome`.

## Decisions

Recorded 2026-09-22 and 2026-09-23. The user decided the first, third and fifth; the second was made on the user's
behalf, with the reasoning stated. The fourth is an amendment recorded the same day, after implementation began.

1. **devcontainer.json fallback is in v1.** `.patchy/agent.yaml` `image` wins; if the file is absent,
   `.devcontainer/devcontainer.json` is consulted and only its top-level string `image` is honoured, parsed as JSONC by
   a small pre-processor in `internal/runnerimage`. A devcontainer that uses `build`, `dockerComposeFile` or `features`
   is not honoured, with a specific reason that reaches the tracking issue (amended by decision 4: not applicable, not
   rejected), because patchy does not build images. Reason: many repositories already name their toolchain image there,
   so the fallback costs owners nothing; the yaml keeps precedence because it is the explicit, strict, agent-specific
   declaration.
2. **Broad egress fails the render.** With `agent.repositoryImages.enabled: true` and `patchy.broadEgress` resolving
   true, the chart `fail`s with the message under Operator policy, naming `agent.networkPolicy.broadEgress: never`.
   Reason: a NOTES warning is invisible in CI, a render failure is not. The cost, accepted: under `mode: none` a fleet
   using repository images is brokered-only.
3. **First live verification target**: `devthenet-labs/patchy-target` on `devthenet-dev` with a golang-derived image in
   `377946145366.dkr.ecr.us-east-1.amazonaws.com/patchy/`, after NetworkPolicy enforcement is enabled on that cluster
   (in progress); see Verification and rollout.
4. **A devcontainer.json patchy cannot honour is no declaration** (2026-09-22, amendment decided by the orchestrator
   after the document was written and applied in the phase 1 pull request). A `.devcontainer/devcontainer.json` that
   uses `build`, `dockerFile`, `dockerComposeFile` or `features`, has no string `image`, or cannot be parsed is not
   applicable: the default runner image runs, the outcome is recorded as informational with its reason (surfaced later
   as the sticky comment), and the finding is never parked. Only an explicit `.patchy/agent.yaml` that fails validation
   or policy is a rejection subject to `onReject`. `runnerimage.Declare` keeps the two apart in its result type
   (`OutcomeNotApplicable` is a value; a rejection is a `*Rejection` error). Reason: a devcontainer was not written for
   patchy, and parking every finding on repositories with build-based devcontainers would be a regression from today.

5. **Trimmed scope for the first release** (2026-09-23, decided by the user after a review of whether the design was
   over-built for a single-operator deployment where the operator also owns every watched repository). Phases 1 to 6
   ship as designed; they were written or in review when this was decided and their guardrails cost nothing when
   repository images are off. The remaining work is cut to what makes the feature easy to use: phase 5 as designed;
   phase 7 limited to the `agent.repositoryImages` values, the allowlist, `pullSecret`/`pullSecretData`, the broker
   limit flags and the broad-egress guard; phase 8 limited to the sticky tracking-issue comment for rejected,
   not-applicable and incompatible images, `onReject` defaulting to `default` (fall back to the default runner image
   rather than parking the finding), the declaring file in `patchy describe`, and the operator and repository-owner
   documentation. Review scales to risk: one reviewer per pull request, adversarial verification only for
   security-relevant or high-severity findings, and a live regression run on `devthenet-dev` after each merge wave.
   Deferred until a real need appears: the startup canary, per-Forge changeset path deny-lists, per-route usage parsing
   for bedrock, vertex and foundry, and any further hardening of signature verification. Reason: the parts that decide
   whether the feature is pleasant to use are the feedback and the fallback, not further supply-chain controls.
6. **Two additions for repository owners** (2026-09-23): a published base image,
   `ghcr.io/devthenet-labs/patchy/agent-base`, that already satisfies the sandbox's constraints (glibc, uid 65532,
   caches under writable paths, no reserved ENV or VOLUME), with example Go, Python and Node images under
   `examples/agent-images/`; and a `patchy image check <ref>` CLI command that runs the resolution checks and a local
   preflight against an image before it is pushed. Same-account ECR is the recommended registry: application CI pushes
   to `patchy/app-envs/<app>` through GitHub OIDC, source-controller resolves through an EKS Pod Identity scoped to that
   prefix, and nodes pull with their existing ECR permissions.

## Open questions

1. A human `dismiss` verb: a new custom verb in `internal/action`, the admission policy, status-server and the CLI, with
   the `HandedOff` to `Dismissed` edge routed through the existing `resolveSource` write-back. Deferred; until it exists
   a held `ignore` is resolved in the scanner of record by hand.
2. An operator per-Forge path deny-list for changesets (beyond the built-in CI paths), which touches
   `charts/patchy-config`.
3. Usage parsing on the bedrock, vertex and foundry routes, so `TokensPerPod` meters them and `activeDeadlineSeconds`
   stops being the only wall there.
