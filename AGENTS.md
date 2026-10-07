# Agent orientation

> **Work in progress:** intent-driven development is mid-flight. Before starting, read [HANDOFF.md](HANDOFF.md) for
> the current state, the next steps, the gates and the security invariants.

Fast map of this repository so a new session can act without re-exploring. For _what_ the system must do — the
requirements, the state machine, and the end-to-end flow — read [DESIGN.md](DESIGN.md); for end-user usage read
[README.md](README.md). This file is the "where things are"; DESIGN.md is the "what it must do".

## What this is

`patchy` is an end-to-end pipeline (module `github.com/bitwise-media-group/patchy`) for triaging and remediating
security findings, using **Kubernetes custom resources as the state machine** — the
`patchy.bitwisemedia.uk/v1alpha1` kinds carry all state, etcd is the only state store, and GitHub issues are a
one-way human-facing projection. GHAS/CodeQL alerts arrive via webhook, accumulate into `Finding` resources for an
hour, get context-enhanced, then a sandboxed `claude -p` run investigates each one; high-confidence verdicts are
remediated in priority order into pull requests, everything else routes to humans. Completed findings expire on a
TTL; `FindingRollup` resources keep the all-time statistics.

Thirteen binaries, one module. "Not monolithic" means separate binaries/deployments with shared `internal/` code:

