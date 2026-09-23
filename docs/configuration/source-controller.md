# source-controller

The `Forge` and `Repository` reconcilers, plus the artifact server. It validates forge credentials, resolves each
`Repository` to its covering `Forge`, pins the head SHA exactly once, downloads the forge's tarball archive at that SHA
(pure HTTP — no controller image carries a git binary), and serves it from an in-cluster artifact endpoint the agent
pods fetch **credential-lessly**: the URL carries an unguessable 128-bit id and the Job pins the sha256 digest.

```sh
source-controller serve --namespace patchy --artifact-addr :9790
```

## Flags

The [shared flags](index.md#shared-flags-all-five-controllers), plus:

| Flag                                 | Env                                       | Default              | Purpose                                                                                                |
| ------------------------------------ | ----------------------------------------- | -------------------- | ------------------------------------------------------------------------------------------------------ |
| `--artifact-addr`                    | `PATCHY_ARTIFACT_ADDR`                    | `:9790`              | Listen address of the artifact server                                                                  |
| `--artifact-base-url`                | `PATCHY_ARTIFACT_BASE_URL`                | in-cluster Service   | Base URL minted into Repository statuses for agent fetches                                             |
| `--artifact-dir`                     | `PATCHY_ARTIFACT_DIR`                     | `/data/artifacts`    | Directory the artifact tarballs are stored in                                                          |
| `--max-artifact-bytes`               | `PATCHY_MAX_ARTIFACT_BYTES`               | `1073741824` (1 GiB) | Largest repository tarball stored; larger repositories stall (`Stalled` condition)                     |
| `--repository-images`                | `PATCHY_REPOSITORY_IMAGES`                | `false`              | Read each tree's runner-image declaration and pin the image; the kill switch                           |
| `--repository-image-registries`      | `PATCHY_REPOSITORY_IMAGE_REGISTRIES`      | —                    | Comma-separated `host/path/` prefixes a declared image must sit under; required with the feature on    |
| `--repository-image-max-bytes`       | `PATCHY_REPOSITORY_IMAGE_MAX_BYTES`       | `4294967296` (4 GiB) | Largest compressed layer total per platform of a declared image                                        |
| `--repository-image-cosign-key-file` | `PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE` | —                    | PEM cosign public key every declared image must be signed with; required unless unsigned is allowed    |
| `--repository-image-allow-unsigned`  | `PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED`  | `false`              | Admit declared images without a signature (an explicit opt-out)                                        |
| `--repository-image-on-reject`       | `PATCHY_REPOSITORY_IMAGE_ON_REJECT`       | `default`            | What a rejected declaration does: `default` runs the default runner image, `handoff` parks the finding |

`--artifact-base-url` only needs setting when the Service name differs from the default
`http://patchy-source-controller.<namespace>.svc.cluster.local:<port>` — the deployments leave it unset.

The `--repository-image-*` flags take effect only with `--repository-images`; see
[repository runner images](#repository-runner-images). `--repository-image-registries` and a valid key file (or
`--repository-image-allow-unsigned`) are then required, and a malformed allowlist entry, key or
`--repository-image-on-reject` value fails startup rather than admitting an image.

## Behavior

- **Forge reconciler** — validates each Forge's referenced credential Secret on its `spec.interval` and maintains its
  `Ready` condition. Matching is host equality, then the optional `orgs` allowlist, then the optional repository-name
  regexes; the most-constrained matching Forge wins, and an ambiguous match stalls the Finding with
  `ForgeResolved: False` / `Ambiguous`.
- **Repository reconciler** — created by the investigation-controller's gate, a `Repository` is resolved to its Forge,
  its head SHA pinned **once**, and the tarball downloaded at exactly that commit. The pin is what guarantees
  investigation and remediation see the same code.
- **Artifact server** — serves the stored tarballs on `--artifact-addr`. The Deployment stores them in an `emptyDir`:
  they are reproducible from the pinned SHA, so a pod restart just re-downloads. The Service is in-cluster only by
  design, and the NetworkPolicies restrict the port to the agent namespace.

## Repository runner images

With `--repository-images`, a repository may name the image its agent runs in — `.patchy/agent.yaml`, or the top-level
`image` of `.devcontainer/devcontainer.json` — and source-controller is the one component that reads the declaration and
decides what becomes of it. The [repository-owner guide](../integrations/agent-images.md) covers the file formats and
the image contract; the [design](../design/repository-runner-images.md) the threat model. Turn the feature on together
with `--repository-images` on the [investigation](investigation-controller.md#agent-job-flags) and
[remediation](remediation-controller.md#agent-job-flags) controllers, which run the pinned image, and on the
[integration-controller](integration-controller.md#flags), which reports the outcome on the tracking issue.

- **Read from the pinned tree, once.** After the artifact is stored, the declaration is read out of that tarball (never
  a second forge call), so it is bound to the commit the agent works on. The image is resolved to a digest exactly once
  and recorded on `status.runnerImage` beside `resolvedSHA`; a pinned Repository is never re-resolved, so a moved tag
  cannot change the environment between the investigation and the remediation of one finding.
- **Checks, all against the digest.** The reference must sit under a `--repository-image-registries` entry; then one
  `HEAD` resolves a tag and every later call uses the digest: each `linux/amd64` and `linux/arm64` manifest's compressed
  layers against `--repository-image-max-bytes`, its platform, no `VOLUME`, no reserved `ENV` name, an absolute `PATH`
  (the same on every platform), and a signature by the `--repository-image-cosign-key-file` key, in either the sigstore
  bundle form (cosign v3, `patchy mirror sign`) or the legacy `.sig` tag. Verification is in-process: no cosign binary
  and no Fulcio or Rekor traffic.
- **Allowlist entries** are `host/path` prefixes with at least one path segment, matched on segment boundaries
  (`ghcr.io/acme/` never matches `ghcr.io/acme-evil/`), without globs, `index.docker.io` read as `docker.io`; a bad
  entry fails startup. Never allowlist a pull-through-cache namespace (`.../docker-hub/`, `.../ecr-public/`, an Artifact
  Registry `*-remote` repository): anyone can populate those paths.
- **Registry credentials** are chosen by host: ECR through the AWS default credential chain (IRSA or EKS Pod Identity),
  Artifact Registry and GCR through Application Default Credentials, everything else through the docker config under
  `DOCKER_CONFIG` (a mounted `dockerconfigjson` Secret), falling back to anonymous. A cloud credential failure is
  retried, never anonymous; a 401 or 403 from the registry is a rejection (`AccessDenied`), not a retry.
- **Status writes.** The artifact, SHA and forge are persisted first with `Ready=False` / `RunnerImageResolving`, so a
  registry outage costs no re-download; resolution then finishes with `Ready=True`. A registry that fails transiently
  (or does not answer within three minutes) leaves `Ready=False` / `RunnerImageResolveFailed` and backs off. Verdicts
  are cached for ten minutes by digest, never by tag.
- **Outcomes.** An accepted image records `declared`, `manifest` (the declaring file), `image` (`name@sha256:…`),
  `searchPath` and `verified`. A devcontainer.json patchy cannot honour (it builds its image, adds features, or names no
  usable `image`) is not a declaration: `manifest` and `message` record why, and the default runner image runs. A
  deterministic failure is a rejection: `rejected` carries the reason label and `message` the explanation (the guide's
  [troubleshooting table](../integrations/agent-images.md#troubleshooting) lists every label). Under
  `--repository-image-on-reject default` the Repository stays `Ready` and the finding runs on the default runner image;
  under `handoff` it is `Stalled` / `RunnerImageRejected`, the investigation gate parks the finding `HandedOff` with the
  message, and approving it runs the remediation on the default runner image.
