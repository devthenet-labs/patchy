# Handoff: intent-driven development in patchy

> **Field notes:** [FIELD-NOTES.md](FIELD-NOTES.md) collects what running intents on real applications taught us, as raw
> material for the docs. Add to it as things happen.
>
> **Site values:** this file is public, so the AWS account ID and the owner's preview IP are written as the
> documentation values `111122223333` and `203.0.113.10/32`. The real ones are in terraform-devthenet's `k8s/` values
> (private).

## Current checkpoint — 2026-10-08 (evening): previews public behind sign-in, and 0.12.26

**The preview IP allowlist is gone**, as the owner asked once previews worked behind sign-in. **0.12.26 (#158) makes a
`sessionTimeout` change never fail an upgrade.**

- Live on devthenet-dev: Helm patchy rev 71 (0.12.26) and patchy-config rev 45. Rollbacks:
  - rev 70: 0.12.25, public;
  - rev 68: 0.12.25 with the allowlist and `sessionTimeout` 3600.

  Rolling back below rev 71 returns to the exact timeout pin; see the guide's "Changing `sessionTimeout` on a live
  install".

- Values: `preview.inboundCIDRs: []`, `preview.allowPublicWithAuth: confirmed`, `previewAuth.sessionTimeout: 900`. The
  preview load balancer's security group admits 443 from anywhere. terraform-devthenet #48 is merged, so its main
  matches the cluster.
- 0.12.26 judges `auth-session-timeout` as a ceiling: a canonical whole number of seconds up to `sessionTimeout`,
  recorded on `patchy-preview-ingresses` as `patchy.bitwisemedia.uk/preview-auth-session-timeout` (900 live). Lowering
  is one upgrade; raising is two, and the first pass fails nothing. The sweep accepts a shorter session too.
- Live check after the upgrade: patch dry-runs of the placeholder Ingress against the live policy admit 600 (0.12.25
  refused exactly this) and refuse 1200 and `0600`.

The cookie probe ran before the allowlist went, on throwaway intent intents#20 (PR closed). Results are in
docs/intents/preview-sign-in.md, "What the probe found":

- The session cookie never reaches the app, and it is HttpOnly, Secure and host-only.
- A copied cookie does replay on another host in the same slot.
- Cross-slot replay is untested.

A demo intent (intents#21, PR closed) showed a preview in public behind sign-in. `patchy check project preview-demo`
passes every check.

Follow-ups:

- Consider a cross-slot replay test once two slots are live.
- `hack/e2e.sh` should set an explicit `-timeout`: the suite runs close to Go's 10-minute default.

Still with the owner:

- **The upstream advisory:** `.claude/plans/upstream-advisory-draft.md`, the owner's to send or drop.
- **The site values in the public fork's history:** the home IP is in 12 commits and the account ID in 22; the current
  files use documentation values. Left as is unless the owner asks for a rewrite.

**Intent context, PR 1 (branch `feature/intent-handoff`, not yet pushed, merged or released).** An iterated intent now
starts from what earlier agents and approvers established, with no new CRD kind, `PATCHY_*` key, envelope version or
GitHub permission:

- **Turns** (`291328d`): IntentRun status records `numTurns` and `firstEditTurn` (the 1-based turn of the first Edit,
  Write, MultiEdit or NotebookEdit; claude and fake only, 0 = none or unknown), clamped to [0, 100000] at all three
  `status.usage` writes. This measures how long an agent spends re-learning before it changes anything.
- **Revise and check-fix rounds** (`c0fd5ce`): each round reads its pull request's whole approver thread since the first
  build of its plan revision. Feedback a failed round never acted on comes back as new, and older items are shown as
  "Earlier feedback", bounded at 16 KiB. When the previous round failed, its outcome is shown too. An edited older item
  is skipped, never fatal. `ReviewIDs`, the trigger cutoff and the retry pin are unchanged.
- **Replan and revival** (`a707291`): the input snapshot stores a `context.md` beside the request, holding the previous
  plan, the latest build-side failure and the approvers' comments up to the previous plan. It is pinned by
  `status.input.contextDigest` and each plan run's `inputs.contextDigest`, which CEL allows on plan runs only, and
  re-hashed at input and at launch. The plan prompt names the file under "Earlier work on this intent" as data. The
  approval is still bound to the request's digest alone. Builds still get only the approved plan.

Gates, all green on the branch: `mise run pr` (envtest included), the full `mise run e2e -- -timeout 25m` (590 s) and
`cd e2e && go vet ./...`. Next: PR 2 (`feature/intent-working-notes`), the agents' own working notes.

**Intent context, PR 2 (branch `feature/intent-working-notes`, stacked on PR 1, not yet pushed).** Agents now carry
their own working notes from run to run, still with no new kind, key, envelope version or permission, and no parser
change:

- **Working notes:** the build prompt asks every build, revise and check-fix run to end its report with
  `## Working notes`, under 6 KiB, and to rewrite any notes it was handed rather than append to them. A round gets the
  latest notes of its repository and plan revision, failed runs included, after the plan and before the feedback. A
  replan's or revival's `context.md` gets each repository's latest notes. The controller re-parses the stored report
  (`report.ParseBuild`; it skips a report that no longer parses), escapes the notes, and cuts them at 6 KiB with a
  visible marker. Over-long notes are never refused.
- **Notes for the builder:** the plan prompt allows an optional `## Notes for the builder` section, under 4 KiB. It is
  part of the plan, and the approval comment adds one line under "Before you approve" when a plan has one.

Gates, all green on the branch: `mise run pr` (envtest included), the full `mise run e2e -- -timeout 25m` (588 s) and
`cd e2e && go vet ./...`.

Parked: never-discard-work and session resume (branch `feature/never-discard-work`, unmerged).

## Earlier checkpoint — 2026-10-08: preview sign-in on, and 0.12.25

**Preview sign-in is on for devthenet-dev, at the `require` stage, and was tested end to end in a browser.** Every
preview host now sends an unauthenticated visitor to the `preview-auth` relay, which signs them in with GitHub through
Dex once per browser session. Access is decided by a SubjectAccessReview on `projects/previews`. The relay hands the
preview's load balancer only an opaque, pairwise subject, never an identity for the app. The preview IP allowlist is
**still on**; removing it is the owner's call.

Switch-on, in order:

1. terraform-devthenet #47 (merged): the relay's DNS record, its Dex static client `patchy-preview-auth`, and the values
   overlay `k8s/patchy-values-preview-auth.yaml`.
2. The two client Secrets, created from one random value and never printed.
3. Dex rev 5. Rev 4 crash-looped because of a values edit; the old pod kept serving. Dex's rollback point is rev 3.
4. patchy rev 66 at `permit`.
5. patchy rev 67 at `require`.

0.12.25 (#154) then landed the three follow-ups from the live test:

- **`patchy check project` judges Dex's answer for real.** The Dex check used to pass on any redirect, and Dex also
  redirects an unknown client at `/auth`. It now follows redirects that stay on Dex's own origin and judges where they
  end: github.com is PASS; `Unregistered redirect_uri.` (400) or `Invalid client_id.` (404) is FAIL.
- **The relay Ingress sets its load-balancer settings explicitly.** It sets `ip` targets and a health check on `/`. It
  sets `internet-facing` only when it is off the chart's edge class.
- **The flaky `TestDNSClusterLeavesTheJobAlone` is fixed.** The fake clientset stamps managed fields with the wall-clock
  second, so two Jobs created on either side of a second boundary differed.

**Live on devthenet-dev:**

- Helm patchy rev 68 (0.12.25, `require`) and patchy-config rev 44.
- Rollback points for patchy: 67 (0.12.24, `require`), 66 (`permit`), 65 (sign-in off).
- Rolling back from `require` is not a plain `helm rollback`. Follow docs/intents/preview-sign-in.md, "Rolling back":
  1. Scale preview-controller to 0.
  2. Delete the binding `patchy-preview-all-slots-ingress-auth`.
  3. Run `helm rollback patchy 65`.
- After the upgrade, `patchy check project preview-demo` passes every check, including `preview-auth-dex`, which now
  ends at github.com.

**Live test, 2026-10-08 (intents#19 → `preview-demo-19`, slot 0; 0.12.24, then again on 0.12.25):**

- **Sign-in worked.** A browser with the owner's GitHub session signed in without a prompt. The relay's audit lines
  were: authorize → `login`, callback → `issued` (access review passed), token 200 (the load balancer's client
  authentication works), userinfo 200. The only identity in them is the pairwise subject. The page served the PR's
  build.
- **Sessions are per host.** In the same slot, the placeholder host started a fresh authorize instead of reusing the
  session, and the relay answered "No live preview": it fails closed.
- **No bypass got through.** An unauthenticated request, forged `x-amzn-oidc-*` headers and a made-up session cookie
  each got the 302 to the relay.
- **The ALB's authorize request carries `state` only.** It sends no nonce and no PKCE, which is why the relay binds its
  codes to client, slot, Preview UID and label itself.
- **Clean-up worked.** Closing the test PR closed the intent, and the slot's Ingress was gone within a minute.

**Not verified:**

- The session cookie's HttpOnly and Domain attributes. browser-use cannot read cookies and the AWS docs say nothing, so
  this needs a DevTools look.
- A non-viewer's 403 page; that needs a second GitHub account.
- Replaying a session across slots; only slot 0 was live.

The e2e suite now runs close to Go's 10-minute default test timeout (579–597 s on the owner's machine). `hack/e2e.sh`
should set an explicit `-timeout`.

**Still waiting on the owner:**

- the cookie check above;
- whether to drop the preview IP allowlist;
- the upstream advisory and the git-history scrub.

Intent context, never-discard-work and session resume stay parked (branch `feature/never-discard-work`, unmerged).

## Earlier checkpoint — 2026-10-07 (overnight): 0.12.22 to 0.12.24

**Three releases, each deployed and checked live on devthenet-dev.** The owner narrowed the night's work to preview
sign-in plus four small fixes; the larger "never discard work" and intent-context work is parked (branch
`feature/never-discard-work`, unmerged).

- 0.12.22, #145: the read-only posture (intent plan and Finding investigate) is narrowed to the file tools: no shell,
  writes confined to the stage's report directory (the investigate stage had no write scope), auto memory off so an
  in-pod report repair loads nothing the agent wrote. Prompts state the real tool surface on claude only; codex and
  copilot cannot express the posture, so the pod stays their boundary. Live: a plan run used only Read/Glob/Grep and one
  Write, and a fresh Finding (alert #48, patchy-target#90) went from seed to Remediated in about six minutes with an
  investigation that used only file tools.
- 0.12.23, #147 the planner is told patchy checks the plan's layout itself (live planners spent their last turns writing
  check scripts); #148 an over-long build note no longer fails the run; #149 a check-fix round heads its diagnostics
  "Check failures". Live: a plan-only run cost $0.16 against $0.26 the night before, with a single Write.
- 0.12.24, #151: preview sign-in through a new relay binary, `preview-auth` (docs/intents/preview-sign-in.md), **off by
  default and off on devthenet-dev**. With it off the only render changes are the admission groundwork (slot pods cannot
  reference Secrets; metadata-only updates to slot Services and Ingresses are admitted, so deletions do not hang),
  Events RBAC for preview-controller, and the Preview `spec.project` field.

Live on devthenet-dev: Helm patchy rev 65 (rollback 64 = 0.12.23, 63 = 0.12.22, 62 = 0.12.21), patchy-config rev 43.

Turning preview sign-in on is prepared but not done: terraform-devthenet draft PR #47 (the relay's DNS record, its Dex
client, a values overlay at the permit stage). Its DNS record needs the owner to apply; then the Dex client and the two
Helm stages, then the browser sign-in and the cookie probe. The preview IP allowlist stays until that probe passes.

Known flaky test: `internal/jobs` `TestDNSClusterLeavesTheJobAlone` fails about one run in 40 on main (it failed #151's
first CI run and passed on re-run).

## Earlier checkpoint — 2026-10-07: 0.12.21

**0.12.21 released, deployed and gated.** Five small items the owner picked from the deferred 0.12.20 list, each its own
PR with tests, reviewed and fixed before merge. Two are opt-in and change nothing until an operator turns them on.

- #138 the issue's status comment shows the revision allowance (the Revisions line).
- #139 a queued preview says it is waiting for a free slot, not that it is being deployed.
- #141 opt-in `spec.checks.rerunFailed`: a failed GitHub Actions check is re-run once, on patchy's own head, before a
  check-fix round is spent on it. Ready then also proves an Actions write grant; a re-run that fails to start, or is
  still running when `checks.timeout` passes, falls back to the normal fix round. Check runs are listed with
  `filter=all` so the original failure (and its logs) stays visible once the re-run starts. Actions write lets the App
  re-run, cancel and dispatch workflows and delete runs, logs and artifacts on every repository it is installed on;
  patchy uses it for the one re-run call, with a token scoped to that repository.
- #142 the plan prompt lists the Project's other open intent pull requests (title and first changed files, newest by
  creation first, bounded), so a planner can avoid colliding with work already in review.
- #143 opt-in `--agent-dns none` (chart `agent.networkPolicy.dns`, kustomize component `agent-dns-none`): agent pods get
  no resolver and the NetworkPolicy's port-53 rule is dropped. Nothing in the pod can resolve an external name, so every
  toolchain and dependency must already be in the image. Turning it on also needs any cluster-level DNS allow policy (on
  devthenet-dev, terraform-devthenet's Auto Mode DNS policy) narrowed to leave agent pods out.

Live on devthenet-dev: Helm patchy rev 62 (rollback 61 = 0.12.20), patchy-config rev 40 (rollback 39). The render diff
against 0.12.20 was the new optional CRD fields plus config checksums. All nine deployments ready on v0.12.21, no
controller errors after the upgrade, all five Projects Ready. Neither opt-in is enabled there yet.

Fresh-Finding gate (2026-10-07): seed `rerungatekey.go` (1024-bit RSA) on patchy-target 01:59Z, alert #47 02:00:42Z,
`finding-514becf18f-22` one second later with issue #87, `/patchy expedite` 02:01:17Z, Investigating, Remediating
02:03:00Z, InReview 02:04:22Z with PR #88 (2048-bit key plus a regression test), all checks green, merged, Remediated
02:05:29Z, issue #87 closed completed, alert #47 fixed, exactly one Finding for it. No approval hold.

Still waiting on the owner: the three design notes (session resume across rounds, preview sign-in, dependencies for
offline agent pods) and the open decisions listed under the 0.12.20 checkpoint.

## Earlier checkpoint — 2026-10-06 (afternoon): 0.12.20

**0.12.20 released, deployed, gated, and its new features checked live.** The owner narrowed the planned batch to four
items: a harness fix, broker spend limits, and two dashboard features. Items (a)–(e) of the 0.12.20 plan (re-run a
failed check before a fix round, tell the planner about open intent PRs, "waiting for a preview slot", the Revisions
status line, per-Project turn limits) are deferred until the owner asks.

- #132 claude's settings sources are pinned for every postured run
  (`--setting-sources user --strict-mcp-config --add-dir <tree>` plus `CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`),
  so a stage's tool posture is patchy's alone, independent of configuration files in the tree under work. The tree's
  root `CLAUDE.md`, `.claude/CLAUDE.md` and `.claude/rules` still load; a subdirectory's `CLAUDE.md` no longer loads on
  demand. Checked on the production CLI (2.1.263) before merge and live after the upgrade: the build pod's `claude`
  process carried the new flags and the variable.
- #134 the timeline's run rows are visibly links: the underlined run name and a trailing "View conversation →" ("Open
  run →" when the panel will show no conversation). A first version made the whole row clickable; review showed it broke
  double-click selection and modified clicks, so it is two plain links.
- #135 the transcript recorder scrubs credential values before stripping terminal escapes as well as after (a bare or
  unfinished escape used to swallow a secret's first byte and let the rest through).
- #136 live command output in the run panel. The claude CLI writes a running Bash command's output to
  `<CLAUDE_CODE_TMPDIR or /tmp>/claude-<uid>/<cwd slug>/<session>/tasks/<task_id>.output` and announces the task on its
  stream; agent-runner follows it (intent stages only, one command at a time) and prints a new `PATCHY-OUTPUT:` stdout
  stream, scrubbed and bounded (1 KiB lines; 64 KiB per command then samples, 64 KiB of samples; 512 KiB per pod), never
  persisted. status-server follows it beside the turns with its own replay ring and channel, to transcripts-tier readers
  only. Review (13 findings, all fixed and re-verified) also fixed an older bug: a stage result whose report quoted
  `PATCHY-TURN:` was dropped, and a tail-hub race could cancel a viewer's stream as another left. Live: on the real CLI
  in a repository-image pod, a command's first chunk arrived at 20:31:19Z and its last, with Done, at 20:31:41Z, while
  it ran.
- Broker spend limits are on in terraform-devthenet (#46): `tokensPerPod` 15M, `tokensPerHour` 60M, `requestsPerPod`
  1000, `concurrentPerPod` 4, sized from real IntentRun usage (largest build 10.5M tokens in ~120 requests).

Helm: patchy **60** (0.12.19 + broker limits; rollback **58**; rev 59 was an interrupted upgrade, failed with no
change), then **61** (0.12.20; rollback **60**); patchy-config **39** (0.12.20; rollback **38**). The render diff for
0.12.19 → 0.12.20 changed only version strings and checksums. Repository-image pods get the new agent-runner and CLI
too: the prepare step copies both from the default image. Release PR #133 was stamps and CHANGELOG only, its CI approved
at the verified head. All nine Deployments Ready on v0.12.20 with zero restarts; all five Projects Ready.

Gate: weak-key alert **#46** → `finding-514becf18f-21` → issue **#85** → `/patchy expedite` → no hold this time → PR
**#86** (2048-bit key plus a committed regression test, all checks green) → merged `fc402d8` → Remediated, issue closed
completed, exactly one Finding for the alert, each marker once; main's CodeQL scan marked alert #46 fixed.

Live test intent: preview-demo-16 (intents#16, a build-version footer) planned in one run ($0.36), was approved by
label, built in the repository image, and opened patchy-preview-demo#12 with every check green, including the changelog
check. Its preview is Queued: both slots are held by the owner's demos (Hello.Web#2, overdub#2). PR #12 is left open for
the owner.

**Next (each needs the owner's go-ahead):**

- Resume the agent's CLI session across rounds (plan revisions first, then build review rounds) instead of re-reading
  everything each round; a short design note first. Warm or paused pods were considered and rejected: the model API is
  stateless and the prompt cache expires long before a human review, so they would save only installs while holding
  capacity and breaking one-stage-per-pod isolation.
- Restrict agent pods' DNS to cluster names (hardening; agent pods otherwise reach only the artifact server and the
  broker).
- Cosign-sign repository agent images, then `allowUnsigned: false`.
- Repositories with real dependencies: agent pods have no network, so dependencies must be in the agent image (patchy
  uses the devcontainer's image, not `postCreateCommand`). Rebuild the scaffolded agent image when the lockfile changes;
  if an agent ever needs new dependencies, a per-Project dependency proxy serving only lockfile and plan-approved
  packages, never general egress.
- The deferred 0.12.20 items (a)–(e).

## Earlier checkpoint — 2026-10-06 (overnight): 0.12.19

**0.12.19 released, deployed and fresh-Finding gated; sign-in and the intents dashboard are on (2026-10-06, overnight;
the owner asked for everything "well tested" while asleep).** Six PRs, each with `make pr`, `mise run e2e` and a
regression test proven to fail without its fix; five were reviewed from two angles and the dashboard from four, every
finding adversarially verified; an integration run merged all five fixes together and passed both gates before they were
merged one at a time, each current with main and every check green:

- #125 intent pull requests are opened with contents read too: GitHub refused overdub-12's PR in a private repository
  (`422 not all refs are readable`). Live: after the upgrade the Blocked intent retried and opened overdub#2.
- #126 the planner is told its own turn, token and wall-clock budget, that it cannot run commands, to batch reads, and
  the report's limits; its writes are scoped to `reports/` by an `Edit(//…/reports/**)` rule (smoke-tested on CLI
  2.1.291; live plans on 2.1.263 since: hello-web-14 in ~10 turns/$0.13, preview-demo-15 in ~12 turns/$0.19).
- #127 the preview link: one sticky comment per previewed PR, edited in place, and a status-comment line. Live on
  Hello.Web#2 and overdub#2 within seconds of the upgrade.
- #128 an invalid or missing report is repaired by resuming the agent's own session (2 bounded rounds) instead of
  discarding the stage. Not yet triggered live; e2e drives the real agent-runner through repair, refusal and give-up.
- #129 preview Deployments roll (maxSurge 1, maxUnavailable 0) instead of Recreate. Review caught that the upgrade's
  strategy patch would have retried every long-Ready preview (fixed, unit + real-apiserver e2e). Live: Hello.Web's
  preview answered 200 throughout the upgrade. A redeploy on a new head is not yet proven live (both slots are held by
  the owner's demos).
- #130 the read-only intents dashboard, first slice, behind `statusServer.intents.enabled`.

Helm: patchy **57** (0.12.19, sign-in off; rollback point **56**), then **58** (sign-in + dashboard on; rollback
**57**); patchy-config **38** (stamps only; rollback **37**). Release PR #131 was stamps and CHANGELOG only, its CI
approved at the verified head. Gate: weak-key alert **#45** → `finding-514becf18f-20` → issue **#83** → expedite → the
investigation held it (`breakingChangeAvailable`) → `/patchy approve` → PR **#84** (2048-bit key plus a committed test,
pushed commit = PR head) → merged `8baa2df` → Remediated, issue closed completed, one Finding, alert fixed, each marker
once.

Sign-in: Dex (`dex.patchy.devthe.net`, terraform-devthenet #39–#43) with "Sign in with GitHub" for devthenet-labs
members; the status server runs mode oidc with `github:` prefixes and the dashboard on; `github:<owner>` is bound to
findings-operator and intents-content. The redirect chain status → Dex (PKCE) → GitHub authorize is verified; the
owner's first sign-in (one "Authorize" click at GitHub) is still to do. Anonymous `/api` answers 401, cross-site 403;
CSP and HSTS are set; the webhook and previews were checked after each step.

Live test intent: preview-demo-15 (intents#15) planned, built, opened PR #11, had its changelog fixed by an automatic
CI-fix round, and merged
($0.72); its preview stayed Queued because both slots were in use. Open for the owner:
Hello.Web#2 (hello-web-13) and overdub#2 (overdub-12, $9.48
of its $10 ceiling, preview live, checks green after a re-run of the known-flaky studio test).

**0.12.18 released, deployed and fresh-Finding gated.** PR #122 added operator-defined agent resource classes and fast
failure for unschedulable Jobs, with fixes for durable run outcomes and public eviction messages. Its local `make pr`
and `mise run e2e` gates passed, and every PR check concluded SUCCESS or SKIPPED before merge. Release-please PR #120
contained only version stamps and CHANGELOG changes; its verified-head CI and the release workflow passed. The
live-values render comparison found only the expected images, chart labels, controller configuration and CRD schema
changes. Before upgrade, Helm rollback points were **patchy 53 / patchy-config 35**. Upgraded patchy first to **rev
54**, then patchy-config to **rev 36**. All nine Deployments are Ready with zero restarts; all five Projects are Ready.
Two controllers each logged one transient broker readiness fetch error during rollout, with no repeat after settling.

The fresh gate used weak-key alert **#44** on patchy-target: Finding `finding-514becf18f-19`, tracking issue **#81**,
`/patchy expedite`, and repair PR **#82**. The Remediation's pushed commit matched the PR head, the PR added a 2048-bit
assertion, and every check passed. Squash merge `c9d9ac7d` led to Remediated, issue closed completed, one comment per
patchy marker and exactly one Finding for the alert. Main CodeQL passed and marked alert #44 fixed.

**Next:** prepare the `large` class in terraform-devthenet `k8s/` values (4 CPU and 8Gi requests, 10Gi memory limit, no
CPU limit) and select it for overdub. Show the complete diff to the owner and **stop before applying**. On approval,
upgrade patchy then patchy-config and confirm all Projects Ready. Re-add `patchy:overdub` to intents issue #10, tell the
owner when its plan is posted, let the owner approve, then observe the build PR and preview. If that intent fails,
report the cause and stop without retrying or changing anything. Broker limits and dashboard work are deferred.

## Historical checkpoint — 2026-10-03

**Previews are set up and ready for the first live demo; no preview has run yet.** Done today, each owner-approved
separately, with rollback points recorded before each Helm revision:

- **Stage 2 (preview ALB):** terraform-devthenet #26 set `preview.placeholder.enabled: true`; patchy Helm **rev 43**.
  Auto Mode created `devthenet-dev-preview` (ARN `…/app/devthenet-dev-preview/4b96adc30c66b9f7`, DNS
  `devthenet-dev-preview-1425308117.us-east-1.elb.amazonaws.com`, created 2026-10-02T17:11:49Z): HTTPS 443 only,
  `ELBSecurityPolicy-TLS13-1-2-2021-06`, the wildcard cert, SG inbound only `203.0.113.10/32` on 443, one empty target
  group. Shared ALB unchanged (same ARN/DNS/creation time); a fresh App delivery returned 202; status page 200.
- **Wildcard DNS:** terraform-devthenet #27 (`patchy-preview-dns.tf`): `*.preview.patchy.devthe.net` A alias to the
  preview ALB, looked up by name like the shared ALB. Applied 1 add; fresh plan no changes. Names resolve to the preview
  ALB; HTTPS from the owner's IP verifies the cert and returns 503 (no targets). The terraform checkout needed
  `terraform init -reconfigure` (same S3 bucket/key/lock table; cached backend metadata had drifted).
- **Demo toolchain image:** dispatched patchy-preview-demo `agent image` on main; the trusted publisher
  (`publish images` → agent job; runtime job skipped) pushed `patchy/app-envs/patchy-preview-demo:toolchain-v1`
  (`sha256:02b294e7…`). `.patchy/agent.yaml` already names it.
- **Intents:** devthenet-labs/intents #3 added the `preview-demo` issue form; label `patchy:preview-demo` exists.
- **Controller + Project:** terraform-devthenet #28: `previewController.enabled: true` with
  `apiServerCIDR: 172.20.0.1/32` → patchy **rev 44**; Project `preview-demo` (repo patchy-preview-demo, approver brvtl,
  preview `{imageRepository: …/patchy/previews/patchy-preview-demo, port: 8080, readinessPath: /healthz}`) →
  patchy-config **rev 26**. Both Projects Ready; `target` has no `spec.preview` (never previewed). Nine Deployments
  Ready, zero restarts; previews 0; preview nodes 0; slots empty.
- **Rollback points:** before the controller: patchy 43 / patchy-config 25; before stage 2: patchy 42. Stage-2 rollback
  must delete the named kept placeholder Ingress/Service (removing the ALB) before `helm rollback patchy 42`, and the
  wildcard record (revert #27 + apply) should go first.

**0.12.13 released and deployed; gate PASSED (2026-10-03 UTC).** #85 (preview-controller Role gets `events`
create/patch), #86 (`chart-render-test` runs under `LC_ALL=C`) and #87 (the remediation prompt makes the regression test
part of the fix) merged; release #88. patchy Helm **rev 47**, patchy-config **rev 27**; rollback points patchy **44** /
patchy-config **26**. Revs 45 and 46 failed on transient network drops between the workstation and the EKS API (Patch
calls hung ~13 min, then `connection reset by peer` / `read: operation timed out`; the chart was not at fault). A third
identical `helm upgrade` succeeded in under 2 min. Nine Deployments Ready on v0.12.13 with zero restarts. The
preview-controller logs have no `events is forbidden`, and it records its LeaderElection Event. Both Projects Ready,
preview pool 0 nodes, status page 200, preview ALB unchanged. Gate: weak-key alert #39 → `finding-514becf18f-14` →
tracking issue #71 → `/patchy expedite` → PR #72 (pushedCommit = PR head `84a62d0c`), checks green, merged `5da5fa39` →
Remediated, issue closed completed with the remediated label, one comment per marker, main CodeQL marked #39 fixed, no
duplicate Finding. **The remediation committed its test** (`releasegatekey_test.go`, a ≥2048-bit assertion) beside the
fix.

**First live preview demo: PASSED (2026-10-03).** The owner opened intent `devthenet-labs/intents#4` ("Hello from
patchy", teal card) with the preview-demo form. An approve label added before the plan existed was correctly ignored and
removed when the plan was posted (80 s,
$0.26); the owner re-approved after reading it. The build (100 s) opened
`devthenet-labs/patchy-preview-demo#7`; the PR CI built the runtime image, the trusted publisher pushed it, and the
Preview reached Ready in ~170 s (cold preview node `t3a.medium` from zero) at
`https://preview-demo-4.preview.patchy.devthe.net`, serving the PR head SHA. The owner's "Request changes" review
(purple) started a revision round after the quiet window ($0.24),
a fast-forward push (`e2256900`, parent `3ffe8b96`), and the Preview redeployed at the new head in ~160 s (one brief
empty response during the ALB target switch). After the owner merged: Intent Merged (merge `0f6d1a94`), issue closed
completed, Preview and slot workloads deleted within ~10 s, the preview node terminated ~40 s later (pool resources all
zero), and the old host returns 404. Total agent cost for the intent: $0.81.

**Fresh-Finding gate after enabling previews: PASSED.** Weak-key alert #38 → `finding-514becf18f-13` → issue #69 →
`/patchy expedite` → PR #70 (pushedCommit = PR head `1c553a4c`), checks green, merged `4a3dd6f0` → Remediated, issue
closed completed with the remediated label, one comment per marker, main CodeQL marked #38 fixed, no duplicate Finding.

**Intent CI-fix round, live: PASSED (2026-10-03, 06:50–07:09 UTC).** patchy-preview-demo #8 (squash `c8c68365`) added
CHANGELOG.md and a `changelog` check that fails a PR with no line naming its `#<number>`. It is a workflow of its own
because `publish images` follows only `test`; the runtime publish for #8 still succeeded. terraform-devthenet #29 set
the `preview-demo` Project's `checks.fix: [changelog]` → patchy-config **rev 28** (rollback point 27; patchy stays rev
47). Intent `devthenet-labs/intents#5` ("Deployed by patchy" footer) → `preview-demo-5`: plan 66 s (`$0.22`; it left
CHANGELOG.md alone, since the PR number does not exist yet), approved, build 118 s (`$0.28`) opened
`devthenet-labs/patchy-preview-demo#9` at `7f4d2aae`. `changelog` failed at 07:00:29 ("CHANGELOG.md has no entry for #9:
add a line under ## Unreleased"). 54 s later intent-controller created `preview-demo-5-rev1-preview-demo-a1`
(`trigger: checks`, `checkRunIDs: [111151376149]`) with the annotations and the Actions log tail fenced as data. The
round (112 s, `$0.24`) fast-forwarded `ff754761` (parent `7f4d2aae`; CHANGELOG.md +1 line naming #9). `changelog` passed
at 07:03:29 and `test` at 07:04:59; the Intent recorded `checkFixes: 1` with `revisions` untouched. The Preview
redeployed at `ff754761` (Ready 07:06:38, then about 15 s of an empty response and a 404 during the ALB switch).
Squash-merged `8604f200` → Merged in 12 s, issue closed completed, Preview deleted within 30 s, preview pool at 0 nodes
about 57 s later. Intent cost: `$0.75` (748594 µUSD).

**Slice-3 demo apps and their infrastructure: ready (2026-10-03, ~08:15 UTC; no patchy-config or Helm change).** Done
overnight under the owner's authorisation (new repositories, merges, additive terraform apply):

- **Repositories** (public, Go stdlib only): `devthenet-labs/marigold-api` (ID `1402832070`) and
  `devthenet-labs/marigold-web` (ID `1402832143`); bootstrap PRs #1 squash-merged (`6255a073`, `4bb88ca4`). The API
  serves only under `/api` (`GET /api/greeting` → `{message, servedAt, revision}`) plus `/healthz`; the web app embeds
  its page and `app.js` calls `/api/greeting` from the browser, same-origin. CI `test` is uncredentialed, and main runs
  are grouped per commit and never cancelled. The trusted `workflow_run` publishers read every constant from repository
  variables and are gated separately (`AGENT_PUBLISH_ENABLED`, `PREVIEW_PUBLISH_ENABLED`, both `true`). Main runtime
  images also get `main-<sha>`.
- **terraform-devthenet #30** (`patchy-apps.tf`, merge `67db71a1`): a per-app map keyed by slug. Variable validation
  fails the plan on a bad or reserved slug, a bad name or ID, or duplicates, and a precondition keeps the agent and
  preview prefixes disjoint. Roles `devthenet-labs-app-<slug>-{agent,runtime}-push` trust the immutable subject, the
  repository and owner IDs, main and the kind's `job_workflow_ref`. Applied 20 added, 0 changed, 0 destroyed; a fresh
  plan shows no changes. Output `patchy_apps` holds each app's repository variables.
- **Images:** the merge pushes published `patchy/previews/marigold-api:{sha,main}-6255a073…` (`sha256:ef40c834…`) and
  `patchy/previews/marigold-web:{sha,main}-4bb88ca4…` (`sha256:c05c0978…`). The dispatched `agent image` runs published
  `patchy/app-envs/marigold-api:toolchain-v1` (`sha256:541b642e…`) and `marigold-web:toolchain-v1` (`sha256:9e79ccb4…`),
  which each `.patchy/agent.yaml` names. `patchy check image --run` at v0.12.13 passes on both.
- **Intents:** devthenet-labs/intents #6 added the `marigold` form, which applies `patchy:marigold` (label created). The
  README marks the project as not active yet.
- **Not done; waits for the slice-3 release:** the `marigold` Project and the Helm values (T-02).

**0.12.14 (slice 3 wave A) released and deployed; fresh-Finding gate and the first target-health preview PASSED
(2026-10-03, 09:47–10:45 UTC).** Release PR #94 (diff: version stamps and CHANGELOG only; its CI run approved, green)
squash-merged `b86897d`; the Release workflow published the images and both charts at 0.12.14. Live values equalled the
terraform-devthenet `k8s/` files (comments aside). patchy **rev 48**, patchy-config **rev 29** (rollback points **47 /
28**); both upgrades succeeded first time (44 s and 4 s, no network drop). The 0.12.14 render differs from 0.12.13 only
by the slot quota (pods 8, services 5), `PATCHY_PREVIEW_TARGET_HEALTH`, the tightened slot admission policies (the kept
placeholder still conforms) and the config checksums; patchy-config renders identically. Nine Deployments Ready on
v0.12.14 with zero restarts, logs with only the known broker-race and status-auth warnings, both Projects Ready, both
ALBs unchanged, status page 200.

- **Gate:** weak-key alert #40 (`slicegatekey.go`) → `finding-514becf18f-15` → issue #73 → `/patchy expedite` (10:25:04)
  → Remediating 10:26:49 → InReview 10:28:13 → PR #74 (pushedCommit = PR head `fca4e720`), checks green including
  CodeQL, merged `9df42243` → Remediated 4 s later, issue closed completed with the remediated label, one comment per
  marker, main CodeQL marked #40 fixed, no duplicate Finding. **The remediation committed its test**
  (`slicegatekey_test.go`, a ≥2048-bit assertion). Cost $0.58 (investigation $0.33, remediation $0.24).
- **Target-health Ready, live: Auto Mode honours the label.** terraform-devthenet #31 set
  `previewController.config.targetHealth: true` → patchy **rev 49** (rollback point 48; patchy-config stays 29). Both
  slot namespaces carry `eks.amazonaws.com/pod-readiness-gate-inject: enabled`. Intent `devthenet-labs/intents#7` ("Show
  the build time in the card footer") → `preview-demo-7`: plan 85 s, approved, build 145 s opened
  `devthenet-labs/patchy-preview-demo#10` at `4ddd96a1`. The planner's `#10` CHANGELOG guess was right, so `changelog`
  passed and no check-fix round ran. The Preview kept the single-component names (Deployment, Service and Ingress
  `preview-preview-demo-7` in slot 0). Its Pod was created with the readiness gate
  `target-health.eks.amazonaws.com/k8s-patchypr-previewp-b35644c0c5` (the TargetGroupBinding's name): ContainersReady
  10:40:18, gate True 10:40:20, Pod Ready 10:40:21, Preview Ready 10:40:23. Requesting the URL every second for 60
  requests from Ready got **zero non-200 responses** (slice 2 gave about 15 s of empty response or 404), and the ALB
  reported the target healthy. Squash-merged `71964ba9` → Merged 32 s later, issue closed completed, Preview deleted 13
  s after that, preview node gone and pool resources zero within a minute, old host 404. Intent cost $0.52 (518693
  µUSD).
- **Anomalies:** on the cold preview node the first Pod sandbox failed once with aws-cni
  `failed to setup network policy` and succeeded on retry about 20 s later. It failed closed, but it is the
  network-policy agent's cold-start race (see the open validation gap). The Pod sat in ImagePullBackOff for about 2 min
  until the trusted publisher pushed `sha-4ddd96a1`; this is expected, since the Preview follows the PR head before its
  image exists. The plan named `main.go` for the page template (it is in `server.go`; PR #9's title made the same slip);
  the build edited the right file. T-02 (the `marigold` Project and its Helm values) is still not done.

**Deployability phase 1 merged; 0.12.15 released and deployed; fresh-Finding gate PASSED (2026-10-03, 16:17–17:58 UTC;
overnight, owner asleep, under the standing permissions below).** Merged: #97 (W5: a Project with `checks.fix` is Ready
only once the App's checks, statuses and actions read are proven by minting), #100 (W2: the CLI's image repositories
stamped from the release registry; isolation probe parameterised), #101 (W6 `patchy setup github-app`), #99 (W9
`patchy check project`), #98 (W7 `deploy/terraform/aws` reference module), #102 (W8 `patchy init app`, its agent base
resolved from the stamped release registry), #104 (the flaky `TestMergeFromRenamedRepository`: `freePort` had handed one
port to both `--listen-addr` and `--health-addr`; now a no-repeat pool). Release PR #103 (diff: version stamps and
CHANGELOG only; both CI runs approved at the verified heads, green) → `8496b2f`; the Release workflow published images,
both charts and the CLI at 0.12.15. Live values equalled the terraform-devthenet `k8s/` files; the 0.12.14→0.12.15
renders differ only by versions and config checksums (patchy-config by versions only). No Job, Preview or active Intent
was running. patchy **rev 50** (45 s), patchy-config **rev 30** (3 s); rollback points **49 / 29**. Nine Deployments
Ready on v0.12.15, zero restarts, status page 200. Both Projects (both have `checks.fix`) were re-validated by the new
controller at start-up and stayed Ready, so the App's new read grants are proven live.

- **Gate:** weak-key alert #41 (`onboardgatekey.go`, seed `8df0b99`) → `finding-514becf18f-16` → issue #75 →
  `/patchy expedite` (17:53:50) → Remediating 17:55:58 → InReview 17:57:21 → PR #76 (`onboardgatekey_test.go`, a
  ≥2048-bit assertion, committed with the fix), checks green incl. CodeQL → merged `0b917e2b` 17:58:19 → Remediated 7 s
  later, issue closed completed with the remediated label, one comment per marker, no duplicate Finding.
- **Also merged after the release (in the next one):** #105 (W3: opt-in chart rendering of the DNS egress rule, the
  preview NodeClass/NodePool and the edge IngressClass, all default off and byte-identical when off; a read-only
  server-side diff against devthenet's hand-applied objects showed no spec change;
  `previewController.config.targetHealth` now defaults to true — devthenet already sets it). Do not flip
  `nodeIsolation.create` or `edgeIngressClass.create` on devthenet: Helm does not adopt the hand-applied objects. **Live
  onboarding of a freely named app (W10): PASSED (2026-10-03, 18:05–18:31 UTC).** With only released tools (CLI
  v0.12.15, cosign-verified): `patchy init app` scaffolded the template `devthenet-labs/app-template` (agent base
  resolved and digest-pinned by the CLI itself), and `devthenet-labs/Hello.Web` (mixed case and a dot on purpose;
  image/slug `hello-web`) was made from it. terraform-devthenet #32 (`patchy-hello-web.tf`, the reference `modules/app`
  at `?ref=v0.12.15`): plan reviewed (strict immutable-subject trust, push to its own repository only) and applied, 10
  added, 0 changed, 0 destroyed. Variables set per repository from the module's dotenv; `AGENT_PUBLISH_ENABLED`,
  dispatch, then `PREVIEW_PUBLISH_ENABLED`; both images published (the agent publish proved the trust with the
  mixed-case name). terraform-devthenet #33 added Project `hello-web` → patchy-config **rev 31**;
  `patchy check project hello-web` all PASS. Intent `intents#8` → plan 62 s → approved → build 2 min → `Hello.Web#1`
  (`test` green) → preview `hello-web-8` Ready (30/30 requests 200) → merged → Merged 20 s later, preview gone 19 s
  after, issue closed completed. 8 minutes, $0.55. The docs bugs it found go into W11 (PR 108).

**Slice 3 wave B merged; 0.12.16 released and deployed; gate PASSED; multi-repo live demo PASSED (2026-10-03,
19:00–20:19 UTC).** PR 95 (multi-repo intents, three review rounds plus a final verification of the last fixes) and PR
105 (W3 chart toggles) shipped in 0.12.16 (release PR 106: stamps and CHANGELOG only; CI approved at the verified head).
PR 109 (low follow-ups: a deleted Project's granted run hands its slot back; PushHeld names the current cause;
departed-repository reads documented and backed off; a transient read never ends an intent) merged after it, for the
next release. Render 0.12.15→0.12.16 with live values: only `PATCHY_INTENT_MULTI_REPO: "false"`, versions and checksums.
patchy **rev 51**, patchy-config **rev 32** (rollback **50 / 31**); nine Deployments on v0.12.16, zero restarts.

- **Gate:** weak-key alert → `finding-514becf18f-17` → issue #77 → `/patchy expedite` 19:58:56 → InReview 20:02:04 → PR
  #78 (with `multirepogatekey_test.go`) → merged 20:03:24 → Remediated 4 s later, no duplicate.
- **T-02 applied:** terraform-devthenet #34: `intentController.config.multiRepo: true`, `maxConcurrentRuns: 2`, Project
  `marigold` (`web` = marigold-web at `/`, `api` = marigold-api at `/api`, `checks.fix: [test]`). patchy **rev 52**,
  patchy-config **rev 33** (rollback **51 / 32**; turning the flag off is the supported rollback). All four Projects
  Ready; `patchy check project marigold` all PASS.
- **Multi-repo demo:** `intents#9` ("Greet the visitor by name") → one plan over both trees (4 min; 3 planner questions
  with sensible defaults) → approved 20:10:09 → both builds in parallel (api 2.5 min, web 3.5 min) → sibling PRs
  `marigold-api#2` and `marigold-web#2` with cross-link comments and the "one of 2" footer → one two-component Preview
  `marigold-9` Ready 20:16:31 (web at `/`, api at `/api`; valid, Unicode and rejected names behaved as planned; the page
  inserts with `textContent` only; 40/40 requests 200) → api merged first: the intent stayed InReview → web merged →
  Merged 38 s later, preview gone 12 s after, issue closed completed, summary listing both PRs. $1.51.
- **Done:** W11 operator guide merged (see below).

**0.12.17 released and deployed; gate PASSED; "Deployable by others" complete except the owner's decisions (2026-10-03,
20:30–22:44 UTC).** Merged: PR 108 (W11 operator guide: "Intents & previews" nav with deploying.md, onboarding-app.md
and the module reference, all seven W10 docs bugs including the `init app` next steps, a fresh-eyes validation against a
fictional org whose 19 unanswered questions and 33 review findings were fixed, then refreshed for 0.12.16 and
multi-repository Projects), PR 113 (W1: `preview.imagePathPrefix`, default `patchy/previews`, with agent/preview prefix
disjointness enforced by the chart render, the terraform module's validation and a new source-controller deny list set
by the chart; the CRD leaf is now a strict DNS label; default render byte-identical), PR 112 (the flaky
`TestBuildPackagesChangeset` cleanup: git auto-gc off in test repos), PR 114 (the account ID and the owner IP replaced
in current files by `111122223333` / `203.0.113.10`; history still has them). Release PR 110 (stamps and CHANGELOG only,
CI approved at the verified head, every job green). Render 0.12.16→0.12.17 with live values: only the source-controller
ConfigMap (`PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES` = the preview prefix) and the Project/Preview CRD patterns.
patchy **rev 53**, patchy-config **rev 34** (rollback **52 / 33**); nine Deployments on v0.12.17, zero restarts; all
four Projects Ready; status page 200.

