<!--
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
-->

# patchy Helm chart

Deploys the patchy stack: the `patchy.bitwisemedia.uk` CRDs, the five pipeline controllers (integration, source,
context, investigation, remediation), the egress broker and the status server into the release namespace, plus the agent
sandbox namespace, RBAC, ConfigMaps, Services, and NetworkPolicies. The intent, preview and evaluation controllers are
opt-in. It is the Helm rendering of [`deploy/kustomize`](../../deploy/kustomize) — same resources, same defaults, same
isolation model — published to OCI on every release.

```sh
helm install patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy \
    --version <X.Y.Z> --namespace patchy --create-namespace
```

The chart version tracks the app release 1:1, and the default image tag is `v<appVersion>` — installing chart `X.Y.Z`
runs images `vX.Y.Z`.

## Architecture

The custom resources are the state machine: findings, investigations, and remediations live as CRs in the release
namespace, and the Kubernetes API is the only state store (`kubectl get patchy -n <namespace>` shows the pipeline). The
five pipeline controllers split the work:

| Controller                   | Role                                                                                                                                                                                           |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **integration-controller**   | The single internet-facing entry point: provider webhook receivers on :8080 (GitHub: `POST /github/webhooks`), scanner-alert ingestion into Findings, tracking-issue projection, human signals |
| **source-controller**        | Forge/Repository reconcilers plus the artifact server on :9790 — SHA-pinned repository tarballs the agent pods fetch credential-lessly                                                         |
| **context-controller**       | The enhancer chain over Opened findings (the CMDB placeholder); no external access at all                                                                                                      |
| **investigation-controller** | The gate and the analysis scheduler: launches analysis agent Jobs, routes the verdict edges                                                                                                    |
| **remediation-controller**   | Queue admission, the remediation agent Jobs, push/PR, and the rollup/TTL loop                                                                                                                  |

All five run as singletons (`replicas: 1` + `Recreate`, with leader election as rollout insurance) and mount their
service-account tokens; [`templates/rbac.yaml`](templates/rbac.yaml) pins verb-by-verb what each identity may do. Of the
five, only two have Services — the integration-controller's :8080 and the source-controller's :9790; every other port is
a kubelet-probed :8081.

The CRDs render as templates gated by `crds.install` — living in `templates/crds/` rather than the chart's install-only
`crds/` directory means `helm upgrade` keeps them current — with `helm.sh/resource-policy: keep` stamped when
`crds.keep` is true so uninstall never deletes them — and with them the all-time FindingRollup statistics.

## Switching the pipeline on

The controllers idle until two custom resources exist: an **Integration** (where findings come from, where the tracking
issues go, webhook validation) and a **Forge** (how repositories are cloned and pushed). Those CRs live in the sibling
[`patchy-config`](../patchy-config) chart, installed **after** this one into the same namespace — Helm validates every
manifest against the API server before applying anything, so the CRs cannot ride in the same first install as the CRDs
they depend on:

```sh
helm install patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config \
    --version <X.Y.Z> --namespace patchy -f values.yaml
```

See the [`patchy-config` README](../patchy-config/README.md) for the values shape, or apply the CRs yourself with
`kubectl` — [`deploy/kustomize/base/crs.example.yaml`](../../deploy/kustomize/base/crs.example.yaml) is the full field
walkthrough (GHES base URLs, org allowlists, repository regexes).

## Secrets

Created out of band (SOPS, external-secrets, or `kubectl` for dev) — the chart references them and refuses to own them.
See [`deploy/kustomize/base/secrets.example.yaml`](../../deploy/kustomize/base/secrets.example.yaml) for shapes and
one-liners:

