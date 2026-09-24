# Intent-driven development

**Status:** Accepted, 2026-09-23. Decisions D1-D6 are made (see "Decisions"). This design is synthesised from:

- three candidate designs: reuse-first, separate subsystem, and GitHub-native;
- two deep dives: previews and projects, and the agent side;
- two judges: security, and delivery.

Section-level claims were checked against the code; line references are to `main` at c6d6fc0. Two sections were added
after the decisions: "Human commands: one vocabulary" and "Failed checks: automatic fix rounds".

## Context

Today patchy handles one kind of work: a security Finding, carried from a scanner alert to a PR that a human reviews.
The goal here is general development work driven by an intent that a human writes:

1. A human opens an issue in an intent repo and labels it for a project.
2. patchy plans the work and writes the plan back to the issue.
3. A human approves by adding a label.
4. patchy builds the plan in one or more app repos and opens PRs.
5. Each PR is deployed as a preview on its own subdomain.
6. Review feedback on a PR starts a revision round.
7. On merge, the preview is torn down and the intent issue is closed.

This has to work for several projects in the devthenet-labs org and be run by one person. It must not regress the
security flow, and it must not weaken the isolation model: agent pods hold no credential, model traffic goes through the
egress broker, and NetworkPolicy is enforced.

The understand phase (`.claude/plans/intent-dev-understand-maps.md`) established the constraints this design works
within:

- Finding is shaped around security from end to end: a frozen accumulation key, a single repo and a single PR, and no
  edge from InReview back to a working phase. The evaluation kinds show that a second workflow can live beside it, using
  local phase enums and a single writer.
- Repository, source-controller, the artifact server, `internal/jobs`, the broker, `schedule.Pick` and `transcriptstore`
  are already independent of the kind of work.
- `ghclient.PushBranch` force-moves an existing branch (push.go:73-81), so it cannot be used unchanged on a branch that
  humans also push to.
- Webhook handler errors after the 202 are lost, and deliveries arrive in no guaranteed order. The live redelivery sweep
  (24 h lookback) and `spec.replay` resend old deliveries. A label event carries no `author_association`.
- The only app repo, patchy-target, is deliberately vulnerable and must never be previewed.

## Goals

- **The Finding flow is untouched in the first slice.** No edit to integration-, investigation- or
  remediation-controller, the Finding types, `transitions.go`, either admission-policy copy, the envelope version or the
  golden Job YAMLs. With intents off, the cluster is byte-for-byte what it is today. The one deliberate exception is the
  command-vocabulary change (see "Human commands"), which ships as its own PR behind its own regression gate.
- **Authority comes from GitHub's API.** Every decision about human authority (trigger, approval, revision feedback) is
  taken from facts GitHub reports through its API and checked against an allowlist the operator owns. It is never taken
  from a handler-time stamp or from `author_association`.
- **The build agent receives exactly what the approving human saw.**
- **patchy never force-moves a branch it did not just create.** Human commits on a PR branch survive every revision.
- **Spend is bounded before it starts.** The bounds are: triggers only from authorised people, one global slot,
  per-stage budgets, `maxRevisions`, a per-intent ceiling and the broker's limits.
- **Previews stay apart from patchy.** They never share an edge, namespace or credential with patchy, and never run a
  deliberately vulnerable app.

## Non-goals (first slice)

- Previews (slice 2).
- Multi-repo intents (slice 3). The types are per-repo lists from the start.
- `/patchy revise`, review-driven rounds and automatic check-fix rounds (slice 1b).
- Status-page or CLI actions on intents.
- Rollups.
- Dependency egress for agent pods. Dependencies are baked into the image the app repo declares.
- Agent edits to CI definitions or to runner-image declarations. These are always refused.
- Webhook ingress for intents. The first slice polls; a webhook "nudge" comes only if latency proves to hurt.

## Design

### Shape

- **A new binary.** A new binary, **intent-controller**, is off by default and owns the whole intent flow.
- **It polls GitHub.** It reads only the few facts it needs: trigger labels, approval label events, reviews and PR
  state.
- **It runs the agents.** Plan, build and revise agent Jobs run through the existing jobs package, in the controller's
  own slot pool.
- **It does the intent-side GitHub writes.** Each write uses a token minted for that one operation and scoped to one
  repository and one permission.
- **State lives in three new CRDs:**
  - `Project` holds operator config.
  - `Intent` is one per intent issue, with a local phase enum.
  - `IntentRun` is one immutable child per attempt.
- **Each run's source tree is an ordinary `Repository`.** This reuses source-controller's pin-once SHA, the
  digest-verified tarball and the repository-declared runner image unchanged.
- **integration-, investigation- and remediation-controller are not edited.**

### Components

- **intent-controller** (new). The binary is `cmd/intent-controller`, the engine is `internal/controller/intent`, the
  chart value is `intentController.enabled: false`, and kustomize ships it as an opt-in component. Its reconcilers:
  - _project_ validates each Project:
    - every repo resolves to exactly one Forge;
    - the App is installed on the intent repo and every app repo;
    - the labels exist.

    It then sets `Ready`. It polls each intent repo once per interval for open issues carrying a trigger label, using a
    conditional list request (ETag). For each new one it creates the Intent `<project>-<issue>` and tolerates
    AlreadyExists.

  - _intent_ is the only writer of Intent: spec at creation, and all of status. It:
    - runs the phase machine;
    - polls only what the current phase waits on;
    - decides human authority;
    - creates Repositories and IntentRuns;
    - posts the status, plan and round comments exactly once;
    - pushes branches, opens PRs and closes the issue.
  - _scheduler/run_ keeps a singleton request key and counts slots from the cluster. It uses `schedule.Pick` FIFO, with
    stage priority revise, then build, then plan. The pool is `--max-concurrent-runs=1`, separate from remediation's. It
    launches with `jobs.Create`, collects with `jobs.Result`, persists transcripts and validates changesets.
  - _ttl_ deletes an Intent 14 days after `completedAt`. Owner references cascade to everything else.
- **source-controller** is unchanged. It pins intent Repositories exactly as it pins Finding ones. A revise Repository
  sets `spec.ref.branch` to the PR branch, which `HeadSHA` already resolves (ghclient/repos.go:23).
- **agent-runner** gains two phases:
  - `plan`: `SandboxReadOnly`. It writes `reports/plan.md` and emits a new, additive envelope type `plan` at Version 4.
  - `build`: used for the initial build and for revisions. `SandboxWorkspaceWrite`. It follows the remediation path
    (`commit.sh`, `verifyCommitted`, `buildChangeset`) and emits the existing `remediation` payload with its Changeset.

  The new stage functions call the existing helpers; `remediate()` is not refactored.

- **egress-broker** is unchanged and carries intent model traffic. Intents run on brokered claude only. codex and
  copilot ignore the sandbox postures (codex.go:68, copilot.go:64-67), so they are refused.
- **The other components are unchanged in slice 1**: integration-, investigation-, remediation- and context-controller,
  and the status-server. Three things keep the Finding flow from ever acting on intent objects:
  - Intent branches use the `patchy-intent/` prefix, which never matches the Finding PR handler's `patchy/` check.
  - Intent Repositories never carry `LabelFinding`, so the Finding mappers ignore them (gate_controller.go:391,
    project.go:929).
  - Intent Jobs carry `run-kind=intent`, which both Finding job watches filter out (investigation_controller.go:745,
    remediation_controller.go:639).