- **Live W1 proof:** a server dry-run of a Project whose preview leaf is `app-` is now refused by the CRD pattern; the
  same with `app` is accepted.
- **Gate:** weak-key alert → `finding-514becf18f-18` → issue #79 → `/patchy expedite` 22:39:17 → InReview 22:42:10 → PR
  #80 (with `prefixgatekey_test.go`) → merged 22:43:27 → Remediated within a second, issue closed completed (tracking
  state caught up a few seconds later), no duplicate.
- **Process slip, recorded:** a background waiter merged the docs-only PR 111 although `ci / test` was red (the flaky
  test PR 112 then fixed). Waiters now merge only when every check concluded SUCCESS/SKIPPED/NEUTRAL.

**Next (owner's choice):** external-dns support (W4, not built; terraform alias records are the documented path);
rewriting git history to drop the old account ID and IP; whether a permanently unreachable but still-listed repository
should stop blocking an ended intent's hand-off; whether a removed repository's unreadable, last-seen-open pull request
should keep an intent in review; the admission policy's preview leaf regex is deliberately wider than the CRD's
(tightening it changes the live VAP). Small follow-ups: `patchy init app --preview-prefix`; the module's `helm_values`
does not emit `preview.nodeIsolation.*`; intent-controller never posts the preview URL on the PR or issue; AGENTS.md
still mentions a Homebrew cask for the fork's CLI.

## Previous checkpoint — 2026-10-02 (Codex stage-1 handover; historical)

**Stop here for handover. Stage 1 is complete; stage 2 has not been approved or started.** The preview foundation is
enabled only to stage retained guardrails. No preview workload, placeholder Service/Ingress, preview ALB, wildcard DNS
record, Preview CR, or `preview-demo` Project exists. `previewController.enabled` is false. Do not create the
placeholder Ingress or enable the controller until the owner approves the respective future steps. The older "Next
steps" and "Preview update" sections below are historical; this checkpoint supersedes their live-state claims.

### Merged, released, applied

- patchy PR #76 merged the Preview CR, fixed-slot preview-controller and Intent-to-Preview projection; release PR #77
  published **0.12.10**. PR #78 merged the kept, selectorless placeholder Service/Ingress and exact-name Helm annotation
  exception; release PR #79 published **0.12.11**. A live server-side dry run found that the _retained old_ slot
  admission policy would reject the placeholder during a direct enablement upgrade. PR #80 therefore added
  `preview.placeholder.enabled` (default true) so admission can be upgraded first with the placeholder off; release PR
  #81 published **0.12.12**. Both OCI charts and images are published. All local gates, separate security/liveness
  reviews and CI passed on the chart PRs; release-please CI approvals followed the standing diff-only rule.
- Live cluster: `patchy` chart/app **0.12.12, Helm revision 42**; `patchy-config` chart/app **0.12.12, revision 25**.
  The 0.12.12 release was first deployed with both preview switches false (revisions **41/25**, pre-upgrade rollback
  points **40/24**). Terraform values PR #25 then merged (`173b66f`) and a _patchy-only_ upgrade made revision **42**
  with `preview.enabled: true`, `preview.placeholder.enabled: false`, `previewController.enabled: false`, and
  `nodeIsolation: {nodePool: patchy-preview, nodeClass: patchy-preview, taintKey: patchy.devthe.net/preview-only}`. The
  pre-stage-1 rollback points were **41/25**. Re-record both live revisions before any future upgrade; if stage 2 is
  approved, **42/25** are the expected rollback points, not a substitute for a fresh check. Live Helm values match the
  merged `k8s/patchy-values.yaml` plus TLS overlay and `k8s/patchy-config-values.yaml`.
- Terraform PRs #18 (preview ECR repositories and split publisher OIDC roles), #19/#20 (certificate request and its
  ACM-compatible correction), #23 (minimal preview node IAM role/policy/access entry), and #24 (runtime image lifecycle)
  are merged; the corrected infrastructure is applied. The wildcard preview certificate is **ISSUED**. The runtime
  repository `patchy/previews/patchy-preview-demo` now expires untagged images after 14 days and `sha-` tagged images
  after 30 days; the agent-toolchain repository is outside that tagged rule. PR #25 changed Helm values only: no
  Terraform resources were applied for stage 1. A fresh `AWS_PROFILE=devthenet terraform plan -detailed-exitcode` on
  2026-10-02 exited **0: no changes** (only unrelated provider deprecation warnings). The separately applied
  `k8s/auto-mode-preview-node.yaml` defines a Ready `patchy-preview` DefaultDeny NodeClass and tainted NoExecute
  NodePool, with `spec.limits.cpu: "4"` and `spec.limits.nodes: "2"`, currently **zero nodes**. Treat CPU as the hard
  spend bound; do not assume Auto Mode enforces the `nodes` limit without re-checking. The demo repo's trusted runtime
  publisher remains enabled; no preview runtime is running.
