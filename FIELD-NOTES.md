# Field notes

Raw material for the docs: what running intents on real applications taught us, written down as it happens. Each note
says what happened, why, and what the docs (or patchy) should say or do about it. Not part of the docs site; when a note
becomes documentation or a fix, link the PR beside it. Newest first within each section.

## Onboarding an application

- **Apps with a Host allowlist get a Ready preview that refuses every request** (overdub, 2026-10-04). overdub's
  `server/serve.js` answers 403 to any Host that isn't `localhost` or an IP (its DNS-rebinding guard). The ALB health
  check reaches the pod by IP and passes, then every real request by name gets 403. Fixed in overdub's runtime image
  with a small entry that runs the server on loopback and forwards as `localhost` (the image holds only public files).
  Docs: the runtime contract should say a preview is reached by name through the ALB; dev servers, Django
  `ALLOWED_HOSTS`, Rails host authorization and the like need the preview host suffix allowed. Patchy:
  `patchy check project` could probe a preview with the real Host header.
- **A preview readiness path cannot contain a dot** (overdub). `/llms.txt` failed the patchy-config schema
  (`^/[a-zA-Z0-9/_-]*$`), so overdub uses `/app/`. `/health.json` would fail the same way. Either relax the pattern
  (dots are harmless in a path) or document it.
- **`patchy init app` is Go-only** (overdub, Node). No `--lang node`, `--port`, `--readiness-path` or `--test-cmd`, and
  `--existing` writes no `test` CI check, so an app without CI writes `ci.yml` and adapts `.patchy/Dockerfile` by hand.
  Docs: an "adapting the scaffold" page; patchy: language templates beyond Go.
- **Agent images for browser-tested apps** (overdub). The agent image bakes Node, playwright-core and a headless
  Chromium, each pinned and checksummed, and the 49 OS packages the browser needs, with env vars pointing the tests at
  them, because the agent pod has no network. Builds must be reproducible: the publisher refuses a changed image at an
  already-published tag (`toolchain-v1`), so unpinned packages break the next publish.
- **Test suites tuned on a Mac fail on Linux** (overdub). 8 of 63 suites failed on linux/amd64 for reasons that are not
  regressions: font metrics overflowing layout budgets, `⌘Z` vs `Ctrl+Z`, wall-clock and GC budgets. The golden render
  hashes did match. The fix was a Linux-safe subset (`npm run test:ci`) used by both CI and the build agent, and a
  paragraph in the app's CLAUDE.md telling the agent which suites to leave alone. Docs: "choose the gate your agent
  runs", with this example.
- **Characterising a full browser suite on Apple Silicon is slow** (overdub onboarding took ~2 h). A linux/amd64 image
  runs under QEMU emulation; Chromium under emulation took 31 minutes for one full run. A GitHub runner probe of the
  full suite ran 38 minutes before it was cancelled. Docs: onboard with a conservative, measured subset first;
  characterise later on native hardware.
- **Private repositories on GitHub Free have ~2,000 Actions minutes a month.** overdub's CI subset takes about 2 minutes
  per run; the full suite would not fit a busy month. Docs: measure CI before enabling it on private repos.
- **An app outside the App's org was mirrored** (overdub: a private repository under a personal account, mirrored as a
  private repository in the org). Onboarding in place needs the repo owner to install a GitHub App, set Actions
  variables (admin) and merge the scaffold PR. Docs: both routes, with the mirror's cost (changes go back to the
  original by hand).

## Running intents

- **A fake client cannot see a Deployment's generation, so it hid an upgrade bug** (#129 review, 2026-10-06). Switching
  live preview Deployments from Recreate to rolling raises their `metadata.generation`; the controller then read the
  not-yet-observed generation as "not ready" and, for a preview Ready longer than the rollout timeout, retried it:
  deleting its Deployment and spending a retry. controller-runtime's fake client never bumps generation and envtest has
  no Deployment controller, so the PR's own tests passed. Caught by an adversarial reviewer, reproduced on a real
  kube-apiserver, fixed before release. Docs/patchy: tests of upgrade paths that patch a live spec need generation
  semantics (an interceptor, or envtest).
- **A plan can be confidently wrong about a security header** (hello-web-14, 2026-10-06). The planner claimed inline
  `style="…"` attributes pass Hello.Web's `default-src 'none'` CSP; they do not (style-src falls back to default-src).
  Its test would not have noticed. Cancelled. Docs: the human plan review is where this is caught; tell reviewers to
  read claims about CSP, auth and permissions skeptically.
