# agent-runner

The in-pod coding-agent runtime: one stage per Job — `investigate` or `remediate` — via the harness CLI its runner image
bundles (`claude -p` in the claude-agent-runner image, `codex exec` in the codex-agent-runner image, `copilot -p` in the
copilot-agent-runner image). It never talks to GitHub or the Kubernetes API, and it has no flags — configuration is
exclusively `PATCHY_*` environment variables, injected into the Job pod by the job controllers. A claude pod holds **no
credential of any kind**: its model traffic goes through the [egress broker](egress-broker.md), authenticated by a
projected ServiceAccount token; a codex or copilot pod holds the one model key of its harness. Results leave the pod as
a `PATCHY-EVENT:` JSONL stream on stdout (which is why all patchy logging goes to stderr).

You normally never configure the agent-runner directly: the
[investigation-controller](investigation-controller.md#stage-flags) and
[remediation-controller](remediation-controller.md#stage-flags) stage flags become this environment. The contract below
matters when debugging a Job spec or running the runtime standalone.

## Identity and phase

| Env                | Default          | Purpose                                                                                           |
| ------------------ | ---------------- | ------------------------------------------------------------------------------------------------- |
| `PATCHY_REPO`      | — (**required**) | `owner/name` of the repository under analysis                                                     |
| `PATCHY_FINDING`   | — (**required**) | Name of the owning Finding resource — echoed in every event, and the branch is `patchy/<finding>` |
| `PATCHY_BASE_SHA`  | —                | The remote commit the workspace tree corresponds to (the changeset's push base)                   |
| `PATCHY_PHASE`     | `investigate`    | `investigate` or `remediate`; `plan` or `build` for an intent run (below)                         |
| `PATCHY_WORKSPACE` | `/workspace`     | Pod workspace root (`repo/`, `input/`, `reports/`)                                                |

### Intent phases

An intent run (the intent-driven development work kind) uses two more phases, over the same Job seams: `PATCHY_FINDING`
carries the IntentRun's name, `input/issue.md` the intent snapshot for a plan, and `input/investigation.md` the approved
plan for a build. A build's `input/issue.md` must be empty, and a build handed a request is refused with a fatal event
before any agent runs: the approved plan, which the approver read verbatim, is the build's whole contract, while the
request was seen only as GitHub rendered it.

- **`plan`** reads the request and the tree read-only and writes `reports/plan.md`, emitted as a `plan` event. It runs
  on the investigate stage's configuration — `PATCHY_INVESTIGATE_HARNESS`/`_MODEL`/`_TIMEOUT`, and
  `PATCHY_INVESTIGATE_MAX_TURNS`/`_TOKEN_BUDGET` as its ceiling, which a per-Job grant may lower but never raise. Its
  sandbox is the investigation's [read-only one](../deployment/isolation.md#agent-tool-postures): no shell, the file
  tools alone, and writes scoped to `reports/`, so the planner can fix its report in place and is refused a write
  anywhere else in the workspace.
- **`build`** builds the approved plan with the workspace writable — the first build and every revise round — writes
  `reports/build.md` and `commit.sh`, and emits a `remediation` event with the changeset. It runs on the remediate
  stage's configuration, with `PATCHY_REMEDIATE_MANUAL_MAX_TURNS`/`_TOKEN_BUDGET` as its ceiling, which a per-Job grant
  may lower but never raise. Unlike a remediation's, a build's grant has no floor: one below
  `PATCHY_REMEDIATE_AUTO_MAX_TURNS`/`_TOKEN_BUDGET` is honoured, and those apply only to a build Job with no grant.

No per-Job timeout reaches the pod, so a stage's wall clock is `PATCHY_INVESTIGATE_TIMEOUT` (plan) or
`PATCHY_REMEDIATE_TIMEOUT` (build, and every revise round), and its idle limit `PATCHY_INVESTIGATE_IDLE_TIMEOUT` or
`PATCHY_REMEDIATE_IDLE_TIMEOUT`: the intent controller launches each stage with its own limits there. Each intent prompt
states the run's own limits up front, exactly as the run is held to them: its turns and output tokens after the grant
(clamped to the stage's ceiling, or the stage's default with no grant) and the stage's wall clock. The plan prompt also
tells the planner the most a build can be granted, read from the plan Job's
`PATCHY_REMEDIATE_MANUAL_MAX_TURNS`/`_TOKEN_BUDGET` (the build stage's ceiling), which the intent controller sets to the
grant the Project's build will receive. Since the planner cannot run anything, the plan prompt points it at the
repository's own guidance (CLAUDE.md, AGENTS.md, CONTRIBUTING.md, the README and the CI workflows) for which tests the
build should run, and states every bound the plan report's frontmatter is held to.

Both run on **brokered claude only**: any other harness, or claude without `PATCHY_BROKER_TOKEN_FILE`, is refused with a
fatal event before a model is called, because codex and copilot do not honour the sandbox postures. No configuration key
exists for the intent phases alone.

The plan and build reports, and the build's `input/investigation.md` with any revise round after the plan, must be
visible text throughout: a report is `report_invalid`, and a build input a fatal event, if it holds invalid UTF-8, a
control character other than tab, line feed or a CRLF's carriage return, U+2028/U+2029, or a character that renders
invisibly or reorders text (a format character such as a zero-width space or a bidi control, a tag character, a
variation selector, or another default-ignorable code point). The detail names the first one's code point, line and
column. A human approves the plan by reading every byte of it, so nothing in it may be hidden.

The plan is also held to a layout rule, since it is read in a code block that does not wrap: no gap of more than 16
columns of blank characters before more text on a line (a tab counts as 8, and any blank character but a space as 2;
blank characters are tabs, space separators, U+2800 BRAILLE PATTERN BLANK, U+1D159 MUSICAL SYMBOL NULL NOTEHEAD and the
private-use characters), no indentation past 64 columns, and no more than 4 combining marks in a row; the detail names
where the run starts. A report is at most 56 KiB, its frontmatter is plain YAML (one document, no explicit tag), and a
plan holds no run of more than 16 backticks and no new dependency over 200 bytes, so that every plan accepted here fits
in the GitHub comment that shows it for approval. The build report and the build input are held to the visible-text rule
only: the build report is recorded on its run, patchy renders the pull request's description from the approved plan, and
the tool output a build quotes routinely aligns its columns past the plan's bounds.

## Report repair

A stage whose report is missing, or refused by its parser (any rule above, and a plan naming a repository outside its
manifest), is not given up at once on claude or the fake harness. agent-runner asks the agent that wrote the report to
repair it in the same session. It resumes the session the run left under `HOME` (`claude -p --resume <id>`, from the
same working directory, with every flag the first run had, so a plan or an investigation stays read-only, with no shell,
and writes only under `reports/`) with one message: patchy's fixed text and the refusal reason, quoted as data in a
fence. The message asks for the report alone, and asks for an untrue claim (tests that failed, a step not built) to be
corrected rather than hidden.

- At most **2 rounds**, each of at most **6 turns** and **10 minutes**, and never more than the stage has left of its
  turns, output tokens (its token budget or grant) and wall clock (its `_TIMEOUT`). No round starts with less than **2
  minutes** of the wall clock left, with no turns or output tokens left, or after the stage is cancelled. These are
  constants: no configuration key exists for them.
- The output-token kill switch and the idle watchdog apply to a repair run as they do to the first. The broker token is
  read afresh for it and registered with the transcript scrubber.
- All the runs of a stage write one transcript: a repair's turns follow the first run's, after a notice saying why it
  was asked for.
- A repair's spend is added to the stage's. Its tokens, turns and time are summed, since a resumed claude run reports
  its own. Its `total_cost_usd` is not, since claude reports that cumulatively over the session: the repair's tokens are
  priced at the model's rates and added to the cost the first run reported.
- On `remediate` and `build`, which write the working tree, a repair may change only the report and `commit.sh`. The
  clone is fingerprinted around each round (`HEAD`, what is staged, and every working file that is not ignored), and a
  repair that changed any of it is refused whole: the stage ends with its original outcome (`report_missing` or
  `report_invalid`), with the original reason followed by
  `(repair refused in round N: it changed the working tree: <paths>, …)`.
- A report still refused after the rounds ends the stage as before, `report_missing` or `report_invalid`, with the last
  reason followed by how the repair went: `(not repaired in 2 rounds)`, how the last repair run ended when it did not
  end `ok`, or why no round ran, such as `(not repaired: less than 2m of the stage's 1m wall clock left)`.

codex and copilot cannot resume a session, so a stage on them ends on the first refusal, exactly as before.

## Stage configuration

Mirrors of the controllers' stage flags: `PATCHY_INVESTIGATE_TIMEOUT` (`15m`), `PATCHY_INVESTIGATE_IDLE_TIMEOUT`
(`20m`), `PATCHY_INVESTIGATE_MAX_TURNS` (`25`), `PATCHY_INVESTIGATE_TOKEN_BUDGET` (`150000`), `PATCHY_REMEDIATE_TIMEOUT`
(`45m`), `PATCHY_REMEDIATE_IDLE_TIMEOUT` (`20m`), `PATCHY_REMEDIATE_AUTO_MAX_TURNS` (`80`),
`PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET` (`400000`), `PATCHY_REMEDIATE_MANUAL_MAX_TURNS` (`240`),
`PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET` (`1200000`), and `PATCHY_MODEL_ALLOWLIST` (canonical model ids, rendered into the
analysis prompt). The **per-Job** `PATCHY_<STAGE>_HARNESS` and `PATCHY_<STAGE>_MODEL` (a canonical, provider-qualified
id) are set by the controller from the harness and model it resolved for this Job — so the pod runs the harness its
runner image was built for on the model the controller chose, and translates that canonical id to the CLI's own model
id. The investigate limits are absolute. The remediate values are a floor and a backstop, not a clamp: every remediation
runs on at least the ceiling whatever the investigation estimated, the per-Job `PATCHY_GRANTED_MAX_TURNS` /
`PATCHY_GRANTED_TOKEN_BUDGET` raise that when a human approved a larger estimate, and the `_HARD` values bound the
result. A hard cap below its ceiling is a configuration error and the runner refuses to start.

### The idle watchdog

`PATCHY_<STAGE>_IDLE_TIMEOUT` ends a run that makes no progress for that long: no model turn and no tool result, read
off the stream the same way the transcript is (a CLI's housekeeping lines, such as a background task update, do not
count). It kills the CLI's process group, as the wall clock does, and the stage ends `timeout` with a detail naming what
it waited on, for example
`no progress for 20m while running Bash (20m without returning): npm run test:ci 2>&1 | tail -60`. The same detail is
the transcript's last turn and reaches the retry's prompt, and the run counts as an attempt, like any timeout. Its usage
is recorded as for any other outcome. `0s` disables it; a negative value is a configuration error.

The `20m` default never ends a working run. Claude Code's Bash tool returns a foreground command within its own timeout
(2 minutes by default, 10 at most), and each model call's content streams as it completes, so a healthy run is never
silent for more than about 10 minutes; 20 is twice that, and a long test suite fits. What it catches is a run waiting on
something that will not return: overdub-10's build sat 50 minutes on `npm run test:ci` until its one-hour wall clock. It
is longer than the investigate stage's `15m` wall clock and equal to the plan stage's `20m`, so it only ever ends a
remediation, build or revise run early.

`PATCHY_CALIBRATION` is a JSON summary of how earlier estimates in this repository compared to reality, rendered into
the analysis prompt so the next estimate can correct for the observed skew. It is advisory — absent on a cold start, and
the prompt then omits the section entirely.

`PATCHY_PREVIOUS_ATTEMPT` is another per-Job variable: on a retry, a JSON copy of the failed attempt's
`spec.previousAttempt` (`attempt`, `outcome`, `detail`), rendered into that stage's prompt as a "previous attempt"
section so the agent does not repeat the failure. The outcome is one the controller recognizes as a stage failure, or
`unknown` — the pod reports its own outcome, and the prompt states it as fact. The detail is untrusted — it can quote
the repository or its image, such as the `git status` behind a `commit_failed` — so the prompt caps it (4 KiB), drops
control characters, and quotes it in a fence no line of it can close, stated to be data, not instructions. Absent on a
first attempt, and the prompt then omits the section.

`PATCHY_OPEN_PULL_REQUESTS` is the last per-Job variable, set only on an intent's plan Job, and only when other intents
of its Project have pull requests open in the Project's repositories: a JSON list of at most 5 of them, each with its
intent, repository, number, URL and title, the first 50 files it changes and how many it changes in all. The plan prompt
lists them in an "Other open pull requests" section, which asks the plan to avoid the files they change wherever the
request allows, and to name each overlap it cannot avoid among its questions. Everything in it is untrusted, written by
whoever can edit or push to those pull requests, so agent-runner bounds it again as it reads it: each text field on one
line, free of control and format characters and cut to its bound (a title to 256 bytes); a path over 256 bytes, or
holding a control or format character or any white space but a plain space, left out and counted with the files not
listed rather than shown altered; and at most 16 KiB of paths across the list. The prompt quotes the list in a fence no
line of it can close, stated to be data, not instructions. Absent otherwise, and the prompt then omits the section.

Brokered (claude) Jobs add two more:

| Env                        | Purpose                                                                                                                                                                                     |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PATCHY_BROKER_TOKEN_FILE` | Path of the projected ServiceAccount token (`/var/run/patchy/broker/token`); read fresh each stage — the kubelet rotates it — and sent to the broker as the `X-Patchy-Broker-Token` header  |
| `PATCHY_MODEL_MAP`         | Comma-joined `canonical=provider-id` pairs; consulted before the registry when translating the stage model to the CLI's `--model` id (Bedrock inference profiles, Foundry deployment names) |

The controllers also set the claude CLI's gateway environment on brokered Jobs — `ANTHROPIC_BASE_URL` or the
`CLAUDE_CODE_USE_*` / `CLAUDE_CODE_SKIP_*_AUTH` / `ANTHROPIC_*_BASE_URL` switches, plus
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` — pointing every model request at the broker's provider route. All of these
names are reserved in `internal/jobs`, so controller-global configuration can never shadow them.

Two knobs exist only here:

| Env                                 | Default            | Purpose                                                             |
| ----------------------------------- | ------------------ | ------------------------------------------------------------------- |
| `PATCHY_CHANGESET_MAX_BYTES`        | `5242880` (5 MiB)  | Size cap on the changeset's file contents carried out of the pod    |
| `PATCHY_TRANSCRIPT_MAX_TURN_BYTES`  | `2048`             | Per-turn text cap in the captured conversation                      |
| `PATCHY_TRANSCRIPT_MAX_TURNS`       | `500`              | Turn cap for one run's conversation                                 |
| `PATCHY_TRANSCRIPT_MAX_TOTAL_BYTES` | `524288` (512 KiB) | Total cap on one run's conversation, before compression             |
| `PATCHY_FAKE_FIXTURE`               | —                  | Stream-JSON fixture the `fake` harness replays (tests, dev overlay) |

Malformed values fail fast with an error naming the exact `PATCHY_<KEY>`.

## The workspace, and how it got there

The pod's **init container** — not the runtime — fetches the repository: `PATCHY_ARTIFACT_URL` points at
source-controller's in-cluster artifact server (an unguessable URL), `PATCHY_ARTIFACT_DIGEST` pins the sha256, and the
init script verifies the digest before extracting to `/workspace/repo` and synthesizing a local git base commit. No
forge credential is involved at any point — `internal/jobs` even lists `GITHUB_TOKEN` as a reserved name so no
configuration can smuggle one in. The per-Job Secret carries only the handoff markdown (`input/issue.md`, plus
`input/investigation.md` for the remediate phase).

## Credentials in the pod

| Harness | In the pod                                                                                                                                                                          |
| ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| claude  | **None.** The broker caller token (an identity document, not a capability) is the pod's only secret material; the model credential lives with the [egress broker](egress-broker.md) |
| codex   | `OPENAI_API_KEY` via `secretKeyRef` (`--codex-secret`; `CODEX_API_KEY` / `CODEX_ACCESS_TOKEN` when `--codex-secret-env` names one)                                                  |
| copilot | `COPILOT_GITHUB_TOKEN` via `secretKeyRef` (`--copilot-secret`) — a GitHub token, not a model API key; `GH_TOKEN` / `GITHUB_TOKEN` when `--copilot-secret-env` names one             |
| fake    | None — the fixture replay authenticates nothing                                                                                                                                     |

At most **one** credential reaches a given pod: the Job wires the `secretKeyRef` of the harness it runs, so a codex Job
carries only the OpenAI credential. The agent container's environment passes through to the harness CLI child process,
so an injected key — or the brokered gateway environment — is inherited by `claude` (or `codex`, or `copilot`)
automatically. The broker caller token is registered with the transcript scrubber the same way credential values are, so
a tool result that dumps the environment cannot leak it into a persisted transcript.

The copilot rows are the exception to "no forge credential ever reaches the pod": the Copilot CLI authenticates with a
GitHub token, so a copilot Job does carry one. It is a model credential by role, not a forge one — the runner passes
`--disable-builtin-mcps`, so no tool in the session speaks the GitHub API, and the harness's egress policy admits only
`api.github.com` (the token exchange) and the `*.githubcopilot.com` inference endpoints. patchy's own forge traffic is
still controller-side only. Scope the token to Copilot with no repository permissions, and note the copilot runner ships
disabled for exactly this reason.

## The event stream

Progress and results are emitted as one JSON object per line, prefixed `PATCHY-EVENT:`, on stdout; the owning controller
tails the pod log and applies them. The event types are `investigation`, `remediation` (a fix or an intent build),
`plan` (an intent plan, carrying the report byte-exact beside its parsed frontmatter) and `fatal`, all at envelope
version 4. Stage outcomes are `ok`, `runtime_error`, `timeout`, `budget_exceeded`, `report_missing`, `report_invalid`,
`commit_failed`, and `changeset_too_large` — only `ok` carries a trusted report. On claude, `report_missing` and
`report_invalid` come only after the [report repair](#report-repair) failed. A fatal error also exits 2 so the Job is
marked failed for the controller's orphan handling.

## Live command output

While an intent's plan or build stage runs a foreground shell command, agent-runner prints the command's output as it is
produced, as `PATCHY-OUTPUT:` lines beside the transcript's `PATCHY-TURN:` lines, so a viewer of the intent's run panel
can watch a long test suite while it runs rather than only when its tool result arrives. It reads the file the claude
CLI keeps a running command's output in (under `CLAUDE_CODE_TMPDIR`, `/tmp` by default). A backgrounded command is not
followed, and a Finding's investigate and remediate stages print none, since nothing shows it there.

A line is shown as a terminal would show it, the text after its last carriage return, so a progress bar shows its latest
state. Escape sequences are stripped, the pod's credentials and the broker caller token are redacted as in the
transcript, and the line is cut at 1 KiB. A line longer than 8 KiB is never shown, not even in part, since it is cut
before it can be redacted: a fixed placeholder stands in for it under its own line number. Lines go out in chunks about
every half second. Commands are followed one at a time, so two commands' output never interleaves: one started while
another is followed waits its turn and is shown from its first line once that one ends, unless it ends first. The volume
is bounded, since the same pod log must also carry the stage result, which can run to several MiB, and the kubelet
rotates a container's log at about 10 MiB; the bounds are fixed, not configurable:

| Bound                      | Limit                   | Past it                                                                                                                                                         |
| -------------------------- | ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| One command                | 64 KiB at full fidelity | Sampled: its newest lines every 5 seconds, at most 10 lines and 2 KiB each time, under their own line numbers so a gap shows                                    |
| One command's samples      | 64 KiB                  | One chunk marks the command's live output truncated, past its last line read; nothing more is printed for it until its last chunk, at its end                   |
| One agent-runner, in total | 512 KiB                 | One chunk marks the command's live output truncated, past the lines it drops; nothing more is printed for any command but that command's last chunk, at its end |

A sample is not a truncation: the gap in the line numbers shows what it left out, and the command's live output goes on.
Only a limit that stops it for good, the samples' or the process's, marks a chunk truncated.

The output is live only. No controller keeps it: the persisted transcript and the stage result are exactly what they
would be without it. It is also not progress to the [idle watchdog](#the-idle-watchdog) and not spend against the token
budget; both read the CLI's own stream alone.
