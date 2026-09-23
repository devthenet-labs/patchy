# Helm charts

The `patchy` chart (in-repo at `charts/patchy`) renders the full stack: the `patchy.bitwisemedia.uk` CRDs, five
singleton controller Deployments — each with its own ConfigMap, ServiceAccount, and NetworkPolicy — the two Services
(integration `:8080`, source `:9790`), and the agent namespace with its RBAC and sandbox policies. The companion
`patchy-config` chart (`charts/patchy-config`) renders the `Integration`/`Forge` custom resources — a separate chart
because Helm validates every manifest against the API server before applying anything, so the CRs cannot ride in the
same first install as the CRDs they depend on. Both are published to `oci://ghcr.io/devthenet-labs/patchy/charts/` on
every release; release-please stamps `version` and `appVersion` 1:1 with the app, and the default image tag is
`v<appVersion>` — chart `X.Y.Z` runs images `vX.Y.Z`.

```sh
helm install patchy oci://ghcr.io/devthenet-labs/patchy/charts/patchy \
  --version <X.Y.Z> --namespace patchy --create-namespace
helm install patchy-config oci://ghcr.io/devthenet-labs/patchy/charts/patchy-config \
  --version <X.Y.Z> --namespace patchy -f values.yaml
```

The chart requires Kubernetes ≥ 1.34 (the oldest line not yet end-of-life) and references (never creates) the
[two Secrets](../getting-started/install.md#create-the-secrets).

## Human access: the admission policy

Findings carry five custom RBAC verbs — `approve`, `retry`, `expedite`, `suspend`, `resume` — that let a grant say "this
developer may approve findings, and nothing else". The [status page](../status-ui.md) honours them on its own because it
writes as its ServiceAccount and asks first; the [CLI](../cli.md) writes as the user, so the API server authorizes it,
and RBAC has no notion of a field: `update` on findings grants the whole object.

`admissionPolicy.enabled` (default `true`) renders a `ValidatingAdmissionPolicy` that binds each human-writable spec
field to its own verb, inside the API server's admission chain — so it holds for the CLI, `kubectl edit`,
`kubectl patch`, server-side apply and raw `curl` alike.

```yaml
admissionPolicy:
  enabled: true
  # A controller of your own that legitimately writes Finding spec.
  # The chart's own components are exempted automatically, from their
  # resolved ServiceAccount names — so renaming the release or overriding
  # a serviceAccount.name cannot lock a controller out.
  extraExemptSubjects: []
```

Disabling it is a real reduction in privilege separation, not a cosmetic toggle: every custom verb becomes advisory, and
anyone holding `update` on findings can change any field, including forging an approval. The template refuses to render
on a cluster below 1.30 rather than silently omitting itself, because a missing policy looks exactly like a working one
until someone tests it.

## Values

Values are validated against `values.schema.json` — a typo'd or relocated key fails the install instead of being
silently ignored. Everything specific to one controller lives under that controller's top-level key; only genuinely
shared settings stay global.

### Global: images, CRDs

| Key                 | Default                         | Purpose                                                                                                             |
| ------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `image.repository`  | `ghcr.io/devthenet-labs/patchy` | Repository prefix (registry included); the binary name is appended                                                  |
| `image.tag`         | `""`                            | Empty = `v<appVersion>`                                                                                             |
| `image.pullPolicy`  | `IfNotPresent`                  |                                                                                                                     |
| `image.pullSecrets` | `[]`                            |                                                                                                                     |
| `crds.install`      | `true`                          | Render the CRDs as templates (so upgrades track schema changes)                                                     |
| `crds.keep`         | `true`                          | Stamp `helm.sh/resource-policy: keep` — uninstall never deletes the CRDs, or the FindingRollup statistics with them |
| `commonLabels`      | `{}`                            | Extra labels on every rendered object                                                                               |
| `commonAnnotations` | `{}`                            | Extra annotations on every rendered object, pods included (per-object annotations win)                              |

Per-component image overrides win key-by-key, and a `digest` pins over any tag: `<controller>.image` and
`agent.runners.<harness>.image` — the latter is the runner image the job controllers stamp into every Job that harness
runs (`PATCHY_<HARNESS>_AGENT_IMAGE`). Unlike kustomize, pinning a runner's digest here is one knob, not two.

### The pipeline switch-on: the `patchy-config` chart

The controllers idle until an `Integration` and a `Forge` exist. They install via the separate `patchy-config` chart —
into the **same namespace** as the patchy release, after it — whose `integrations` / `forges` list entries each render
one CR. Its `values.schema.json` embeds the CRD spec schemas (regenerated by `mise run codegen`), so a typo'd or
mistyped `spec` field fails the install client-side before the API server sees it. See the
[install page](../getting-started/install.md#switch-the-pipeline-on-the-patchy-config-chart) for a worked example and
`deploy/kustomize/base/crs.example.yaml` for the full field walkthrough. The referenced Secrets are yours to create;
`kubectl apply`-ing the CRs directly instead of using the chart works just as well.

### The webhook entry point

A provider has one webhook URL, so exposure is chart-level: `webhook.host` plus exactly one flavour, both fronting the
**integration-controller** — see [Webhook exposure](webhook.md).

| Key                                                   | Default    | Purpose                                                           |
| ----------------------------------------------------- | ---------- | ----------------------------------------------------------------- |
| `webhook.host`                                        | `""`       | The single external hostname (required when a flavour is enabled) |
| `webhook.ingress.{enabled,className,annotations,tls}` | `false`, … | Plain-Ingress flavour                                             |
| `webhook.httpRoute.{enabled,annotations,parentRefs}`  | `false`, … | Gateway API flavour; TLS is the Gateway's concern                 |

### Shared pipeline config

Each controller renders its own ConfigMap (consumed with `envFrom`): the shared `config.*` keys plus its own `config`
block; every key is the matching `PATCHY_*` variable minus the prefix. Any shared key can be repeated under
`<controller>.config` to override it for that controller alone.

| Key                            | Default | Purpose                                                                                 |
| ------------------------------ | ------- | --------------------------------------------------------------------------------------- |
| `config.logLevel`              | `warn`  | `debug`, `info`, `warn`, `error`                                                        |
| `config.maxAttempts`           | `2`     | Agent attempts per finding before it fails (both job controllers)                       |
| `config.priorityAgingInterval` | `24h`   | Wait per effective-priority point of aging boost                                        |
| `config.priorityAgingCap`      | `25`    | Maximum aging boost                                                                     |
| `config.extra`                 | `{}`    | Verbatim `PATCHY_*` keys for every controller; `<controller>.config.extra` wins over it |

The listen (`:8080`), health (`:8081`), and artifact (`:9790`) addresses are not values — the chart hardcodes them
everywhere they appear (env, container ports, probes, Services, NetworkPolicies).

### Per-controller blocks

`integrationController`, `sourceController`, `contextController`, `investigationController`, and `remediationController`
all share one shape: `image`, `config` (+ `config.extra`), `resources`, `podAnnotations`, `podLabels`, `nodeSelector`,
`tolerations`, `affinity`, `serviceAccount.{create,name,annotations}`, and `networkPolicy.create`. A
`service.{type,port,nodePort,annotations}` block exists only on the two controllers anything dials — integration
(`:8080`, fronted by `webhook.*`; `NodePort` covers the kind/dev flow) and source (`:9790`, in-cluster only by design).

The controller-specific `config` defaults:

| Key                                                          | Default  | Maps to                                             |
| ------------------------------------------------------------ | -------- | --------------------------------------------------- |
| `integrationController.config.accumulationWindow`            | `1h`     | `PATCHY_ACCUMULATION_WINDOW`                        |
| `investigationController.config.findingMinAge`               | `1h`     | `PATCHY_FINDING_MIN_AGE`                            |
| `investigationController.config.maxConcurrentInvestigations` | `3`      | `PATCHY_MAX_CONCURRENT_INVESTIGATIONS`              |
| `investigationController.config.confidenceThreshold`         | `"0.75"` | `PATCHY_CONFIDENCE_THRESHOLD`                       |
| `remediationController.config.maxConcurrentRemediations`     | `1`      | `PATCHY_MAX_CONCURRENT_REMEDIATIONS`                |
| `remediationController.config.findingTTL`                    | `336h`   | `PATCHY_FINDING_TTL` (`"0"` keeps findings forever) |

Everything else a binary binds (see the [configuration reference](../configuration/index.md)) is reachable through
`config.extra` / `<controller>.config.extra`.

### Agent sandbox

| Key                                      | Default                                               | Purpose                                                                                                                                                                                                               |
| ---------------------------------------- | ----------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.namespace`                        | `patchy-agents`                                       | Created by the chart with the `restricted` PSS labels                                                                                                                                                                 |
| `agent.createNamespace`                  | `true`                                                | Set `false` when the namespace is managed elsewhere                                                                                                                                                                   |
| `agent.serviceAccount`                   | `patchy-agent`                                        | The Job identity: no Role, token not mounted                                                                                                                                                                          |
| `agent.jobDeadline` / `agent.jobTTL`     | `1h` / `1h`                                           | `activeDeadlineSeconds` / `ttlSecondsAfterFinished`                                                                                                                                                                   |
| `agent.modelAllowlist`                   | `anthropic/claude-sonnet-5,anthropic/claude-opus-5`   | Canonical model ids the investigation may request for remediation                                                                                                                                                     |
| `agent.investigate.*`                    | `anthropic/claude-sonnet-5` / `15m` / `25` / `150000` | model/timeout/maxTurns/tokenBudget — **absolute** (harness derived from the model)                                                                                                                                    |
| `agent.remediate.*`                      | `anthropic/claude-sonnet-5` / `45m` / `80` / `400000` | Same shape; maxTurns/tokenBudget are **ceilings** the report's requests are clamped to                                                                                                                                |
| `agent.runners.<harness>`                | claude enabled; codex, copilot disabled               | Per-harness `enabled`/`image`; codex/copilot add `secret`/`secretKey`/`secretEnv`/`hosts`/`dnsPatterns`, claude a `provider` block instead (brokered)                                                                 |
| `agent.runners.claude.provider`          | `name: anthropic`                                     | Which model API the egress broker fronts: `anthropic`/`bedrock`/`vertex`/`foundry` + `region`/`regionPrefix`/`projectID`/`resource`/`modelMap`/`env`                                                                  |
| `egressBroker.*`                         | deploys when a claude runner is enabled               | The [egress credential broker](../configuration/egress-broker.md): `anthropicSecret`, `anthropicAuth` (`key`/`token`), `foundrySecret`, `serviceAccount.annotations` (workload identity), `networkPolicy.extraEgress` |
| `egressBroker.limits.*`                  | off                                                   | Per-pod and broker-wide spend and request limits — see [Egress broker limits](#egress-broker-limits)                                                                                                                  |
| `agent.repositoryImages.*`               | `enabled: false`                                      | Let a repository name the image its claude agent runs in — see [Repository runner images](#repository-runner-images)                                                                                                  |
| `agent.networkPolicy.create`             | `true`                                                | Default-deny both directions + DNS + artifact + broker + TCP-443-only egress                                                                                                                                          |
| `agent.networkPolicy.clusterCIDRs`       | RFC-1918 + link-local                                 | Cluster-internal ranges excluded from agent egress                                                                                                                                                                    |
| `agent.runners.<harness>.hosts`          | codex: `api.openai.com`; claude: none (brokered)      | Per-runner egress allowlist — deliberately **no** forge hosts                                                                                                                                                         |
| `agent.networkPolicy.clusterDNSPatterns` | `*.svc.cluster.local`                                 | Cilium only: cluster-local names every runner may resolve (for the artifact fetch)                                                                                                                                    |
| `agent.networkPolicy.mode`               | `auto`                                                | Hostname-egress dialect: `auto`/`none`/`cilium`/`gke`/`istio` — one policy per runner                                                                                                                                 |
| `agent.networkPolicy.broadEgress`        | `auto`                                                | Keep the base "443 to anywhere" rule: `auto`/`always`/`never` — see the warning below                                                                                                                                 |
| `agent.networkPolicy.cilium.enabled`     | `false`                                               | Deprecated alias for `mode: cilium`, honoured only while `mode` is `auto`                                                                                                                                             |
| `agent.networkPolicy.istio.enabled`      | `false`                                               | Deprecated alias for `mode: istio`, honoured only while `mode` is `auto`                                                                                                                                              |

`mode: auto` reads the cluster's API surface on every render against a live cluster — including every helm-controller
reconcile — and picks `gke` (GKE's `FQDNNetworkPolicy`, needs a cluster with `--enable-fqdn-network-policy`), `cilium`
(a real Cilium; never selected on GKE, where a `CiliumNetworkPolicy` enforces nothing) or `none`. It never selects
`istio`. An off-cluster `helm template` sees no cluster and renders `none`, so pin `mode` when you need a deterministic
render. Enabling both Cilium and Istio via the deprecated aliases fails the render — pick one.

Because network policies are **additive**, an FQDN allowlist alongside the base policy's "443 to anywhere" rule
constrains nothing; `broadEgress: auto` therefore drops that rule whenever a hostname mode is enforcing. See the
[isolation model](isolation.md#network-egress-the-floor-and-the-fence) for what each layer requires and what it doesn't
cover.

### Repository runner images

`agent.repositoryImages` lets a watched repository name the image its claude agent runs in (`.patchy/agent.yaml`, or the
top-level `image` of `.devcontainer/devcontainer.json`), so the agent has that repository's toolchain;
[Agent images](../integrations/agent-images.md) covers building one, and the
[design](../design/repository-runner-images.md) covers resolution, signing and the threat model. source-controller pins
the declared image to a digest once per Repository, after checking it against the policy below. The block is off by
default, and `enabled: false` renders none of it: that value is the kill switch, and the next Job runs the default
runner image without any change to the custom resources.

| Key                                          | Default      | Purpose                                                                                                                                                      |
| -------------------------------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agent.repositoryImages.enabled`             | `false`      | The kill switch; `PATCHY_REPOSITORY_IMAGES` on the source, investigation and remediation controllers                                                         |
| `agent.repositoryImages.registries`          | `[]`         | Registry path prefixes (`host/path/`) a declared image must sit under. **Required** when enabled; never a pull-through cache namespace                       |
| `agent.repositoryImages.cosignPublicKey`     | `""`         | PEM public key declared images must be cosign-signed with, rendered into a ConfigMap and mounted into source-controller. **Required** unless `allowUnsigned` |
| `agent.repositoryImages.allowUnsigned`       | `false`      | Admit unsigned images: an explicit opt-out of signature verification                                                                                         |
| `agent.repositoryImages.maxBytes`            | `4294967296` | Largest compressed layer total of one platform of a declared image, in bytes                                                                                 |
| `agent.repositoryImages.onReject`            | `default`    | A rejected declaration runs on the default runner image, recording why (`default`), or parks the finding for a human (`handoff`)                             |
| `agent.repositoryImages.ephemeralStorage`    | `""`         | Ephemeral-storage request and limit on both containers of every agent Job, such as `8Gi`. **Required** when enabled                                          |
| `agent.repositoryImages.changesetMaxEntries` | `500`        | Most files a changeset from a repository-image run may touch before remediation-controller rejects it                                                        |
| `agent.repositoryImages.pullSecret`          | `""`         | dockerconfigjson Secret in the release namespace that source-controller resolves images with; also listed in the agent ServiceAccount's `imagePullSecrets`   |
| `agent.repositoryImages.pullSecretData`      | `""`         | The `.dockerconfigjson` content (JSON, not base64); when set, the chart renders the `pullSecret` Secret into `agent.namespace` too                           |

```yaml
agent:
  networkPolicy:
    broadEgress: never # required under mode none or istio, see below
  repositoryImages:
    enabled: true
    registries:
      - 123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/
    cosignPublicKey: |
      -----BEGIN PUBLIC KEY-----
      ...
      -----END PUBLIC KEY-----
    ephemeralStorage: 8Gi
```

The render fails, with a message naming the value to set, when the block is enabled and:

- `registries` is empty, `ephemeralStorage` is empty, or `cosignPublicKey` is empty without `allowUnsigned: true` (each
  would otherwise stop a controller at startup);
- `cosignPublicKey` is set but lacks its `-----BEGIN PUBLIC KEY-----` / `-----END PUBLIC KEY-----` armour;
- `pullSecretData` is set without `pullSecret`, which names the Secret it renders;
- `agent.networkPolicy.create` is false, or `agent.networkPolicy.broadEgress` resolves to keeping the base policy's "TCP
  443 to anywhere" rule, which would let a hostile image reach a model API with a key of its own. Under `mode: none` or
  `istio` set `broadEgress: never`. That removes the rule for every runner, so codex and copilot lose their model egress
  and the fleet is brokered-claude only; under `cilium` or `gke`, `auto` already drops the rule.

Whether or not the block is enabled, the values schema refuses a `registries` entry that is not `host[:port]/path` with
at least one path segment and no tag, digest, glob, comma or whitespace, and an `ephemeralStorage` that is not an
unsigned quantity such as `8Gi` or `1.5Gi` (the controllers refuse either at startup).

The CNI must also enforce NetworkPolicy, which the chart cannot check (EKS Auto Mode does not by default): every
repository-image Job probes it before untrusted code runs and fails `SandboxUnenforced` when it is not enforced.

**Registry credentials.** For ECR, source-controller mints a registry token with its workload identity and nodes pull
with their own role, so no Secret is needed. Enabling the block adds the EKS Pod Identity agent (`169.254.170.23/32` and
`fd00:ec2::23/128`, TCP 80) to source-controller's NetworkPolicy, plus a `CiliumNetworkPolicy` granting the node-local
endpoint under `mode: cilium`, so a Pod Identity association on the source-controller ServiceAccount is all that
remains:

```sh
# The ServiceAccount is <fullname>-source-controller: patchy-source-controller for a release named patchy.
aws eks create-pod-identity-association --cluster-name <cluster> --namespace <release namespace> \
  --service-account patchy-source-controller --role-arn <role with ECR read on the allowlisted prefix>
```

IRSA works as well (annotate `sourceController.serviceAccount` with `eks.amazonaws.com/role-arn`). Artifact Registry
resolves through GKE Workload Identity on the same ServiceAccount; Dataplane V2 always admits the metadata server, and
on other CNIs `sourceController.networkPolicy.extraEgress` can add `169.254.169.254/32` on TCP 80. For any other
registry set `pullSecret`: source-controller mounts that Secret's `.dockerconfigjson` key as `config.json` under
`DOCKER_CONFIG`, and the agent ServiceAccount lists it in `imagePullSecrets`. The kubelet reads pull Secrets from the
pod's own namespace, so a Secret of the same name must also exist in `agent.namespace`: set `pullSecretData` to have the
chart render it there, or create it yourself.

### Model providers (brokered claude)

All claude model traffic goes through the [egress credential broker](../configuration/egress-broker.md), which deploys
automatically whenever a claude runner is enabled. `agent.runners.claude.provider` picks the API it fronts:

```yaml
# First-party Anthropic (the default): the API key lives with the broker in
# the RELEASE namespace. anthropicAuth: token sends a `claude setup-token`
# OAuth token as a bearer instead.
egressBroker:
  anthropicSecret: patchy-anthropic # key: api-key

# Amazon Bedrock: SigV4-signed with the broker's AWS identity (IRSA here).
agent:
  runners:
    claude:
      provider:
        name: bedrock
        region: us-east-1 # model ids derive as us.anthropic.<model>
egressBroker:
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/patchy-broker

# GCP Vertex AI: OAuth via GKE Workload Identity.
agent:
  runners:
    claude:
      provider:
        name: vertex
        region: europe-west1
        projectID: my-project
egressBroker:
  serviceAccount:
    annotations:
      iam.gke.io/gcp-service-account: patchy-broker@my-project.iam.gserviceaccount.com

# Microsoft Foundry: deployment names cannot be derived, so the model map is
# mandatory and validated at controller startup against the allowlist.
agent:
  runners:
    claude:
      provider:
        name: foundry
        resource: my-foundry
        modelMap:
          anthropic/claude-sonnet-5: my-sonnet-deployment
          anthropic/claude-opus-5: my-opus-deployment
egressBroker:
  foundrySecret: patchy-foundry # omit for Entra via Azure Workload Identity
```

Scope the cloud role to Invoke only: `bedrock:InvokeModel*`, `aiplatform.endpoints.predict`, Cognitive Services User.

!!! note "Migrating from a pre-broker release"

    The claude model credential moves: it used to be `patchy-anthropic` in the **agent** namespace, wired into the pod;
    now the broker is its only consumer and it lives in the **release** namespace. Create the Secret in the release
    namespace, upgrade the chart (the broker lands), then delete the old Secret from the agent namespace. The
    `agent.runners.claude.secret*` values are ignored (a NOTES warning fires if they are still set); if the old
    `secretEnv` was `CLAUDE_CODE_OAUTH_TOKEN`, set `egressBroker.anthropicAuth: token` so the broker sends the OAuth
    token as a bearer. Codex and copilot are unaffected.

### Egress broker limits

`egressBroker.limits` sizes what the broker enforces before any upstream call, one value per limit flag of the
[egress broker](../configuration/egress-broker.md). Every value defaults to the binary's own default (off, or the
built-in value where noted), and an unset value renders nothing, so upgrading changes nothing until one is set. Under a
repository-declared image the in-pod budget is advisory and these limits are the enforced bound on spend, so size them
before enabling `agent.repositoryImages`; the broker's audit line reports per-pod totals to size them from. Evaluation
Jobs run under the same per-pod limits.

| Key                                            | Default | Purpose                                                                                                         |
| ---------------------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------- |
| `egressBroker.limits.requestsPerPod`           | `0`     | Requests one agent pod may make in its lifetime; `0` is off                                                     |
| `egressBroker.limits.concurrentPerPod`         | `0`     | In-flight requests one agent pod may hold; `0` is off                                                           |
| `egressBroker.limits.concurrencyWait`          | `""`    | How long a request over `concurrentPerPod` waits for a slot; empty is the binary's 2s, negative refuses at once |
| `egressBroker.limits.tokensPerPod`             | `0`     | Tokens (input, cache creation, cache read, output) one pod may consume; `0` is off                              |
| `egressBroker.limits.tokensPerHour`            | `0`     | Broker-wide trailing-hour token ceiling; `0` is off                                                             |
| `egressBroker.limits.maxTokensCeiling`         | `0`     | Largest `max_tokens` a request may set; `0` is off                                                              |
| `egressBroker.limits.modelAllowlist`           | `[]`    | Model ids pods may name (the claude-haiku helper family is always admitted); empty admits every model           |
| `egressBroker.limits.betaDenylist`             | `[]`    | `anthropic-beta` glob patterns to strip; empty keeps the built-in list, `[none]` strips nothing                 |
| `egressBroker.limits.maxAnthropicRequestBytes` | `0`     | Largest buffered request body on the anthropic, vertex and foundry routes; `0` is the binary's 2 MiB            |
| `egressBroker.limits.maxRequestBytes`          | `0`     | Largest request body on the payload-signing bedrock route; `0` is the binary's 10 MiB                           |
| `egressBroker.limits.preauthRequestsPerSecond` | `0`     | Per-source-IP request rate admitted before authentication; `0` is off                                           |
| `egressBroker.limits.preauthBurst`             | `0`     | Per-source-IP burst and in-flight cap before authentication                                                     |
| `egressBroker.limits.tokenReviewsPerSecond`    | `0`     | Broker-wide TokenReview rate, with a short queue; `0` is off                                                    |

`egressBroker.config.extra` still wins over anything these render.

## Operational notes

!!! warning "Singletons by design"

    All five controllers are `replicas: 1` with `strategy: Recreate`; the leader-election Lease is insurance
    against a botched rollout, not a scaling mechanism. Do not scale the Deployments.

- `helm uninstall` deletes the agent namespace (killing any running agent Job) but — with `crds.keep` — never the CRDs,
  so the Findings and the all-time FindingRollup statistics survive a reinstall.
- Lint and render locally with `mise run helm-lint`.
- Chart and images carry build-provenance attestations:
  `gh attestation verify --owner devthenet-labs oci://ghcr.io/devthenet-labs/patchy/charts/patchy:X.Y.Z`.