- `cmd/integration-controller` — the single internet-facing entry point, driven by `Integration` CRs: validates
  provider webhooks (`/github/webhooks` HMAC, `/google-cloud/webhooks` Pub/Sub OIDC, `/wiz/webhooks` bearer
  token, the `/generic/{name}/webhooks` wildcard with strictly per-name HMAC; per-Integration secrets), ingests
  scanner alerts into Findings (accumulation, duplicate merge), projects Findings out as tracking issues
  (trackingRef falls back to the namespace's issues-enabled Integration for non-github sources), applies human
  signals (issue close, `/patchy` commands, PR merge) back onto Findings, and POSTs dismissal verdicts to generic
  integrations' resolver endpoints. With `--repository-images` it also keeps the runner-image sticky comment
  (what patchy did with a repository-declared agent image), reading each finding's Repository.
- `cmd/source-controller` — `Forge` + `Repository` reconcilers: validates forge credentials, pins
  each Repository's head SHA once, downloads the tarball archive at that SHA (pure HTTP, no git binary), and
  serves it from the artifact endpoint (`:9790`) agent pods fetch credential-lessly. With
  `--repository-images` on, it also reads the tree's runner-image declaration out of that stored tarball and
  pins the image to a digest exactly once beside the SHA (`status.runnerImage`, its only writer).
- `cmd/context-controller` — runs the enhancer chain (CMDB placeholder + the Cloud Asset Inventory lookup, whose
  config is the `cloudAssetInventory` block on the `google-cloud` Integration, + the AWS and Azure resource-tags
  lookups, whose configs are the `resourceTags` blocks on the `aws` and `azure` Integrations, + the generic HTTP
  fan-out — one signed synchronous call per `generic` Integration with `enhance` on, N instances, each attributed
  by its own name) over `Opened` Findings, writes enrichments/owners to status, transitions to `Enhanced`. No
  GitHub access at all; reads Integrations read-only plus generic signing Secrets by name.
- `cmd/investigation-controller` — the gate (admits accumulated, aged findings; creates the Repository and one
  immutable `Investigation` per attempt) plus the analysis scheduler (bounded concurrency, launches agent Jobs,
  routes verdicts onto the Finding).
- `cmd/remediation-controller` — queue admission (approvals/revivals), the priority scheduler, remediation agent
  Jobs, changeset push + PR via the forge write seam (the finding flow's only write credential), and hosts the
  rollup/TTL loop.
- `cmd/agent-runner` — the in-pod coding-agent runtime: one stage per Job (`investigate` or `remediate`) via
  `claude -p`, results emitted as a `PATCHY-EVENT:` JSONL stream on stdout, beside the `PATCHY-TURN:` transcript and the
  live, never-persisted `PATCHY-OUTPUT:` command output. Never talks to GitHub or the Kubernetes API; a claude pod holds
  no credential at all (model traffic goes through the egress broker, authenticated by a projected SA token read fresh
  per stage), a codex/copilot pod only its model key.
- `cmd/egress-broker` — the egress credential broker (NOT a controller: no reconcilers, no leases): the reverse
  proxy all claude model traffic goes through, one route per provider (anthropic — key or `claude setup-token`
  bearer via `--anthropic-auth` — plus bedrock SigV4, vertex OAuth, foundry key/entra). Validates caller
  tokens via TokenReview (its only Kubernetes access), strips them, injects/signs the model credential
  outbound, streams SSE with idle keep-alive pings, audits one slog line per request. Engine in
  `internal/broker`; deployed by the chart exactly when a claude runner is enabled (claude ⇒ broker;
  proxy-only, no in-pod credential mode).
- `cmd/preview-auth` — OPTIONAL (default-off; chart wiring not built yet): the preview sign-in relay (NOT a
  controller: no reconcilers, no leases), the OpenID provider every preview host's ALB signs viewers in through. It
  signs a viewer in once per browser session at Dex (one fixed redirect URI, `<relay>/dex/callback`), admits them by a
  SubjectAccessReview for get on `projects/previews` named for the Preview's `spec.project`, and hands the ALB only
  opaque, pairwise, short-lived values. Its Kubernetes access is Previews (get/list/watch, release namespace; no
  Intents), SubjectAccessReviews and get/update on its one code-ledger Lease; its keys come from a mounted Secret.
  Every authorize error is a relay page, never a redirect; a transient token failure is 503, never `invalid_grant`.
  Per-address rate limit; a background probe of every Ready preview host without credentials
  (`patchy.preview_auth.unprotected_hosts`). Flags carry a `preview-auth-` prefix (`PATCHY_PREVIEW_AUTH_*`). Engine
  in `internal/previewauth` (core), `internal/previewauth/adapters/*` and `internal/previewauth/httpapi`.
- `cmd/evaluation-controller` — OPTIONAL (default-off in the chart): remote skill-evaluation execution for
  evolve. Hosts the bearer-authenticated HTTP API (`pkg/evaluation` wire contract: workspace upload streamed to
  source-controller's `:9791` blob endpoint, submission, snapshot, SSE monitoring, cancel; OIDC verify + SAR on
  the `evaluations` resource, native verbs only) and the reconcilers: gate (Evaluation → EvaluationUnit
  children), unit scheduler (bounded concurrency over the same sandboxed agent-Job machinery; pods run
  `evolve exec-unit` and emit `EVOLVE-EVENT:` JSONL), and the TTL loop. Patchy never learns eval semantics —
  bounded summaries land on unit status, the opaque results entry in a per-unit ConfigMap.
- `cmd/intent-controller` — OPTIONAL (default-off in the chart, an opt-in kustomize component): intent-driven
  development (docs/design/intent-driven-development.md). Polls GitHub instead of taking webhooks: each
  `Project`'s intent repository (ETag listing) for issues carrying its trigger label, and each `Intent`'s issue for the
  facts its phase waits on. It plans in a read-only agent Job on the default image, posts the plan verbatim for an
  approver, builds only the approved plan in the repository's accepted image (`runnerguard.PinFor`), pushes it
  two-phase to `patchy-intent/<intent>` (create-only, never forced) and opens the PR. It closes the issue on merge.
  With `--intent-multi-repo` (slice 3; off, a multi-repository Project is not Ready and its intents are held Blocked)
  a Project lists up to eight repositories: one plan Job reads them all (the others as digest-pinned `Trees`, recorded
  on the run), each repository the plan names gets its own build run (`<intent>-bld-r<rev>-<key>-a<n>`, its own image)
  and PR, rounds are serialised per Intent and keyed by repository, and the Intent is Merged only when every PR is.
  The second forge-writing code path: in the release namespace, `secrets get` is restricted by `resourceNames` to the
  Forge Secrets. Its agent-jobs Role can get, create, update and delete any Secret in the agents namespace, including
  model keys, image-pull credentials and other Jobs' handoffs. It uses a GitHub token per operation
  (`forge.Store.TokenWith`), has no ClusterRole and no inbound surface. Only
  writer of Project status, Intent and IntentRun. Its own flags carry an `intent-` prefix (`PATCHY_INTENT_*`); it runs
  on brokered claude only (or the fake harness in dev).
- `cmd/preview-controller` — OPTIONAL (default-off): renders one `Preview` per eligible Intent PR into a fixed,
  isolated slot namespace. It uses only the Kubernetes API, with namespaced Roles and no GitHub, AWS, ECR or Secret
  access. It owns Preview status, fixed Deployment/Service/Ingress objects, bounded retries, slot cleanup and a
  periodic orphan sweep. Intent-controller projects the operator's Project preview config and recorded PR head into
  Preview spec only when explicitly enabled: one component per previewed repository (at most four, path-routed on one
  host), a repository with no PR running its default-branch head from `status.previewBases`, all derived by the one
  pure `v1alpha1.DesiredPreviewComponents` the writer and the controller's re-check share. The writer also stamps the
  Intent's Project into `spec.project` (set once, then immutable by CEL; backfilled onto older Previews), which the
  re-check holds to the Intent's, so a per-Project reader needs only Previews. The intent reconciler reads
  Preview status (one uncached get per pass, never written) to link the preview from the issue's status comment and one
  sticky comment per previewed PR, only once it checks out (`preview_view.go`: UID, observed generation, derived
  revisions, a bare `https://<intent>.<suffix>`). See `docs/configuration/preview-controller.md`.
- `cmd/status-server` — the human-facing status page (NOT a controller: no reconcilers, no leases): the embedded
  SPA + JSON projection of Findings/FindingRollups, SSE refetch signal, OIDC sign-in, the access-review-gated
  approve/retry/expedite/suspend/resume actions, and the user-menu demo tooling (replay → Integration
  `spec.replay`; reset → delete all pipeline CRs). Rollup statistics are public; the findings surface always
  requires auth. Writes SPEC only (`spec.approval`, `spec.suspend`, `spec.replay`) — never status, never a phase.
  With `--intents-enabled` (chart `statusServer.intents.enabled`, kustomize component `status-intents`; default
  off) it also serves the read-only intents views (board, timeline, run panel; docs/intents/dashboard.md): per-Project
  access reviews on the virtual subresources `projects/intents` and `projects/transcripts`, mode oidc with both claim
  prefixes required, and a hardened browser envelope for the whole page. No intent write of any kind.
- `cmd/patchy` — the workstation CLI (the only binary not deployed): `patchy <verb> <noun>` over the
  caller's own kubeconfig, no channel through any controller. get/describe/review/browse/can-i plus the five
  action verbs. Writes SPEC only, same as status-server; enforcement of the custom verbs for direct API
  writes is the ValidatingAdmissionPolicy in `deploy/kustomize/base/admission-policy.yaml`, NOT the CLI's
  own SelfSubjectAccessReview (that is ergonomics). Five cluster-free commands ride along: `dev` (the
  generic-integration test harness), `mirror` (vendored chart/artifact mirroring over `internal/mirror`;
  kubeconfig flags inert, git never touched), `check image` (source-controller's runner-image checks through
  `resolve.Inspect`, plus `--run`: `agent-runner preflight` in a local docker shaped like the agent pod; engine in
  `cmd/patchy/internal/imagecheck`), `setup github-app` (creates the GitHub App through the manifest flow with
  exactly `intentperm.ForApp`'s permissions and events for `--security`/`--intents`/`--checks`/`--rerun-failed`, then
  writes its ghsecret-keyed Secret manifest to a 0600 file or a pipe, never to the cluster and never the private key to
  a terminal or a file other users can read; engine in `cmd/patchy/internal/ghapp`, plain net/http, no GitHub
  client) and `init app` (scaffolds an application repository's agent image, CI builds and split trusted ECR
  publishers from embedded templates; engine in `cmd/patchy/internal/scaffold`, golden trees checked as their own
  CI would by `mise run scaffold-check`). `check project` is `check image`'s cluster-reading sibling: a read-only
  Project preflight (engine in `cmd/patchy/internal/projectcheck`) that reads Ready/IntentNameConflict, resolves
  every repository over the Forge CRs, and judges agent and preview images, DNS and TLS with the caller's own
  credentials. It never reads a Secret, so never the App key. Both `check` nouns render through
  `cmd/patchy/internal/checkreport` (PASS/FAIL/SKIP lines, `-o json|yaml`, inert reasons). Builds for windows too,
  and ships a `kubectl-patchy` alias. Ships no container image: it is distributed as its own `patchy-cli` release
  archive (separate from the cluster binaries' `patchy` archive) and as a Homebrew cask in
  bitwise-media-group/homebrew-tap.

## Layout

```text
api/v1alpha1/       The CRD types: one <kind>_types.go per kind, transitions.go (the phase table +
                    SetPhase), conditions.go, generated deepcopy. `mise run codegen` regenerates
                    deepcopy + the CRD manifests (kustomize + helm); CI fails on drift.
cmd/<binary>/       package main, thin: build root command, delegate to internal/cli.Execute.
                    cmd/patchy is the exception — the CLI, with its own internal/ tree
                    (cli = one file per VERB, render = one file per NOUN; the two are separate
                    axes on purpose, since `get` resolves nouns through a registry at runtime).
                    cmd/patchy/internal/tools/docgen is dev tooling, not a binary: it renders
                    docs/cli from the cobra tree. It sits there because the internal rule puts
                    cmd/patchy/internal/cli out of reach of the repo root, and NOT under cmd/
                    because hack/build.sh builds everything there.
internal/           All private code, one package per concern (see "Packages" below).
pkg/                PUBLIC plugin seams only: pkg/source (finding sources), pkg/enhance (context
                    enhancers), pkg/generic (the generic integration's HTTP wire contract, importable
                    by external processes), pkg/evaluation (the remote-evaluation wire contract —
                    submissions, the in-pod EVOLVE-EVENT stream, the SSE monitor — stdlib-only,
                    imported by evolve). Exported signatures must not reference internal/ types.
deploy/             kustomize base/overlays; deploy/README.md is the operator doc. The container
                    Dockerfile.* live at the repo root (goreleaser dockers_v2 builds them).
charts/             Helm rendering of the same stack, pushed to ghcr OCI on release
                    (.github/workflows/helm.yaml): charts/patchy (CRDs + controllers) and
                    charts/patchy-config (the Integration/Forge CRs — a separate chart because
                    helm validates CRs against CRDs that must already exist). release-please
                    stamps both Chart.yaml versions. Lint/render with `mise run helm-lint`.
e2e/                SEPARATE Go module: envtest carries the CRDs, the real binaries run against it,
                    fakegithub (in-memory API) stands in at the network edge, recorded webhook
                    fixtures + the replay tool drive it (`make e2e`). envtest has no kubelet, so
                    Finding Jobs never run there; the intent tests register a fake kubelet
                    (kubelet_test.go) that runs hack/fake-agent for run-kind=intent Jobs (staging
                    the working tree, a plan Job's trees and the handoff as the prepare init
                    does) — or, for the phases a test opts into (useAgentRunner), the real
                    agent-runner driving hack/fake-agent/claude, a scripted claude CLI, so
                    in-pod behaviour such as report repair runs end to end — beside an
                    in-memory OCI registry (registry_test.go) serving the
                    repository runner image. fakegithub's refs and PR listings are per
                    repository, so intent_multirepo_test.go runs multi-repository intents end to
                    end; cluster.stoppableController restarts a binary with other flags.
docs/ overrides/    Zensical docs site (zensical.toml at the root; patchy-branded theme in
                    docs/stylesheets/extra.css + overrides/). `mise run serve` to preview,
                    `mise run docs-build` to build; the reusable release workflow publishes it
                    to GitHub Pages (oss.bitwisemedia.uk/patchy). uv provisions zensical
                    (pyproject.toml / uv.lock). docs/cli/ is GENERATED (docgen, above) — edit
                    the commands' Short/Long/Example, not the markdown; docs/cli.md beside it
                    is the hand-written tour.
completions/        GENERATED shell completions, committed so the Homebrew cask installs them
                    as static files (executing a freshly-downloaded binary at install time
                    trips Gatekeeper). `mise run docs` writes both this and docs/cli/.
                    kubectl_complete-patchy is the exception — hand-written, and the reason
                    `kubectl patchy` completes: kubectl ignores a plugin's completion script
                    and instead runs kubectl_complete-<plugin> from PATH.
.mise/              Shared toolchain submodule (bitwise-media-group/toolchain): pinned dev CLIs +
                    the go-cli task archetype. Makefile is a one-line forwarder; repo-local tasks
                    (multi-binary build, e2e, envtest, codegen, replay) live in tasks.toml.
.claude/plans/      The living implementation plan (git-ignored).
```

## Packages (`internal/`)

- `controller/` — one engine per controller binary; the binaries are thin wiring over these:
  `controller/integration` (receiver, ingest, projection, human signals), `controller/source` (Forge +
  Repository reconcilers), `controller/context` (the enhancer chain), `controller/investigation` (gate +
  analysis scheduler), `controller/remediation` (spawner + priority scheduler + push/PR), `controller/rollup`
  (all-time stats + finding TTL; hosted by the remediation binary), `controller/evaluation` (Evaluation gate +
  unit scheduler + evaluation TTL; single writer for both evaluation kinds — their phases are local enums,
  never part of the Finding transition table), `controller/intent` (Project validation + discovery, the Intent
  phase machine and its GitHub writes, the IntentRun scheduler with launch/collect/push, the Intent TTL; one
  writer reconciler per status and its own phase table, `v1alpha1.SetIntentPhase`, beside Finding's; `doc.go` holds
  the single-writer table and the durable-settle rules), `controller/preview` (fixed slot workload renderer,
  bounded rollout, cleanup and orphan sweep; no forge access; with `--preview-auth-required`, default off, it renders
  each slot's pinned sign-in annotations from the chart's JSON, the one the slot admission policy compares, and the
  sweep reports a slot Ingress without the current or previous generation's set, deleting it only after three poll
  intervals; an Ingress write refused at admission or by RBAC waits and requeues, never spending a retry).
- `kube` — the controller-runtime manager wrapper: scheme, kubeconfig/in-cluster config, leader election,
  multi-namespace cache, health probes, logr↔slog bridge. Secrets are never cached; a controller that needs only
  its own ConfigMaps confines their informer by label (`ConfigMapSelector`; intent-controller does, so the Finding
  transcripts beside them are never in its memory).
- `forge` — the shared forge seam: resolve a repository URL to its covering `Forge` CR (host → orgs → repo
  regexes; most-constrained wins) and mint scoped read/write tokens. Consumers: source (read), remediation
  (write), intent (a token per operation, one repository and one permission each: `TokenWith`; opening a pull
  request also reads contents, which GitHub needs in a private repository). `ghclient`, `ghpush`, `ghsecret` sit
  beneath it.
- `schedule`, `priority`, `stats` — pure logic: slot picking with anti-starvation aging, the 0–100 scheduling
  score, rollup delta arithmetic + OTel taps.
- `labels` — the trimmed human-facing label vocabulary the issue projection renders (one-way; never parsed back
  into state).
- `templates` — the finding handoff/issue body, the stage prompts (investigate, remediate, and the intent plan and
  build), and the PR body, rendered from embedded templates with golden tests. Also the intent side, which
  intent-controller renders:
  the plan comment, which shows the plan report VERBATIM (its exact bytes) in a ```markdown block whose fence no line of
  it can close, only patchy's header outside it, and refuses (`ErrPlanRefused`, with a notice to post instead) a plan
  over GitHub's comment limit, not UTF-8, or holding characters no block can show (tag characters, bidi controls, stray
  variation selectors: `planview.go`), whose header otherwise counts what the block hides at a glance (other invisible
  characters less an emoji's, lines past 100 columns, long blank runs); `Sanitize`/`SanitizeInline`, the pass every
  OTHER piece of agent text bound for GitHub takes (hidden markup shown literally, tables as text, characters that
  render as nothing as their code point; mentions, issue references on any host and so closing keywords made inline
  code); both with seeded properties checked against goldmark as a stand-in for GitHub; and the intent status comment,
  notices, the preview comment (`intent_preview.go`: the host linked only while live and only as bare DNS labels, the
  preview controller's message never shown), PR body ("Part of", never a closing keyword), PR title and commit message,
  over plain values. The last three can land on the default branch as plain text (a squash commit copies the title
  and, if the repo says so, the body), where inline code protects nothing, so each also `defang`s every reference and
  mention, code spans included; seeded properties read them raw.
- `webhook`, `telemetry`, `cli`, `version` — service plumbing (the webhook server is used by
  integration-controller only).
- `action` — the human-action vocabulary (the custom verbs) and the state-machine gating behind each one:
  `Apply` mutates spec, `Available` reports what a finding currently admits. Owned here so the status
  server and the CLI cannot drift; `web/authz` re-exports the verb constants. It decides whether an action
  is MEANINGFUL, never whether the caller may take it. The intent verbs (replan/cancel/revise) are constants
  here too, deliberately in none of the Finding verb lists.
- `command` — the one GitHub command grammar: `/patchy <verb> [note]` on a comment's first non-blank line,
  plus the legacy `/approve` (or configured approveComment) alias, honoured only by a `Parser` whose Surface is
  the Finding issue and matched exactly as the Finding webhook handler matches it today; and which verbs each
  surface (Finding issue, intent issue, intent PR) offers, with the help replies for an unknown verb (`Help`)
  and one the current phase does not admit (`HelpFor`); `Note` is the one note rule, event aliases included.
  Pure: it imports only the standard library and `action` (a test pins that); availability and authorisation
  are the caller's. Consumed by integration-controller for Finding tracking issues (Signals records a command on
  `status.commands.pending`, slots shared by account; the projection's `settleCommands` authorises it by
  `ghclient.CanWrite`, applies it via `action.Apply`, reacts and replies at most once without listing the thread,
  then consumes it), and by intent-controller on its poll path (Surface `IntentIssue`: approve, replan, cancel).
- `web` (+ `web/auth`, `web/authz`) — the status-server backend: wire types mirroring the SPA's
  `ui/src/types.ts` (keep the two in lockstep), the action handlers, SSE broker + cache-informer watcher, and
  the embedded UI (`internal/web/ui`, Vite/Preact, single-file build embedded behind the `withui` tag; `mise run
  ui` builds it, bare `go build` compiles a stub). `auth` = who you are (OIDC/none/anonymous/unconfigured,
  cookie sessions, zero k8s imports; claim prefixes and verified email applied once, in `MapClaims`); `authz` = what
  you may do (SubjectAccessReviews for the custom verbs approve/retry/expedite/suspend/resume + native get, and
  `ProjectReviewer`'s per-Project read tiers, plus `Allowed` for a single subresource such as `projects/previews`, the
  preview viewer grant). The intents side (`intents*.go`, `envelope.go`) reads every ConfigMap
  through `guardedConfigMap` (the intent's label and a controller reference to its very owner), strips the live run
  stream per subscriber (turns, and the `PATCHY-OUTPUT` command output the tail hub keeps on its own replay ring and
  channel: tier 2 only, live only, never part of the activity), and pins its wire types to `types.ts` by parsing it
  (`TestIntentWireTypesMatchTypeScript`).
- `sealed` — the shared authenticated-encryption primitives for sign-in surfaces: `PurposeKey` (HKDF-SHA256 of one
  secret, one key per purpose), `Seal`/`Open` (AES-256-GCM of a JSON value with caller-supplied AAD, so a token of one
  kind never opens as another; every failure is `ErrOpen`) and `RandomToken`. Stdlib only (a test pins that), so a pure
  core may import it; callers bound a blob's length before opening it.
- `previewauth` — the pure core of the preview sign-in relay (no HTTP server, Kubernetes client, Dex client or
  signing key; those are `cmd/preview-auth`'s adapters, below). The redirect-URI grammar (`Callbacks`: exactly
  `https://<label>.<suffix>/oauth2/idpresponse`), the per-slot ALB clients and their client secrets, the `KeyRing`
  (current and previous generation) that seals codes, access and refresh tokens, login states and relay sessions as
  `pa1.<kind>.<gen>.<blob>` with the kind and generation in the AAD and strict base64, the binding every token is
  checked against (client, slot, Preview UID, label: `Bound.Matches`), the pairwise subject, the login double-submit
  (a cookie name per sealed state), the OAuth input checks and the access-review input (`ReviewFor`, refused for a
  Preview with no Project), and `JudgeProbe` (is a preview host's unauthenticated answer the ALB's redirect to this
  relay for its own slot). Access tokens carry no identity. Seeded properties; stdlib, `sealed` and `api/v1alpha1`
  only (a test pins that). Its adapters, one package each under `previewauth/adapters/`: `kubeview` (the
  `PreviewLookup` over the Preview cache), `access` (the `Authorizer` over `web/authz.ProjectReviewer.Allowed`),
  `ledger` (single-use codes on one Lease, resourceVersion compare-and-swap, capped), `dex` (go-oidc client of Dex,
  lazy retried discovery, `web/auth.MapClaims` under the intents-views claims posture), `signer` (RS256, JWKS of
  current and previous key, RFC 7638 kids), `keydir` (the mounted keys Secret) and `hostprobe` (the unauthenticated
  host probe, critique F5). `previewauth/httpapi` is the HTTP surface (endpoints, pages, envelope headers, rate
  limit, audit line, OTel counters); `previewauth/fakedex` is test support only (an in-memory Dex).
- `intentview` — the pure public projection of intents for the status page: board columns, fixed public wording
  for outcomes and block reasons (never a run's detail or a condition's message), limits with schema defaults, cost
  parsing, and `Text` (templates.VisibleText plus a cap) for every shown string. Copies of intent-controller facts
  are pinned to their originals by `internal/controller/intent/intentview_pin_test.go`.
- `ghas`, `enhancers` — the built-in `pkg/source` and `pkg/enhance` implementations.
- `generic` — the generic integration's behavior over the `pkg/generic` wire contract: the validating source
  handler (source id = the Integration's NAME; N integrations coexist) and the HMAC-signing outbound client behind
  both the verdict resolver and the enhancer call. The enhancer fan-out itself is
  `enhancers.DynamicGeneric`/`MultiEnhancer` (internal seam; `pkg/enhance` stays one-plugin-one-identity).
- `harness`, `runner` — adapted from evolve: harness builds argv, runner executes (observe-and-collect with a
  token-budget kill switch), harness parses stdout. Keep that separation.
- `agentrun` — the in-pod stage flow (`investigate` | `remediate`, and the intent stages `plan` | `build`, which
  reuse investigate's and remediate's configuration and helpers and run on brokered claude only);
  `report`/`envelope` are its contracts (frontmatter schemas in, JSONL events out — a `plan` event beside the
  others at v4); `agentresult` converts envelope results onto CR status (`FromPlan` re-derives a plan from its
  report). A missing or refused report is first repaired in the agent's own session (`repair.go`: the optional
  `harness.Resumer`, claude and fake only; at most 2 bounded rounds; one transcript per stage; a writable stage's
  clone fingerprinted so a repair may change only the report and commit.sh). In the intent stages, a running
  foreground command's output is printed live as `PATCHY-OUTPUT:` chunks (`output.go`: read from the file the CLI
  keeps it in, via the optional `harness.TaskWatcher`; bounded per command and per process by constants; never
  persisted, never a turn, never idle-watchdog progress; `jobs.scanLog` skips it), and one lock serialises every
  stdout line.
- `jobs` — the Kubernetes Job the agent runs in. The isolation model lives here, and it STRENGTHENED with the
  broker: a brokered (claude) pod holds no credential of any kind — its projected SA token (audience-bound,
  agent container only, never the init) is an identity document, not a capability; its fixed, non-secret
  `ANTHROPIC_AUTH_TOKEN` placeholder (`provider.PlaceholderAuthToken`) exists only to get past the CLI's login
  gate and is stripped by the broker — while non-brokered runners
  keep the one SecretKeyRef; the init container fetches the digest-verified artifact tarball, identity-free.
  `reservedEnv` covers every credential channel plus the provider gateway names; `Runner.Env` is the per-runner
  gateway env (wins over `Config.Env`, can never name a credential). `eval.go` is the evaluation Job flavour
  (same posture; `evolve exec-unit` instead of `agent-runner` — wrapped in a capture-once token export when
  brokered — no git init, unit.json handoff); `ResultLines` is the envelope-agnostic log reader the evaluation
  collector decodes its own events from.
- `broker`, `provider` — the egress-broker engine (TokenReview auth + verdict cache, per-route credential
  strategies, SSE-safe reverse proxy, audit) and the pure logic of brokered claude runners (gateway env,
  canonical→provider model-id maps with per-provider derived defaults, the `PATCHY_MODEL_MAP` codec both the
  controllers and agentrun share). `provider.BrokerTokenHeader` is the one definition of the caller-token
  header.
- `artifact` — the tarball store + HTTP handler source-controller serves agent fetches from, plus the
  content-addressed workspace-blob side: sha256-named bundles (64-hex files beside the 32-hex repo tarballs),
  index rebuilt from disk on restart, last-access retention sweep, the `:9791` internal upload handler, and
  the `Client` other processes reach it with.
- `evalapi` — the evaluation controller's HTTP surface (`pkg/evaluation` contract): bearer OIDC verify (claims
  via `web/auth.MapClaims`), SAR authorization (`web/authz.ResourceReviewer`, native verbs on `evaluations`),
  workspace upload proxy, submission validation, snapshot, SSE monitor (replay + change re-emit + explicit
  `end`). `evalresults` is the per-unit results ConfigMap store (transcriptstore's sibling).
- `ghpush` — replays the agent's changeset through the GitHub Git Data API (blob → tree → commit → ref); the
  finding flow's only place a write credential is exercised. intent-controller pushes through `ghclient` directly
  (`CreateCommit`, then `CreateBranchRef`, create-only and never forced). No git binary anywhere controller-side.
- `changeset` — the pure changeset validator run before any forge call (`Validate`: pinned base, path shape,
  upsert modes/content; on a repository-declared image also the entry cap, control characters, CI
  definitions), shared so the controllers that push never import each other: remediation holds a Finding's
  changesets to `Rules` without a deny list, intent-controller an intent's to `IntentRules` (plus `.github`,
  `.patchy`, `.devcontainer` refused). Imports only the stdlib and `envelope` (a test pins that).
- `intentperm` — the one table of the GitHub App permissions patchy needs, in two views of the same rows. `For`: what
  a Project needs per repository (issues write on the intent repository; contents and pull requests write and issues
  read on each app repository, the last being the token reviewer permissions and the rate budget are read with;
  checks, statuses and actions read there too when `spec.checks.fix` is set, and actions write beside them when
  `spec.checks.rerunFailed` is too). `ForApp`: the permissions and webhook events an App registered for a set of
  features (`security`, `intents`, `checks`, `rerun-failed`, the last its own feature so check fixes never widen to a
  write on CI) must hold, metadata read included, and nothing more. Pure (stdlib + `api/v1alpha1`, a test pins that), so
  the CLI can read it without linking a GitHub client; intent-controller's Ready mints a token per grant of `For`, and
  an App manifest is built from `ForApp`, so the App holds exactly what a Project is checked for. Every token
  intent-controller mints must be a grant of `For` for its repository (`TestEveryTokenIsInTheTable`), so a new GitHub
  call cannot widen what Ready proves. One read-only exception is deliberate: an open intent's pull request in a
  repository the Project no longer lists is still read (pull requests read, plus issues read for the rate check) where
  the installation allows, so its merge counts; nothing is written there, and once refused there it is not asked again
  for a while (`readDepartedPullRequest`).
- `runnerimage` (+ `runnerimage/resolve`) — repository-declared agent runner images. The parent is the pure
  core (declaration files out of a tar.gz stream, `.patchy/agent.yaml` and the devcontainer.json fallback with
  precedence, reference grammar and strict digests, the allowlist `Policy`, PATH/ENV/VOLUME checks, the
  `Resolver` seam and `Rejection`); `resolve` is the go-containerregistry implementation source-controller
  wires in (one HEAD then digest-only calls, index enumeration, host-selected keychain, in-process cosign
  verification in bundle and legacy forms, a digest-keyed verdict cache). No Kubernetes types in either.
- `runnerguard` — the job controllers' side of repository-declared images, shared by investigation,
  remediation and intent: whether a launch may run the Repository's pin (kill switch, not revived by a human, sandbox
  breaker), the pull fail-fast gated on the Job's `runner-image-source` annotation (never on config), and the
  in-memory sandbox breaker a prepare exit 78 trips until restart (`patchy.sandbox.breaker` gauge). `PinFor`
  (beside the untouched `Pin`) is the same decision for a launch that requires the image (intent build and
  revise runs): "" only when it copied an accepted pin, a reason otherwise. It has no revival rule: an intent
  brought back by its trigger label is a new plan and a new approval, not a Finding revival.