- **patchy CLI** gains Kinds entries for `projects`, `intents` and `intentruns`, rendered from their printcolumns. It
  gains no verbs.

**Credentials and RBAC.**

- _Secret access._ intent-controller reads the covering Forge's Secret through the uncached API reader. Its
  `secrets get` is restricted by `resourceNames` to the Forge secret names given in values, which is tighter than any
  existing controller.
- _Tokens._ `ghclient.TokenPerms` gains `Issues` and `PullRequests`. `forge.Store.TokenWith` mints a token per
  operation, scoped to one repository and one permission set, and caches it per (repo, perms) until shortly before it
  expires. The unscoped installation client is never used.
- _Role in `patchy`:_
  - `projects`: get/list/watch; `projects/status`: update.
  - `intents` and `intentruns`: full lifecycle, plus `status` and `finalizers`.
  - `repositories`: create/get/list/watch/delete.
  - `forges`: get/list/watch.
  - `configmaps`: create/get/update/delete.
  - `secrets`: get, restricted by `resourceNames`.
  - `events` and `leases`.
- _In `patchy-agents`:_ its own copy of the agent-jobs Role (the evaluation precedent). There is no ClusterRole.
- _NetworkPolicy:_ egress to the API server and to GitHub on 443.
- _Admission policy test:_ `internal/action/policy_test.go` skip-lists its ServiceAccount with the reason "writes no
  Finding spec".
- _GitHub App:_ no new permission and no new event subscription in slice 1a. Slice 1b adds three read-only permissions
  for check-fix rounds: Checks, Commit statuses and Actions.

**Jobs.** The intent flow reuses `jobs.Create` unchanged rather than adding a new Job flavour:

- `Spec.Kind = "intent"`. `NameFor` gains an `intent → int` entry, which gives Job names of the form
  `patchy-<hash>-int-a<n>`.
- `Spec.Finding` and `Spec.Owner` carry the IntentRun name.
- `issue.md` carries the intent snapshot. `investigation.md` carries the approved plan and, for a revision, that round's
  feedback and compare patch.
- `stageEnvNames` gains a `build → PATCHY_REMEDIATE_*` mapping.

The golden Job YAMLs, `prepareScript` and `buildJob` stay byte-identical. The cost is some naming debt, documented at
each seam: `PATCHY_FINDING` and `LabelFinding` hold a run name, and `investigation.md` holds a plan. This is harmless
because the only `LabelFinding` consumers index Investigations, Remediations and Repositories by Finding name, and an
IntentRun name matches none of them.

The intent `jobs.Client` sets `AllowRepositoryImages`, `EphemeralStorage` and its own runnerguard Breaker.
`runnerguard.PinFor(spec, repo, revived bool)` is added beside `Pin`, which is not touched.

### Custom resources

All of them are in `patchy.bitwisemedia.uk/v1alpha1`, in namespace `patchy`, with `categories=patchy`. Every field is
bounded. Nothing goes into `transitions.go`.

**Project** is operator config.

- **Writers.** The operator writes the spec through the patchy-config chart; only intent-controller writes status.
  Writing `projects` is admin-only in RBAC, because a Project is the power to point agents at repositories.
- **Spec:**
  - `intentRepository`
  - `labels.trigger` (default `patchy:<name>`) and `labels.approve` (default `patchy:approved`)
  - `approvers.logins[]` (1-20): the only logins whose trigger, approval and review feedback count
  - `repositories[]` (at most 8; slice 1 enforces exactly 1) `{name, url}`, where the first entry is the planning repo
  - `limits`:
    - `maxActiveIntents` (2)
    - `maxRevisions` (3)
    - `maxCostMicroUSD` (10000000)
    - per-stage `maxTurns` and `tokenBudget`, clamped by controller flags
  - `requireRepositoryImage` (true)
  - `preview` (slice 2)
  - `suspend`
- **Status:**
  - the `Ready` condition, with reasons `ForgeUnresolved`, `AppNotInstalled` and `AmbiguousIntentRepository`
  - `activeIntents`
  - `lastPolledAt`

**Intent** is one per intent issue.

- **Writers.** intent-controller is the only writer. Humans may patch `spec.suspend` only, using the native verb.
- **Spec** (CEL-immutable except `suspend`): `project`, `issue{repository, number, url}` and
  `requestedBy{login, at, eventID}`.
- **Status:**
  - `phase` and `phaseTimes`
  - conditions: `BudgetExhausted`, `RevisionLimitReached`, `ImageRequired`, `ApprovalRejected`
  - `input{revision, digest, configMap}`
  - `plan{revision, digest, configMap, commentID, commentDigest, postedAt, summary, repositories}`
  - `approval{by, eventID, at, planRevision, planDigest, inputDigest}`
  - `branch`
  - `pullRequests[]` (at most 8, keyed by repository):
    `{repository, number, url, nodeID, headSHA, state, mergedAt, mergeCommitSHA}`
  - `revisions`
  - `usage` (micro-USD as int64, plus tokens)
  - `tracking{statusCommentID, statusDigest}`
  - `activeRun`
  - `completedAt`
- **Printcolumns:** Project, Issue, Phase, PRs, Revisions, Cost, Age.

**IntentRun** is one immutable attempt of one stage on one repository.

- **Lifecycle.** Creating it under its deterministic name acts as the lease. The Intent is its owner. It carries
  `FinalizerJobs`, and it owns its Repository and its input ConfigMap.
- **Spec** (`self == oldSelf`):
  - `intentRef` (pinned by UID)
  - `stage`: `plan`, `build` or `revise`
  - `repository{url, repositoryRef}`
  - `round` and `attempt`
  - `inputs{configMap, inputDigest, planRevision, planDigest, reviewIDs[] (at most 32)}`
  - `imageFrom`: the build-round Repository, used by revise runs
  - `grant{maxTurns, tokenBudget, timeout}`
  - `previousAttempt`
- **Status:**
  - `phase` (RunPhase) and `job`
  - `runnerImage`: what `jobs.Create` returned
  - `baseSHA` and `pushedCommit`
  - `outcome`, `report` (at most 64 KiB) and `detail`
  - `usage` and `transcript`
  - timestamps
- **Names:** `<intent>-plan-r<rev>-a<n>`, `<intent>-bld-<repokey>-a<n>` and `<intent>-rev<k>-<repokey>-a<n>`.

**Repository** is reused as it is.

- intent-controller creates and deletes it. Its owner is the IntentRun. It carries the labels
  `patchy.bitwisemedia.uk/intent` and `patchy.bitwisemedia.uk/intent-run`, and never `LabelFinding`.
- Status is still written only by source-controller.
- The build-round Repository (R0) is the runner-image anchor and is kept until the intent completes. Plan and revise
  Repositories are deleted once their run is collected.

### Intent phases

Local enum. There is one writer for every edge, intent-controller. A small edge table and a `SetIntentPhase` helper live
in `intent_types.go`, following the idiom of `transitions.go` but separate from it.

