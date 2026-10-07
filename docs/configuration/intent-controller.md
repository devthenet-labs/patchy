# intent-controller

Intent-driven development, and the second **optional** controller: deployments without Projects do not run it. A human
opens an issue in an intent repository and labels it for a project. intent-controller plans the work in a read-only
agent Job and posts the plan to the issue. An approver approves it by label or command, and it builds exactly that plan
in the application repository's own image. It pushes the result to a branch it creates once, opens the pull request, and
closes the issue when the pull request merges. With `--intent-multi-repo`, one intent can change several application
repositories of a Project, with one pull request in each (see [Several repositories](#several-repositories)). The
design, its security posture and the roadmap are in [Intent-driven development](../design/intent-driven-development.md).

```sh
intent-controller serve --namespace patchy \
  --claude-agent-image ghcr.io/devthenet-labs/patchy/claude-agent-runner:v0.12.0 \
  --broker-url http://patchy-egress-broker.patchy.svc.cluster.local:8080
```

It **polls GitHub** instead of taking webhooks: it reads only what the current phase of each intent waits on, so it adds
no inbound surface and leaves the internet-facing integration-controller untouched. Every reconcile is a function of
what GitHub's API reports and what the custom resources hold. A lost or repeated event cannot move an intent, and every
GitHub write is idempotent.

State lives in three custom resources, all written by intent-controller alone:

| Kind        | What it is                                                                                         |
| ----------- | -------------------------------------------------------------------------------------------------- |
| `Project`   | Operator configuration: the intent repository, labels, approvers, app repositories and limits      |
| `Intent`    | One per intent issue, named `<project>-<issue>`, carrying the phase and the approved plan's digest |
| `IntentRun` | One immutable attempt of one stage (`plan`, `build` or `revise`); it owns its Repositories and Job |

`patchy get intents`, `patchy get irun` and `patchy get proj` list them (see [the CLI](../cli.md)). Humans write only an
Intent's `spec.suspend`; everything else happens on the issue.

Suspending an intent launches nothing and writes nothing to GitHub until the suspension is cleared. A build Job already
running finishes; its push (the commit and the branch) waits, marked `PushHeld` on its IntentRun, and gives its slot of
the run pool to other intents meanwhile. The finished Job is kept only for `--job-ttl` (1h by default): a suspension
that outlasts it loses the unpushed changeset, and the run ends `hold_expired`. That costs the agent's spend but not one
of the build's attempts, and clearing the suspension starts the next one.

Deleting a Project (a `patchy-config` uninstall, or a rename) holds its intents the same way: they wait, nothing is
written to GitHub for them, a run granted a slot but not yet launched hands the slot back, and a Job that finishes
meanwhile has its push held (`PushHeld`, reason `ProjectGone`) until a Project of that name exists again. The push is
then checked against what that Project lists. A held push's reason is always what it waits on now: one held for a
suspension that is lifted while its Project is gone says `ProjectGone`.

## Flags

The [shared flags](index.md#shared-flags-every-controller), plus the settings below. They carry an `intent-` prefix no
other binary binds, so the shared kustomize ConfigMap cannot set one by accident.

| Flag                                            | Env                                                       | Default                     | Purpose                                                                                                                                                                                                                                               |
| ----------------------------------------------- | --------------------------------------------------------- | --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--intent-poll-interval`                        | `PATCHY_INTENT_POLL_INTERVAL`                             | `60s`                       | How often each Project's intent repository and each active intent's issue are polled                                                                                                                                                                  |
| `--intent-approval-poll-interval`               | `PATCHY_INTENT_APPROVAL_POLL_INTERVAL`                    | `30s`                       | How often an intent awaiting approval polls its issue's events                                                                                                                                                                                        |
| `--intent-pr-poll-interval`                     | `PATCHY_INTENT_PR_POLL_INTERVAL`                          | `60s`                       | How often an intent in review polls its pull request                                                                                                                                                                                                  |
| `--intent-previews-enabled`                     | `PATCHY_INTENT_PREVIEWS_ENABLED`                          | `false`                     | PRs, and unchanged repositories' main heads, into Preview CRs, and post each preview's link (see [The preview link](#the-preview-link)); Helm: `previewController.enabled`                                                                            |
| `--intent-multi-repo`                           | `PATCHY_INTENT_MULTI_REPO`                                | `false`                     | Run intents of Projects listing several repositories; off, their intents are held `Blocked`                                                                                                                                                           |
| `--intent-max-concurrent-runs`                  | `PATCHY_INTENT_MAX_CONCURRENT_RUNS`                       | `1`                         | Intent agent Jobs at once, a pool apart from remediation's; a multi-repository intent's builds share it                                                                                                                                               |
| `--intent-rate-limit-floor`                     | `PATCHY_INTENT_RATE_LIMIT_FLOOR`                          | `1000`                      | Pause intent polling while the installation has fewer core requests left than this; `0` disables                                                                                                                                                      |
| `--intent-ttl`                                  | `PATCHY_INTENT_TTL`                                       | `336h` (14 days)            | How long an ended intent is kept, with everything it owns; `0` keeps it forever                                                                                                                                                                       |
| `--intent-job-deadline`                         | `PATCHY_INTENT_JOB_DEADLINE`                              | `90m`                       | `activeDeadlineSeconds` on every intent Job; at least both stage timeouts                                                                                                                                                                             |
| `--intent-plan-model`                           | `PATCHY_INTENT_PLAN_MODEL`                                | `anthropic/claude-sonnet-5` | Canonical model the plan stage runs                                                                                                                                                                                                                   |
| `--intent-plan-max-turns`                       | `PATCHY_INTENT_PLAN_MAX_TURNS`                            | `40`                        | Most agent turns a plan run may take                                                                                                                                                                                                                  |
| `--intent-plan-token-budget`                    | `PATCHY_INTENT_PLAN_TOKEN_BUDGET`                         | `200000`                    | Most output tokens a plan run may spend                                                                                                                                                                                                               |
| `--intent-plan-timeout`                         | `PATCHY_INTENT_PLAN_TIMEOUT`                              | `20m`                       | Wall-clock limit of a plan run                                                                                                                                                                                                                        |
| `--intent-plan-idle-timeout`                    | `PATCHY_INTENT_PLAN_IDLE_TIMEOUT`                         | `20m`                       | End a plan run that makes no progress for this long; `0s` disables                                                                                                                                                                                    |
| `--intent-build-model`                          | `PATCHY_INTENT_BUILD_MODEL`                               | `anthropic/claude-sonnet-5` | Canonical model the build stage runs                                                                                                                                                                                                                  |
| `--intent-build-max-turns`                      | `PATCHY_INTENT_BUILD_MAX_TURNS`                           | `150`                       | Most agent turns a build run may take                                                                                                                                                                                                                 |
| `--intent-build-token-budget`                   | `PATCHY_INTENT_BUILD_TOKEN_BUDGET`                        | `800000`                    | Most output tokens a build run may spend                                                                                                                                                                                                              |
| `--intent-build-timeout`                        | `PATCHY_INTENT_BUILD_TIMEOUT`                             | `60m`                       | Wall-clock limit of a build run                                                                                                                                                                                                                       |
| `--intent-build-idle-timeout`                   | `PATCHY_INTENT_BUILD_IDLE_TIMEOUT`                        | `20m`                       | End a build run that makes no progress for this long; `0s` disables                                                                                                                                                                                   |
| `--intent-revise-max-turns`                     | `PATCHY_INTENT_REVISE_MAX_TURNS`                          | `80`                        | Most agent turns a revise or check-fix run may take                                                                                                                                                                                                   |
| `--intent-revise-token-budget`                  | `PATCHY_INTENT_REVISE_TOKEN_BUDGET`                       | `400000`                    | Most output tokens a revise or check-fix run may spend                                                                                                                                                                                                |
| `--intent-revise-timeout`                       | `PATCHY_INTENT_REVISE_TIMEOUT`                            | `45m`                       | Wall-clock limit of a revise or check-fix run                                                                                                                                                                                                         |
| `--intent-revise-idle-timeout`                  | `PATCHY_INTENT_REVISE_IDLE_TIMEOUT`                       | `20m`                       | End a revise or check-fix run that makes no progress for this long; `0s` disables                                                                                                                                                                     |
| `--agent-namespace`                             | `PATCHY_AGENT_NAMESPACE`                                  | `patchy-agents`             | Namespace the agent Jobs run in                                                                                                                                                                                                                       |
| `--agent-service-account`                       | `PATCHY_AGENT_SERVICE_ACCOUNT`                            | `patchy-agent`              | Service account the agent Jobs run as                                                                                                                                                                                                                 |
| `--job-ttl`                                     | `PATCHY_JOB_TTL`                                          | `1h`                        | `ttlSecondsAfterFinished` on a finished agent Job                                                                                                                                                                                                     |
| `--repository-images`                           | `PATCHY_REPOSITORY_IMAGES`                                | `false`                     | Run a Repository's pinned repository-declared image; a build requires one (see below)                                                                                                                                                                 |
| `--agent-ephemeral-storage`                     | `PATCHY_AGENT_EPHEMERAL_STORAGE`                          | —                           | Ephemeral-storage request and limit on both agent containers; **required** with the flag above                                                                                                                                                        |
| `--changeset-max-entries`                       | `PATCHY_CHANGESET_MAX_ENTRIES`                            | `500`                       | Most files a build's changeset may touch; more is rejected before any forge call                                                                                                                                                                      |
| `--intent-resource-classes`                     | `PATCHY_INTENT_RESOURCE_CLASSES`                          | —                           | The operator's resource classes as JSON, name to `{"requests": {"cpu", "memory"}, "limits": {"memory", "cpu"}}` (the CPU limit optional); a Project picks one per repository ([Resource classes](#resource-classes)). Helm: `agent.resources.classes` |
| `--agent-cpu-request`, `--agent-memory-request` | `PATCHY_AGENT_CPU_REQUEST`, `PATCHY_AGENT_MEMORY_REQUEST` | —                           | The default CPU and memory request on both containers of every agent Job without a class; unset requests none. Helm: `agent.resources.default`                                                                                                        |
| `--agent-cpu-limit`, `--agent-memory-limit`     | `PATCHY_AGENT_CPU_LIMIT`, `PATCHY_AGENT_MEMORY_LIMIT`     | —                           | The default CPU and memory limit, at or above its request; unset sets none                                                                                                                                                                            |

Every resource quantity is checked at startup: positive, between `10m` and `64` CPUs or `128Mi` and `512Gi` of memory,
and each request at or below its limit; each class must also set `requests.cpu`, `requests.memory` and `limits.memory`,
and at most 16 are accepted. A value that fails stops the controller from starting, naming the class and the quantity,
rather than becoming a Job the API server refuses or a pod no node fits.

The per-stage limits are ceilings. A Project's `limits` may lower them for its own intents, never raise them. The idle
timeouts are the runner's no-progress watchdog ([agent-runner](agent-runner.md#the-idle-watchdog)), set per stage and
not by a Project: a run that waits that long on a command that never returns ends as a `timeout` naming the command,
which counts as one of the run's attempts, as the wall clock's timeout does. The plan stage's equals its wall clock by
default, so only a build, revise or check-fix run is ever ended early. The controller refuses to start with a Job
deadline shorter than either stage timeout. Any longer deadline works: each Job's broker caller token is minted for the
deadline plus 15 minutes (at least an hour), so it always outlives the Job. `--job-deadline`, `--model-allowlist` and
the `--investigate-*`/`--remediate-*` flags belong to the finding job controllers and are not read here.

### Brokered claude only

Intents run on brokered claude and nothing else, because no other harness honours the plan stage's read-only sandbox.
The runner flags are the shared ones (`--claude-agent-image`, `--broker-url`, `--broker-token-audience`,
`--claude-provider*`, `--claude-model-map`, `--claude-provider-env`, `--harnesses`; see the
[investigation-controller](investigation-controller.md#agent-job-flags)). At startup the controller refuses both stage
models unless they resolve to the same harness, and that harness is brokered claude (or the fake harness in dev; the e2e
suite runs it on brokered claude, as production does). The Helm chart stamps `PATCHY_HARNESSES=claude` for it and
deploys the egress broker whenever it is enabled, even if the finding fleet runs no claude.

### Repository-declared images

A plan runs read-only on the default runner image. A build runs only in the application repository's **accepted**
repository-declared image (see
[repository-declared runner images](investigation-controller.md#repository-declared-runner-images)): the pin must be set
and not rejected, and the default image, which has no toolchain, is never a fallback. If the image is missing or
rejected, or `jobs.Create` reports that the default image ran, the intent goes to `Blocked` with `ImageRequired`. A
Project can opt out with `requireRepositoryImage: false`. So without `--repository-images` every build blocks unless its
Project opts out.

A block does not lift on its own when the image becomes acceptable. source-controller pins a Repository's image exactly
once, so fixing its allowlist or cosign key never changes the blocked build's pin. The block is checked again, with a
new build attempt pinned afresh, when:

- the Project's spec changes (setting `requireRepositoryImage: false` lifts it outright);
- the app repository's default branch moves, since a new commit may declare an image that is accepted;
- intent-controller restarts, which a change to its configuration does.

A block because `--repository-images` is off lifts once it is on, and a block because the sandbox breaker tripped lifts
once the breaker is clear; both need a restart. Each check is a new attempt, and once all 16 attempt numbers of the
build are spent the intent fails instead.

A build's changeset is held to the intent rules whatever image ran it. Nothing under `.github/`, `.patchy/` or
`.devcontainer/` is accepted, so an agent cannot choose its own next sandbox.

## Projects

A Project is operator configuration, written through the patchy-config chart's `projects` array (see
[Helm charts](../deployment/helm.md)) or applied directly. Writing `projects` is admin-only in RBAC, because a Project
is the power to point agents at repositories.

```yaml
projects:
  - name: target # at most 25 characters; its Intents are target-<issue>
    spec:
      intentRepository: https://github.com/acme/intents # immutable
      approvers:
        logins: [octocat] # the only logins whose actions count
      repositories: # one, or up to eight with --intent-multi-repo
        - name: target # the key: a DNS label of at most 16 characters
          url: https://github.com/acme/target
          # agentResourceClass: large  # one of the operator's classes; see Resource classes below
      # labels: {trigger: patchy:target, approve: patchy:approved}  (the defaults)
      # limits: {maxActiveIntents: 2, maxCostMicroUSD: 10000000, plan: {...}, build: {...}}
      # checks: {fix: [test], timeout: 30m, rerunFailed: false}  (see CI-fix rounds below)
      # requireRepositoryImage: true
```

The Project reports `Ready` once:

- it lists one repository, or `--intent-multi-repo` is on (`UnsupportedRepositories` otherwise);
- each of its repositories resolves to exactly one Forge (`ForgeUnresolved` otherwise), whose credential Secret
  intent-controller may read (`ForgeSecretUnreadable` otherwise); the message names the repository;
- the App is installed on the intent repository and every app repository, with the permissions intents use: issues write
  on the intent repository; contents and pull requests write and issues read (a reviewer's permission and the rate
  budget are read with it) on each app repository; when `spec.checks.fix` names a check, checks, statuses and actions
  read on each app repository too; and when `spec.checks.rerunFailed` is set beside it, actions write there as well
  (`AppNotInstalled` otherwise, naming the repository and the permission);
- no other Project shares its intent repository and trigger label (`AmbiguousIntentRepository`).

It creates the trigger and approve labels when they are missing. An issue whose Intent name is held by another
repository's issue is reported as `IntentNameConflict`, never skipped silently.

Use exactly one Project trigger label per issue. If two are present before discovery, neither Project starts an Intent;
each reports the issue in `IntentNameConflict` until one label is removed. If a second Project labels an issue that
already has an Intent, patchy removes the second trigger and reports the conflict. A shared approval label or
`/patchy approve` is accepted only when the issue has one Intent; an anomalous issue with two Intents refuses both
approvals without removing the shared label. Resolve the conflict, then remove and reapply the label or post a new
command.

### Resource classes

`spec.repositories[].agentResourceClass` names one of the classes the operator defines (`--intent-resource-classes`,
Helm `agent.resources.classes`). That repository's build, revise and check-fix runs get the class's CPU and memory, on
both containers, in place of the default (`--agent-*`, Helm `agent.resources.default`); the ephemeral-storage wall is
unchanged. Plans always run on the default, whatever the Project picks, and so does every repository that picks none.
The class is read from the live Project at each launch, so an edit applies from the next run; a running Job keeps its
size. A Job on a class records it in its `patchy.bitwisemedia.uk/resource-class` annotation; a Job on the default
carries none. The class list is the spend ceiling: a Project can pick only a size the operator defined.

- **A class intent-controller does not define** launches nothing. The run waits `Pending`: it holds no slot of the run
  pool and spends no attempt, and the scheduler never grants it one, so it cannot hold up other runs. Its intent is
  `Blocked` with `ResourcesUnavailable` (reason `UnknownResourceClass`), naming the repository, the class and the
  classes defined. The Project stays `Ready` and reports `ResourceClassesResolved: False`, naming each repository whose
  class is unknown (`True` once every pick is defined; absent when it picks none), so discovery, plans and the other
  repositories carry on. Defining the class, or changing the pick, lifts the block, and the same run launches.
- **A pod no node can fit.** An agent pod whose `PodScheduled` condition stays `False` with reason `Unschedulable` for
  10 minutes, long enough for a node autoscaler to add a node that fits, has its Job deleted, and the run ends
  `unschedulable` with what the pod asked for and the scheduler's message in its detail (which never reaches GitHub).
  The agent never ran, so the attempt does not count; it is not retried either, because a retry would wait the same way.
  A plan or build blocks its intent with `ResourcesUnavailable` (reason `Unschedulable`) until the Project's spec
  changes or intent-controller restarts (a chart upgrade that changes the classes restarts it), and the next attempt
  then runs; a revise or check-fix round ends, its notice on the pull request saying no node could fit the agent. This
  holds for every intent run, on a class or on the default.
- **An agent that stopped without a result** says why in the run's detail when the Job does: OOM-killed, naming the
  memory limit and the class and what to raise; evicted; or past its deadline. An eviction records outcome `evicted`;
  its raw Kubernetes pod message stays on the run for operators, while the intent issue says only that the pod was
  evicted. An OOM kill and an eviction count as attempts: the agent may have run.

Every agent pod, on a class or not, carries `karpenter.sh/do-not-disrupt: "true"`, so Karpenter (EKS Auto Mode's node
manager) does not evict a running agent to consolidate its node: an agent Job runs once (`backoffLimit: 0`), and an
eviction would end the run with nothing to show for it. Sizing advice, with a worked example, is in
[Deploying intents](../intents/deploying.md#sizing-agents).

## On the issue

| Action                                            | Effect                                                  |
| ------------------------------------------------- | ------------------------------------------------------- |
| The trigger label (`patchy:<project>`)            | Starts an intent: plans it                              |
| The approve label, or `/patchy approve`           | Approves the posted plan; the build starts              |
| The trigger label re-applied, or `/patchy replan` | Plans again, with approver comments since the last plan |
| The trigger label re-applied on a `Failed` intent | Revives it: a new plan, and a new approval              |
| `/patchy cancel`                                  | Closes the intent and its issue (`not_planned`)         |

An action counts only when GitHub's API shows who took it: the actor of a label event, or a comment's author. That
account must be in the Project's `approvers.logins`, have write access to the intent repository, and not be a bot.
Anything else gets one refusal and changes nothing; a refused approve label is removed, and a trigger from anyone else
closes the intent before it plans. A label applied as the issue is created, by an issue form's `labels:` or by
`gh issue create --label`, is a label event whose actor is the issue's author: such an issue starts an intent only when
its author is an approver, so give everyone else a form without the trigger label, for an approver to add. A command
comment gets a 👀 reaction, and every answered action gets exactly one reply, never a second, even if patchy's reply is
deleted.

A comment edited after it was posted is never taken as a command: GitHub lets anyone with write access edit anyone's
comment and still shows the original author. patchy answers it once, saying so, and the author can post the command
again in a new comment. An edited comment is also left out of a replan's approver comments. An account that is not an
approver (or is a bot) is refused whatever its command says, and gets that refusal once per intent; its later commands
get neither a reaction nor a reply, so nobody can make patchy write to GitHub once per comment.

The plan comment shows the plan's exact bytes in a code block. An approval counts only if all of these hold:

- it is newer than the plan comment;
- the plan comment still hashes to the digest recorded when it was posted;
- the issue still reads as it did when the plan was made;
- for the label, the label is still on the issue.

The build receives only the approved plan, never the issue. The pull request comes from `patchy-intent/<intent>`, which
patchy creates once and never forces. Its body says `Part of <intent repository>#<n>` and carries no closing keyword.
patchy closes the issue itself when the pull request merges.

patchy never deletes an intent branch, and an intent's name comes back once its issue becomes an intent again (reopened
and labelled after the TTL deleted the first). Before a build launches, patchy reads `patchy-intent/<intent>`: if it
exists at a commit this intent did not push, the intent is `Blocked` with `BranchConflict` before any build is spent,
and resumes once someone deletes the branch. Delete a merged intent's branch (or let GitHub delete head branches on
merge) to keep that from happening.

### Other intents' open pull requests

A plan is told of the pull requests other intents of its Project have open, so it does not plan a change that collides
with one it cannot see: the default branch it reads has none of their changes. As the plan run is created, patchy reads
each other intent of the Project that has not ended, and each pull request it records open in one of the Project's
repositories, as GitHub has it then: its title, how many files it changes and the first 50 of them. At most five are
read, the longest-open intents' first. They are recorded in the run's input ConfigMap (`open-pull-requests.json`),
beside the request and outside its digest, and handed to the plan Job, whose prompt lists them as data, not
instructions, under a statement that their titles and paths come from whoever opened or pushed to those pull requests.
The plan is asked to avoid the files they change wherever the request allows, and to name each overlap it cannot avoid
among its questions, so the approver can decide whether to wait for that pull request to merge first.

Intents of another Project are never read, even in the same repository. A pull request GitHub reports closed, or no
longer has, is left out. Any other failure to read one, the rate floor included, leaves the whole list out, with a
warning in the controller's log, and the intent plans as it would have without the list. The reads take the pull
requests read token on each repository, so they need no permission a Ready Project does not already have.

## On the pull request

Once the pull request is open, the intent is `InReview`, and patchy runs rounds on it, each a new agent run on the pull
request's head, in the same image as its build, pushed as a fast-forward of the intent branch (never forced):

- **Revision rounds** from an approver: a review requesting changes (after a two-minute quiet period, so a review in
  several parts is read whole), or `/patchy revise <what to change>` commented on the pull request. The round reads the
  approvers' reviews and comments since the last round. `limits.maxRevisions` bounds them.
- **CI-fix rounds** from a failed check: when a check the Project names in `checks.fix` (`[test]`, say) fails on the
  head patchy last pushed, the round reads that check's output, annotations and the tail of its Actions job log.
  `limits.maxCheckFixes` bounds them. A CI-fix round that does not fix the failure stops automatic fixing: the same
  failure again holds the intent `Blocked` with `ChecksFailing` (`RepeatedFailure`) for a human, until the Project
  changes. Two failures are compared without the log's times, durations (`412ms`, and `2.345 s` or `(5 ms)` as Jest and
  Maven print them), commit hashes, long ids (runner, job and process numbers), process ids (`(node:2073)`, `pid 2073`),
  addresses, and source positions (`app_test.go:12`, `line 12`, `app.test.ts(12,5)`); every other number counts, so a
  failure whose values moved (coverage from 71.3% to 76.1%, a test from `got 3` to `got 4`) is progress, and gets
  another round. A tool that prints some other volatile number in its last lines (a random seed, say) can still make the
  same failure read as a new one, which costs a round, up to `limits.maxCheckFixes`.