| Secret               | Namespace         | Keys                                                 | What                                                                                                                                                                                                                                                                                                                                                                                                                              |
| -------------------- | ----------------- | ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| e.g. `patchy-github` | release namespace | `appID` + `privateKey` (or `token`), `webhookSecret` | The forge/provider credential, named by each Integration/Forge CR's `spec.secretRef` — **not** mounted into any Deployment; read on demand through the API                                                                                                                                                                                                                                                                        |
| `patchy-anthropic`   | release namespace | `api-key`                                            | The claude runner's Anthropic credential, consumed ONLY by the egress broker — never an agent pod. An API key, or a `claude setup-token` OAuth token with `egressBroker.anthropicAuth: token`. Needed when the claude provider is `anthropic`; bedrock/vertex/foundry-entra use the broker's workload identity instead. **Migration:** it previously lived in `patchy-agents` — create it here, upgrade, then delete the old copy |
| `patchy-openai`      | `patchy-agents`   | `api-key`                                            | The codex runner's OpenAI API key. Only needed when `agent.runners.codex.enabled: true`; a ChatGPT-plan workspace token works too with `agent.runners.codex.secretEnv: CODEX_ACCESS_TOKEN` (or `CODEX_API_KEY`)                                                                                                                                                                                                                   |
| `patchy-copilot`     | `patchy-agents`   | `token`                                              | The copilot runner's **GitHub** token — not a model API key. Only needed when `agent.runners.copilot.enabled: true`; scope it to Copilot with no repository permissions                                                                                                                                                                                                                                                           |

One GitHub Secret may serve both CRs, or you can split read and write identities across two GitHub Apps. The provider
has exactly one webhook URL; point it at `https://<webhook.host>/github/webhooks` and enable one flavour of the chart's
entry point — `webhook.ingress` (plain Ingress, works anywhere) or `webhook.httpRoute` (Gateway API). Both front the
**integration-controller**, which validates each delivery against the matching Integration's `webhookSecret`. See the
[webhook exposure docs](../../docs/deployment/webhook.md) for details and per-platform (EKS, AKS, GKE) notes.

## Preview security foundation (opt-in)

`preview.enabled` defaults to `false`. Enabling it creates **empty** `patchy-preview-0` and `patchy-preview-1`
namespaces (or 1–4 slots), their isolation NetworkPolicies, quotas, limits, and fail-closed admission policies, plus a
non-default EKS Auto Mode `alb-preview` class. It does **not** create an Ingress, ALB, wildcard DNS record, preview
workload, Project, or preview-controller: that controller has its own default-off `previewController.enabled` switch.
Supply the ECR registry host, node-local DNS `/32`, ALB public subnet CIDRs, tightly scoped operator `/32` inbound
CIDRs, issued wildcard ACM certificate ARN, preview host suffix, and a distinct ALB name. Before enabling the
foundation, supply `preview.nodeIsolation.nodePool`, `nodeClass`, and `taintKey` from a dedicated EKS Auto Mode NodePool
whose NodeClass uses `networkPolicy: DefaultDeny` and whose taint is `<taintKey>=true:NoExecute`. `DefaultAllow` has a
start-up interval with unrestricted egress even when the slot NetworkPolicy exists. The chart renders that NodeClass
(with `DefaultDeny` fixed, not a value) and NodePool only with `preview.nodeIsolation.create: true` plus the node
`role`, `subnetIDs` and `securityGroupIDs` (see `values.yaml`); otherwise they must already exist. It never creates the
node IAM role. The [chart-render fixture](../../hack/testdata/chart-render/preview-foundation.yaml) shows the shape;
these are cluster-specific values, not defaults. The default `alb` class must already be limited to the patchy namespace
before slots are enabled; `edgeIngressClass.create` renders an edge class that is limited to the release namespace and
is never the default. On EKS Auto Mode set `clusterDNSCIDR` (or `preview.dnsCIDR`) to the node-local DNS `/32`: the slot
policy allows DNS only there.

Preview images are `<preview.imageRegistry>/<preview.imagePathPrefix>/<app>:sha-<full SHA>`. `preview.imagePathPrefix`
(default `patchy/previews`) is lowercase registry path segments with no leading or trailing slash, never empty. One
helper renders it into the slot admission policy and the preview-controller's `PATCHY_PREVIEW_IMAGE_PREFIX`, so with the
default both are byte for byte what they were when the path was fixed. It must be disjoint from the agent image
allowlist: the render fails when an `agent.repositoryImages.registries` entry equals the preview prefix, contains it or
sits under it, compared on path segment boundaries after folding case, an explicit `:443` and ECR's dual-stack and FIPS
endpoint names. A runtime image is built from an unreviewed pull request head, so it must never be admissible as an
agent sandbox image, nor a toolchain image as a preview. With repository images enabled, source-controller also refuses
any declared agent image under the preview prefix on its own (`PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES`).