- The 0.12.11 fresh-Finding gate completed: CodeQL alert #36, Finding `finding-514becf18f-11`, issue #65, fix PR #66,
  merge `291bd486bc541762618354399f0cf302dac10431`. The 0.12.12 gate also completed without manual correction: alert
  #37, `finding-514becf18f-12`, issue #67, fix PR #68, merge `c4b37565d30ad9c5cb1ad2b79d775d09703ac87d`. In each gate
  the remediation pushed SHA matched the PR head, Go/CodeQL checks passed, the Finding reached Remediated with the merge
  SHA and no ReviewClosePending, the issue closed as completed with the remediated label, each patchy comment marker
  appeared once, and main CodeQL fixed the alert without a duplicate Finding. No fresh Finding was seeded after the
  values-only stage-1 revision.

### Stage-1 live verification and rollback

- All eight existing Deployments are 1/1 Ready with zero restarts; post-upgrade controller logs had no errors, panics or
  failed reconciles. The `target` Project is Ready. Both slot namespaces have their `preview-isolation` NetworkPolicy
  and preview admission bindings, but no Deployment, Pod, Service or Ingress. No active Job remains in `patchy-agents`,
  and no probe is running. The dedicated NodePool is Ready at zero nodes.
- A server-side dry run **accepted** only the rendered `patchy-preview-placeholder` Service and Ingress in slot 0 with
  the exact Helm ownership/keep annotations. Dry runs **denied** the same annotations on a differently named Service and
  Ingress in the slot, and denied `alb-preview` on an Ingress in `patchy`. This verifies the kept-policy staging fix
  without creating the placeholder. AWS still returns `LoadBalancerNotFound` for `devthenet-dev-preview`; no preview ALB
  or wildcard DNS exists. The existing webhook/status ALB kept ARN
  `arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/devthenet-dev/35727d407a91ad54`, DNS
  `devthenet-dev-806232275.us-east-1.elb.amazonaws.com`, and creation time `2026-09-22T17:41:15.593Z`;
  `https://status.patchy.devthe.net/` returned HTTP 200. No new App-delivery probe was run for stage 1; verify webhook
  deliveries after the later ALB step as agreed.