- **Two intents on one file collide** (hello-web-14). It planned against `main` while Hello.Web#2 (another intent's PR)
  changes the same file, so merging it would have left the demo PR conflicting. Patchy: the planner could be told about
  open intent PRs in the same repository.
- **A flaky check can still start a needless CI-fix round** (overdub#2, 2026-10-06). `studio-test.js` failed in CI and
  passed in the agent's pod; re-running the failed job before patchy's check-fix round launched turned it green, which
  mattered at $9.48 of a $10 ceiling. Patchy: re-run a failed check once before spending a fix round on it.
- **A queued preview reads as "being deployed"** (preview-demo-15). With both slots held, its status comment said the
  preview was being deployed when it was waiting for a slot. Patchy: say "waiting for a preview slot".
- **The 0.12.19 gate needed an approval** (2026-10-06). The investigation recommended remediation at 0.95 confidence but
  held it (`breakingChangeAvailable`); earlier gates went straight through. Docs: the gate procedure may need
  `/patchy approve` after `/patchy expedite`.
- **Dex on the shared ALB, three snags** (operator, 2026-10-06): the chart's image runs as the named user `dex`, which
  `runAsNonRoot` cannot verify (pin uid 1001); its entrypoint renders config into `/tmp` (emptyDir under a read-only
  root); and the shared ALB's IngressClassParams admitted only the `patchy` namespace (FailedLoadGroupID; the allowlist
  now names `dex` too, and the refusal left the ALB untouched). Docs: list these in the sign-in guide.
- **golangci-lint's cache can report another worktree's paths** (operator). After a scratch worktree was deleted,
  `make pr` in a different worktree failed on `../patchy-integ/...` lll findings from the cache;
  `golangci-lint cache clean` fixed it.
- **The first private repository could not get its pull request** (overdub-12, 2026-10-06). The build succeeded and
  pushed `patchy-intent/overdub-12`, then GitHub refused the pull request: `422 ... not all refs are readable`. Patchy
  opened pull requests with a pull-requests-write token only; in a public repository the refs are readable anyway, so
  Hello.Web, preview-demo and marigold never showed it. The intent parks Blocked (`PullRequestRefused`) and retries when
  the Project changes or the controller restarts. The fix (for 0.12.19) opens pull requests with contents read added.
  Docs: test onboarding against a private repository; patchy: e2e has no notion of a private repository.
- **A report can still fail on the last attempt, so it was fixed by hand** (overdub-12 attempt 2, operator, owner
  approved). Attempt 1 failed `report_invalid` ($3.70: a sentence where the schema wants a list). Attempt 2's report had
  a 664-character note (limit 500); the agent had checked its YAML parsed but did not know the limit. The operator split
  that note at a sentence boundary inside the pod, wording unchanged, before the agent finished; the report then passed.
  Report repair (give the validation error back to the same session) makes this automatic.
- **Claude's Bash tool backgrounds a command after 10 minutes** (overdub-12). The agent ran the app's full 63-suite run;
  at 10 minutes the CLI moved it to the background and returned, and the agent went on (CI subset, screenshots, report)
  while polling it. So one long command does not trip the 20-minute idle watchdog. Docs: an agent can leave a long
  command running; patchy: nothing to change.
- **On the large class, overdub's build behaved** (overdub-12 attempt 2): 38 s for the transport browser test, about a
  minute for the 11-suite CI subset, $3.56 and about 37 minutes, most of it fitting the new toggle into the transport
  bar at 1280/1366/1440 px. Auto Mode picked a `c6a.2xlarge` in under a minute. The intent's total was $9.48 of its $10
  ceiling (plan $2.22, two builds).
- **Hello.Web is the quick demo** (hello-web-13, 2026-10-06). "Greet visitors by time of day on a styled card": plan
  $0.47 (3.5 minutes), build $0.42, PR about 7 minutes after filing, preview Ready about 3 minutes later, serving the
  greeting with the stylesheet allowed by its `sha256` hash in the CSP. Patchy did not post the preview link anywhere
  (intent-controller never reads Preview status); a fix is going into 0.12.19.
