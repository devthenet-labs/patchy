# investigation-controller

Two engines in one binary. The **gate** admits findings that are `Enhanced`, have a closed accumulation window, and are
older than `--finding-min-age`; admission materializes the `Repository` (if absent) and one immutable `Investigation`
per attempt — the child create is the lease. The **analysis scheduler** then runs agent Jobs under bounded concurrency
in severity order, parses the `PATCHY-EVENT:` stream, stamps the results, and routes the verdict onto the Finding.

```sh
investigation-controller serve --namespace patchy \
  --claude-agent-image ghcr.io/devthenet-labs/patchy/claude-agent-runner:v0.9.0 # match your installed release
```

## Pipeline flags

The [shared flags](index.md#shared-flags-every-controller), plus:

| Flag                              | Env                                    | Default | Purpose                                             |
| --------------------------------- | -------------------------------------- | ------- | --------------------------------------------------- |
| `--finding-min-age`               | `PATCHY_FINDING_MIN_AGE`               | `1h`    | How old a finding must be before the gate admits it |
| `--gate-concurrency`              | `PATCHY_GATE_CONCURRENCY`              | `4`     | Findings admitted through the gate in parallel      |
| `--max-attempts`                  | `PATCHY_MAX_ATTEMPTS`                  | `2`     | Analysis attempts per finding before it fails       |
| `--max-concurrent-investigations` | `PATCHY_MAX_CONCURRENT_INVESTIGATIONS` | `3`     | Simultaneously running investigation Jobs           |
| `--confidence-threshold`          | `PATCHY_CONFIDENCE_THRESHOLD`          | `0.75`  | Confidence required to queue automated remediation  |
| `--priority-aging-interval`       | `PATCHY_PRIORITY_AGING_INTERVAL`       | `24h`   | Wait per effective-priority point of aging boost    |
| `--priority-aging-cap`            | `PATCHY_PRIORITY_AGING_CAP`            | `25`    | Maximum aging boost                                 |

## Agent Job flags

Shared with the [remediation-controller](remediation-controller.md) — both job controllers build Jobs the same way, from
a per-harness runner fleet. A runner is _configured_ when its image flag is set and _enabled_ when it is also in
`--harnesses` (or, when that is empty, its credential Secret exists). The controller resolves each stage's model to a
harness and runs the matching runner. At startup it validates that every allowlisted model has an enabled harness that
supports it, refusing to start otherwise.

| Flag                              | Env                                    | Default                | Purpose                                                                                                                                                                              |
| --------------------------------- | -------------------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `--claude-agent-image`            | `PATCHY_CLAUDE_AGENT_IMAGE`            | —                      | claude-agent-runner image (bundles the claude CLI); unset disables the claude runner                                                                                                 |
| `--codex-agent-image`             | `PATCHY_CODEX_AGENT_IMAGE`             | —                      | codex-agent-runner image (bundles the codex CLI); unset disables the codex runner                                                                                                    |
| `--copilot-agent-image`           | `PATCHY_COPILOT_AGENT_IMAGE`           | —                      | copilot-agent-runner image (bundles the copilot CLI); unset disables the copilot runner                                                                                              |
| `--fake-agent-image`              | `PATCHY_FAKE_AGENT_IMAGE`              | —                      | fake agent image for dev/e2e (replays fixtures, no credential)                                                                                                                       |
| `--harnesses`                     | `PATCHY_HARNESSES`                     | — (auto)               | Restrict enabled harnesses to this comma list; empty = any harness whose credential exists                                                                                           |
| `--broker-url`                    | `PATCHY_BROKER_URL`                    | —                      | The [egress broker](egress-broker.md)'s base URL — **required** when a claude runner is configured (claude runs proxy-only)                                                          |
| `--broker-token-audience`         | `PATCHY_BROKER_TOKEN_AUDIENCE`         | `patchy-egress-broker` | Audience the agent pods' projected broker caller tokens are bound to (must match the broker's `--token-audience`)                                                                    |
| `--claude-provider`               | `PATCHY_CLAUDE_PROVIDER`               | `anthropic`            | Model provider behind the broker: `anthropic`, `bedrock`, `vertex`, or `foundry`                                                                                                     |
| `--claude-provider-region`        | `PATCHY_CLAUDE_PROVIDER_REGION`        | —                      | Provider region (required for bedrock and vertex)                                                                                                                                    |
| `--claude-provider-region-prefix` | `PATCHY_CLAUDE_PROVIDER_REGION_PREFIX` | —                      | Bedrock inference-profile prefix override when it cannot be derived from the region (`us`/`eu`/`apac`)                                                                               |
| `--claude-provider-project-id`    | `PATCHY_CLAUDE_PROVIDER_PROJECT_ID`    | —                      | GCP project id (required for vertex)                                                                                                                                                 |
| `--claude-model-map`              | `PATCHY_CLAUDE_MODEL_MAP`              | —                      | `canonical=provider-id` overrides, comma separated — **required** for foundry, whose deployment names cannot be derived                                                              |
| `--claude-provider-env`           | `PATCHY_CLAUDE_PROVIDER_ENV`           | —                      | Extra `NAME=value` agent-pod env for the provider, comma separated (credential names rejected)                                                                                       |
| `--claude-secret{,-key,-env}`     | `PATCHY_CLAUDE_SECRET{,_KEY,_ENV}`     | (legacy)               | **Ignored** — claude is brokered and its pods hold no credential; customizing these logs a migration notice                                                                          |
| `--codex-secret`                  | `PATCHY_CODEX_SECRET`                  | `patchy-openai`        | Secret (agent namespace) holding the OpenAI credential                                                                                                                               |
| `--codex-secret-key`              | `PATCHY_CODEX_SECRET_KEY`              | `api-key`              | Key within the OpenAI credential Secret                                                                                                                                              |
| `--codex-secret-env`              | `PATCHY_CODEX_SECRET_ENV`              | `OPENAI_API_KEY`       | Env var it is injected as: `OPENAI_API_KEY`, or `CODEX_API_KEY` / `CODEX_ACCESS_TOKEN` for a ChatGPT-plan workspace token                                                            |
| `--copilot-secret`                | `PATCHY_COPILOT_SECRET`                | `patchy-copilot`       | Secret (agent namespace) holding the GitHub Copilot credential — a GitHub token, not a model API key                                                                                 |
| `--copilot-secret-key`            | `PATCHY_COPILOT_SECRET_KEY`            | `token`                | Key within the GitHub Copilot credential Secret                                                                                                                                      |
| `--copilot-secret-env`            | `PATCHY_COPILOT_SECRET_ENV`            | `COPILOT_GITHUB_TOKEN` | Env var it is injected as: `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN`                                                                                                     |
| `--agent-namespace`               | `PATCHY_AGENT_NAMESPACE`               | `patchy-agents`        | Namespace the agent Jobs run in                                                                                                                                                      |
| `--agent-service-account`         | `PATCHY_AGENT_SERVICE_ACCOUNT`         | `patchy-agent`         | ServiceAccount for the agent pods                                                                                                                                                    |
| `--job-deadline`                  | `PATCHY_JOB_DEADLINE`                  | `1h`                   | `activeDeadlineSeconds` for an agent Job                                                                                                                                             |
| `--job-ttl`                       | `PATCHY_JOB_TTL`                       | `1h`                   | `ttlSecondsAfterFinished` for a finished Job                                                                                                                                         |
| `--model-allowlist`               | `PATCHY_MODEL_ALLOWLIST`               | canonical ids          | Canonical model ids the investigation may request for remediation (comma-separated)                                                                                                  |
| `--repository-images`             | `PATCHY_REPOSITORY_IMAGES`             | `false`                | Run a Repository's pinned [repository-declared image](#repository-declared-runner-images) (brokered claude runner only); off runs every Job on its harness's image — the kill switch |
| `--agent-ephemeral-storage`       | `PATCHY_AGENT_EPHEMERAL_STORAGE`       | —                      | Ephemeral-storage request and limit on both agent containers, a quantity such as `8Gi`; unset leaves Jobs without one; **required** with `--repository-images`                       |