- Stage-1 rollback, if required: roll patchy back to revision **41** (patchy-config remains 25). Helm `keep` annotations
  mean slot namespaces/guardrails may remain; inspect and deliberately drain before any explicit deletion. Never assume
  `helm rollback` or uninstall silently removes a slot with workloads. The preview controller is off, so stage 1 cannot
  schedule a preview.
- Local checkout caveat for the next agent: `/Users/peter/code/patchy` has pre-existing, untracked `data/`, `debug.log`
  and `info.log`; the original terraform-devthenet checkout has untracked `AGENTS.md`, `debug.log` and `info.log`. These
  are not preview changes and were deliberately left untouched. Task branches and `/tmp` worktrees were audited; see the
  final handover report for their remote-HEAD states. Work in a clean branch/worktree and preserve those files.

### Open items and next steps — in this order

1. **Ask for the owner's separate stage-2 ALB approval before changing anything.** The proposed change is only
   `preview.placeholder.enabled: false -> true` in `k8s/patchy-values.yaml`, followed by a separate patchy Helm revision
   using 0.12.12. It renders the kept, selectorless placeholder Service and `alb-preview` Ingress in slot 0; this starts
   the agreed estimated ~$25–35/month incremental ALB cost even without workloads. Re-record rollback revisions first.
   Keep `previewController.enabled: false`. Verify the new ALB name/DNS, certificate, HTTPS listener, exact
   `203.0.113.10/32` inbound CIDR, empty target group, zero preview nodes/workloads, unchanged shared ALB ARN/DNS/
   creation time, 2xx GitHub App Recent Deliveries to `patchy.devthe.net`, and the status page. On failure, inspect
   empty slots, explicitly delete the **named** kept placeholder Ingress/Service to remove the ALB, then roll back to
   the guardrails-only revision; Helm rollback alone will not delete kept resources. Check in before this apply.