- **With 100 turns the replan finished** (overdub-12, 2026-10-06): about 67 turns, $2.22, 10 minutes, a 243-line plan
  with 3 questions. It also showed the planner's sandbox is uneven. It tried to run the transport browser test 4 times
  (each refused; plans are read-only) before going back to reading, and its Write tool was not refused: it created a
  scratch file inside its copy of the repository, then could not delete it because `rm` is. Harmless (a plan's tree is
  discarded and nothing is pushed from it), but patchy should tell the planner up front that it cannot run commands and
  either refuse Write in the repository tree or say "read-only" means shell only.
- **The per-intent cost ceiling survives a re-label** (operator). Re-labelling a Failed issue resumes the same Intent,
  whose recorded spend still counts toward `limits.maxCostMicroUSD`: overdub-10 had spent $6.07 of $10, too little left
  for a 100-turn plan and a build. It was closed and re-filed as intents#12. Docs: retry an expensive intent as a new
  issue, or raise the ceiling.
- **40 plan turns is too few for a real app** (overdub-10, 2026-10-06). The successful first plan used about 38 turns;
  the replan's two attempts both hit 40 ($1.46 and $0.96) while reading sensibly: the transport UI, the recording tests,
  the icon helper, the user guide. Each turn made one tool call, never several in parallel. The plan prompt gives the
  build's turn budget but not the planner's own until a retry, after it has already run out. On devthenet
  `intentController.config.plan` is now 100 turns and 30m (chart-wide; there is no per-Project setting). Patchy: tell
  the planner its limit up front, encourage parallel reads, let a Project override its plan and build limits, and make a
  mid-run dollar cap (broker limits) the real spend guard. Docs: a transcript's `turns` counts entries (each tool call
  and each result), about twice the model turns.
- **The 0.12.18 fresh-Finding gate passed** (2026-10-06). A new weak-key alert became one Finding and one tracking
  issue. `/patchy expedite` took it through investigation and remediation to a repair PR. The agent raised the key size
  and added a regression test; all PR and post-merge CodeQL checks passed. Patchy closed the issue as completed, emitted
  each tracking marker once, and recorded no duplicate Finding.
- **Broker replacement briefly affects controller startup** (0.12.18 rollout, 2026-10-05). During the patchy chart
  upgrade, investigation-controller and remediation-controller each logged one broker readiness fetch error while the
  broker pod was being replaced. All nine Deployments became Ready with zero restarts, and neither controller logged
  another error in the following minute. Check the settled state after a rollout, as well as the first startup lines.
- **Pod eviction messages can expose cluster details through public intent issues** (resource-class review, 2026-10-05).
  The first eviction detail copied Kubernetes' raw pod message to the run, then the Failed intent's status comment
  copied it to GitHub. It may name nodes or other private infrastructure. The run keeps the message for operators; the
  issue now says only that the pod was evicted and names the run to inspect.
- **A scheduling fast-fail needs a durable outcome before deleting its Job** (resource-class review, 2026-10-05). PR
  #122's first unschedulable path deleted the Job before writing the run's uncounted outcome. A transient status write
  failure would make the retry see a missing Job and count an ordinary aborted attempt, even though the agent never ran.
  The handler now settles first, then deletes; a terminal pass retries a failed delete. Regression tests cover both
  failed writes and failed deletes.