### Repository-declared runner images

With `--repository-images` on, a launch runs the image source-controller pinned on the finding's Repository
(`status.runnerImage.image`, see [source-controller](source-controller.md#repository-runner-images)) instead of the
claude runner image, with patchy's own `agent-runner` and claude CLI copied in from the runner image by the trusted
`prepare` init container. The pin is used only when every one of these holds; otherwise the Job runs the default image,
exactly as with the flag off:

- the runner is the brokered claude runner (codex, copilot and the fake harness always run their own image);
- the finding has not been revived by a human (`approve` on `HandedOff`, `retry` on `Failed` run the default image from
  then on, so whatever sent it to a human is not retried in the same environment);
- the sandbox breaker has not tripped.

What ran is recorded from what the Job client wrote, never from configuration: `status.runnerImage` on the Investigation
(and Remediation), the Job and pod annotations `patchy.bitwisemedia.uk/runner-image` and
`patchy.bitwisemedia.uk/runner-image-source` (`repository` | `default`), and the latter again as a pod label, so
repository-image pods are selectable.

- **Sandbox probe.** Before a repository image runs anything, `prepare` checks from inside the pod that the public
  internet (`1.1.1.1:443`), the Kubernetes API server and the cloud metadata service (`169.254.169.254:80`) are all
  unreachable. It retries for about 20 seconds, because some CNIs (EKS Auto Mode among them) attach a new pod's policy a
  few seconds after it starts, and concludes enforcement only after three consecutive quiet rounds. If egress is still
  open, `prepare` exits 78, the agent never starts, the run fails `SandboxUnenforced` without consuming an attempt, and
  an in-memory breaker refuses repository images for every later launch until the controller restarts (gauge
  `patchy.sandbox.breaker`).
- **Pull failures.** A repository-image Job whose pod cannot pull is looked at again after a two-minute grace;
  `InvalidImageName`, `CreateContainerConfigError`, or a pull error saying the manifest is unknown or not found then
  fails the run (`agent image pull failed: …`) and deletes the Job. Any other pull error waits for `--job-deadline`.
  Default-image Jobs keep the plain Job-watch behaviour.
- **Incompatible images.** `agent-runner` runs a preflight in the image (`claude --version`, `git --version`,
  `bash -c true`); a failure ends the stage `image_incompatible` and retries or fails like any other non-`ok` outcome.

Every outcome is reported to the repository owner on the tracking issue by the
[integration-controller](integration-controller.md#behavior).

## Stage flags

The investigation stage's limits are **absolute** — the stage runs on exactly these. Its model is a canonical,
provider-qualified id (e.g. `anthropic/claude-sonnet-5`); the harness that runs it is derived from the model. The two
`remediate` ceilings exist here because this controller clamps what the investigation report requests before it ever
reaches the queue.

| Flag                         | Env                               | Default                     | Purpose                                             |
| ---------------------------- | --------------------------------- | --------------------------- | --------------------------------------------------- |
| `--investigate-model`        | `PATCHY_INVESTIGATE_MODEL`        | `anthropic/claude-sonnet-5` | Canonical model id the analysis stage runs on       |
| `--investigate-timeout`      | `PATCHY_INVESTIGATE_TIMEOUT`      | `15m`                       | Wall-clock limit for the analysis stage             |
| `--investigate-max-turns`    | `PATCHY_INVESTIGATE_MAX_TURNS`    | `25`                        | Agent turns allowed for the analysis stage          |
| `--investigate-token-budget` | `PATCHY_INVESTIGATE_TOKEN_BUDGET` | `150000`                    | Output-token budget for the analysis stage          |
| `--remediate-max-turns`      | `PATCHY_REMEDIATE_MAX_TURNS`      | `80`                        | Ceiling on the report's suggested remediation turns |
| `--remediate-token-budget`   | `PATCHY_REMEDIATE_TOKEN_BUDGET`   | `400000`                    | Ceiling on the suggested remediation budget         |

The stage flags are re-serialized into `PATCHY_*` environment variables injected into every investigation pod
([agent-runner](agent-runner.md) reads them), so this controller's flags are the single operator-facing configuration
point for the analysis stage.

## Verdict routing

When the analysis Job completes, the controller stamps the summary onto `Finding.status.investigation`, sets the
scheduling priority, and moves the phase:

| Report says                                                 | Finding moves to                                             |
| ----------------------------------------------------------- | ------------------------------------------------------------ |
| `ignore` (false positive)                                   | `Dismissed` — alerts dismissed, issue closed                 |
| `ignore` from a run on a repository-declared image          | `HandedOff` — the alert is **not** dismissed                 |
| `manual`                                                    | `HandedOff` — owners take over; `/patchy approve` can revive |
| `remediate`, confidence < threshold or breaking-change hold | `AwaitingApproval`                                           |
| `remediate`, confidence ≥ threshold                         | `Queued`                                                     |
| Stage outcome not `ok`                                      | `Enhanced` (retry) while attempts remain, then `Failed`      |

A partial report is never trusted: outcomes other than `ok` (`runtime_error`, `timeout`, `budget_exceeded`,
`report_missing`, `report_invalid`, `image_incompatible`) always retry or fail — they never route on whatever
frontmatter survived. A run the sandbox probe refused (`SandboxUnenforced`) goes back to `Enhanced` without counting
against `--max-attempts`. An `ignore` verdict from a run whose own launch stamp
(`Investigation.status.runnerImage.source`) says `repository` is held for a human instead of dismissing the alert: that
image controls the process the verdict came out of, and dismissal writes back to the scanner with no human gate. The
decision reads the stamp, never the controller's current configuration, so flipping `--repository-images` between launch
and collection cannot change it. Nothing is written back to the scanner; the alert stays open there until a human
resolves it.