- **Re-runs before a CI-fix round**, with `checks.rerunFailed: true` (off by default): the first time a named check
  fails on a head patchy pushed, patchy re-runs the failed jobs of the GitHub Actions run behind it once, instead of
  starting a round, so a flaky test that passes the second time costs no agent run. The re-run is recorded on the
  intent's pull request (`status.pullRequests[].checksRerun`: the head, the check runs, the Actions runs, when), so a
  restart neither asks again nor forgets it, and `checks.timeout` counts from it. If the re-run passes, the head is
  settled and nothing more happens; if it fails again at the same head, the CI-fix round starts on that second failure.
  Each new head patchy pushes (a round's fix included) gets its one re-run. A run whose other jobs are still running is
  waited for, since GitHub re-runs only a completed run. The round starts at once, with no re-run, when a failure has no
  Actions run to re-run (a commit status, or a check another App reports), when GitHub refuses the re-run (a run too old
  to re-run, say), or when the Actions run is still running as `checks.timeout` passes. A re-run that GitHub started but
  that is still running when `checks.timeout`, counted from the re-run, passes does not settle the head either: the
  failure it re-ran stands, and the CI-fix round starts on it. It needs the App's **Actions** permission at **Read and
  write** on every app repository (`patchy setup github-app --rerun-failed` asks for it). Without it the Project is not
  `Ready` (`AppNotInstalled`), which stops discovery: no new intent is picked up from the intent repository. Intents
  already in flight keep running; their re-run requests are refused, and each falls back to the CI-fix round at once.

Each round posts one comment on the pull request saying what kind of round it was ("Revision round", or "CI-fix round
for `test`") and what it pushed, and when it pushed, asks the approvers to review again. Once the intent has a pull
request, the issue's status comment counts its revision rounds against `limits.maxRevisions` ("Revisions: 1 of 3") as
the limit counts them: a round that failed counts, and a CI-fix round is not a revision. The summary patchy posts when
the intent ends counts revision and CI-fix rounds apart, the same way.

### The preview link

With `--intent-previews-enabled` and a Project that previews the pull request's repository, patchy tells reviewers where
the preview is, in two places:

- **The issue's status comment** gains a **Preview** line: the link once the preview is live, "waiting for a free
  preview slot" while its Preview is `Queued` behind others (every slot is taken), "being deployed" while
  preview-controller rolls it out, or why it is not available (it could not be deployed, or it expired after its time to
  live). With several previewed repositories it lists what each path serves, at which commit.
- **Each previewed pull request** gets one comment of its own, posted the first time the preview is live at that pull
  request's head. patchy then edits that same comment, never posting another: to "being deployed" when a round or a push
  moves the head (or "waiting for a free preview slot" when there is none to deploy it in yet), back to live with the
  new commit once it is served, to say why when the preview fails or expires, and last to say the preview was removed
  once the intent ends. Edits notify nobody; the round's own comment already asks for the review. A pull request whose
  repository is not previewed (a library) gets no preview comment.

The link is posted only when it can be checked: the Preview is this intent's, has rolled out its current spec, serves
exactly the commits the intent recorded (each pull request's head, or a preview base), and its address is a bare
`https://<intent>.<host suffix>` with no path, port, query or user. Anything else shows no link, and a Ready preview at
an address patchy does not link says so. The Preview's own status message, which can quote the cluster, is never posted:
a failure names the Preview resource instead (`kubectl -n <namespace> get preview <intent> -o yaml` shows why).

patchy reads the Preview once per pass (a poll interval apart at most, a minute by default, so the link can lag Ready by
up to that long), through the API server rather than a watch, so it needs only the `get` on previews the chart already
grants with `previewController.enabled`. A failed read shows no link that pass and leaves the pull request comments as
they are. The comment writes are best effort, like the comment linking a multi-repository intent's pull requests: each
one asks the rate floor of its own repository, nothing is written to a repository that left the Project, one GitHub
refuses (a locked conversation, say) is tried again only once it would say something else, and an ended intent's last
edit never holds anything back. A comment someone deletes is posted again the next time the preview goes live. Turning
`--intent-previews-enabled` off stops all of it, and leaves the comments as they last were.

## Several repositories

With `--intent-multi-repo` (`intentController.config.multiRepo: true` in the chart), a Project may list up to eight
application repositories, and one intent can change several of them:

```yaml
spec:
  repositories:
    - name: web # the key: names the repository's runs, its tree and its preview component
      url: https://github.com/acme/Acme.Web_App # any owner and name
    - name: api
      url: https://github.com/acme/api
```

- **One plan.** The plan Job reads every repository, read-only, on the default image. The first repository is its
  working tree; each other one is a tree fetched digest-pinned to `/workspace/repos/<key>`, and the run records the
  commit and digest of each, so the record keeps what the planner saw. The planner names only the repositories that must
  change, and when it names several, the plan comment lists them above the plan. The approver approves that one plan.
- **One build per repository.** Each repository the plan names gets its own build run,
  `<intent>-bld-r<plan revision>-<key>-a<attempt>`, in that repository's own accepted image, handed the same approved
  plan and told which repository it is in. The builds launch together and run as the run pool allows, so set
  `--intent-max-concurrent-runs` (`intentController.config.maxConcurrentRuns`) to at least the number of repositories an
  intent usually changes, or they run one after another. The Project's cost ceiling is checked once before they launch,
  so the builds of one intent can pass it by up to one fewer than their number.
- **Before any build is spent**, every branch is read: a `patchy-intent/<intent>` that an earlier round of the same
  intent left behind holds the intent `Blocked` with `BranchConflict` (`StaleRoundBranch`), naming the repository, until
  a human deletes it. Every block names the repository it is about.
- **Pull requests open only once every build has pushed**, so a build that fails leaves no pull request behind. Each one
  says it is one of several, and patchy then comments on each with links to the others (best effort: a refused comment
  is reported as `SiblingsLinked` False, retried only once the Project or a pull request's head changes, and holds
  nothing back; a pull request whose repository has left the Project gets no comment, and the condition names it).
- **The pull requests open one per pass**, so an intent can end while they are being opened: a `/patchy cancel` or the
  issue closed, or an approved repository removed from the Project (which fails it), perhaps while a block on a later
  one holds it. The ones already opened are left open, and patchy comments on each, once, that the intent ended, that
  the pull request is no longer tracked and not part of a completed change, and which repositories never got theirs; the
  intent's `UntrackedPullRequests` condition records it. One whose repository has left the Project is not written to,
  and one patchy can no longer reach (no Forge covers its repository, the App's installation refuses it, GitHub refuses
  the comment) is not retried: the condition names each (`NoticeRefused`), and the intent's revival never waits on it.
  patchy closes none of them. Reviving a failed intent starts its pull requests afresh.
- **Repository keys name the runs.** Changing a key while an intent builds is safe; giving one repository's key to
  another (a swap) can make a build's name another repository's run, and then the intent is held `Blocked` with
  `UnsupportedRepositories` (`RepositoryKeyChanged`), naming both, until the Project changes again.
- **Rounds run one at a time per intent**, each on one pull request's repository: a review or `/patchy revise` on a pull
  request revises its own repository, a failed check fixes its own. Pull requests with feedback waiting take turns, and
  feedback that arrives while another pull request's round runs is read by its own next round. The revision and CI-fix
  limits are per intent.
- **A repository removed from the Project is left alone.** patchy writes nothing more to it: a build or round there is
  not launched, or is aborted with nothing pushed (`the repository ... left the project`), a failed round there is not
  retried, and its pull request gets no further round, notice or comment. The other pull requests' rounds go on, even
  once patchy can no longer reach the removed repository at all (no Forge covers it, the App was uninstalled from it):
  patchy still reads its pull request where it can, read-only, so its merge counts. This is the one read patchy makes
  outside what the Project's `Ready` proved. One it can no longer reach keeps the state it was last read in, and is
  asked again only every 15 minutes; while that state is open, the intent waits on it as on any open pull request, and
  closing the issue ends the intent. One it cannot read for a reason that may pass (that installation's rate floor, a
  GitHub error) holds only the intent's ending, once every other pull request has settled: patchy waits to read it
  rather than end the intent on a state that may be stale.
- **Endings.** The intent is `Merged` once every pull request has merged. If one is closed without merging, the intent
  stays in review while any other is open, then ends `Closed`: patchy posts a notice of what merged (already on its
  default branch; patchy reverts nothing) and what did not, and closes the issue as not planned. patchy never closes a
  pull request because another closed.

Off, which is the default, a Project listing several repositories is not Ready (`UnsupportedRepositories`), and every
intent of one is held `Blocked` with `UnsupportedRepositories` wherever it stands: no run is launched and nothing is
pushed. A Job already running when the flag goes off finishes, and spends, all the same, and its push waits: turning the
flag on again resumes each intent where it was held, its held push made (an approve label applied meanwhile is then
honoured). A wait longer than the Job TTL (`agent.jobTTL`, `--job-ttl`) loses the finished Job, and with it the unpushed
work, which is then run again (the attempt does not count). One-repository Projects behave the same either way.

Turning the flag off is the supported rollback. Do not roll intent-controller back to a release before multi-repository
intents while an intent of a multi-repository Project is open: the older controller does not hold them, and would lose
their sibling pull requests' records. Cancel each such intent first, or suspend it (`spec.suspend: true`) until the
controller is rolled forward again.

## Permissions

intent-controller is the second code path that writes to a forge (remediation-controller is the first):

- **Secrets:** in the release namespace, `secrets get` is restricted by `resourceNames` to the Secrets your Forges
  reference (`intentController.forgeSecrets` in the chart, `rbac.yaml` in the kustomize component). A Forge that
  references any other Secret, or a Secret that does not exist, leaves its Projects not Ready with the reason
  `ForgeSecretUnreadable`, whose message names the Secret. Its agent-jobs Role can get, create, update and delete any
  Secret in the agents namespace, including model keys, image-pull credentials and other Jobs' handoffs.
- **GitHub tokens:** each GitHub operation mints its own token, scoped to one repository and one permission. Opening a
  pull request is the one exception: it also asks contents read, without which GitHub refuses a pull request in a
  private repository. The unscoped installation client is never used.
- **Writes:** it writes only to the repositories a Project lists, plus issues on the intent repository. Branches are
  only ever `patchy-intent/…`, and the default branch is never touched.
- **RBAC:**
  - in the release namespace, projects (read, status), intents and intent runs (their lifecycle, status and finalizers),
    Repositories (create, read, delete), Forges (read), ConfigMaps (create, read, update), leases and events; with
    `previewController.enabled`, previews too (create, get, update, delete: the spec it writes, and the status it reads
    for the preview link), never list or watch;
  - in the agents namespace, its own copy of the agent-jobs Role;
  - no ClusterRole.
- **Network:** egress to DNS, the Kubernetes API server and GitHub on 443. It never dials the artifact server or an
  agent pod.
- **GitHub App:** intents need no event subscription. They use issues, contents and pull requests (write) and metadata
  (read), issues read on the app repository too; a Project with `spec.checks.fix` also needs checks, statuses and
  actions (read) on its app repository, for the check-fix rounds, and one with `spec.checks.rerunFailed` actions write
  there, the one write on a repository's CI, minted alone for each re-run request. See
  [Create the GitHub App](../getting-started/github-app.md#intents).

It writes no Finding spec, so it is not exempt from the finding admission policy.

## Polling cost

Each active intent reads about three GitHub resources per interval (its issue, its events, and its comments since the
newest one it has already read). An intent in review also reads its pull request. A plan run, as it is created, also
reads at most two resources for each of up to five other intents' open pull requests (the pull request and one page of
its files). A conditional listing that has not changed returns 304, which costs nothing against the installation's rate
limit. The rate-limit floor pauses all intent polling, pull requests included, while the installation's remaining core
budget is below it, so intents can never starve the security flow of the requests it shares with them. Each poll checks
the installation of the repository it reads: the intent repository and an app repository may be covered by two
installations, and the app repository's is the one the Finding flow shares, so its pull request and a blocked build's
default branch wait on its floor, the issue on the intent repository's. The headers GitHub reports are not consistent
from one response to the next, so the floor is a coarse guard, not an exact budget.