The slot policy selects every pod. Inbound traffic can reach only a port named `http` from the configured ALB subnets;
outbound traffic can reach only the configured DNS IP on UDP/TCP 53. There is no API, broker, patchy Service, metadata,
Pod Identity, or internet exception. Admission matches the actual namespace name in CEL, not only the automatic
`kubernetes.io/metadata.name` label. An unselected workload in an ordinary namespace is unaffected; only the
outside-slot toleration and IngressClass rules below change its workload admission. Slot Ingresses must use
`alb-preview`, which is non-default and refused outside the slots. Slot Pods/Deployments must use immutable full-SHA
images one leaf beneath `<preview.imageRegistry>/<preview.imagePathPrefix>/` (`patchy/previews` by default), exactly one
container, the default ServiceAccount with token automount explicitly disabled, no init/ephemeral containers, and only
`emptyDir` volumes. Services must remain ClusterIP, and a Service may carry only a safe healthcheck-path annotation (a
multi-component Preview's per-component health check). Ingress hosts are single-label subdomains of
`preview.hostSuffix`; only a safe healthcheck-path annotation is allowed. An Ingress has one rule of at most four
`Prefix` paths in the component path grammar, each backed by a `preview-` Service on port 80; the kept placeholder backs
`/` with its own Service. The slot quota fits one Preview of up to four components: five Services (one each, plus slot
0's placeholder) and eight Pods. With `previewController.config.targetHealth: true` (the default, since previews run
only on EKS Auto Mode and Auto Mode was seen injecting the gate on a live preview) the slot namespaces are labelled
`eks.amazonaws.com/pod-readiness-gate-inject: enabled`, so EKS Auto Mode's load balancer injects a target-health
readiness gate into each slot Pod, and the preview-controller marks a Preview Ready only once its targets are healthy;
`false` restores the ungated Ready. See `docs/configuration/preview-controller.md`. The default was `false` up to
0.12.15, so an upgrade from there that never set it turns it on, and a Preview deploying during that upgrade may spend
one retry (the upgrade note in `docs/deployment/helm.md`).

Slot Pods and Deployment templates must also select the configured NodePool and NodeClass, tolerate the exact
`NoExecute` taint, use the default scheduler, and leave `nodeName` unset. A second fail-closed policy on **all other
namespaces** rejects both the named preview taint toleration and an empty-key `Exists` toleration (which would tolerate
every taint), plus direct `nodeName` placement on new Pods and Deployment templates. An ordinary scheduled Pod without
those tolerations is unaffected, including later metadata updates after its node is assigned. Keep both policies while
any preview node or slot workload exists; both policies have cluster-wide bindings and their match conditions use the
actual namespace name. Another name-based policy refuses `alb-preview` Ingresses outside slots. No workload should be
admitted until the NodePool and node IAM role are ready and a Deployment-managed first-instruction probe proves
isolation on a fresh node.

Every slot namespace **and every chart-owned guardrail** carries `helm.sh/resource-policy: keep`. Helm uninstall or
rollback to a revision without previews therefore orphans them rather than silently deleting an occupied slot or leaving
its workloads without admission/network controls. The slot policies still match every supported slot name (0–3) when
`slotCount` is reduced, while outside-slot policies deny new tolerations and `alb-preview` Ingresses in retired slots.
Retained per-slot bindings and NetworkPolicies remain until drained. The operator must not treat rollback as cleanup.
With `preview.enabled: true` and the default `preview.placeholder.enabled: true`, the chart also keeps a selectorless
`patchy-preview-placeholder` Service and Ingress in slot 0. The Service has no endpoints and schedules no Pod; the
Ingress keeps the separate `alb-preview` ALB and its DNS name stable between previews. The only slot resources allowed
Helm ownership/keep annotations are that exact Service and Ingress in slot 0. **Creating the placeholder starts ALB
charges even while `previewController.enabled: false`.** If re-enabling previews after a rollback that kept an older
slot admission policy, first upgrade with `preview.enabled: true` and `preview.placeholder.enabled: false` to update the
guardrails without creating an ALB. Verify the new policy admits the placeholder via a server-side dry run, then enable
the placeholder in a separate, approved Helm revision. Drain deliberately:

1. Disable new preview scheduling and wait for or delete the Preview CRs after their finalizers complete. Before
   removing the chart or namespace, inspect **each** slot with
   `kubectl get pods,deployments,replicasets,statefulsets,daemonsets,jobs,cronjobs,services,ingresses -n patchy-preview-0`
   (and `patchy-preview-1`, or every configured slot). Empty means no application objects, not merely no Ready pods.
   Investigate and delete leftovers explicitly.
2. Remove the wildcard DNS alias before deleting the kept placeholder Ingress and Service; then verify the preview ALB
   is gone. Remove any other preview Ingress using the infrastructure rollback plan. Only after the slots are empty,
   remove the kept namespace resources and their cluster-scoped `patchy-preview-*` admission policies/bindings and
   `alb-preview` class/params intentionally. Retain the guards if any workload remains. Helm will no longer manage kept
   resources after uninstall/rollback, so a later reinstall needs an explicit adoption/cleanup check.

The render gate checks the exact DNS-only egress, ALB source ingress, exact namespace selectors, and keep annotations.
The envtest gate applies the _rendered_ admission policies to a real API server and proves slot placement and
outside-slot toleration rejections plus an unaffected ordinary workload. NetworkPolicies require a real dataplane test
after rollout: run a disposable diagnostic **Deployment** using an approved immutable preview image on the dedicated
node. Probe forbidden endpoints at the first executable instruction, with no start-up delay; prove metadata, Pod
Identity, the Kubernetes API endpoint, every patchy Service (including the egress broker), and an external internet
address are unreachable. DNS may initially be denied by `DefaultDeny` until its allow rule is programmed, then must
resolve. Repeat cold starts and inspect the node agent for reconciliation errors. Do not claim this property from an
envtest API server, which has no kubelet or networking dataplane.

The repeatable [cold-start isolation probe](../../hack/preview-isolation-probe/README.md) uses a disposable, unmerged
demo-repo PR image, not the demo app's main image. Re-run it after every EKS, Auto Mode or VPC CNI upgrade and before
relying on previews again. The 2026-09-30 observed network result passed; direct Auto Mode policy-agent log inspection
remains an open validation gap. Never call those logs checked merely because Pod events are clean.

## Preview controller (opt-in, off by default)

`previewController.enabled` requires `preview.enabled`, `intentController.enabled`, and an exact Kubernetes Service
`/32` in `previewController.config.apiServerCIDR`. With it off, neither a Preview spec projector nor the
preview-controller runs. With it on, only Projects that have an operator-authored `spec.preview` block, or a `preview`
block on any of their `repositories` (a Project with several, which needs `intentController.config.multiRepo: true`),
get previews; a Project whose application must never be exposed (a deliberately vulnerable test target, say) has none.
The fixed renderer takes each component's runtime repository, HTTP port, readiness path and route path only from those
Project blocks, and the tag only from the recorded PR head, or for a repository the intent did not change, its
default-branch head recorded once when review began. It never reads issue/agent text as deployment configuration, and
holds no GitHub, registry, cloud or Secret credential. Its release-namespace Role is limited to Preview and Intent
reads/writes; one Role per fixed slot grants only Deployment, Service and Ingress CRUD and Pod/ReplicaSet reads. No
ClusterRole is installed. Its NetworkPolicy permits only DNS and the Kubernetes API Service `/32` out; there is no
internet or broker egress rule.

The controller serializes slot leases, queues by creation time, waits for a Ready Pod with a recorded image ID before
creating the `alb-preview` Ingress, and withdraws the old Ingress before a PR-head update. Each new head gets at most
three rollout attempts (10 minutes each by default). Failed and expired Previews free their slot only after all rendered
resources and Pod/ReplicaSet children are gone. An open PR's preview expires after 72 hours from its last successful
deployment; a new PR head can start a new preview. Deletion uses a finalizer, and a periodic orphan sweep cleans owned
resources even after a lost CR. **Do not reduce `slotCount` or disable the controller while a Preview owns a slot**:
restore the old slot count and drain via the Preview finalizer first. Helm's `keep` annotations protect the namespace
and guardrails but are not a substitute for that drain.

The chart renders the placeholder only when both `preview.enabled` and `preview.placeholder.enabled` are true. The
separate ALB and the wildcard DNS record are staged with the infrastructure, in order:
[Deploying intents and previews](../../docs/intents/deploying.md) walks through the stages and their rollback points.

## Agent isolation

The agent Jobs run in their own namespace (`agent.namespace`, created by the chart with the `restricted` Pod Security
labels; `helm uninstall` deletes it, killing any running agent Job). The isolation model, in order of load-bearing:

1. **Credential absence** — the agent pod holds no forge credential at all, not even in an init container, and a claude
   pod holds **no credential of any kind**: its model traffic goes through the egress credential broker (deployed with
   the chart whenever a claude runner is enabled), authenticated by an audience-bound projected ServiceAccount token —
   an identity document, not a capability. The repository arrives as a digest-verified tarball fetched from the
   source-controller's in-cluster artifact server (:9790); the only Secrets in a (non-brokered codex/copilot) pod are
   that harness's one model credential and the per-Job handoff markdown. The agent ServiceAccount has no Role and its
   API token is not mounted.
2. **NetworkPolicy** (`agent.networkPolicy.create`) — default-deny both directions, re-permitting only DNS, the artifact
   server, the broker, and TCP 443 externally (the non-brokered harness CLIs → their model APIs), with `clusterCIDRs`
   excluded. A claude pod's entire egress is cluster-local.
3. **Hostname policy** (defence in depth) — `agent.networkPolicy.mode` picks the dialect the cluster can actually
   enforce. Each renders **one policy per enabled runner**, selecting that harness's pods by their
   `patchy.bitwisemedia.uk/harness` label, so each reaches only its own model API (`agent.runners.<harness>.hosts` —
   `api.openai.com` for codex; claude has **no external hosts at all**, its Cilium policy being cluster-only and its
   GKE/Istio entries skipped) plus the in-cluster endpoints. No GitHub hosts, because the pod never talks to GitHub. The
   `copilot` runner is the one exception: its CLI exchanges its token at `api.github.com` before reaching a model, so
   that host and `*.githubcopilot.com` are in its allowlist — an authentication dependency, not forge access, and the
   runner disables the built-in GitHub MCP server so no tool in the session can spend the token against the API.

| `mode`   | renders                                     | requires                                                                     |
| -------- | ------------------------------------------- | ---------------------------------------------------------------------------- |
| `auto`   | whichever of the below the cluster supports | nothing — the default                                                        |
| `gke`    | `FQDNNetworkPolicy` (networking.gke.io)     | GKE Dataplane V2 created/updated with `--enable-fqdn-network-policy`         |
| `cilium` | `CiliumNetworkPolicy` with `toFQDNs`        | a real Cilium with DNS-based policy (the DNS proxy). **Not GKE** — see below |
| `istio`  | `Sidecar` (REGISTRY_ONLY) + `ServiceEntry`  | native sidecars (`ENABLE_NATIVE_SIDECARS=true`) + the Istio CNI node agent   |
| `none`   | nothing beyond the base NetworkPolicy       | —                                                                            |

**`auto` detection.** Helm resolves `.Capabilities` against the live cluster on every `install`/`upgrade` and every
helm-controller reconcile, so `auto` reads the API surface: `gke` when GKE's `FQDNNetworkPolicy` CRD is present (that
CRD only exists once `--enable-fqdn-network-policy` is on, which is what makes the signal trustworthy), `cilium` when a
real Cilium is, `none` otherwise. Two deliberate exclusions. It never selects `istio`, because CRD presence says nothing
about native sidecars, without which the agent Job never completes. And it never selects `cilium` on GKE, because
Dataplane V2 publishes cilium.io CRDs but has not honoured `CiliumNetworkPolicy` since 1.21.5-gke.1300 and rejects every
L7 rule — a CNP there enforces nothing while reading as protection in `kubectl get`. Setting `mode: cilium` explicitly
on GKE is trusted (a self-managed Cilium without Dataplane V2). An off-cluster `helm template` sees no cluster and
resolves to `none`; pin `mode` when you need a deterministic render.