- `Pending` → `Planning` when an approver applied the trigger label. Otherwise it goes to `Closed` with one notice.
- `Planning` → `AwaitingApproval` when a valid plan has been posted.
- `AwaitingApproval` → `Building` when an approval is accepted, or back to `Planning` when an approver re-applies the
  trigger label to request a replan.
- `Building` → `InReview` once every PR is open.
- `InReview` → `Revising` → `InReview`, for a review round, a `/patchy revise`, or a check-fix round. A failed round
  returns to `InReview` with a condition set and a notice posted.
- Any non-terminal phase → `Blocked` on `maxRevisions`, the cost ceiling, a missing or rejected repository image, or a
  tripped breaker. `Blocked` is re-evaluated when the Project changes, so raising a limit resumes the intent.
- `InReview` → `Merged` when every PR is merged.
- Any non-terminal phase → `Closed` when a human closes the intent issue or runs `/patchy cancel`, or when every PR is
  closed unmerged.
- `Planning` or `Building` → `Failed` when attempts are exhausted or the plan is invalid twice. `Failed` stamps
  `completedAt`, but an approver re-applying the trigger label revives it to `Planning`.
- `Merged` and `Closed` are terminal.

### End-to-end flow (slice 1)

1. **Trigger.** A human opens an issue in `devthenet-labs/intents` through the project's issue form, which applies
   `patchy:target`.
2. **Discovery.** Within one poll interval (default 60 s), the conditional list returns the issue. The reconciler reads
   the issue's events and takes the actor of the `labeled` event:
   - An actor outside `approvers.logins`, or a Bot, gets one notice, and the Intent goes to `Closed`.
   - Otherwise the Intent moves from `Pending` to `Planning`, and the status comment
     `<!-- patchy:intent patchy/target-1 -->` is posted exactly once.
3. **Planning.**
   - Snapshot the issue title and body into the immutable ConfigMap `<intent>-input-r<N>`, together with its digest. On
     a replan, the snapshot also includes approver comments made since the last plan.
   - Create a Repository at the default branch.
   - Create the IntentRun `…-plan-r1-a1`.
   - When a slot frees, launch the plan Job: default runner image, read-only, brokered.
4. **Collect.**
   - Persist the transcript and parse the plan frontmatter.
   - Reject a plan that names repositories outside the Project.
   - Store the raw report in the immutable ConfigMap `<intent>-plan-r1`. Its digest is the sha256 of those bytes.
   - Delete the plan Repository.
5. **Write-back.**
   - Remove the approve label if it is present.
   - Post the plan comment, rendered from the ConfigMap bytes and sanitised (see below), with the marker
     `<!-- patchy:plan patchy/target-1 r1 sha256:<12> -->`.
   - Record `commentID`, `commentDigest`, and `postedAt` as returned by GitHub.
   - Move to `AwaitingApproval`.
6. **Approval.** While in `AwaitingApproval`, the controller polls the issue's events every 30 s. It accepts the newest
   `labeled` event for the approve label only when all of these hold:
   - the actor is in the approvers list and is not a Bot;
   - the event's `created_at` is later than `plan.postedAt`;
   - the plan comment, re-fetched now, still hashes to `commentDigest`;
   - the issue body still hashes to the input snapshot;
   - the label is still on the issue.

   It then records `status.approval{by, eventID, at, planRevision, planDigest, inputDigest}` and moves to `Building`. If
   the comment or the issue body has changed, it posts a notice, removes the label and asks for a replan.

7. **Build.**
   - Create R0 at the head of the default branch.
   - Require an _accepted_ repository image: `status.runnerImage.image` must be set and not rejected. The live values
     use `onReject: default`, so `Ready` alone is not enough. runnerguard must also allow the launch. Otherwise the
     Intent goes to `Blocked` with `ImageRequired`.
   - Launch the Job in that image, with workspace-write access:
     - `investigation.md` is the approved plan, read from its ConfigMap. It is re-hashed at launch, and a mismatch stops
       the launch.
     - `issue.md` is the input snapshot.
   - If `jobs.Create` reports that the Job ran the default image, delete the Job and block the Intent.
8. **Push and PR.**
   - Validate the changeset (see "Build environment and changeset rules").
   - Compose the commit message on the controller side.
   - Push in two phases:
     1. `CreateCommit`.
     2. Persist `pushedCommit`.
     3. `CreateRef patchy-intent/<intent>`. This is create-only. A 422 is adopted only when the ref already points at
        `pushedCommit`; otherwise the outcome is `branch_exists`. Nothing is ever forced.
   - Open the PR against the default branch with a controller-rendered body.
   - Record the PR's `{repository, number, url, nodeID, headSHA}`, move to `InReview`, and link the PR from the status
     comment.
9. **Review.** While in `InReview`, poll the PR's reviews and its state every 60 s. A new `CHANGES_REQUESTED` review
   from an approver starts a 2-minute quiet window. Later reviews from approvers extend the window, up to 10 minutes.
10. **Revise.**
    - Check limits. If one is exceeded, the Intent goes to `Blocked` with a notice.
    - Create a Repository with `ref.branch = patchy-intent/<intent>`. This pins the current PR head, including any human
      commits.
    - Render the feedback and the compare patch into `investigation.md`, after the plan.
    - Launch with the image from R0, never from the PR head. Move to `Revising`.
11. **Revise push.**
    - Validate: the changeset's base must equal the PR-head pin.
    - Push in two phases, fast-forward only (`UpdateRef force=false`).
    - A 422 not-fast-forward gives `head_moved`. The controller re-pins and re-runs once, without counting a revision.
      If it happens a second time, it posts a notice and waits.
    - On success, post the round comment, re-request review from the reviewer, increment `revisions`, and return to
      `InReview`.
12. **Merge.** The poll sees the PR merged, checked by the recorded repository and PR number. The Intent moves to
    `Merged`, gets a final summary comment (PRs, revisions, cost), and the intent issue is closed with
    `state_reason: completed`.

    If a human closes the intent issue, or every PR is closed unmerged, the Intent moves to `Closed` instead. Running
    Jobs are deleted through the finalizer, and open PRs are left to the human.

13. **TTL.** 14 days after `completedAt`, the Intent is deleted and everything it owns cascades.

### Human commands: one vocabulary

Every human action in patchy is one verb from one vocabulary, whatever surface it arrives on. `internal/action` already
holds that vocabulary for the status page and the CLI (`approve`, `retry`, `expedite`, `suspend`, `resume`). GitHub,
however, has only the ad-hoc `/approve` comment, and this design adds labels and reviews. Rather than three mechanisms,
there is one grammar, and everything else is an alias for it.

- **Grammar.** `/patchy <verb> [note]` as the first line of a comment. The note is at most 1 KiB, with control
  characters stripped. Which verbs apply depends on where the comment is:

  | Where                  | Verbs                                               |
  | ---------------------- | --------------------------------------------------- |
  | Finding tracking issue | `approve`, `retry`, `suspend`, `resume`, `expedite` |
  | Intent issue           | `approve`, `replan`, `cancel`                       |
  | Intent PR              | `revise`, `retry`                                   |

  The Finding verbs mean exactly what they mean on the status page and in the CLI. The intent verbs join the same
  registry, so when intents gain status-page or CLI actions (slice 5) they use the same names.