2. **Wildcard DNS after the preview ALB exists:** prepare the Route53 alias Terraform PR/plan for
   `*.preview.patchy.devthe.net` against the actual new ALB DNS/canonical zone, review the plan and rollback with the
   owner, and do not apply until separately approved. The preview cert is ready; DNS is not present.
3. **Then the `preview-demo` Project:** configure only `devthenet-labs/patchy-preview-demo` in patchy-config, enable the
   preview-controller/Intent preview projection in a separate approved Helm revision, and run the live preview
   demo/cleanup and fresh-Finding gate. **Never preview patchy-target.** Keep the preview pool at zero when idle.
4. **Known validation gap:** direct EKS Auto Mode network-policy-agent logs were unavailable during the accepted
   first-instant, Deployment-managed cold-start isolation probe. The observed network behaviour passed, but the
   `no bpf context registered` agent-log check remains **open**. Use `hack/preview-isolation-probe/README.md` and
   `run.sh` (merged on main); re-run its disposable, unmerged PR-image probe after any EKS, Auto Mode or VPC CNI upgrade
   and before relying on previews again. Any forbidden success is a security failure: remove the probe Deployment, keep
   previews unused, and investigate. Obtain managed-agent/node diagnostics later to close the log gap. Intent CI
   check-fix has not been exercised live; Finding PR CodeQL fix rounds remain a separate follow-up below. The
   intent-controller advisory broker-probe cleanup **is already implemented**: it uses
   `runnercfg.ResolveWithoutBrokerProbe`, with a regression test asserting no controller-side broker readiness call.

## Historical checkpoint — 2026-09-27