**Why the base policy shrinks under an FQDN mode.** Network policies are **additive** — a packet matching _any_ policy
that selects the pod is allowed, and no policy can subtract from another. A `CiliumNetworkPolicy` or `FQDNNetworkPolicy`
naming `api.anthropic.com`, rendered next to a NetworkPolicy that already allows 443 to `0.0.0.0/0`, therefore
constrains nothing: the union is still "443 to anywhere". So `agent.networkPolicy.broadEgress: auto` drops that broad
rule under `cilium` and `gke`, and keeps it under `none` and `istio` (the sidecar enforces on SNI in a separate plane
and still wants the L3 floor beneath it). Under `cilium` the whole `-agents-egress` policy goes, since the CNP itself
grants DNS — through the proxy, which is what learns the resolved addresses — plus the artifact server and the model
API. Under `gke` it stays minus the 443 rule, because an `FQDNNetworkPolicy` is egress-only and can name neither a
resolver nor a ClusterIP Service. Set `broadEgress: always` to keep the broad rule while soaking a new mode: a mode that
turns out not to be enforcing then fails open rather than blackholing every runner. The namespace default-deny is never
dropped, so a missing CRD fails the sandbox closed.

## Values worth knowing

Per-controller blocks — `integrationController`, `sourceController`, `contextController`, `investigationController`,
`remediationController` — all share one shape:

- `<controller>.image` — key-by-key override of the global `image.*` prefix/tag; a `digest` pins that image.
- `<controller>.config` — the `PATCHY_*` keys that binary binds (integration: `accumulationWindow`; investigation:
  `findingMinAge`, `maxConcurrentInvestigations`, `confidenceThreshold`; remediation: `maxConcurrentRemediations`,
  `findingTTL`), rendered into a per-controller ConfigMap. `config.*` holds the shared keys (`logLevel`, `maxAttempts`,
  `priorityAgingInterval`, `priorityAgingCap`), each overridable per controller by repeating it under
  `<controller>.config`; `config.extra` and `<controller>.config.extra` render arbitrary `PATCHY_*` keys and win over
  anything the chart derives. Each pod template carries a `checksum/config` annotation of its own ConfigMap (plus
  `checksum/auth` for a chart-rendered auth Secret), so a `helm upgrade` that changes a component's configuration rolls
  exactly that component — no manual `kubectl rollout restart`.
- `<controller>.serviceAccount` / `networkPolicy` — that controller's identity and L3/L4 policy; `service` exists only
  on the two controllers anything dials (integration :8080, source :9790; NodePort covers the kind/dev flow).
- `<controller>.resources`, `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity` — per-controller
  pod tuning.

The genuinely shared settings stay global:

- `image.*` — repository prefix (registry included), tag (default `v<appVersion>`), pull policy, pull secrets.
- `webhook.*` — the single external entry point (`host`, plus one of `ingress` / `httpRoute`) in front of the
  integration-controller; a provider has one webhook URL, so exposure is a chart-level concern.
- `agent.*` — the sandbox: namespace, service account, `jobDeadline`/`jobTTL`, and the two stages' limits:
  `modelAllowlist` (canonical, provider-qualified model ids), `investigate.*` (absolute), `remediate.*` (`auto.*` is
  what an unattended fix gets and the line past which an estimate needs approval, `manual.*` the most an approval can
  grant; `model` is the fallback when the report's choice is off the allowlist).
- `agent.resources.*` — CPU and memory for the agent Jobs: `default` (every Job without a class; `{}`, the default, sets
  none) and `classes` (named sizes a Project picks per repository with `agentResourceClass`, for its intent builds,
  revise and check-fix rounds; the largest is the most any agent Job can request). Each key renders only when set. See
  [Sizing agents](../../docs/intents/deploying.md#sizing-agents) in the docs.
- `agent.runners.<harness>` — the per-harness runner fleet: `enabled`, the runner `image` (default
  `<prefix>/<harness>-agent-runner`; pinning its digest is one knob, unlike kustomize's two), and — for the non-brokered
  codex/copilot — the credential `secret`/`secretKey`/`secretEnv` and egress `hosts`/`dnsPatterns`. The claude runner is
  brokered and carries a `provider` block instead (`name`, `region`, `regionPrefix`, `projectID`, `resource`,
  `modelMap`, `env`) selecting which API the egress broker fronts. A non-brokered harness is enabled only when its
  runner is enabled and its credential exists; claude is enabled by configuration alone. The model chosen for a stage
  decides which runner (image + credential channel + egress policy) the Job runs, so an OpenAI model routes to `codex`
  and an Anthropic model to `claude`. `copilot` brokers both vendors, so it can run any model in the registry and is the
  fallback when a model's own harness is not enabled — never the preferred one.
- `egressBroker.*` — the egress credential broker (deployed exactly when a claude runner is enabled): the credential
  Secrets (`anthropicSecret` + `anthropicAuth`, `foundrySecret`), `ssePingInterval`, and — for bedrock/vertex/entra —
  the `serviceAccount.annotations`/`podLabels` workload-identity attachment point, with `networkPolicy.extraEgress` for
  the cloud metadata side channels.
- `egressBroker.limits.*` — what the broker enforces per pod and broker-wide before any upstream call: `requestsPerPod`,
  `concurrentPerPod` + `concurrencyWait`, `tokensPerPod`, `tokensPerHour`, `maxTokensCeiling`, `modelAllowlist`,
  `betaDenylist`, `maxAnthropicRequestBytes`, `maxRequestBytes`, and the pre-authentication `preauthRequestsPerSecond`,
  `preauthBurst` and `tokenReviewsPerSecond`. Each defaults to the binary's own default (off, or its built-in value) and
  renders only when set; `egressBroker.config.extra` still wins. Size them before enabling repository images, under
  which the in-pod budget is advisory.
- `agent.repositoryImages.*` — off by default: lets a watched repository name the image its claude agent runs in
  (`.patchy/agent.yaml`, or the `image` of `.devcontainer/devcontainer.json`). `registries` (the allowlist of
  `host/path/` prefixes), `cosignPublicKey` (or `allowUnsigned: true`) and `ephemeralStorage` (the disk wall on every
  agent Job) are required when `enabled`; `maxBytes`, `onReject` (`default` runs a rejected declaration on the default
  image, `handoff` parks the finding), `changesetMaxEntries`, and `pullSecret`/`pullSecretData` (a dockerconfigjson
  Secret for registries other than ECR or Artifact Registry; the kubelet needs a same-named copy in `agent.namespace`,
  and `pullSecretData` renders both) tune it. Enabling it fails the render while the agent egress is broad — set
  `agent.networkPolicy.broadEgress: never` under `mode: none` or `istio` — and adds the EKS Pod Identity agent to
  source-controller's NetworkPolicy, so an ECR allowlist needs only a Pod Identity association on the source-controller
  ServiceAccount. See [Helm charts](../../docs/deployment/helm.md#repository-runner-images) in the docs.
- `agent.networkPolicy.*` — the sandbox policies (above).
- `commonLabels` / `commonAnnotations` — stamped on every object the chart renders (annotations reach the pods too;
  per-object annotations win key-by-key).
- `crds.install` / `crds.keep` — CRD lifecycle.

Do not scale the controllers: all five are singletons by construction, so the Deployments hardcode `replicas: 1` with
`strategy: Recreate`; the leader-election Lease is insurance against a botched rollout, not a scaling mechanism.

## Publishing

`charts/patchy` (and the sibling `charts/patchy-config`) is packaged and pushed to
`oci://ghcr.io/devthenet-labs/patchy/charts` by [`.github/workflows/helm.yaml`](../../.github/workflows/helm.yaml) when
a release is published; release-please stamps `version`/`appVersion` in each `Chart.yaml` as part of the release PR.
Lint locally with `mise run helm-lint`.