- `mirror` — the engine behind `patchy mirror` (CLI-only, except `imageref`, which `runnerimage` also builds
  on; no controller consumes the rest): vendored mirroring of
  upstream helm charts and OCI artifacts into one or more platform registries (mirror.yaml lists them; every
  entry publishes to all, lock files record targets per registry name, and each registry may carry its own
  wholesale `signing` override — `sync --registry` restricts a run). One concern per subpackage: `spec` (the
  mirror.yaml/manifest/lock schema + glob discovery — the tree is the registry of entries), `imageref`,
  `semverpick` (constraints + cooldown walks), `yamledit` (comment-preserving byte-splice edits, never
  re-encode), `helmchart` (pull/extract/tree-diff/push), `render` (byte-stable `helm template` equivalent —
  helm SDK pinned; upgrades are deliberate, the validate gate byte-compares output), `discover` (4-pass image
  discovery), `distro` (distribution-manifests → generated images.extra.yaml sidecar), `verify`/`sign`
  (shell out to the cosign binary: upstream provenance in, bundle-referrers signatures out; KMS support is
  the installed cosign build's), `scan` (pluggable: osv + grype/kubescape, all shell-outs; every image
  scanner defaults off, and an image scan with none enabled is a hard error), `allowlist` (derive with
  keep-expiry/drop-stale rules). The engine never touches git — the calling pipeline owns
  commits/branches/PRs — and only the upgrade path may consult the wall clock, keeping `validate`'s
  byte-identity gate deterministic. cosign (≥ v3) and osv-scanner (v2) are runtime dependencies of the
  respective verbs, never linked.

