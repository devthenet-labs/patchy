# egress-broker

The egress credential broker: a reconciler-less reverse proxy that agent pods send **all** claude model traffic through.
It is what makes claude agent pods fully credential-less — the pod carries only an audience-bound projected
ServiceAccount token, and the broker injects or signs the real model credential outbound. It also makes 3rd-party model
providers work: Amazon Bedrock, GCP Vertex AI, and Microsoft Foundry each get a route the claude CLI's documented
gateway mode points at, with the broker holding the cloud identity that authenticates the provider-native requests.

There is no in-pod credential mode for claude — claude-without-broker is not a configuration. The codex and copilot
runners (both disabled by default) keep their in-pod Secret channels; brokering them is future work.

## How a request flows

```text
agent pod (claude CLI)                 egress-broker (release ns)              upstream
  ANTHROPIC_BEDROCK_BASE_URL=            1. TokenReview(X-Patchy-Broker-Token)
    http://…-egress-broker:8080/bedrock  2. audit line (pod, route, path,        bedrock-runtime.<region>.amazonaws.com
  CLAUDE_CODE_SKIP_BEDROCK_AUTH=1           status, duration — never bodies)     api.anthropic.com
  X-Patchy-Broker-Token: <projected      3. strip caller headers; SigV4-sign /   <region>-aiplatform.googleapis.com
    ServiceAccount token>                   inject key / attach OAuth token      <resource>.services.ai.azure.com
                                         4. stream the response back, flushing
                                            every write; SSE keep-alive pings
```

The caller token is validated via `TokenReview` (audience `patchy-egress-broker`; only
`system:serviceaccount:<agent-ns>:<agent-sa>` is accepted), its verdict cached briefly (`--verdict-ttl`, 1m) so the API
server sees one review per token, not one per request, and the header is stripped before anything is forwarded. The
audit identity — the calling pod's name — comes from the pod-bound token's claims.

## Routes and credentials

A route exists exactly when its identifying flag is set; at least one is required.

| Route        | Enable with                            | Upstream (default)                               | Credential                                                                                                                                                                                                      |
| ------------ | -------------------------------------- | ------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/anthropic` | `--anthropic-api-key-file`             | `https://api.anthropic.com`                      | The mounted Secret file, read per request so rotation propagates without a restart; `--anthropic-auth key` injects it as `x-api-key`, `token` as an `Authorization` bearer (a `claude setup-token` OAuth token) |
| `/bedrock`   | `--bedrock-region`                     | `https://bedrock-runtime.<region>.amazonaws.com` | SigV4, signed with the broker's ambient AWS identity (IRSA / EKS Pod Identity / env credentials)                                                                                                                |
| `/vertex`    | `--vertex-region` + `--vertex-project` | `https://<region>-aiplatform.googleapis.com`     | OAuth bearer from Application Default Credentials (GKE Workload Identity in-cluster), cached/refreshed; only requests naming that project and location (the region) are admitted                                |
| `/foundry`   | `--foundry-resource`                   | `https://<resource>.services.ai.azure.com`       | `--foundry-auth key`: `x-api-key` from a file; `entra`: a cached, auto-refreshed Entra bearer                                                                                                                   |

Cross-cutting behavior: responses stream back with an immediate flush per write; when an event-stream goes quiet for
`--sse-ping-interval` (30s) the broker injects a spec-legal `: ping` comment frame so intermediaries do not idle-close a
long thinking pause; the Bedrock route buffers request bodies (SigV4 signs a payload hash) up to `--max-request-bytes`
(10 MiB). Errors are emitted as Anthropic-style JSON error envelopes.

`/healthz` is liveness; `/readyz` additionally requires every configured route's credential source to be usable — a
missing Anthropic key file or unresolvable cloud credentials fail readiness. This replaces the job controllers' old
claude-Secret probing as "is the model credential present": the controllers enable the claude harness by configuration
and probe `readyz` advisorily at startup.

## Flags

As everywhere, each flag is also the matching `PATCHY_*` environment variable (`--tokens-per-pod` is
`PATCHY_TOKENS_PER_POD`).

| Flag                            | Default                | Purpose                                                                                              |
| ------------------------------- | ---------------------- | ---------------------------------------------------------------------------------------------------- |
| `--listen-addr`                 | `:8080`                | The proxy listener                                                                                   |
| `--health-addr`                 | `:8081`                | `/healthz` and `/readyz`                                                                             |
| `--token-audience`              | `patchy-egress-broker` | Audience callers' projected tokens must be bound to (the job controllers' `--broker-token-audience`) |
| `--verdict-ttl`                 | `1m`                   | How long one caller token's TokenReview verdict is cached                                            |
| `--sse-ping-interval`           | `30s`                  | Idle keep-alive period for event-stream responses; negative disables ping injection                  |
| `--agent-namespace`             | `patchy-agents`        | Namespace whose agent ServiceAccount callers must be                                                 |
| `--agent-service-account`       | `patchy-agent`         | The only ServiceAccount the broker answers to                                                        |
| `--max-request-bytes`           | `10485760` (10 MiB)    | Largest request body the payload-signing route (bedrock) accepts                                     |
| `--max-anthropic-request-bytes` | `2097152` (2 MiB)      | Largest request body every other route accepts; bodies are buffered so they can be inspected         |

