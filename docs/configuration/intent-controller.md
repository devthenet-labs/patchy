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

## Flags

The [shared flags](index.md#shared-flags-every-controller), plus the settings below. They carry an `intent-` prefix no
other binary binds, so the shared kustomize ConfigMap cannot set one by accident.

| Flag                              | Env                                    | Default                     | Purpose                                                                                                 |
| --------------------------------- | -------------------------------------- | --------------------------- | ------------------------------------------------------------------------------------------------------- |
| `--intent-poll-interval`          | `PATCHY_INTENT_POLL_INTERVAL`          | `60s`                       | How often each Project's intent repository and each active intent's issue are polled                    |
| `--intent-approval-poll-interval` | `PATCHY_INTENT_APPROVAL_POLL_INTERVAL` | `30s`                       | How often an intent awaiting approval polls its issue's events                                          |
| `--intent-pr-poll-interval`       | `PATCHY_INTENT_PR_POLL_INTERVAL`       | `60s`                       | How often an intent in review polls its pull request                                                    |
| `--intent-previews-enabled`       | `PATCHY_INTENT_PREVIEWS_ENABLED`       | `false`                     | PRs, and unchanged repositories' main heads, into Preview CRs; Helm: `previewController.enabled`        |
| `--intent-multi-repo`             | `PATCHY_INTENT_MULTI_REPO`             | `false`                     | Run intents of Projects listing several repositories; off, their intents are held `Blocked`             |
| `--intent-max-concurrent-runs`    | `PATCHY_INTENT_MAX_CONCURRENT_RUNS`    | `1`                         | Intent agent Jobs at once, a pool apart from remediation's; a multi-repository intent's builds share it |
| `--intent-rate-limit-floor`       | `PATCHY_INTENT_RATE_LIMIT_FLOOR`       | `1000`                      | Pause intent polling while the installation has fewer core requests left than this; `0` disables        |
| `--intent-ttl`                    | `PATCHY_INTENT_TTL`                    | `336h` (14 days)            | How long an ended intent is kept, with everything it owns; `0` keeps it forever                         |
| `--intent-job-deadline`           | `PATCHY_INTENT_JOB_DEADLINE`           | `90m`                       | `activeDeadlineSeconds` on every intent Job; at least both stage timeouts                               |
| `--intent-plan-model`             | `PATCHY_INTENT_PLAN_MODEL`             | `anthropic/claude-sonnet-5` | Canonical model the plan stage runs                                                                     |
| `--intent-plan-max-turns`         | `PATCHY_INTENT_PLAN_MAX_TURNS`         | `40`                        | Most agent turns a plan run may take                                                                    |
| `--intent-plan-token-budget`      | `PATCHY_INTENT_PLAN_TOKEN_BUDGET`      | `200000`                    | Most output tokens a plan run may spend                                                                 |
| `--intent-plan-timeout`           | `PATCHY_INTENT_PLAN_TIMEOUT`           | `20m`                       | Wall-clock limit of a plan run                                                                          |
| `--intent-build-model`            | `PATCHY_INTENT_BUILD_MODEL`            | `anthropic/claude-sonnet-5` | Canonical model the build stage runs                                                                    |
| `--intent-build-max-turns`        | `PATCHY_INTENT_BUILD_MAX_TURNS`        | `150`                       | Most agent turns a build run may take                                                                   |
| `--intent-build-token-budget`     | `PATCHY_INTENT_BUILD_TOKEN_BUDGET`     | `800000`                    | Most output tokens a build run may spend                                                                |
| `--intent-build-timeout`          | `PATCHY_INTENT_BUILD_TIMEOUT`          | `60m`                       | Wall-clock limit of a build run                                                                         |
| `--intent-revise-max-turns`       | `PATCHY_INTENT_REVISE_MAX_TURNS`       | `80`                        | Most agent turns a revise or check-fix run may take                                                     |
| `--intent-revise-token-budget`    | `PATCHY_INTENT_REVISE_TOKEN_BUDGET`    | `400000`                    | Most output tokens a revise or check-fix run may spend                                                  |
| `--intent-revise-timeout`         | `PATCHY_INTENT_REVISE_TIMEOUT`         | `45m`                       | Wall-clock limit of a revise or check-fix run                                                           |
| `--agent-namespace`               | `PATCHY_AGENT_NAMESPACE`               | `patchy-agents`             | Namespace the agent Jobs run in                                                                         |
| `--agent-service-account`         | `PATCHY_AGENT_SERVICE_ACCOUNT`         | `patchy-agent`              | Service account the agent Jobs run as                                                                   |
| `--job-ttl`                       | `PATCHY_JOB_TTL`                       | `1h`                        | `ttlSecondsAfterFinished` on a finished agent Job                                                       |
| `--repository-images`             | `PATCHY_REPOSITORY_IMAGES`             | `false`                     | Run a Repository's pinned repository-declared image; a build requires one (see below)                   |
| `--agent-ephemeral-storage`       | `PATCHY_AGENT_EPHEMERAL_STORAGE`       | —                           | Ephemeral-storage request and limit on both agent containers; **required** with the flag above          |
| `--changeset-max-entries`         | `PATCHY_CHANGESET_MAX_ENTRIES`         | `500`                       | Most files a build's changeset may touch; more is rejected before any forge call                        |

The per-stage limits are ceilings. A Project's `limits` may lower them for its own intents, never raise them. The
controller refuses to start with a Job deadline shorter than either stage timeout. Any longer deadline works: each Job's
broker caller token is minted for the deadline plus 15 minutes (at least an hour), so it always outlives the Job.
`--job-deadline`, `--model-allowlist` and the `--investigate-*`/`--remediate-*` flags belong to the finding job
controllers and are not read here.

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
      # labels: {trigger: patchy:target, approve: patchy:approved}  (the defaults)
      # limits: {maxActiveIntents: 2, maxCostMicroUSD: 10000000, plan: {...}, build: {...}}
      # requireRepositoryImage: true
```

The Project reports `Ready` once:

- it lists one repository, or `--intent-multi-repo` is on (`UnsupportedRepositories` otherwise);
- each of its repositories resolves to exactly one Forge (`ForgeUnresolved` otherwise), whose credential Secret
  intent-controller may read (`ForgeSecretUnreadable` otherwise); the message names the repository;
- the App is installed on the intent repository and every app repository, with the permissions intents use: issues
  write on the intent repository; contents and pull requests write and issues read (a reviewer's permission and the
  rate budget are read with it) on each app repository; and, when `spec.checks.fix` names a check, checks, statuses
  and actions read on each app repository too (`AppNotInstalled` otherwise, naming the repository and the permission);
- no other Project shares its intent repository and trigger label (`AmbiguousIntentRepository`).

It creates the trigger and approve labels when they are missing. An issue whose Intent name is held by another
repository's issue is reported as `IntentNameConflict`, never skipped silently.

Use exactly one Project trigger label per issue. If two are present before discovery, neither Project starts an Intent;
each reports the issue in `IntentNameConflict` until one label is removed. If a second Project labels an issue that
already has an Intent, patchy removes the second trigger and reports the conflict. A shared approval label or
`/patchy approve` is accepted only when the issue has one Intent; an anomalous issue with two Intents refuses both
approvals without removing the shared label. Resolve the conflict, then remove and reapply the label or post a new
command.

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
closes the intent before it plans. A command comment gets a 👀 reaction, and every answered action gets exactly one
reply, never a second, even if patchy's reply is deleted.

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
  changes. Two failures are compared without the log's times, durations, commit hashes, long ids (runner, job and
  process numbers), addresses, and the line numbers after a file name; every other number counts, so a failure whose
  values moved (coverage from 71.3% to 76.1%, a test from `got 3` to `got 4`) is progress, and gets another round.

Each round posts one comment on the pull request saying what kind of round it was ("Revision round", or "CI-fix round
for `test`") and what it pushed, and when it pushed, asks the approvers to review again. The summary patchy posts when
the intent ends counts revisions and CI-fix rounds apart.

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
  nothing back).
- **The pull requests open one per pass**, so an intent can end while they are being opened: a `/patchy cancel` or the
  issue closed, or an approved repository removed from the Project (which fails it), perhaps while a block on a later
  one holds it. The ones already opened are left open, and patchy comments on each, once, that the intent ended, that
  the pull request is no longer tracked and not part of a completed change, and which repositories never got theirs;
  the intent's `UntrackedPullRequests` condition records it. patchy closes none of them. Reviving a failed intent starts
  its pull requests afresh.
- **Repository keys name the runs.** Changing a key while an intent builds is safe; giving one repository's key to
  another (a swap) can make a build's name another repository's run, and then the intent is held `Blocked` with
  `UnsupportedRepositories` (`RepositoryKeyChanged`), naming both, until the Project changes again.
- **Rounds run one at a time per intent**, each on one pull request's repository: a review or `/patchy revise` on a pull
  request revises its own repository, a failed check fixes its own. Pull requests with feedback waiting take turns, and
  feedback that arrives while another pull request's round runs is read by its own next round. The revision and CI-fix
  limits are per intent.
- **Endings.** The intent is `Merged` once every pull request has merged. If one is closed without merging, the intent
  stays in review while any other is open, then ends `Closed`: patchy posts a notice of what merged (already on its
  default branch; patchy reverts nothing) and what did not, and closes the issue as not planned. patchy never closes a
  pull request because another closed.

Off, which is the default, a Project listing several repositories is not Ready (`UnsupportedRepositories`), and every
intent of one is held `Blocked` with `UnsupportedRepositories` wherever it stands: no run is launched and nothing is
pushed. A Job already running when the flag goes off finishes, and spends, all the same, and its push waits: turning the
flag on again resumes each intent where it was held, its held push made (an approve label applied meanwhile is then
honoured). A wait longer than the Job TTL (`agent.jobTTL`, `--job-ttl`) loses the finished Job, and with it the
unpushed work, which is then run again (the attempt does not count). One-repository Projects behave the same either way.

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
- **GitHub tokens:** each GitHub operation mints its own token, scoped to one repository and one permission. The
  unscoped installation client is never used.
- **Writes:** it writes only to the repositories a Project lists, plus issues on the intent repository. Branches are
  only ever `patchy-intent/…`, and the default branch is never touched.
- **RBAC:**
  - in the release namespace, projects (read, status), intents and intent runs (their lifecycle, status and finalizers),
    Repositories (create, read, delete), Forges (read), ConfigMaps (create, read, update), leases and events;
  - in the agents namespace, its own copy of the agent-jobs Role;
  - no ClusterRole.
- **Network:** egress to DNS, the Kubernetes API server and GitHub on 443. It never dials the artifact server or an
  agent pod.
- **GitHub App:** intents need no event subscription. They use issues, contents and pull requests (write) and metadata
  (read), issues read on the app repository too; a Project with `spec.checks.fix` also needs checks, statuses and
  actions (read) on its app repository, for the check-fix rounds. See
  [Create the GitHub App](../getting-started/github-app.md#intents).

It writes no Finding spec, so it is not exempt from the finding admission policy.

## Polling cost

Each active intent reads about three GitHub resources per interval (its issue, its events, and its comments since the
newest one it has already read). An intent in review also reads its pull request. A conditional listing that has not
changed returns 304, which costs nothing against the installation's rate limit. The rate-limit floor pauses all intent
polling, pull requests included, while the installation's remaining core budget is below it, so intents can never starve
the security flow of the requests it shares with them. Each poll checks the installation of the repository it reads: the
intent repository and an app repository may be covered by two installations, and the app repository's is the one the
Finding flow shares, so its pull request and a blocked build's default branch wait on its floor, the issue on the intent
repository's. The headers GitHub reports are not consistent from one response to the next, so the floor is a coarse
guard, not an exact budget.
