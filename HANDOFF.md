# Handoff: intent-driven development in patchy

Status as of 2026-09-24. Written so another coding agent (for example Codex) can continue without the previous session's
context. Read this whole file, then `AGENTS.md` (orientation), then `docs/design/intent-driven-development.md` (the
accepted design — the source of truth for what to build).

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

| Thing                           | Where                                                                                                                                                                                                                                                                                                                           |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| patchy fork (work here)         | `devthenet-labs/patchy`, local `/Users/peter/code/patchy`. `upstream` (bitwise-media-group) is read-only. Always pass `--repo devthenet-labs/patchy` to `gh` (bare PR numbers can resolve to upstream).                                                                                                                         |
| Infra (terraform + Helm values) | `/Users/peter/code/DevTheNet/terraform-devthenet` (remote `brvtl/terraform-devthenet`), Helm values in `k8s/`. AWS profile `devthenet` (`~/.aws`).                                                                                                                                                                              |
| Cluster                         | EKS Auto Mode `devthenet-dev`, us-east-1. Namespaces `patchy`, `patchy-agents`. `export AWS_PROFILE=devthenet`.                                                                                                                                                                                                                 |
| Helm releases                   | `patchy` (chart `oci://ghcr.io/devthenet-labs/patchy/charts/patchy`, values `-f k8s/patchy-values.yaml -f k8s/patchy-values-tls.yaml`) and `patchy-config` (`.../charts/patchy-config`, `-f k8s/patchy-config-values.yaml`). Live: **0.12.0**, patchy rev 18, patchy-config rev 11 (rollback points rev 17 / rev 10 = 0.11.11). |
| Hostnames                       | `patchy.devthe.net` (webhooks `/github/webhooks`), `status.patchy.devthe.net`.                                                                                                                                                                                                                                                  |
| GitHub App                      | `patchy-devthenet` (id 5036888), installation 163854331 on all devthenet-labs repos; credentials in Secret `patchy/patchy-github` (keys `appID`, `privateKey`, `webhookSecret`).                                                                                                                                                |
| App repo (demo target)          | `devthenet-labs/patchy-target` (deliberately vulnerable Go server; its agent image is `patchy/app-envs/patchy-target` in ECR, pushed by the per-app role `devthenet-labs-app-env-push-patchy-target`).                                                                                                                          |
| Intent repo                     | `devthenet-labs/intents` (private): README, issue form `.github/ISSUE_TEMPLATE/target.yml` applying `patchy:target`, labels `patchy:target` and `patchy:approved`.                                                                                                                                                              |
| Scratch repo                    | `devthenet-labs/patchy-smoke` (private) for API experiments.                                                                                                                                                                                                                                                                    |
| Plans (git-ignored)             | `.claude/plans/intent-wave3-brief.md` (the controller contract), `.claude/plans/intent-dev-understand-maps.md` (codebase maps). Local only.                                                                                                                                                                                     |

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

## Next steps, in order

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
  `command cp -f`).
- The owner's `gh` token lacks the `workflow` scope: pushes that touch `.github/workflows` must go over SSH git.
- CodeQL occasionally uploads a zero-rule Go analysis; push an empty commit to re-run it.
- `kubectl get … -w` stops when a Helm upgrade replaces a CRD; re-arm watches after upgrades.
- Some editing tools turn `\uXXXX` escapes in Go source into literal invisible characters; scan changed files.
- Before deleting worktrees, verify the branch is pushed (`git ls-remote` equals `HEAD`) and the tree is clean.

## Known follow-ups (not started)

- In the next release, have intent-controller skip the advisory egress-broker startup probe or log its result at info:
  its NetworkPolicy deliberately blocks controller-to-broker traffic, while intent agent pods have their own broker
  allowance. The current `runnercfg.Resolve` probe times out and warns on every intent-controller start, although it
  does not gate readiness or launches.
- Extend slice 1b's bounded check-fix rounds to Finding PRs: during the 0.12.1 live gate, patchy's `go/request-forgery`
  remediation passed its Go tests but its PR still failed CodeQL with a new critical alert, so it needed a separate
  manual correction. This is the second such miss after the earlier path-traversal case. A failing CodeQL check on a
  Finding PR should trigger a capped fix round rather than leave an apparently successful remediation in review with an
  unfixed alert.
- The security remediation prompt should require committing the tests the agent writes (seen: a test written, run, then
  deleted). The intent build prompt already requires it.
- `finding-1678e4a376-5` (reflected XSS, suspended) absorbed real alert 15 in `shout.go`; resume it to get it fixed.
- Findings flow still uses the unscoped installation client for pins and PR creation (intents never do).
- Command replies cost ~2 GitHub calls per comment from anyone on public repos; a per-actor rate limit may be needed.
- `/patchy approve` was exercised live on held Finding `finding-7b91e0ec7e-1` during the 0.12.1 gate; the legacy
  `/approve` alias on a held Finding has not been exercised live.
- Go clients send `"0s"` for non-pointer `metav1.Duration` fields with schema defaults (Forge/Integration intervals).
- Cosign-sign app images, then set `allowUnsigned: false`; `patchy describe repository` says "Source: not recorded yet"
  for accepted images; investigations cannot run tests; fallback-image runs can open untested PRs.