## Conventions

- Go 1.26; cobra + viper (`PATCHY_` env prefix); `log/slog` to stderr (stdout is reserved — agent-runner's event
  stream lives there); OpenTelemetry with an otelslog fanout that never fails startup.
- Every package has a `doc.go`; every file starts with the MIT SPDX header (enforced by revive + addlicense).
- Table-driven stdlib tests, no testify; fakes over mocks; controller-runtime fake client for reconciler tests;
  envtest suites skip without `KUBEBUILDER_ASSETS` (`mise run envtest`, `mise run e2e`).
- Conventional Commits; release-please + goreleaser drive releases; `make pr` is the local gate.
- The harness/runner packages are adapted from `../evolve` (`internal/harness`, `internal/runner`) — keep their
  "harness builds argv, runner executes, harness parses stdout" separation intact.

## State machine (the heart of the system)

`api/v1alpha1` owns the phase taxonomy and legal transitions (`transitions.go`: `CanTransition`, `Terminal`,
`SetPhase`); no phase edge has two writer components. The flow:
`Opened → Enhanced → Investigating → Queued → Remediating → InReview → Remediated`, with `AwaitingApproval`
before `Queued` on holds, and `Dismissed`/`HandedOff`/`Failed` terminal (`HandedOff` revivable by approval,
`Failed` by human retry back to the pre-failure state; `spec.expedite` skips accumulation/min-age and jumps both
schedulers' queues).
Accumulation is a condition (`AccumulationComplete`), not a phase. See DESIGN.md for the full flow and
.claude/plans/ for the transition table with writers.