- **A timed-out run records no usage, so the cost ceiling cannot see it** (overdub-10, 2026-10-04). The second build
  attempt timed out after an hour; its run status has no usage, so the intent reports $3.65 while the broker counted
  another 2.9 million tokens for that pod (about $1.50 to $2.00 more; the real total is about $5.50). The ceiling is
  checked against the reported usage, so spend in killed runs is invisible to it. Patchy: record usage for every outcome
  (from the stream read so far, or the broker's per-pod count). Fixed in
  [#118](https://github.com/devthenet-labs/patchy/pull/118): every outcome now records the usage its stream reported,
  input and cache tokens exactly and output tokens as a floor (claude streams each call's output count before it is
  final), so the broker's count remains the authoritative one.
- **An agent can wait on one command until the stage times out** (overdub-10 attempt 2). After ten minutes of work it
  ran `npm run test:ci` (11 browser suites, three at a time) on the 2 vCPU, 3.7 GiB node; the command never returned and
  the agent sat idle for 50 minutes until the one-hour timeout. The same subset takes under two minutes on a 2 vCPU CI
  runner with two at a time. Patchy: a no-progress watchdog (no model request for N minutes ends the run with "a command
  ran N minutes") and right-sized agents (agent resource classes,
  [#122](https://github.com/devthenet-labs/patchy/pull/122)). Watchdog in
  [#119](https://github.com/devthenet-labs/patchy/pull/119): 20 minutes with no model turn or tool result ends the run
  as a timeout naming the command (`no progress for 20m while running Bash (20m without returning): npm run test:ci …`),
  which counts as an attempt; per-stage `*-idle-timeout` flags and chart values, `0s` disables it.
- **The agent handled pre-existing failures well** (overdub-10 attempt 2). It saw `shell-test.js` fail, ran it again on
  the untouched tree with `git stash` to prove the failures were there before its change, then moved on to the CI
  subset. Worth showing in the docs as the behaviour a repository's CLAUDE.md test note makes possible.
- **overdub-10's outcome:** plan $1.45; build attempt 1 failed `report_invalid` ($2.20); attempt 2 timed out (about
  $1.80, unrecorded). Both attempts used, so the intent ended Failed and nothing more ran: the attempt cap and timeout
  held.
- **A report-format slip throws away a whole build** (overdub-10, 2026-10-04). The build ran 110 turns and 31 minutes
  ($2.20) and then failed `report_invalid`: one of its report's notes was 574 characters, over the 500 the build report
  allows. patchy discarded the changeset with it and started a second attempt from scratch in a fresh pod. Patchy: a
  report problem should never cost the work. Validate the report in the pod and let the agent fix it in the same
  session, or trim an over-long note. Docs: until then, `report_invalid` retries are full reruns and cost as much again.
- **The egress broker's token limits were never set on devthenet** (2026-10-04). The broker counts every pod's tokens
  (`pod_tokens` in its audit line) but enforces a limit only when `egressBroker.limits` is configured, and it was not,
  although helm.md says to size it before enabling repository images. The grant's 800k token budget evidently does not
  count cached re-reads: overdub-10's build passed 5 million tokens through the broker (almost all cache reads) without
  tripping it. Patchy/docs: say plainly which limit bounds spend; set broker limits from observed runs.
- **Spend on a real app, for scale** (overdub-10, Sonnet 5): plan $1.45 (~7 min), first build attempt $2.20 (31 min on 2
  vCPU, about 20 of them waiting on browser tests). Hello.Web's whole intent was $0.55; marigold's two-repo intent
  $1.51.
- **Agent Jobs request no CPU or memory, so heavy builds starve** (overdub-10, 2026-10-04). Pods set only
  ephemeral-storage, so EKS Auto Mode placed an overdub build (Chromium + audio rendering) on a `c6a.large`: 2 vCPU,
  ~3.7 GiB, shared. Builds are slow and risk OOM. Addressed in
  [#122](https://github.com/devthenet-labs/patchy/pull/122): the operator defines named resource classes in the patchy
  chart (`agent.resources.classes`) and a Project picks one per repository (`agentResourceClass`) for its builds, revise
  and check-fix rounds; plans and Findings stay on `agent.resources.default` (none, as before). A pod no node fits is
  stopped after 10 minutes without spending an attempt, an OOM kill names the class to raise, and every agent pod
  carries `karpenter.sh/do-not-disrupt`. Docs: "Sizing agents" in docs/intents/deploying.md. Still to measure live:
  overdub's real peak memory and the node Auto Mode picks for `large`.
- **The planner can ignore the repository's own test guidance** (overdub-10). Its plan named `tools/shell-test.js` as
  "should run cleanly in the build image", although the app's CLAUDE.md lists `shell` among the Mac-tuned suites.
  Patchy: the plan prompt should tell the planner to read the repository's test guidance and prefer its CI test command.
- **The planner caught that the request already existed** (overdub-10). "Add a count-in toggle": the toggle was already
  on the transport; the plan said so and reduced the change to the one missing behaviour (locking during a take). Good
  behaviour to show in the docs. Issue authors should check the app before filing.
- **A flaky check plus `checks.fix` can start a needless CI-fix round** (overdub). `studio-test.js` failed once on main
  under runner load and passed on re-run. Docs: keep flaky checks out of `checks.fix`, or expect a fix round to look at
  them.
- **An interrupted `gh issue create` may already have run** (operator). A stopped command had created intents#10; filing
  again made a duplicate (#11, cancelled and closed before it planned). Check for an existing issue before re-filing.

## Earlier onboardings

- Hello.Web (W10, 2026-10-03): the onboarding path with released tools, and the docs bugs it found (all fixed in PR
  108). Details in HANDOFF.md and gbrain page `patchy-w10-live-onboarding-phase1`.