The route flags are in [Routes and credentials](#routes-and-credentials): `--anthropic-api-key-file`, `--anthropic-auth`
(`key`), `--anthropic-base-url` (`https://api.anthropic.com`), `--bedrock-region`, `--bedrock-base-url`,
`--vertex-region`, `--vertex-project` (the only GCP project the vertex route admits; required with `--vertex-region`),
`--vertex-base-url`, `--foundry-resource`, `--foundry-base-url`, `--foundry-auth` (`key`) and `--foundry-api-key-file`.

### Limit flags

Every limit is off at `0` (or empty), so an unconfigured broker behaves as it always has; the Helm chart sizes them.

| Flag                            | Default | Purpose                                                                                                                                                                                         |
| ------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--requests-per-pod`            | `0`     | Requests one agent pod may make in its lifetime                                                                                                                                                 |
| `--concurrent-per-pod`          | `0`     | In-flight requests one agent pod may hold                                                                                                                                                       |
| `--concurrency-wait`            | `2s`    | How long a request over `--concurrent-per-pod` waits for one of the pod's slots before it is refused; `0` takes the default, negative refuses at once                                           |
| `--tokens-per-pod`              | `0`     | Tokens (input, cache creation, cache read and output) one agent pod may consume                                                                                                                 |
| `--tokens-per-hour`             | `0`     | Broker-wide trailing-hour token ceiling across every pod                                                                                                                                        |
| `--max-tokens-ceiling`          | `0`     | Largest `max_tokens` a request may set                                                                                                                                                          |
| `--model-allowlist`             | empty   | Comma-separated model ids pods may name (canonical or wire form; dated variants and the `claude-haiku` helper family are admitted); empty admits every model                                    |
| `--beta-denylist`               | empty   | Comma-separated `anthropic-beta` glob patterns to strip; empty uses the built-in list (`mcp-client-*`, `web-fetch-*`, `code-execution-*`, `files-api-*`, `context-1m-*`), `none` strips nothing |
| `--preauth-requests-per-second` | `0`     | Per-source-IP request rate admitted before authentication                                                                                                                                       |
| `--preauth-burst`               | `0`     | Per-source-IP burst and in-flight cap before authentication                                                                                                                                     |
| `--token-reviews-per-second`    | `0`     | Broker-wide TokenReview rate, with a short queue                                                                                                                                                |

## Enforcement

Under a [repository-declared agent image](../integrations/agent-images.md) the agent pod runs code the repository
controls, and the caller token is readable by anything in the pod, so the in-pod budget is advisory. The broker is the
enforcement point for what a pod may ask of the model API and how much, in layers that all run before any upstream
contact:

1. **Pre-authentication.** A per-source-IP token bucket and in-flight cap (`--preauth-*`), then a syntactic check of the
   token: three base64url segments, an `aud` naming the broker's audience, an unexpired `exp`, and a `kubernetes.io.pod`
   claim. **Callers must present pod-bound tokens** — the projected ServiceAccount token the kubelet mounts into an
   agent pod; a token minted with `kubectl create token` or the TokenRequest API without a pod binding is refused here,
   before any TokenReview. A failed authentication counts against the source IP.
2. **Authentication.** The TokenReview, its verdict cached for `--verdict-ttl` and rate-limited broker-wide
   (`--token-reviews-per-second`), so a pod varying its token header degrades only itself. With any per-pod limit set, a
   review without the `authentication.kubernetes.io/pod-name` extra is refused rather than counted in an anonymous
   bucket.
3. **Route surface.** Each route forwards a positive list of methods and paths and answers 404 to everything else, so
   the Files, Batches and other upstream APIs are unreachable: `POST /v1/messages`, `POST /v1/messages/count_tokens` and
   `GET /v1/models` on anthropic; `invoke` and `invoke-with-response-stream` on bedrock; `rawPredict` and
   `streamRawPredict` in the configured project and location on vertex; the messages API on foundry. On bedrock and
   vertex the model id is read from the path and checked against `--model-allowlist` there.
4. **Body inspection.** Every body is buffered (`--max-anthropic-request-bytes`, 2 MiB, on every route but bedrock,
   which keeps `--max-request-bytes`) and refused when it asks for a server-side tool that reaches the internet or a
   container (`web_search_*`, `web_fetch_*`, `code_execution_*`, `mcp_toolset`), carries `mcp_servers`, `container`, a
   `file_id` or a `source.type: file` block, names a model off the allowlist, or sets `max_tokens` above
   `--max-tokens-ceiling`. Denied `anthropic-beta` entries are stripped. **The 2 MiB cap can refuse a legitimate request
   with 413** when the agent's context carries many images (screenshots, diagrams); raise
   `--max-anthropic-request-bytes` if runs fail that way.
5. **Spend.** Per-pod request, concurrency and token counters and the broker-wide hourly ceiling are checked before
   proxying. Tokens are charged from the usage every response reports (`message_start` and `message_delta` on a stream,
   `usage` on a JSON body), as it streams, so a stream cut after `message_start` has already paid for its input. A
   response whose usage never becomes final — a cut stream, a usage-less body, or any body the broker cannot parse — is
   charged its worst case: the larger of the reported input and a request-size estimate (body bytes / 4), plus the
   larger of the reported output and `max_tokens` (else the ceiling, else bytes streamed / 4). **Bedrock event streams
   (`invoke-with-response-stream`) are binary and never parsed, so every such response is charged the request estimate
   plus its `max_tokens`**, which overstates a normal run's spend; size `--tokens-per-pod` for it on bedrock.

Over any limit the broker answers 429 in the Anthropic error envelope, with a message starting
`egress broker: per-pod limit`, which `agent-runner` maps to the `budget_exceeded` outcome. The audit line carries the
pod's running totals, so the limits can be sized from observed runs; the `patchy.broker.tokens` and
`patchy.broker.preauth.rejections` counters carry the same figures. Aggregate spend is bounded by
`max concurrent Jobs x --tokens-per-pod x Job turnover`, and by `--tokens-per-hour` when set. Evaluation pods run under
the same ServiceAccount and share the per-pod limits, so size them for the longest legitimate evaluation too.

## The controller side

The job controllers (and the evaluation controller, via the shared flags) point claude runners at the broker with
`--broker-url` — required whenever a claude runner is configured — and describe the provider with `--claude-provider`,
`--claude-provider-region`, `--claude-provider-region-prefix`, `--claude-provider-project-id`, `--claude-model-map`, and
`--claude-provider-env`. Canonical model ids stay canonical everywhere controller-side; the provider-specific
translation (Bedrock `us.anthropic.<model>` inference profiles, Vertex bare ids, Foundry deployment names) is rendered
into `PATCHY_MODEL_MAP` and applied in-pod. Bedrock and Vertex ids are derived per registry model; **Foundry has no
derivable ids**, so `--claude-model-map` must cover every claude-resolving allowlisted model and stage default — the
controllers refuse to start otherwise, which turns a would-be mid-run failure into a startup error.

## Workload identity

For bedrock/vertex/foundry-entra the broker's ServiceAccount is the cloud-identity attachment point, the same recipe as
the context-controller enhancers:

- **AWS (Bedrock)** — IRSA: annotate with `eks.amazonaws.com/role-arn`; or associate EKS Pod Identity and allow the
  agent's link-local endpoint (`169.254.170.23:80`) in `egressBroker.networkPolicy.extraEgress`. Scope the role to
  `bedrock:InvokeModel*`.
- **GCP (Vertex)** — GKE Workload Identity: annotate with `iam.gke.io/gcp-service-account`; the metadata server
  (`169.254.169.254:80`) may need an `extraEgress` rule on clusters that police link-local egress. Grant
  `aiplatform.endpoints.predict` (roles/aiplatform.user).
- **Azure (Foundry, Entra mode)** — Workload Identity: annotate with `azure.workload.identity/client-id` and add the
  `azure.workload.identity/use: "true"` pod label via `egressBroker.podLabels`. Grant Cognitive Services User.

Invoke-class permissions only — the broker needs to call models, never to manage them.

## Hardening posture

- RBAC: `create tokenreviews` is the broker's entire Kubernetes surface — no Secret API access (credential files are
  mounted volumes), no writes, no leases.
- The Deployment runs under the restricted Pod Security posture like every other patchy component: non-root 65532,
  read-only root filesystem, no capabilities.
- Its NetworkPolicy admits the proxy port from the agent namespace only, and egress to DNS, TCP 443, and the API server.
- Every request produces one audit log line — caller pod, route, method, path, status, duration, bytes — and never
  bodies or headers.
- In-cluster traffic to the broker is plaintext HTTP carrying an audience-scoped identity token, the same posture as the
  artifact server; broker TLS or mesh mTLS is a hardening follow-up.

## Known limitations

- Codex and copilot remain in-pod-credentialed (both ship disabled); routing them through the broker is a follow-up.
- The limit counters live in memory, per replica, evicted after 24 hours: a broker restart resets every pod's totals,
  and more than one replica multiplies the effective limits (the chart runs one).
- Bedrock streaming responses are metered by estimate only (see [Enforcement](#enforcement)); the other routes are
  metered from the usage they report.
- Bedrock/Vertex runs may report no `total_cost_usd`; cost accounting then falls back to the registry's first-party
  rates, which approximates provider pricing.