- **Aliases, not second mechanisms.** Each shortcut maps to a verb and gets the same authorisation and acknowledgement:
  - adding the approve label (`patchy:approved`) is `/patchy approve`;
  - re-applying the trigger label is `/patchy replan`;
  - a "Request changes" review is `/patchy revise`, with the review as the note;
  - `/approve` on a Finding tracking issue stays as a deprecated alias for `/patchy approve`.
- **One authorisation rule.** For every verb arriving from GitHub, the actor must have write access to the repository
  (the collaborator-permission API: `admin`, `maintain` or `write`) and must not be a Bot. For intents the actor must
  also be in the Project's `approvers.logins`. `author_association` is no longer used. This tightens today's `/approve`,
  which accepts any org `MEMBER`, even one without write access.
- **One acknowledgement.** A command that is seen gets a 👀 reaction, then exactly one reply with the outcome: done, not
  available in this phase (listing what is), or not allowed. An unknown verb gets the list of verbs available there.
  Commands and events from the App's own bot login are ignored.
- **One parser.** A pure package, `internal/command`, parses the grammar. integration-controller uses it on the webhook
  path for Findings, and intent-controller uses it on the poll path for intents. It has seeded property tests: parsing
  never panics, text that does not start with the command prefix never parses as a command, and the note never contains
  the command line.

Sequencing: slice 1a ships the parser, the intent verbs `approve`, `replan` and `cancel` with their label aliases, and,
as its own PR, the Finding migration (`/patchy <verb>`, the `/approve` alias, and the write-permission check). Slice 1b
adds `revise` with its review alias, and `retry` on intent PRs.

### Why polling rather than webhooks

- **It leaves the internet-facing binary alone.** integration-controller, which the security flow depends on, is
  untouched, and the App needs no new subscription.
- **Authority facts must come from the API anyway.** A webhook handler's `at` is its own clock (webhooks.go:148-151
  stamps `s.now()`). With redelivery on a 24 h lookback, and `spec.replay` resending everything, handler time is not
  evidence of when a label was applied. The events API returns the actor, `created_at` and the event id.
- **Delivery problems stop mattering.** Every reconcile is a function of GitHub state plus CR state, so lost, duplicated
  or out-of-order deliveries have no effect.
- **The cost is bounded.** Polling adds 30-60 s of latency and read traffic on the installation rate limit that the
  security flow shares. That is bounded by:
  - ETag conditional requests (a 304 is documented as free; to verify for installation tokens);
  - polling only non-terminal intents, and only the thing the current phase waits on;
  - `--rate-limit-floor` (default 1000), which pauses intent polling when the installation's remaining budget drops
    below it.

### Plan contract and output sanitisation

The plan frontmatter is strict and parsed by `report.ParsePlan`:

