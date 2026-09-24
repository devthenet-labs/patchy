# intent-controller

Intent-driven development, and the second **optional** controller: deployments without Projects do not run it. A human
opens an issue in an intent repository and labels it for a project. intent-controller plans the work in a read-only
agent Job and posts the plan to the issue. An approver approves it by label or command, and it builds exactly that plan
in the application repository's own image. It pushes the result to a branch it creates once, opens the pull request, and
closes the issue when the pull request merges. The design, its security posture and the roadmap beyond this first slice
are in [Intent-driven development](../design/intent-driven-development.md).

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
| `Project`   | Operator configuration: the intent repository, labels, approvers, app repository and limits        |
| `Intent`    | One per intent issue, named `<project>-<issue>`, carrying the phase and the approved plan's digest |
| `IntentRun` | One immutable attempt of one stage (`plan` or `build`); it owns its Repository and agent Job       |

`patchy get intents`, `patchy get irun` and `patchy get proj` list them (see [the CLI](../cli.md)). Humans write only an
Intent's `spec.suspend`; everything else happens on the issue.

## Flags

The [shared flags](index.md#shared-flags-every-controller), plus the settings below. They carry an `intent-` prefix no
other binary binds, so the shared kustomize ConfigMap cannot set one by accident.

| Flag                              | Env                                    | Default                     | Purpose                                                                                          |
| --------------------------------- | -------------------------------------- | --------------------------- | ------------------------------------------------------------------------------------------------ |
| `--intent-poll-interval`          | `PATCHY_INTENT_POLL_INTERVAL`          | `60s`                       | How often each Project's intent repository and each active intent's issue are polled             |
| `--intent-approval-poll-interval` | `PATCHY_INTENT_APPROVAL_POLL_INTERVAL` | `30s`                       | How often an intent awaiting approval polls its issue's events                                   |
| `--intent-pr-poll-interval`       | `PATCHY_INTENT_PR_POLL_INTERVAL`       | `60s`                       | How often an intent in review polls its pull request                                             |
| `--intent-max-concurrent-runs`    | `PATCHY_INTENT_MAX_CONCURRENT_RUNS`    | `1`                         | Intent agent Jobs running at once: a pool of its own, separate from remediation's                |
| `--intent-rate-limit-floor`       | `PATCHY_INTENT_RATE_LIMIT_FLOOR`       | `1000`                      | Pause intent polling while the installation has fewer core requests left than this; `0` disables |
| `--intent-ttl`                    | `PATCHY_INTENT_TTL`                    | `336h` (14 days)            | How long an ended intent is kept, with everything it owns; `0` keeps it forever                  |
| `--intent-job-deadline`           | `PATCHY_INTENT_JOB_DEADLINE`           | `90m`                       | `activeDeadlineSeconds` on every intent Job; at least both stage timeouts                        |
| `--intent-plan-model`             | `PATCHY_INTENT_PLAN_MODEL`             | `anthropic/claude-sonnet-5` | Canonical model the plan stage runs                                                              |
| `--intent-plan-max-turns`         | `PATCHY_INTENT_PLAN_MAX_TURNS`         | `40`                        | Most agent turns a plan run may take                                                             |
| `--intent-plan-token-budget`      | `PATCHY_INTENT_PLAN_TOKEN_BUDGET`      | `200000`                    | Most output tokens a plan run may spend                                                          |
| `--intent-plan-timeout`           | `PATCHY_INTENT_PLAN_TIMEOUT`           | `20m`                       | Wall-clock limit of a plan run                                                                   |
| `--intent-build-model`            | `PATCHY_INTENT_BUILD_MODEL`            | `anthropic/claude-sonnet-5` | Canonical model the build stage runs                                                             |
| `--intent-build-max-turns`        | `PATCHY_INTENT_BUILD_MAX_TURNS`        | `150`                       | Most agent turns a build run may take                                                            |
| `--intent-build-token-budget`     | `PATCHY_INTENT_BUILD_TOKEN_BUDGET`     | `800000`                    | Most output tokens a build run may spend                                                         |
| `--intent-build-timeout`          | `PATCHY_INTENT_BUILD_TIMEOUT`          | `60m`                       | Wall-clock limit of a build run                                                                  |
| `--agent-namespace`               | `PATCHY_AGENT_NAMESPACE`               | `patchy-agents`             | Namespace the agent Jobs run in                                                                  |
| `--agent-service-account`         | `PATCHY_AGENT_SERVICE_ACCOUNT`         | `patchy-agent`              | Service account the agent Jobs run as                                                            |
| `--job-ttl`                       | `PATCHY_JOB_TTL`                       | `1h`                        | `ttlSecondsAfterFinished` on a finished agent Job                                                |
| `--repository-images`             | `PATCHY_REPOSITORY_IMAGES`             | `false`                     | Run a Repository's pinned repository-declared image; a build requires one (see below)            |
| `--agent-ephemeral-storage`       | `PATCHY_AGENT_EPHEMERAL_STORAGE`       | —                           | Ephemeral-storage request and limit on both agent containers; **required** with the flag above   |
| `--changeset-max-entries`         | `PATCHY_CHANGESET_MAX_ENTRIES`         | `500`                       | Most files a build's changeset may touch; more is rejected before any forge call                 |

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
      repositories: # exactly one in this slice
        - name: target
          url: https://github.com/acme/target
      # labels: {trigger: patchy:target, approve: patchy:approved}  (the defaults)
      # limits: {maxActiveIntents: 2, maxCostMicroUSD: 10000000, plan: {...}, build: {...}}
      # requireRepositoryImage: true
```

The Project reports `Ready` once:

- its repository resolves to exactly one Forge (`ForgeUnresolved` otherwise), whose credential Secret intent-controller
  may read (`ForgeSecretUnreadable` otherwise);
- the App is installed on the intent and app repositories, with the issues, contents and pull-requests permissions
  intents use (`AppNotInstalled` otherwise);
- no other Project shares its intent repository and trigger label (`AmbiguousIntentRepository`).

It creates the trigger and approve labels when they are missing. An issue whose Intent name is held by another
repository's issue is reported as `IntentNameConflict`, never skipped silently.

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

## Permissions

intent-controller is the second code path that writes to a forge (remediation-controller is the first), and its posture
is the tightest of any controller:

- **Secrets:** `secrets get` is restricted by `resourceNames` to the Secrets your Forges reference
  (`intentController.forgeSecrets` in the chart, `rbac.yaml` in the kustomize component). A Forge that references any
  other Secret, or a Secret that does not exist, leaves its Projects not Ready with the reason `ForgeSecretUnreadable`,
  whose message names the Secret.
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
- **GitHub App:** this slice needs no new permission or event subscription. It uses issues, contents and pull requests
  (write) and metadata (read).

It writes no Finding spec, so it is not exempt from the finding admission policy.

## Polling cost

Each active intent reads about three GitHub resources per interval (its issue, its events, and its comments since the
newest one it has already read). An intent in review also reads its pull request. A conditional listing that has not
changed returns 304, which costs nothing against the installation's rate limit. The rate-limit floor pauses all intent
polling, pull requests included, while the installation's remaining core budget is below it, so intents can never starve
the security flow of the requests it shares with them. The headers GitHub reports are not consistent from one response
to the next, so the floor is a coarse guard, not an exact budget.