The `__Host-` cookie prerequisite (PR #67) is merged and released by PR #68 as **0.12.7**. Live: patchy revision **34**,
patchy-config revision **20**, intents enabled. Pre-upgrade rollback points were **33 / 19** (0.12.6). All eight pods
are Ready, target is Ready, and status.patchy.devthe.net returns HTTP 200. OIDC is not configured on this cluster, so no
live sign-in was claimed. Known advisory broker-startup warnings are accepted; no errors, panics, CrashLoops or failed
reconciles were observed.

The fresh-Finding gate passed without manual correction: alert 31, `finding-514becf18f-6`, issue #55, repair PR #56,
merge `9f2f08c4fff7e2fd29fb261f0a8d212aedae398a`. Go/CodeQL passed; the Finding reached Remediated with that merge SHA
and no ReviewClosePending; the issue closed as completed with the remediated label; each marker occurred once; main's
CodeQL re-analysis fixed the alert without a duplicate Finding. Existing Finding/Intent phases were unchanged.

The default `alb` namespace prerequisite is now applied (terraform-devthenet PR #17, merge `e784fd3`): its selector is
exactly `spec.namespaceSelector.matchLabels: {kubernetes.io/metadata.name: patchy}`, the automatic namespace label.
Applied 2026-09-27 16:58:05 UTC after the owner's specific approval. Both ingresses reconciled successfully; ALB ARN,
DNS and creation time stayed unchanged, targets remained healthy, and Route53 aliases were unchanged. A fresh probe on
completed target issue #55 returned 202 in App Recent Deliveries; all 36 deliveries in the checked apply window were
2xx. Status root and rollup API returned 200; visual verification was unavailable (no connected browser).

The post-ALB fresh-Finding gate passed without manual correction: alert 32, `finding-514becf18f-7`, issue #57,
[repair PR #58](https://github.com/devthenet-labs/patchy-target/pull/58), merge
`fcdeb7d60252707606ee3e72d49bf5538b5c067d`. Go/CodeQL passed, Remediated with mergeCommitSHA, no ReviewClosePending,
closed/completed/remediated issue, unique markers, and no duplicate after main's re-analysis fixed the alert. Existing
Finding/Intent phases stayed unchanged. The 17:07:41 UTC sweep scanned 121 deliveries with no error; zero failures in
the apply window meant no redeliveries were needed (failure recovery was not exercised).

Rollback: restore `k8s/auto-mode-alb.yaml` from terraform commit `952b675439c5a3e85eec65f160caa1ac48c90037` and apply
that manifest. If the ALB is ever replaced, the owner also requires a DNS repair: `patchy.tf` defines
`aws_route53_record.patchy_webhook` and `aws_route53_record.patchy_status`, both using `data.aws_lb.devthenet_dev`.
Refresh the lookup and prepare a plan targeting the replacement ALB's DNS/canonical zone, **check in before any
Terraform apply**, then verify endpoint recovery and that the normal redelivery sweep re-requests failed-window
deliveries. Do not force a full replay/reset. No replacement occurred in this apply; no Terraform/DNS repair was used.

Evidence is in `/tmp/patchy-alb-gate.cbeLEb/rollout.md`. No preview namespaces, preview ALB or demo resources exist yet.
The approved preview scope, individual infrastructure check-ins and safeguards below still apply.

The owner says to **leave Actions settings and release credentials unchanged**. Release-please currently uses
GITHUB_TOKEN; GitHub makes its PR-triggered CI runs approval-required. This is not caused by the external-fork approval
setting. The standing permission for these run approvals is recorded below; it does not waive any merge gate.

## Previous checkpoint — 2026-09-26

Slice 1a/1b code is merged and live on **0.12.6**, patchy revision **33**, patchy-config revision **19**, intents
enabled. PR #65 fixed PR-comment permissions and durable notices; the missing round-1 notice on target PR #46 was
recovered exactly once. The new human review completed a successful revision
($0.295620), the owner merged PR #46,
target-2 reached Merged, and intents issue #2 closed as completed with one summary. Its total reported cost was
$1.150306.
The automatic check-fix path remains **unexercised live**: the revision's checks passed on its first push.

The post-demo fresh-Finding gate passed: alert 30, `finding-514becf18f-5`, issue #53, repair PR #54, merge
`bf707b46c24c68889288e6b3d91e1709ffaef247`. Go/CodeQL checks were green, the Finding reached Remediated with its merge
SHA and no ReviewClosePending, the issue closed with the remediated label, markers were unique, and main's CodeQL
re-analysis fixed the alert without a duplicate Finding. No manual correction was needed for this gate.

**Slice 2 is approved, starting with prerequisite PRs, cookies first.** The owner approved the new benign repository
`devthenet-labs/patchy-preview-demo`, Project `preview-demo`, and the light-use incremental budget of ~$25–35/month
(including the two-AZ ALB's public IPv4 charges, assuming existing node capacity). Preview inbound CIDRs must be exactly
`203.0.113.10/32`. Never preview patchy-target.

- Check in with the owner **before each infrastructure apply**, including Helm upgrades; the general approval does not
  authorise unattended applies. Record current rollback revisions again before every upgrade.
- Before any preview namespace exists: release/gate the `__Host-` cookie prerequisite, then restrict the existing `alb`
  IngressClassParams to namespace `patchy`. Verify unchanged ALB identity, successful **2xx GitHub App Recent
  Deliveries** to patchy.devthe.net, and that status.patchy.devthe.net loads after that change.
- Runtime image builds have no credentials and **no id-token access**. Fork PRs must never publish. The separate
  publisher must never execute PR code and can write only its own immutable runtime ECR repository, never app-envs.
- Use reviewable draft PRs, full local/CI gates, a separate security/liveness review, releases and fresh-Finding gates.
  No preview infrastructure has been applied yet. Cookie work starts on `fix/status-host-cookies` from main `be00e9a`.

## Historical checkpoint — before the 0.12.6 release

This checkpoint supersedes the historical implementation status below; the permissions, gates and security invariants
still apply. Slice 1a and slice 1b are merged. PRs #61 and #63 repaired GraphQL-only edit detection, empty feedback and
the expired legacy Job. Both releases passed fresh-Finding gates. Live is 0.12.5, patchy revision 31 and config revision
18, with intents disabled after rolling back enablement revision 30 to revision 29.

The remaining live fault is PR-comment token scoping: target-2 settled back to InReview with zero revisions charged, but
its round-1 notice on patchy-target PR #46 received 403 and was discarded. The existing review is consumed and must not
replay. The owner has authorised a permission audit, scope correction, durable notice recovery, full gates/review,
merge/release/fresh-Finding gate, then re-enablement and exactly-one-notice verification before a new human review.

See [the permission audit](docs/design/intent-github-permissions.md) for non-writing live probe results. The fix adds
`roundNoticesThrough` to Intent status; it recovers old missing notices without launching work or charging revisions.
Keep intents disabled until that fix is released and gated. Before enabling, record both Helm rollback revisions.

Historical status as of 2026-09-24; the current checkpoint at the top supersedes this and the 2026-10-01 update. Written
so another coding agent can continue without the previous session's context. Read this whole file, then `AGENTS.md`
(orientation), then `docs/design/intent-driven-development.md` (the accepted design — the source of truth for what to
build).

## Historical preview update (2026-10-01; superseded by the current checkpoint above)

- Slice 1a and 1b are merged and live. The preview prerequisites are live through patchy 0.12.9 (Helm revision 38) and
  patchy-config 0.12.9 (revision 22). `preview.enabled` is **false** in the live Helm values and in terraform-devthenet.
  Do not enable it while developing the preview-controller. The dedicated `patchy-preview` DefaultDeny NodeClass and
  tainted NodePool are Ready and scale to zero. No preview ALB exists yet.
- The first-instant isolation result is accepted as passing by the owner: three separate cold starts of a disposable,
  unmerged preview-demo PR image on Deployment-managed Pods (slot 0, slot 1, slot 0) each recorded 128 blocked
  connections and no reachable or inconclusive forbidden target. The targets were IMDSv2 token PUT (status only), Pod
  Identity, Kubernetes API, patchy services and broker, and the internet; DNS succeeded after policy programming. The
  probe Deployments were deleted, the node pool returned to zero, and the PR was closed unmerged.
- **Open validation gap:** the EKS Auto Mode network-policy agent logs were not directly available. No
  `no bpf context registered` error was seen in the Kubernetes events or available control-plane logs, but that is
  **not** an agent-log check. Do not report the agent-log check as passed. Obtain node diagnostics or managed-agent
  delivery in a later validation and close this gap explicitly.
- **Standing rule:** re-run the disposable-PR-image, Deployment-managed **cold-start isolation probe** after every EKS,
  EKS Auto Mode or VPC CNI upgrade, and before relying on previews again. The re-run procedure and probe source are in
  `hack/preview-isolation-probe/README.md`; never merge the probe into preview-demo's main branch. Any forbidden success
  is a security failure: remove the probe Deployment, keep previews unused, and investigate.
- At this 2026-10-01 checkpoint, ECR still expired only untagged images. Terraform PR #24 later added and applied 30-day
  `sha-` tagged-image expiry; see the current checkpoint above.
- Next: implement the Preview CR and preview-controller, the Intent-to-Preview PR-head projection, and their chart
  wiring, tests and release with preview disabled. Check in and get approval **before** applying the preview ALB,
  placeholder Ingress or wildcard DNS. The user's later instructions supersede the historical next-step list below.

## Running this work: environment and permissions

- **Run locally, with network.** Work in `/Users/peter/code/patchy` on the owner's machine, not a cloud sandbox. The
  gates download tools, Go modules and envtest binaries. The live steps need the owner's `gh` login (`brvtl`), `kubectl`
  against `devthenet-dev` with `AWS_PROFILE=devthenet`, and SSH git pushes. In Codex CLI that means a mode with network
  access that may run these commands. In a sandbox without them the gates fail and the live checks silently do not
  happen, and that must never be reported as passing.
- **Authorised so far without asking:**
  - branching and pushing;
  - opening PRs and squash-merging them once CI is green and review findings are resolved;
  - merging release-please PRs;
  - approving a release-please PR's CI run without asking, **only after checking the current PR diff contains nothing
    except release-please version stamps and CHANGELOG changes**. Verify the exact head being approved; re-check if it
    changes. If any other change is present, stop and ask rather than approving. Leave Actions settings and release
    credentials unchanged; CI must still pass before merge;
  - `helm upgrade` and `helm rollback` of the two releases on `devthenet-dev`;
  - pushing deliberate test vulnerabilities to `devthenet-labs/patchy-target`, and merging patchy's own fix PRs there
    during a gate;
  - posting commands on patchy's own test issues.
- **Ask the owner first** before anything else outward-facing: new repositories, GitHub App permission or webhook
  changes, `terraform apply`, deleting branches, repositories or cluster resources, or changing who is an approver.
- **Review without the old tooling.** The previous agent had independent reviewers plus two refuters check every serious
  finding. Instead, review your own diff in a separate pass (for example Codex `/review`, or a fresh session) focused on
  the security invariants below and on liveness (can an Intent get stuck, can a run hold a slot forever, can anything
  hot-loop against GitHub). Check each finding against the code before acting on it.
- **Report honestly.** Give failing tests with their output, state skipped steps plainly, and never call something
  verified that was not run.

## The goal

patchy started as a security pipeline (CodeQL alert → `Finding` → sandboxed `claude -p` investigation and remediation →
PR). The owner's goal is **intent-driven development**: a human opens an issue in an _intent repo_; patchy plans it and
posts the plan; an approver approves by label (or `/patchy approve`); patchy builds the plan in the project's app repo
and opens a PR; the PR branch is deployed as a _preview_ on a subdomain; PR review (and failing CI checks) trigger
revision rounds; merge closes the loop. It must work for several projects in one GitHub org.

Decisions already made (see the design's "Decisions" section): a separate, default-off `intent-controller` with its own
`Project` / `Intent` / `IntentRun` CRDs and local phase enums (the Finding state machine is untouched); one org intent
repo with one `Project` CR per project; slice 1a (plan → approve → build → PR → merge), then slice 1b (revise rounds
from a "Request changes" review or `/patchy revise`, plus automatic fix rounds for failing CI checks), then slice 2
(previews via a slot-pool `preview-controller` on a separate IP-restricted ALB). One GitHub command grammar,
`/patchy <verb>`, for Findings and intents, authorised by **write access** (the collaborator-permission API; public
repos report `read` for everyone, so `read` is never enough).

## Security invariants (do not weaken)

- Agent pods hold no credentials; model traffic goes through the egress broker; all GitHub writes are controller-side
  with per-operation, single-repository scoped tokens.
- The approver sees the plan **verbatim** (the exact bytes, in a code fence no line can close); plans containing
  invisible/bidi/tag characters or layout padding are refused; the build agent receives **only** the approved plan
  (re-hashed at launch; `issue.md` is empty for builds).
- Authority comes only from GitHub API facts: labeled-event actors and comment authors who are write+ **and** in the
  Project's approvers, never bots or the App's own `patchy-devthenet[bot]`. An edited comment never counts (GraphQL
  `lastEditedAt`/`includesCreatedEdit`, verified live: someone with write access can edit another user's comment while
  REST still shows the original author).
- Agent text reaching GitHub goes through `internal/templates` renderers; PR title/body/commit messages are defanged as
  **plain text** too (squash merges copy them into commits, where code spans protect nothing).
- Intent changesets refuse `.github/**`, `.patchy/**`, `.devcontainer/**`; builds only run in an accepted
  repository-declared image; pushes are create-only then fast-forward-only — never force, never the default branch.
- Webhook handlers are acked (202) before they run and never retried: they must not depend on a GitHub call. Record
  intent/command state in CR status and settle it in a reconciler with backoff (see
  `internal/controller/integration/review.go` and `commands.go`).

## Where everything is

| Thing                           | Where                                                                                                                                                                                                                                                                                                    |
| ------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| patchy fork (work here)         | `devthenet-labs/patchy`, local `/Users/peter/code/patchy`. `upstream` (bitwise-media-group) is read-only. Always pass `--repo devthenet-labs/patchy` to `gh` (bare PR numbers can resolve to upstream).                                                                                                  |
| Infra (terraform + Helm values) | `/Users/peter/code/DevTheNet/terraform-devthenet` (remote `brvtl/terraform-devthenet`), Helm values in `k8s/`. AWS profile `devthenet` (`~/.aws`).                                                                                                                                                       |
| Cluster                         | EKS Auto Mode `devthenet-dev`, us-east-1. Namespaces `patchy`, `patchy-agents`, and guarded empty slots `patchy-preview-0/1`. `export AWS_PROFILE=devthenet`.                                                                                                                                            |
| Helm releases                   | `patchy` (chart `oci://ghcr.io/devthenet-labs/patchy/charts/patchy`, values `-f k8s/patchy-values.yaml -f k8s/patchy-values-tls.yaml`) and `patchy-config` (`.../charts/patchy-config`, `-f k8s/patchy-config-values.yaml`). Current live state and rollback points are in the current checkpoint above. |
| Hostnames                       | `patchy.devthe.net` (webhooks `/github/webhooks`), `status.patchy.devthe.net`.                                                                                                                                                                                                                           |
| GitHub App                      | `patchy-devthenet` (id 5036888), installation 163854331 on all devthenet-labs repos; credentials in Secret `patchy/patchy-github` (keys `appID`, `privateKey`, `webhookSecret`).                                                                                                                         |
| App repo (demo target)          | `devthenet-labs/patchy-target` (deliberately vulnerable Go server; its agent image is `patchy/app-envs/patchy-target` in ECR, pushed by the per-app role `devthenet-labs-app-env-push-patchy-target`).                                                                                                   |
| Intent repo                     | `devthenet-labs/intents` (private): README, issue form `.github/ISSUE_TEMPLATE/target.yml` applying `patchy:target`, labels `patchy:target` and `patchy:approved`.                                                                                                                                       |
| Scratch repo                    | `devthenet-labs/patchy-smoke` (private) for API experiments.                                                                                                                                                                                                                                             |
| Plans (git-ignored)             | `.claude/plans/intent-wave3-brief.md` (the controller contract), `.claude/plans/intent-dev-understand-maps.md` (codebase maps). Local only.                                                                                                                                                              |

## Where the work stands

Merged to `main` and live-gated (each wave released, deployed and proven with a fresh live Finding):

- Wave 0 fixes (#36–#39), design (#35), per-app ECR push roles (terraform #14, patchy-target #29).
- Wave 1: `/patchy` command parser (#41), `Project`/`Intent`/`IntentRun` CRDs (#42), GitHub seams (#43).
- Wave 2: in-pod `plan`/`build` stages (#45), templates/sanitiser/`internal/changeset`/`PinFor` (#46), NaN confidence
  fix (#47), Finding `/patchy` commands with write-access authorisation (#48, BREAKING), refusal-quiet fix (#50, merged,
  **not yet released**).

**In flight — PR #51 `feat(intent): intent-controller for slice 1a (core, wiring, e2e)`** (draft, branch
`feature/intent-controller`, head `8019f6c` = the round-2 fixes plus a merge of `main`): the controller, chart /
kustomize / goreleaser wiring, CLI kinds, docs, and an envtest e2e driving an intent from issue to merged PR against
fakegithub. All gates and CI were green at `8019f6c`. It has had three review rounds: round 1 found 20 confirmed issues
(including a high: an approver's `/patchy approve` forged by someone with write access editing the approver's comment),
round 2 found 13; both rounds are fixed on the branch. **Round 3 was stopped before its fixer changed anything** (to
save tokens for the handoff): its six findings below were each confirmed by two independent refuters and are **not yet
fixed**. They are the first job.

1. **Intent stays stuck in Building when its pushed branch is deleted before the PR is opened** (medium;
   `internal/controller/intent/intent_build.go:129`)

   - Scenario: openPullRequest assumes patchy-intent/<intent> still exists at the build's commit. If a human deletes the
     branch after the build pushed it and before the PR is created, CreatePullRequest gets GitHub's 422 (head invalid).
     That error is returned unclassified: openPullRequest wraps it, building() returns it, and the reconcile retries
     with backoff indefinitely. Nothing blocks the Intent, fails it, or sets a condition. The round's latest run is
     RunComplete, so every retry goes back to openPullRequest and never launches a new build that would push the branch
     again. Concrete path that the design itself invites: after a build pushes, someone opens a PR from the intent
     branch. openPullRequest blocks the Intent with BranchConflict/ForeignPullRequest, whose message says 'Close it to
     resume.' The human closes that PR and clicks GitHub's 'Delete branch' button, or deletes the branch directly, which
     also auto-closes the PR. branchBlockHolds sees no foreign PR, and blocked() resumes to Building without a new run
     (done == true). openPullRequest then fails CreatePullRequest with 422 on every pass. The status comment keeps
     saying Building. Only `/patchy cancel` gets out, and the approved build is lost. The same wedge follows any refused
     PR create, for example a 403 after the App loses pull_requests:write.

   - Suggested fix: Before CreatePullRequest, read the branch head (HeadSHA). If the branch is missing or not at the
     run's PushedCommit, do not retry blindly: settle the round as a failed attempt (e.g. a new outcome such as
     `branch_missing`) so building() launches the next attempt, which pushes the branch again through
     blockOnBranch/createBranch. Alternatively block with a BranchConflict reason that says so. Also classify
     ghclient.IsRefused(err) from CreatePullRequest as terminal for the round (block or fail with a condition) rather
     than an endless transient retry.

2. **One approve label or /patchy approve approves every Project's Intent on the same issue** (low / security;
   `internal/controller/intent/project_controller.go:240`)

   - Scenario: The multi-project model (D3) shares one intent repository across Projects that differ only by trigger
     label. The approve label defaults to the global `patchy:approved`, and `/patchy approve` names no intent or plan.
     validate() rejects only Projects that share both the intent repository and the trigger label. Nothing scopes an
     approval to one Intent. When one issue carries two projects' trigger labels, discovery creates two Intents (a-<n>
     and b-<n>). Each posts its own plan comment. Each approveLabelAction and each command settle independently accepts
     the same labeled event or comment: the actor is authorized against its own Project, the approval is after its own
     PostedAt, and its own plan comment and the issue are unchanged. Concrete scenario: approver Y (Project B only)
     triggers B on issue 5, which already has A's intent. Approver X, who is on both approver lists, reads A's plan and
     follows its instructions ('Add the patchy:approved label ... patchy then builds exactly the plan below'). Intent
     b-5 is approved too. B's plan, which X never read, is built in B's app repository and a PR is opened under
     'approved by X'. Conversely, when the actor is not one of B's approvers, B refuses the shared label and removes it.
     This can race A's acceptance and silently drop X's label approval of A. The plan-comment wording ('builds exactly
     the plan below') and the 'approval bound to what was shown' guarantee do not hold in this setup. The final merge is
     still human-gated.

   - Suggested fix: Scope approvals to one Intent. Options: make validate() report AmbiguousIntentRepository when two
     Projects on the same intent repository share an approve label (and default the approve label to a per-project name
     such as `patchy:<project>:approved`). Or refuse, or report a conflict for, an issue that carries more than one
     Project's trigger label, with discovery creating at most one Intent per issue. Or require `/patchy approve` to name
     the plan revision/digest when more than one Intent is on the issue.

3. **Replan feedback admits approver comments edited in the second they were posted** (low / security;
   `internal/controller/intent/intent_plan.go:122`)

   - Scenario: feedbackSince filters edited comments with edited(c), which only compares REST updated_at with created_at
     at second resolution. Round 2 added the GraphQL check (everEdited: lastEditedAt/includesCreatedEdit) precisely
     because an edit within the posting second leaves updated_at == created_at. That check is applied to commands and to
     the plan comment, but not to the comments that go into a replan's snapshot. The package doc ('For the same reason
     an edited comment never reaches a replan's snapshot') and docs/configuration/intent-controller.md ('An edited
     comment is also left out of a replan's approver comments') both promise more than the code does. Concrete scenario:
     a write-access non-approver runs a webhook-driven bot that rewrites an approver's new comment within the same
     second it was posted. The rewritten text passes edited() and is rendered into the replan snapshot under the
     approver's login, as 'the approvers' comments ... data about what to build'. The planner receives text attributed
     to an approver that the approver never wrote. Impact is limited: an approver must still approve the resulting plan,
     and a writer can already influence the snapshot through the issue body. It is still a gap in a stated authority
     guarantee.

   - Suggested fix: In feedbackSince, after the cheap edited(c) filter, call p.everEdited(ctx, c) for each remaining
     approver comment (bounded by maxFeedbackItems) and drop any that GitHub records as ever edited or that are gone
     (ErrNodeNotFound). Alternatively, reword the docs to state that only REST-visible edits are excluded from feedback.

4. **intent-controller caches every Finding Repository in the release namespace, so its memory grows with the Finding
   backlog** (medium; `cmd/intent-controller/serve.go:202`)

   - Scenario: The manager limits only the ConfigMap informer, using the intent label (the round-1 fix ddbc899). The
     RunReconciler also calls `Watches(&v1alpha1.Repository{}, mapRunChild)`
     (internal/controller/intent/run_settle.go:205), and several code paths read Repositories through the cached client:
     launchable, pending, launch, deletePlanRepository and headUnmoved. So intent-controller starts one Repository
     informer covering the whole release namespace. The investigation gate creates one Repository per admitted Finding
     (gate_controller.go:184, owner-referenced to the Finding). Those Repositories stay until the Finding is deleted.
     Each object in the informer holds conditions, artifact URL and digest, and runnerImage, and costs a few KiB in
     memory. intent-controller itself only ever needs the handful of Repositories that carry LabelIntent/LabelIntentRun.
     Failure scenario: the project is sized for brownfield estates (docs/dev/benchmarks.md targets 100k to 2M findings).
     An operator on such a cluster has already raised the Finding controllers' limits, because values.yaml tells them
     to. They then set intentController.enabled=true, with its default 256Mi limit and no sizing note. The informer must
     list, for example, 100k Finding Repositories (roughly 200-400MiB) before the cache syncs, so the pod is OOMKilled
     before it serves anything, even with zero Projects. The crash loop compounds the damage: blockedAt is in memory, so
     every restart lifts each Blocked intent's image block. Each lift creates a new build attempt and downloads a new
     tarball, which burns the 16 attempt ordinals until those intents go Failed.

   - Suggested fix: Scope the Repository informer by label the same way as ConfigMaps. Generalise
     kube.Options.ConfigMapSelector into a per-kind label-selector map, or add a RepositorySelector, and have
     intent-controller select `patchy.bitwisemedia.uk/intent` Exists. Every Repository intent-controller creates already
     carries LabelIntent. A foreign object under a derived name is already read through the APIReader in
     ensureRunChildren. Add a manager_test case and a runs_test case for this, like TestEveryConfigMapIsSelected. At
     minimum, add the backlog-sizing note to intentController.resources in values.yaml.

5. **The agents-namespace Role gives intent-controller unrestricted Secret get/update/delete, contrary to the documented
   resourceNames-only posture** (low / security; `charts/patchy/templates/intent-controller.yaml:169`)

   - Scenario: The chart template header, NOTES.txt ("It reads only the Forge Secrets named in
     intentController.forgeSecrets"), docs/configuration/intent-controller.md ("`secrets get` is restricted by
     `resourceNames` to the Secrets your Forges reference"), deploy/README.md, docs/deployment/kustomize.md, DESIGN.md
     and AGENTS.md all describe intent-controller's Secret access as limited to the named Forge Secrets. That is true
     only in the release namespace. The copied agent-jobs Role (`patchy-intent-controller-jobs`, also
     deploy/kustomize/components/intent-controller/rbac.yaml) grants `secrets: [create, get, update, delete]` in
     agent.namespace with no resourceNames. Failure scenario: a cluster runs codex or copilot for findings, or sets
     agent.repositoryImages.pullSecretData. Its patchy-agents namespace then holds `patchy-openai` / `patchy-copilot`
     model keys and the `patchy-registry` dockerconfigjson. It also holds every Finding Job's handoff Secret, which
     contains vulnerability details and the approved investigation.md. intent-controller is the component that polls
     untrusted GitHub issue content. If it is compromised, it can read all of these Secrets. It can also rewrite a
     pending remediation Job's handoff Secret before the pod mounts it, which steers a Finding remediation that
     remediation-controller then pushes with the forge write credential. An operator who audited the documented posture
     would not expect any of this. The Finding job controllers hold the same grant, so this is not an escalation over
     them, but it contradicts the stated 'tightest posture' boundary.

   - Suggested fix: State in every place listed above that the resourceNames restriction applies to the release
     namespace only. Say that in the agents namespace intent-controller has the full agent-jobs Secret grant
     (get/create/update/delete on any Secret there), and list what lives there. If the stronger boundary is wanted, the
     per-Job handoff Secret path would need to avoid `get` on arbitrary names (for example, adopt from the Create
     response or a label-scoped read), or intents would need their own agents namespace.

6. **Opting out of the repository image cannot un-block a build whose Repository stalled under onReject: handoff; the
   intent burns all 16 attempts and fails** (medium; `internal/controller/intent/run_controller.go:292`)

   - Scenario: This comes from combining b689851 (handoff stall -> image_required for builds) with decb781 (the opt-out
     lifts any image block). RunReconciler.pending() settles a build run image_required whenever its Repository is
     Stalled with RunnerImageRejected (line 292), and it never checks requireRepositoryImage(proj). launchable()
     (line 196) likewise grants a build only when its Repository is Ready: planIgnoresStall covers plan runs only. The
     Project is not consulted at either point. Meanwhile imageBlockHolds (intent_blocked.go:169) lifts every
     ImageRequired block as soon as the Project opts out. The configuration docs name requireRepositoryImage: false as
     the remedy ('lifts it outright') and say the build then runs on the default image. Concrete scenario:
     source-controller runs with --repository-image-on-reject=handoff (the chart default is 'default', but handoff is a
     supported value, and source-controller treats an empty value as handoff). The app repo declares an image the
     allowlist refuses. The Project sets requireRepositoryImage: false, either from the start or as the documented fix
     after the first block. 1. After approval, building() launches build attempt N. ensureRunChildren creates its
     Repository. 2. source-controller pins the Repository and stalls it (Stalled=True RunnerImageRejected, Ready=False,
     artifact stored). 3. pending() settles the run image_required. building() sees imageBlocked and blocks on
     RepositoryImageRejected. 4. On the next pass, blocked() -> imageBlockHolds returns false at once because of the
     opt-out. blocked() calls createRun(N+1) and resumes Building. The new run gets a fresh Repository, which stalls the
     same way. 5. The cycle repeats with no human input and no wait until rs.next() > MaxIntentRunAttempt (16).
     building() then calls fail(): the trigger label is removed and the Intent goes Failed. The build never runs on the
     default image even though its tree is pinned and stored. Each of the ~16 iterations makes source-controller
     download the tarball from GitHub and resolve the registry image again. All 16 build Repositories (and their stored
     artifacts) are kept until the Intent's TTL. A revival repeats the whole cycle. TestOptOutLiftsAnImageBlock covers
     only the images-off and breaker reasons, not the handoff stall.

   - Suggested fix: Make the run scheduler read the Project's requireRepositoryImage for builds. In launchable(), treat
     a RunnerImageRejected stall with a stored artifact and ResolvedSHA as launchable for a build whose Project does not
     require the image, i.e. extend planIgnoresStall to builds under the opt-out. In pending(), settle image_required on
     that stall only when the Project requires the image, and otherwise leave the run pending so it launches. stageSpec
     already ignores PinFor's SkipRejected when requireImage is false, so the build then runs on the default image as
     the opt-out promises. Add the handoff-stall case to TestOptOutLiftsAnImageBlock.

Also on #51: a small fidelity fix in `e2e/fakegithub/graphql.go`. Real GitHub (verified live) answers a GraphQL call
made with a token lacking the permission with HTTP **200** and `errors[{type: FORBIDDEN}]` (the fake returns 403), and
reports `includesCreatedEdit: true` after any edit (the fake always says false).

No other work is in flight: every background agent and workflow has been stopped or has finished, and every earlier
branch is merged. Stale local worktrees under `.claude/worktrees/wf_451798cb-b4d-*` and `wf_5ea15417-dc3-*` hold only
already-pushed commits (or nothing) and can be removed once #51 is merged.

## Historical next steps (completed or superseded; use the current checkpoint above)

1. **Finish #51.** Check out `feature/intent-controller`, confirm the six round-3 items above are fixed (read the latest
   commits), apply the fakegithub fix, run every gate (below), push, wait for CI green. Merge `origin/main` in if it
   moved. Mark ready and squash-merge.
2. **Release + gate** (procedure below). Expect release-please to open "chore(main): release 0.12.1"
   (`bump-patch-for-minor-pre-major` keeps pre-1.0 features as patch bumps). Deploy with intents still **off** and run
   the Finding gate — this proves the release did not regress the security flow.
3. **Enable intents.** The values are ready on terraform branch `feat/patchy-intents-target` (pushed, not merged, not
   applied): `intentController.enabled: true` with `forgeSecrets: [patchy-github]`, and a `target` Project (intent repo
   `devthenet-labs/intents`, repository `devthenet-labs/patchy-target`, approver `brvtl`). Open a PR for it, merge, then
   `helm upgrade` both releases as their own revisions. Check `kubectl get projects -n patchy` shows Ready.
4. **Live demo** (the design's "Demo script", slice 1a steps 0–5, 9, 10): open an issue with the patchy-target form in
   `devthenet-labs/intents` (for example "Add GET /version returning {sha, built} as JSON"), watch the Intent go Pending
   → Planning, read the verbatim plan comment, add `patchy:approved` as `brvtl`, watch the build run in patchy-target's
   image (`runner-image-source=repository`, `go test` in the transcript), the PR open from `patchy-intent/target-1` with
   "Part of devthenet-labs/intents#1" and no closing keyword, merge it, and see the Intent reach Merged and the intent
   issue close. Also verify the negatives the design lists where you can. Afterwards a fresh Finding must still reach
   its PR. Rollback: `intentController.enabled=false`.
5. **Slice 1b** (design "Revise loop" and "Failed checks: automatic fix rounds"): revise rounds from a "Request changes"
   review or `/patchy revise` (feedback bounded and fenced, pinned at the PR head, image from the build round,
   fast-forward-only push, `head_moved` handling), and automatic check-fix rounds for the Project's `checks.fix` names.
   Needs new ghclient calls (reviews, review comments, compare-with-patch, check runs/statuses/job logs,
   `RequestReviewers`) and three read-only App permissions: Checks, Commit statuses, Actions — the owner must grant
   those in the App settings.
6. **Slice 2: previews** (design "Previews"): first the `__Host-` cookie rename on status-server and a
   `namespaceSelector` admitting only `patchy` on the default `alb` IngressClassParams (check live that the webhook ALB
   is not recreated), a benign demo app repo (never preview patchy-target), then the preview-controller.
7. **Deployable by others** (design
   [roadmap section](docs/design/intent-driven-development.md#deployable-by-others-after-previews)): make the Helm
   charts own the full in-cluster install; provide GitHub App, reference AWS Terraform, app scaffolding, template-repo
   and Project-preflight helpers plus an operator guide; audit devthenet-specific assumptions. Decide whether to support
   external-dns and cert-manager or keep DNS/TLS Terraform-only.

## Gates and process (the owner's working agreement)

- Branch per piece of work (`feature/…`, `fix/…`, `chore/…`, `docs/…`), never commit to `main`; Conventional Commits
  with the _why_ in the body; draft PRs. Feature work goes understand → design → implement → test → review, and every
  review finding is independently verified before it is acted on.
- Local gates before every push: `go build ./...`, `go vet ./...`, `go test ./...`, `mise run lint` (golangci-lint,
  license, prose, shell; give each worktree its own `GOLANGCI_LINT_CACHE` — a shared cache produced phantom failures),
  `mise run codegen` then `git diff --exit-code` when `api/` changes, `mise run envtest`, `mise run helm-lint` and
  `bash hack/chart-render-test.sh` for charts, and in `e2e/`: `go build ./... && go vet ./...` plus `mise run e2e`.
  Tools come from `mise` (`mise exec -- go …`); if `.mise/` is empty run `git submodule update --init .mise`.
- New behaviour needs tests; bug fixes need a regression test that fails before the fix; seeded property tests
  (`testing/quick` with a fixed seed) for pure, invariant-rich code. Never weaken or delete a test to pass.
- **Regression gate after every merged wave** (the owner insists "we need to make sure things still work"):
  1. main CI green; merge the release-please PR; wait for the Release workflow (goreleaser images
     `ghcr.io/devthenet-labs/patchy/*:vX.Y.Z` and both charts);
  2. record current Helm revisions (rollback points); confirm `helm get values` equals the `k8s/` files (ignoring
     comments); `helm upgrade` both releases to the new version; all pods Ready, startup logs clean;
  3. push a NEW small stdlib vulnerability to `devthenet-labs/patchy-target` whose CodeQL rule has no open or suspended
     Finding (already used: reflected-xss, command-injection, incorrect-integer-conversion, disabled-certificate-check;
     never reflected-xss — a suspended Finding absorbs it), wait for the alert and Finding, drive it with
     `/patchy expedite` on the tracking issue, follow to its PR (`Remediation.status.pushedCommit` must equal the PR
     head), merge it, verify Remediated, the issue closed, one comment per patchy marker, no duplicate Finding;
  4. on any failure caused by the release: `helm rollback` both releases and report with evidence.
- Secrets: never print, log or write a secret, key, JWT or token; keep them in shell variables or pipes and print only
  lengths.

## Gotchas

- The shell is zsh: unquoted variables are **not** word-split (use `${=VAR}` or arrays); `cp` is aliased to `cp -i` (use
  `command cp -f`). `$app:tag` applies zsh's `:t` modifier (it became `marigold-apioolchain-v1`): write `${app}:tag`.
- The owner's `gh` login has the `workflow` scope now; workflow-file pushes can use the normal SSH git path.
- CodeQL occasionally uploads a zero-rule Go analysis; push an empty commit to re-run it.
- `kubectl get … -w` stops when a Helm upgrade replaces a CRD; re-arm watches after upgrades.
- Some editing tools turn `\uXXXX` escapes in Go source into literal invisible characters; scan changed files.
- Before deleting worktrees, verify the branch is pushed (`git ls-remote` equals `HEAD`) and the tree is clean.

## Other known follow-ups

- The check-fix repeat guard cannot fire for GitHub Actions checks. `checkDiagnostics` hashes the raw job-log tail into
  the round's signature, and Actions log lines carry timestamps and runner metadata, so a repeated failure never matches
  (seen in the 2026-10-03 round's handoff; the repeat path itself was not exercised). `maxCheckFixes` is the only
  effective bound. Hash the annotations and a timestamp-stripped tail instead.
- A check-fix round is not named as one: the PR comment says "Revision round pushed commit", and the done summary says
  "Revisions: 0" without the check-fix round. (Its handoff now heads the diagnostics "Check failures".)
- Extend slice 1b's bounded check-fix rounds to Finding PRs: during the 0.12.1 live gate, patchy's `go/request-forgery`
  remediation passed its Go tests but its PR still failed CodeQL with a new critical alert, so it needed a separate
  manual correction. This is the second such miss after the earlier path-traversal case. A failing CodeQL check on a
  Finding PR should trigger a capped fix round rather than leave an apparently successful remediation in review with an
  unfixed alert.
- Done in 0.12.13 (#87): the security remediation prompt now requires committing the regression test it writes; the
  0.12.13 gate's PR #72 carried one. Watch that it holds across rule types.
- `finding-1678e4a376-5` (reflected XSS, suspended) absorbed real alert 15 in `shout.go`; resume it to get it fixed.
- Findings flow still uses the unscoped installation client for pins and PR creation (intents never do).
- Command replies cost ~2 GitHub calls per comment from anyone on public repos; a per-actor rate limit may be needed.
- `/patchy approve` was exercised live on held Finding `finding-7b91e0ec7e-1` during the 0.12.1 gate; the legacy
  `/approve` alias on a held Finding has not been exercised live.
- Go clients send `"0s"` for non-pointer `metav1.Duration` fields with schema defaults (Forge/Integration intervals).
- Cosign-sign app images, then set `allowUnsigned: false`; `patchy describe repository` says "Source: not recorded yet"
  for accepted images; investigations cannot run tests; fallback-image runs can open untested PRs.