- `summary` (at most 200 characters)
- `repositories[]` (a subset of the Project's)
- `new_dependencies[]` (at most 16)
- `questions[]` (at most 10)
- `confidence`
- `estimated_max_turns` and `estimated_token_budget`

The body is at most 48 KiB: approach, per-repo steps, test plan and risks.

All agent-authored text that reaches GitHub passes through one sanitiser in `internal/templates`. Its property tests are
seeded, and they check that the sanitiser is idempotent, that its output never matches GitHub's closing-keyword grammar,
and that no raw HTML survives. What it does:

- **Raw HTML is escaped**, so an HTML comment or a `<details>` block in a plan is shown literally. A prompt-injected
  planner therefore cannot hide instructions that the approver would not see but the build agent would read. The
  controller's marker is the only HTML comment.
- **Closing keywords, issue references and mentions are neutralised** by rendering them as inline code. This covers
  forms like `fixes #3` and `owner/repo#3`, and `@mentions`.
- **The controller composes the commit message:** `<project>: <summary> (<intent repo>#N, round k)`, plus the trailers
  `Patchy-Intent:` and `Patchy-Run:`. The agent's own commit messages are dropped.

This matters for the security flow. A Finding's tracking issue lives in the same app repo, and `Signals.issues` moves
any non-terminal Finding to `HandedOff` when its tracking issue is closed, no matter who closed it (webhooks.go:85-91).
An agent-written `Fixes #N` merged to the default branch would therefore silently pull a security finding out of
automated remediation. Intent PR bodies refer to the intent issue as `Part of devthenet-labs/intents#N`, and patchy
closes the intent issue itself.

### Revise loop: inputs and bounds

- **Feedback.** Reviews, inline review comments and PR conversation comments written by approvers since the previous
  round.
  - Inline comments carry path, line and side, plus the tail of the diff hunk (at most 1 KiB).
  - Limits: at most 40 items, 2 KiB each, 24 KiB in total.
  - Control characters are stripped, and each item is fenced under a "data, not rules" preamble (the quoting idiom in
    templates.go:150-186).
  - Comments from non-approvers are counted but never included. App repos are public, so anyone can comment.
- **Diff.** The compare patch for `base...head`, at most 48 KiB, with any truncation stated. There is no second tree in
  the pod.
- **Idempotency.** Consumed review IDs are recorded on the IntentRun spec, so a restart or a repeated poll never runs a
  round twice.
- **Bounds:**
  - `maxRevisions` (default 3);
  - the per-intent ceiling;
  - per-stage limits (turns / tokens / time), all inside agentrun's `grant()` clamp:

    | Stage  | Turns | Tokens | Time |
    | ------ | ----- | ------ | ---- |
    | plan   | 40    | 200k   | 20m  |
    | build  | 150   | 800k   | 60m  |
    | revise | 80    | 400k   | 45m  |

  - a Job deadline of 90 m (the broker caller token lasts 105 m);
  - two attempts per stage;
  - one `head_moved` retry per round.
- **The cost ceiling is advisory for build and revise runs.** Those runs use the repository-declared image, and the
  runner-images design treats usage reported by the CLI from such an image as untrusted. The limits that are actually
  enforced are:
  - the per-run limits;
  - the Job deadline;
  - `maxRevisions`;
  - the broker's per-pod `tokensPerPod`, which is off by default today and should be switched on together with intents.

### Failed checks: automatic fix rounds

A PR that patchy opened can fail CI even though the build agent ran the tests in the app's own image: CI covers what the
sandbox cannot, such as other platforms, integration tests and lint configuration. In slice 1b, intent-controller
iterates on those failures itself, using the revise machinery.

- **Trigger.** After every patchy push (build or revise), the controller polls the PR head's check runs and commit
  statuses until they settle, or until `checks.timeout` (default 30 m) passes. If any check the Project names concluded
  `failure`, `timed_out` or `startup_failure`, a fix round starts without a human. The approved plan already covers the
  work, and the round is bounded. Cancelled, skipped and neutral checks do not count.
- **Which checks.** `Project.spec.checks.fix[]` is an explicit list of check names, such as `test` and `lint`. An empty
  list means patchy never auto-fixes: a failing check is reported in the status comment, and an approver can run
  `/patchy revise`. The list is explicit because a flaky or unrelated check would otherwise burn budget, as the CodeQL
  zero-rule upload glitch would have.
- **Only on patchy's own head.** A fix round starts only when the failing head is the commit patchy pushed. If a human
  has pushed since, the failure is reported and a human decides.
- **What the agent sees.** For each failed check:
  - the name and conclusion;
  - the check run's output title, summary and text;
  - up to 50 annotations (path, line and message);
  - for a GitHub Actions job, the last 32 KiB of the failed job's log.

  All of it is bounded (48 KiB in total), stripped of control characters and fenced as data, exactly like review
  feedback. The round is pinned at the PR head, uses the R0 image, and pushes fast-forward only.

- **Bounds.** `limits.maxCheckFixes` (default 2) per intent, counted separately from `maxRevisions` but under the same
  cost ceiling. If a check fails again after a fix round with the same failure signature (its annotations, or the tail
  of its log), the controller stops and the Intent goes to `Blocked` with the condition `ChecksFailing` and a notice.
  The agent cannot touch `.github/**`, so a failure in the CI configuration is reported, not fixed.
- **Records.** IntentRun gains `spec.trigger` (`review`, `command` or `checks`) and `spec.inputs.checkRunIDs[]` (at most
  32), so a check failure is consumed exactly once.
- **GitHub App.** Checks: read, Commit statuses: read and Actions: read (for job logs). All three are read-only, and the
  installation owner accepts them when they are added.
- **Security Findings.** Remediation PRs do not get this in this design. The same mechanism could later drive a Finding
  retry from `InReview`, but that needs a new Finding edge; it is on the roadmap.

### Build environment and changeset rules

**Dependencies.**

- _Baked in._ Dependencies are baked into the image the app repo declares. patchy-target already does this: its module
  cache is baked into the image and `GOPROXY` is off.
- _Image required._ Intent build and revise runs require that image. There is no fallback to the default image, which
  has no toolchain. That closes, for intents, the follow-up about untested PRs from runs that fell back to the default
  image.
- _New dependencies._ The plan lists `new_dependencies`, and the plan comment tells the approver to add them to the
  image on main first.
- _Unchanged:_ egress, NetworkPolicy and the sandbox probe.

**Changeset rules.** Every intent changeset must pass the exported remediation validator:

- the base equals the pinned SHA;
- path shape, file modes and base64 content are valid;
- at most 500 entries;
- no control characters.

It must also pass a deny-list that applies whichever image the run used:

- `.github/**`;
- `.patchy/**`, including `.patchy/Dockerfile`, the agent-image recipe that main's CI builds into the allowlisted ECR
  prefix;
- `.devcontainer/**`.

Without this deny-list an agent could choose its own next sandbox. source-controller reads the image declaration from
whatever tree it pins (runner_image.go:162-170), and a revise round pins the PR head. For the same reason:

- revise runs take their image from R0;
- a PR-head Repository is used only for its SHA and tarball. Its own runner-image outcome, whether `Ready` or `Stalled`,
  is ignored.

### Multi-repo: types now, behaviour in slice 3

- One Job per app repo, each running in that repo's own image.
- Each repo gets its own Repository, changeset and PR, all on the branch `patchy-intent/<intent>`.
- PRs are correlated only by the (repository, number, nodeID) patchy recorded when it opened them, never by head ref.
  PRs from forks are rejected.
- The Intent is `Merged` when every one of its PRs is merged.

Slice 1 enforces one repository per Project.

## Multi-project model (D3)

- **Intent repos.** One private intent repo for the org, `devthenet-labs/intents`, with one issue form per project that
  applies `patchy:<project>`. That label both triggers the work and names the project; the issue body is never parsed
  for routing. A project may instead name its own intent repo. When two Projects share a repo, their labels must differ;
  otherwise the Project reports `AmbiguousIntentRepository`.
- **Registration.** One Project CR per project, delivered through patchy-config values. `hack/codegen.sh` is extended to
  emit the Project schema; today it covers only Integration and Forge.
- **Namespace.** Everything runs in namespace `patchy`, because the controllers are single-namespace.
- **Limits.** Per project: `maxActiveIntents`, `maxRevisions` and the cost ceiling. The global cap is
  `--max-concurrent-runs`.
- **Onboarding a project** takes four steps:
  1. add a Project to the patchy-config values;
  2. make sure the App covers its repos (it is installed on all repositories today);
  3. give each app repo a `.patchy/agent.yaml` image;
  4. for previews, the slice 2 onboarding.

## Previews (slice 2, D5)

The recommendation is a **preview-controller**, off by default, working over a chart-provisioned pool of slot
namespaces. patchy renders every manifest.

- **Contract.** A `Preview` CR per intent. intent-controller writes the spec: `intentRef`, `hostLabel`,
  `components[]{name, imageRepository, revision (40 hex), port, readinessPath}` and `ttl`. preview-controller writes the
  status: `phase`, `slot`, `url` and each component's `imageID`.
- **Images.**
  - The app repo's CI builds the PR head's runtime Dockerfile in a job that holds no credentials.
  - A separate publish job holds an OIDC role pinned to that repo's immutable ID (`:pull_request`). It pushes
    `patchy/previews/<app>:sha-<40hex>` to an immutable ECR repo, and it can never push to `patchy/app-envs/*`.
  - Because the tags are immutable, preview-controller needs no ECR call and no cloud identity.
- **Slots.** The chart renders N (default 2) namespaces, `patchy-preview-<i>`. Each has:
  - PSA `restricted`;
  - a default-deny NetworkPolicy both ways, allowing ingress only from the ALB public subnets (10.40.128.0/24 and
    10.40.129.0/24) on the app port, and egress only to DNS at 172.20.0.10/32;
  - a ResourceQuota with `services.loadbalancers: 0`, `services.nodeports: 0` and small CPU, memory and pod caps;
  - a LimitRange.
- **preview-controller's permissions.** A namespaced Role in each slot over deployments, services and ingresses, plus
  read access to pods and replicasets. It has no access to namespaces, NetworkPolicies, quotas, RBAC or Secrets, nothing
  cluster-scoped, and no GitHub access.
- **Admission.** A fail-closed ValidatingAdmissionPolicy on the slot namespaces applies to every user:
  - Ingresses must use class `alb-preview`, and hosts must match `^[a-z0-9-]+\.preview\.patchy\.devthe\.net$`.
  - Ingresses may not set `tls` or `defaultBackend`, and only allowlisted annotations are accepted.
  - Images must match `patchy/previews/<app>:sha-<40hex>`.
  - Pods need `automountServiceAccountToken: false` and the default ServiceAccount, may use only emptyDir volumes, and
    may not have init containers.
  - Services must be ClusterIP.
- **Edge.** A separate `alb-preview` IngressClass with its own IngressClassParams:
  - its own group and ALB name;
  - a `namespaceSelector` matching the slot namespaces;
  - `inboundCIDRs` set to the operator's IPs;
  - a TLS 1.3 policy;
  - a class-level certificate.

  It also needs a free ACM certificate for `*.preview.patchy.devthe.net`, one Route53 wildcard alias, and a placeholder
  Ingress that keeps the ALB and its DNS name stable. Hosts are `<project>-<issue>.preview.patchy.devthe.net`. This
  costs about $18-24 a month. Before any preview namespace exists, the default `alb` class gets a namespaceSelector that
  admits only `patchy`. That change must be checked live to make sure it does not recreate the webhook ALB.

- **Lifecycle.**
  - A Preview is created once the intent's PRs exist.
  - Its revision is updated on every patchy push, and on human pushes (the PR head is polled).
  - It is deleted when the intent ends; a finalizer empties the slot.
  - It expires 72 h after the last deploy, and each pass sweeps orphans.
  - The number of slots is the global cost bound.
- **Prerequisite.** The status-server cookies become `__Host-` names. `internal/web/auth/cookies.go` uses the bare names
  `patchy-auth` and `patchy-oauth2-state`, and every preview host is same-site with `status.patchy.devthe.net`.
- **Demo app.** A new, benign repo. patchy-target is never previewed.

**Alternative: app-repo GitHub Actions deploys.** patchy gains no deploy power at all. The deploy has to run from main's
context:

- a `workflow_run` into an Environment whose branch policy allows main only. Environments on private repos need a paid
  plan.
- a per-app role behind a STANDARD EKS access entry, bound to a namespaced Role;
- the same VAP, edge and quota as above.

A deploy triggered by `pull_request` cannot be gated by an Environment branch rule, because GitHub checks
`refs/pull/N/merge`.

## Security posture changes

1. **A second code path writes to forges.** DESIGN.md and forgewriter.go:16-20 say remediation-controller is the only
   holder of forge write scope. That is a convention about code paths, not a credential boundary:
   - the live Integration and Forge share the Secret `patchy-github`;
   - integration-controller's `Creds.Client` returns the unscoped installation client (creds.go:80-98);
   - all five controllers hold unscoped `secrets get`.

   intent-controller becomes a second code path that writes to forges, with the tightest posture of any controller:
   - `secrets get` restricted by `resourceNames`;
   - a token per operation, scoped to one repository and one permission;
   - writes only to repos listed in a Project, plus issue operations on the intent repo;
   - branches only under `patchy-intent/`, created once and then only fast-forwarded;
   - never the default branch; humans merge.

   DESIGN.md and CLAUDE.md say this plainly.

2. **Human free text becomes agent instructions.** The intent issue is the task. The safeguards:
   - only issues triggered by an approver are processed;
   - only feedback written by approvers reaches a prompt;
   - all of it is bounded and fenced;
   - the planner is read-only;
   - nothing that writes code runs before an approval bound to both the plan digest and the input digest.

   A plan carrying a prompt injection remains possible; a human reads it and a human merges.

3. **Agent text reaching GitHub is sanitised.** This closes the cross-flow path from an intent PR to a Finding's
   tracking issue.
4. **Stricter changeset and image rules for intents:**
   - `.github/**`, `.patchy/**` and `.devcontainer/**` are always refused;
   - a repository image is required;
   - revise runs take their image from R0;
   - claude only.
5. **Agent pods are unchanged:** no credential, brokered model traffic, default-deny egress, and the sandbox probe on
   Jobs that run a repository image.
6. **No new inbound surface.** Polling is outbound only. The rate-limit floor protects the security flow's share of the
   installation limit.
7. **New spend exposure.** It is bounded by:
   - triggers accepted from approvers only;
   - one global slot;
   - per-stage limits;
   - `maxRevisions`;
   - the per-intent ceiling;
   - the broker's per-pod limits, once they are switched on.
8. **Slice 2 only.**
   - patchy gains namespaced deploy power, limited to chart-created slot namespaces and fenced by a VAP.
   - Agent-built code runs as an internet service, gated by IP, in namespaces that cannot reach patchy, patchy-agents,
     the broker, the API server or link-local addresses.
   - The residual risk is the VPC CNI's policy-attach race, which leaves a new pod open for a few seconds.
9. **Pre-existing, flagged separately.** The app-env push role trusts `main` of every repo in the org. The allowlist is
   the whole `patchy/app-envs/` prefix, with `allowUnsigned: true`. So the main branch of any org repo can publish an
   image the sandbox will admit. Narrow the role to registered app repos in terraform-devthenet.

## First slice, in two parts (D4)

**Scope:** one Project and one app repo, patchy-target. Its Go image is already live, and using it exercises coexistence
with Findings in the same repo. No previews.

- **Slice 1a (about 6-8 days):** plan, approval (the label or `/patchy approve`), replan (re-applying the trigger label
  or `/patchy replan`), `/patchy cancel`, build, PR and merge. It also ships the shared command parser and, as its own
  PR, the Finding command migration.
- **Slice 1b (about 4-5 days):** revise rounds from a "Request changes" review or `/patchy revise`, automatic check-fix
  rounds, and the fast-forward-only push path. The GitHub calls marked (1b) below land here.

**Waves.** Each wave ends with the regression gate:

- `make e2e`;
- the real claude CLI through the broker;
- a release;
- a fresh live Finding on patchy-target reaching its PR, with the Helm rollback revision recorded.

The waves:

- **Wave 0 (about 1 day): prerequisite fixes**, each as a separate `fix/` PR (listed under "Prerequisite fixes").
- **Wave 1 (about 2-3 days): API and GitHub seams.**
  - Types, codegen, hand-added kustomization entries and schema envtests.
  - New ghclient calls:
    - `CreateCommit`, split out of `PushBranch`;
    - `CreateBranchRef` (never forces), and `FastForwardRef` (returns `ErrNotFastForward`) (1b);
    - `ListIssueEvents`, and `ListIssues` with ETag;
    - comments since a time;
    - `GetPR`; `ListReviews` and `ListReviewComments` (1b);
    - compare with patch (1b);
    - check runs, commit statuses and Actions job logs (1b);
    - reactions, `App.Slug` and `CollaboratorPermission`; `RequestReviewers` (1b).
  - Matching fakegithub routes, including a fast-forward-only `UpdateRef` that returns 422.
- **Wave 2 (about 2-3 days): pod and shared seams.**
  - agentrun `plan` and `build`.
  - `ParsePlan` and `ParseBuild`.
  - The envelope `plan` type.
  - Templates and the sanitiser, with goldens and property tests.
  - The `NameFor` `int` entry and the `stageEnvNames` build mapping.
  - `runnerguard.PinFor`.
  - The exported changeset validator with deny prefixes, plus a seeded property test showing Finding verdicts are
    unchanged.
- **Wave 3 (about 3-4 days): intent-controller.**
  - Reconcilers, scheduler, launch/collect/push/PR, revise, merge/close, limits and TTL.
  - Chart template:
    - its own ConfigMap with `--intent-*` flags, so it cannot collide with the shared kustomize ConfigMap;
    - Roles and NetworkPolicy;
    - values and their schema;
    - the `patchy.brokerEnabled` OR;
    - `PATCHY_REPOSITORY_IMAGES` and `PATCHY_AGENT_EPHEMERAL_STORAGE` stamped for it;
    - a render case in `helm-lint.sh` and `chart-render-test.sh`.
  - Kustomize opt-in component, goreleaser entries, the `policy_test` skip-list entry and CLI Kinds.
  - Docs: a DESIGN.md section, CLAUDE.md orientation, and a configuration page.
  - e2e: drive fakegithub state from `Pending` to `Merged`, asserting Job shapes, pushes and PRs, and run one existing
    Finding e2e with intent-controller running.

**Estimate.** 1a is about 6-8 working days and 1b about 4-5, each including its regression gates. Intents are enabled on
devthenet-dev in a separate Helm upgrade from the release that ships them.

**Demo script.** Steps 0-5, 9 and 10 are the slice 1a demo; steps 6-8b are slice 1b.

0. **Pre-flight.** Run the regression gate on main and record the Helm revisions for `patchy` and `patchy-config`.
1. **Setup.**
   - Create the private repo `devthenet-labs/intents` with an issue form that applies `patchy:target`.
   - Run `helm upgrade patchy … --set intentController.enabled=true` as its own revision.
   - Add Project `target` to patchy-config: intent repo `intents`, repositories `[patchy-target]`, and the owner's login
     as approver.
   - Check that `kubectl get projects -n patchy` shows `Ready=True`.
2. **Trigger.** Open the issue "Add GET /version returning {sha, built} as JSON". Within about a minute, `target-1` goes
   from `Pending` to `Planning` and the status comment appears. The plan Job
   (`-l patchy.bitwisemedia.uk/run-kind=intent`) runs on the default image.
3. **Plan and replan.**
   - Plan comment r1 appears with its digest, and the Intent is in `AwaitingApproval`.
   - Negative check: a second account that is not an approver applies `patchy:approved`. It gets one notice, the label
     is removed, and nothing builds.
   - Negative check: edit the plan comment, then approve. The approval is refused and a replan is requested.
   - Comment "also add a unit test" and re-apply `patchy:target`. Plan r2 appears.
4. **Approve.** The owner applies `patchy:approved`, and the Intent moves to `Building`. The Job runs patchy-target's
   ECR image (`runner-image-source=repository`), and `go test ./...` passes in the transcript.
5. **PR.** A PR opens from `patchy-intent/target-1`. Its body says "Part of devthenet-labs/intents#1", with no closing
   keyword. The Intent is in `InReview`.
6. **Revise.** Push a human commit to the branch, then submit "Request changes" with an inline comment. After the quiet
   window:
   - the Intent goes to `Revising`;
   - the run is pinned at the PR head;
   - the new commit's parent is the human commit;
   - a round comment appears, and review is re-requested.
7. **Race.** Push a human commit while a revise Job is running. The outcome is `head_moved`, followed by one re-pin and
   never a force-push.
8. **Limit.** Set `maxRevisions: 1` and request changes again. The Intent goes to `Blocked` with a notice. Raise the
   limit and it proceeds.

   8b. **Failed check.** With `checks.fix: [test]`, ask in review for a change whose first attempt breaks a test that
   only CI runs. The `test` check fails on patchy's head, a check-fix round starts on its own, and the next push turns
   the check green. A second failure with the same signature would block the Intent instead.

9. **Merge.** The Intent moves to `Merged`, a summary comment is posted, and the intent issue is closed.
   `kubectl get findings -n patchy` shows every Finding's phase unchanged, and their tracking issues are untouched.
10. **Post-flight.** A fresh Finding reaches its PR. To roll back, set `intentController.enabled=false`; the CRDs stay
    but do nothing.

## Roadmap

- **Slice 2: previews.** About 5-7 dev days plus 1-2 infra days. The preview-controller, or the GitHub Actions
  alternative (D5), comes first. Before any preview goes live:
  - the `__Host-` cookie fix;
  - the namespaceSelector on the `alb` class;
  - a benign demo repo.
- **Slice 3: multi-repo and commands.** About 5-7 days.
  - Lift the one-repo guard: plan over several read-only trees, fan the build out to one Job per repo, cross-link
    sibling PRs, and add a partial-failure policy.
  - One `/patchy revise` that revises every sibling PR of a multi-repo intent.
  - A webhook "nudge", if polling latency hurts: integration-controller annotates the Project that matches a delivery
    and carries no intent semantics.
- **Slice 4: hardening.** About 3-5 days.
  - A dedicated GitHub App for intents. This needs explicit Forge selection, because `forge.Resolve` is not aware of
    consumers.
  - `resourceNames` on the existing controllers' `secrets get`.
  - An Admin-tier ClusterNetworkPolicy for preview namespaces, after a live test.
  - cosign-signed app images, then `allowUnsigned: false`.
  - An allowlisted dependency proxy reachable only from `run-kind=intent` pods.
  - Check-fix rounds for security Finding PRs, which need a Finding edge out of `InReview`.
- **Slice 5: visibility, if GitHub plus kubectl prove insufficient.**
  - `patchy describe intent`.
  - A read-only Intents tab on the status page.
  - Per-project cost totals.
  - Custom verbs, plus a VAP on Intent, before any human gets write RBAC on intents.

## Alternatives considered

- **Reuse-first.** The engine is hosted in remediation-controller, with webhook ingress, an authorizer and a projector
  in integration-controller.
  - It adds no binary.
  - But every wave edits the two binaries the security flow depends on: receiver routing, a refactor of `comments.go`
    (fix #31), and wider Roles.
  - An intent fault shares a process with security remediation. The "rollup precedent" is not comparable, because rollup
    handles no GitHub input.
  - It creates a contract between two binaries that both write Intent status.
  - Its "only write holder" benefit is a code-path convention, not a credential boundary.
  - The good parts are kept: no fallback image, `.patchy/**` refused, revise image taken from the build round, reusing
    `jobs.Create` with an additive envelope type, and the demo discipline.
- **GitHub-native.** Ingress stamps spec, and GitHub Actions deploys.
  - The GitHub-native pieces are good.
  - But the review feedback fed to the agent was unfiltered, and the repos are public.
  - The handler-time `at` it relied on is not ordering evidence.
  - It refused only `.github/**`.
  - Previews needed Environments, which means public repos or a paid plan.
  - It is the largest operational surface for one person.
  - Kept: `resourceNames` on secrets, and the `alb` namespaceSelector prerequisite.
- **Separate subsystem as proposed.** This is the base of the recommendation. Changes made to it:
  - no slash commands, trailer adoption, ETag transport layer or multi-tree plan in slice 1;
  - reuse `jobs.Create` rather than a new Job flavour that refactors `buildJob`;
  - no separate `PATCHY-INTENT-EVENT` stream;
  - previews in namespaced slots rather than with cluster-scoped namespace and workload power.
- **Generalising Finding.** Rejected. It would need a synthetic advisory under the frozen accumulation key, bypasses for
  the accumulation and min-age gates, new edges from `InReview` back to working phases, edits to both admission copies,
  and it would pollute security labels and rollups. It carries the largest regression risk.
- **Intent with `status.runs[]` instead of IntentRun.** One CRD fewer, but it loses the immutable per-run record (plan
  and input digests, the image that actually ran) and the ownership of each run's Repository, ConfigMap and Job
  finalizer.
- **Registry in an Integration `intents` block.** No new CRD, but the intent flow would be coupled to the
  capability-singleton rules that Finding routing depends on, and there would be no per-project `Ready` status.
- **One Job over N trees.** One image cannot hold N toolchains, and it needs a multi-changeset envelope.
- **A plan committed as a file, or approval by merging a plan PR.** Needs contents write on the intent repo and a second
  PR per intent. Optional later.
- **Revising on every comment, or force-push regeneration.** Runaway cost, loops, and human commits get clobbered.
- **A dependency proxy now.** A new egress and exfiltration channel. Deferred.
- **Argo CD ApplicationSet.** Needs Identity Center, a second GitHub credential, about $24-27 a month, and ongoing
  operations.

## Risks

- **Approval rests on unverified GitHub details:**
  - whether the actor on a label applied by an issue form is the issue author;
  - the fields in the events API;
  - `UpdateRef force=false` semantics;
  - whether 304 responses are free for installation tokens.

  Verify all of these before wave 3 goes live.

- **Shared code still ships in the same images as the Finding flow:** the split `ghclient` push, `stageEnvNames`,
  `NameFor`, the exported validator and runnerguard. Every change is additive and guarded by goldens and property tests,
  and the regression gate is mandatory for every wave.
- **Polling cost and latency.** Bounded as described, but an approval or review can take up to a minute to register.
- **Prompt injection by an approver, or through the issue text, is accepted within trust.** The limit on damage is what
  reaches a PR, which a human merges.
- **Builds that need a new dependency fail offline** until a human updates the image. This friction stays until a proxy
  exists.
- **Naming debt from reusing `jobs.Create`:** `PATCHY_FINDING` and `LabelFinding` hold run names, and `investigation.md`
  holds a plan. Each seam is documented; renaming is slice-4 work.
- **Scope.** Three CRDs and a binary in slice 1 is more than the trimmed runner-images feature was. Hold the slice line.
- **Slice 2 risks:**
  - unverified Auto Mode behaviour for IngressClassParams `namespaceSelector` and `inboundCIDRs`, and for the
    placeholder Ingress;
  - the CNI attach race;
  - a CIDR misconfiguration exposing a preview publicly.

## Prerequisite fixes

Items 1-3, 5 and 6 are wave 0. Item 4 moves into wave 1 with the other ghclient work. `/approve` accepting any org
`MEMBER` is fixed by the command-vocabulary PR in slice 1a.

1. **Finding PR-close handler.** Make it check the repository and PR number against `status.pullRequest`, not just the
   head ref (internal/controller/integration/webhooks.go:170-216).
2. **`PushBranch` returns the commit SHA.** Remediation then records `Remediation.status.pushedCommit`, which is
   declared but never written today. Split `CreateCommit` out so that the intent push can record the commit before
   moving the ref. The zero value keeps today's force semantics for Findings.
3. **`reservedEnv`.** Add `PATCHY_PREVIOUS_ATTEMPT` to it (internal/jobs/jobs.go:715-739; the variable is set at
   940-942).
4. **Scoped tokens.** `TokenPerms` gains `Issues` and `PullRequests`, and `forge.Store.TokenWith` is added. Additive
   only.
5. **Test the optional-controller template.** Render `evaluationController.enabled=true` in `hack/helm-lint.sh` and
   `hack/chart-render-test.sh`, add evaluation-controller to the dev overlay and `hack/dev-colima.sh`, and fix the "five
   controllers" drift in the docs. This is the template intent-controller copies, so it needs to be tested.
6. **Issue-close ordering.** Add a regression test documenting what happens when `issues.closed` is handled before
   `pull_request.closed` (webhooks.go:85-91 against 192-193).
7. **Before slice 2:**
   - `__Host-` cookie names on the status server;
   - a namespaceSelector on the default `alb` IngressClassParams, checked to make sure the ALB is not recreated.
8. **Separately, in terraform-devthenet:** narrow `devthenet-labs-app-env-push` from `…/*:ref:refs/heads/main` to the
   registered app repos.
9. **Operational checks before wave 3 goes live:**
   - the App installation covers `devthenet-labs/intents`;
   - the Forge `github` resolves every repo;
   - the collaborator-permission endpoint works with the App's permissions;
   - the 304 rate-limit behaviour.

## Decisions

Made on 2026-09-23.

- **D1: Work item.** Intent plus IntentRun with local enums, reusing Repository.
- **D2: Engine placement.** A separate, default-off intent-controller that polls GitHub. The documented "only
  remediation-controller writes to forges" statement is updated to name it as the second forge-writing code path.
- **D3: Multi-project.** One private org intent repo, a label per project, and one Project CR per project. A Project may
  still name its own intent repo.
- **D4: First slice.** Split: 1a is plan, approve, build, PR and merge; 1b is revise and check-fix rounds. No previews.
- **D5: Previews.** A slot-pool preview-controller on a separate, IP-restricted ALB, in slice 2.
- **D6: Revise trigger.** A "Request changes" review from an approver, with `/patchy revise` as the command form, both
  in slice 1b. Commands are made consistent across Findings and intents (see "Human commands: one vocabulary"). Added
  with this decision: automatic check-fix rounds on failed CI checks (see "Failed checks: automatic fix rounds").

## Open questions

1. Is the actor of a label applied by an issue form the issue author?
2. Do ETag 304 responses count against an App installation's rate limit?
3. Does `UpdateRef` with `force=false` return 422 on a non-fast-forward, and succeed as a no-op when the ref already
   points at the commit?
4. Does the collaborator-permission endpoint work with the App's current permissions? Until that is verified, the
   allowlist alone is the authority.
5. Should the status page or the CLI also be able to approve, through a custom verb and a VAP on Intent? Deferred.
6. Should demo reset delete Intents? It lists kinds by hand in `integration/reset.go` and `web/admin.go`. Slice 1 leaves
   them out, and reset must never close human-authored intent issues.
7. Is a 14-day TTL right, or should intents be kept forever, with GitHub as the durable record?

## Corrections to the candidate designs (verified)

- **"Only forge-write holder"** is a convention about code paths, not a credential boundary (see posture change 1). The
  claim that intent-controller would be "a second holder of the App key" is also wrong: at least three processes already
  read that key.
- **No new envelope version is needed.** `envelope.Decode` accepts any non-empty type at Version 4
  (envelope.go:198-217), and the collectors switch on type. So an additive `plan` type needs neither a v5 nor a separate
  event stream.
- **Job labels.** The Job kind label comes from `Spec.Kind` on each call (`jobLabels`), not from the Client.
  `jobs.Create` requires `Spec.Finding` (jobs.go:345) and stamps both `LabelFinding` and `PATCHY_FINDING` from it.
- **Handler-time `at`** (webhooks.go:148-151) is not evidence of ordering, given live redelivery and replay.
- **A deploy triggered by `pull_request` cannot be gated by an Environment branch policy**, because GitHub evaluates
  `refs/pull/N/merge`.
- **Panic recovery.** controller-runtime v0.25 recovers panics in reconcilers by default, but not in webhook handlers or
  other goroutines.
- **Refusing only `.github/**`** still lets the agent choose its next sandbox through `.patchy/agent.yaml` on the PR
  head.
- **The reuse-first thin-slice preview** had a namespaced deploy Role but no VAP and no namespaceSelector on the default
  class. That would let anyone holding the deploy role claim `patchy.devthe.net` on the webhook ALB.
