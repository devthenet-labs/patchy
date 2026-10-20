# Repository skills and tools

**Status:** Proposed, 2026-10-20; plugins added the same day (repository settings and an operator list, on both
harnesses). Nothing here is built. The claims about the claude and codex CLIs that the design depends on but that patchy
has not yet probed are marked _(probe)_; phase 0 settles each one against the pinned CLIs before any other phase starts.

## Context

A team that uses Claude Code or Codex on its own machines already tells the agent how it wants its code written, and
gives it tools, in files it commits: `CLAUDE.md` and `AGENTS.md`, `.claude/rules/`, skills under `.claude/skills/` or
`.agents/skills/`, and MCP servers in `.mcp.json`. Some of it also lives in plugins installed on the machine.

Patchy's agent runs honour only part of that today:

| In the repository              | claude run                                             | codex run                                           |
| ------------------------------ | ------------------------------------------------------ | --------------------------------------------------- |
| `CLAUDE.md`, `.claude/rules/`  | loaded (`--add-dir` + `CLAUDE_CODE_ADDITIONAL_…`)      | not loaded by the CLI; the plan prompt points at it |
| `AGENTS.md`                    | not loaded by the CLI; the plan prompt points at it    | loaded by codex's own default, never turned off     |
| `.claude/skills/`              | **ignored** (`--setting-sources user`)                 | not a codex location                                |
| `.agents/skills/`              | not a claude location                                  | probably loaded, raw, unaudited _(probe)_           |
| `.mcp.json`                    | **refused** (`--strict-mcp-config`, no `--mcp-config`) | not a codex location                                |
| `.claude/settings*.json`       | ignored (deliberately)                                 | not a codex location                                |
| `.codex/config.toml`           | not a claude location                                  | skipped while the project is untrusted _(probe)_    |
| `enabledPlugins` / `[plugins]` | none installed in any pod                              | `features.plugins=false`                            |

The claude side is the deliberate result of the 2026-10-06 setting-sources review
(`.claude/plans/security-setting-sources.md`): the project settings source let a repository run hooks with no tool call,
set the CLI's environment, pick its permission mode, add allow rules, start `.mcp.json` servers, and grant tools through
a skill's `allowed-tools`. Turning the whole source off closed all of that, and took skills with it as collateral. The
review's open item to audit codex for the same class of problem has not been done: codex skills and `AGENTS.md` load by
codex's defaults, and nothing in `internal/harness/codex.go` decides either.

The result is that a repository cannot shape the agent the way it shapes a person's agent, and is told nothing about
what was dropped. "I added a skill and the agent ignored it" has no answer anywhere in patchy's output.

## Goals

- **What works on a laptop works in patchy, minus what the sandbox forbids.** Patchy reads the files teams already
  commit, in their native formats. There is no patchy dialect for skills or MCP servers and nothing to keep in sync.
- **Every drop is visible.** Each skill and server is reported as loaded or refused, with a reason, where the repository
  owner already looks: the tracking issue, the intent's comments, `patchy describe`, and a local check command.
- **The posture does not weaken.** Read-only stages still execute nothing the repository wrote; hooks, settings, the
  permission mode and the model stay patchy's; the pod still holds no credential; network stays the proxy's to grant.
- **One skill directory serves both harnesses.** A repository can keep its skills in either `.claude/skills/` or
  `.agents/skills/` and both claude and codex runs see them.
- **Plugins work the way teams already share them**, from the repository's own plugin settings and from an operator
  list, on both harnesses, pinned to a commit and taken apart under the same rules as everything else.
- Off by default in the first release; on is one value.

## Non-goals

- **Repository settings files.** `.claude/settings.json`, `.claude/settings.local.json` and `.codex/config.toml` stay
  unread, except the plugin-enablement keys named under [Plugins](#plugins). They configure the CLI rather than describe
  the work (hooks, `env`, `permissions`, helper commands, model routing), and taking any more of them means re-auditing
  every key each CLI release adds.
- **Hooks from any source**: settings, skill frontmatter, plugins.
- **Remote MCP servers** (`http`, `sse`, `ws`, `streamable-http`). The pod has no route to them. A later design can
  serve them through the egress proxy, which would hold their credential the way it holds registry credentials today.
- **Loading a plugin as a unit.** A plugin bundles skills, subagents, commands, hooks, monitors, MCP and LSP servers and
  a `bin/` directory. Patchy takes it apart and loads only the admitted components ([Plugins](#plugins)).
- **Plugins baked into a repository image.** The pod's `HOME` is an emptyDir, so the CLI would not find them, and
  telling patchy where they are would be a patchy dialect. The repository's settings and the operator's list cover the
  same need.
- **Subagents (`.claude/agents/`) and legacy commands (`.claude/commands/`)** in the first release. Subagent definitions
  carry their own model, tools, permission mode and hooks, and need their own normaliser. Open question 2.
- **Hardening codex's read-only stages.** Codex runs every stage with `--sandbox danger-full-access`
  (`internal/harness/codex.go:50-58`), so on codex the plan and investigate stages can already run commands. This design
  does not make that worse, but it does not fix it either; it is tracked separately (Decisions needed, 4).

## Approaches considered

1. **Pass the native files through.** Re-enable the project settings source (or load skills from the `--add-dir`
   directory, which depends on it). Simplest, and identical to a laptop. Rejected: it re-opens every channel the
   setting-sources review closed, including hooks that run before any tool call.
2. **A patchy declaration.** Extend `.patchy/agent.yaml` with `skills:` and `mcp:` blocks that patchy validates. Strict
   and easy to audit. Rejected: teams already declared these things in native files, so they would write them twice, the
   copies would drift, and the patchy copy is the one nobody tests locally.
3. **Read the native files, normalise them, report the result (chosen).** Patchy reads `.claude/skills/`,
   `.agents/skills/` and `.mcp.json` from the pinned tree, rewrites each skill to a frontmatter allowlist for the
   stage's posture, admits each MCP server against a fixed policy, stages the result where the CLI reads it from the
   user source (which patchy already controls), and reports every item it loaded or refused. The cost is a normaliser to
   maintain as the skill format grows; its allowlist shape means a new frontmatter key is dropped until patchy decides
   on it, never honoured by accident.

## Threat model

**Who writes these files.** Anyone who can commit to the branch source-controller pins. That principal already controls
`CLAUDE.md` (which steers the agent as text), the code the writable stages build and test (which runs with the agent's
powers), and, with repository images on, the image those stages run in. Skills and MCP servers come from the same
principal and the same pinned tree (`status.resolvedSHA`), so they add no new principal. What they could add is new
_capability_ in the places the existing posture withholds it.

**What a skill can do beyond text** (Claude Code skill reference, current CLI):

- `hooks:` in frontmatter registers hooks for the rest of the session. Hooks run without a tool call, so they would be
  execution in a read-only stage. **Never honoured.**
- `` !`command` `` lines and ` ```! ` blocks in the body run when the skill renders, before the model sees it. The CLI
  checks each against the permission rules and aborts the skill unless the result is _allow_; the
  `disableSkillShellExecution` setting replaces each with a fixed placeholder instead. **Disabled in read-only stages.**
- `allowed-tools:` grants tools without asking for the turn, and workspace trust does not gate it. Deny rules still win.
  In a read-only stage, `allowed-tools: Write` would lift the report-directory scope on writes (`claudeAllow`), because
  the scope is built from allow rules, not deny rules. **Dropped in read-only stages.**
- `model:` and `effort:` switch the model or effort for the turn. The model is patchy's and the broker's to choose and
  to price. **Never honoured.**
- `context: fork` with `agent:` runs the skill in a subagent. The read-only posture denies `Task`; the writable posture
  allows it with the CLI's own subagents. **Never honoured in the first release** (open question 2).
- Scripts in the skill directory run only when the model calls them through Bash, under the stage's normal rules.

**What an MCP server is.** A stdio server is a process the CLI starts from a command line in the config, with its stdin
and stdout as the protocol. In a writable stage it is the same risk class as the repository's own test suite: code from
the tree or the image, running as the agent, inside the pod's NetworkPolicy, holding no credential. In a read-only stage
it would be the first execution there. Its tool results are untrusted input to the model like any file in the tree.

**What does not change.** The broker, the egress proxy, the pod's credential-free posture, changeset validation, and
human approval. A skill or server can make the agent better or worse at the task; it cannot reach anything the agent
could not already reach.

**Steering.** A skill that tells the investigator a finding is a false positive is the same attack as a `CLAUDE.md` that
says so, and is already in scope today; the routing rules that hold an `ignore` from a repository-image run
(`ranRepositoryImage`) are unchanged.

## Design

### Where the files come from

From the stage's own tree only: `/workspace/repo`, extracted by the prepare init from the digest-verified artifact at
the pinned SHA. The extra read-only trees of a multi-repository plan (`/workspace/repos/<key>`) are reference material,
and their skills are not loaded.

| Source                           | Read as                                                              |
| -------------------------------- | -------------------------------------------------------------------- |
| `.claude/skills/<name>/SKILL.md` | a skill, plus the regular files beside it                            |
| `.agents/skills/<name>/SKILL.md` | a skill, plus the regular files beside it; `agents/openai.yaml` kept |
| `.mcp.json` at the tree root     | MCP server declarations (`mcpServers` map), writable stages only     |

Both skill directories are read and merged by name. A name in both with byte-identical content (the common case of one
directory symlinked to the other, or a copy) is one skill; a name in both with different content is refused in both with
`ConflictingDefinitions`, because which one wins on a laptop depends on the harness.

Only the tree root's directories are read. Nested `.claude/skills/` and per-directory `AGENTS.md` lookups are left to
the CLIs' own behaviour, which for claude means not at all under the user source (as today for nested `CLAUDE.md`).

### Normalising a skill

`internal/repoext` (new, pure: stdlib and the YAML decoder the repo already uses) parses each `SKILL.md` into
frontmatter and body and renders a new file from an allowlist that depends on the stage's posture:

| Frontmatter key                                              | read-only stage | writable stage |
| ------------------------------------------------------------ | --------------- | -------------- |
| `name`, `description`, `when_to_use`                         | kept            | kept           |
| `argument-hint`, `arguments`, `paths`                        | kept            | kept           |
| `disable-model-invocation`, `user-invocable`                 | kept            | kept           |
| `license`, `compatibility`, `metadata`                       | kept            | kept           |
| `allowed-tools`, `disallowed-tools`, `shell`                 | **dropped**     | kept           |
| `hooks`, `model`, `effort`, `context`, `agent`, `background` | **dropped**     | **dropped**    |
| any key not in this table                                    | **dropped**     | **dropped**    |

The body is copied byte for byte. Its `` !`…` `` lines are neutralised by setting, not by rewriting text: in a read-only
stage agent-runner writes `{"disableSkillShellExecution": true}` to the user settings file
(`$HOME/.claude/settings.json`, which patchy already controls and which is empty today), so each one renders as the
CLI's fixed placeholder. If the pinned CLI predates the setting _(probe)_, the deny on Bash still makes the CLI abort
such a skill rather than run it, and the report records the skill as refused with `ShellInjectionUnsupported` instead of
loading a skill that cannot render.

Every dropped key is reported by name, so a repository owner can see that their `model: opus` was ignored and why.

Bounds, each a constant beside its test: at most 64 skills; `SKILL.md` at most 64 KiB; a skill directory at most 1 MiB
and 256 files; all skills together at most 8 MiB; names match `^[a-z0-9][a-z0-9-]{0,63}$`; regular files only, so a
symlink inside a skill directory is not followed (a symlinked skill directory at the top level is followed only when its
target is inside the tree). A skill over a bound is refused whole, with the bound named.

For codex, the same rules apply to `SKILL.md`, and `agents/openai.yaml` keeps `interface` and `policy` and loses
`dependencies` (which names MCP tools, some by URL).

### Staging a skill where the CLI reads it

agent-runner stages the skills before it starts the CLI, in both images (it is the trusted injected binary on a
repository image, and the entrypoint on the default one):

- claude: `$HOME/.claude/skills/<name>/`, the personal skills location, read under `--setting-sources user` _(probe:
  personal skills load under that flag on the pinned CLI)_. The project source stays off, so the tree's own
  `.claude/skills/` is still never read raw.
- codex: `$HOME/.agents/skills/<name>/`, its user location. Codex also scans the repository scope
  (`/workspace/repo/.agents/skills`) on its own, so the harness disables every raw repository skill it found with
  `-c skills.config=[{path=…,enabled=false},…]` _(probe: an inline `skills.config` disables repository-scope skills
  under `codex exec`)_. If it cannot, codex skills fall back to "loaded raw, reported" and Decisions needed 3 applies.

`HOME` is `/workspace`, an emptyDir; nothing unpacked from the tree lands there, so the staged copies are the only
skills a run has besides the CLI's bundled ones. In a read-only claude stage the CLI cannot write there (writes are
scoped to the report directory), so a report repair that resumes the session loads the same skills. In a writable stage
the agent can edit the staged copies mid-run, which gives it nothing it lacked: it can already write and run anything in
the workspace.

### MCP servers (writable stages on an accepted repository image)

An MCP server is admitted only when all of these hold:

- the stage is writable (remediate, build, revise);
- the Job runs an accepted repository image (`RunnerImageRef.Source == repository`): the binaries a server runs then
  come from an image the operator's allowlist and signature check accepted, and the default image ships none of a
  repository's tools anyway;
- `type` is absent or `stdio`;
- `command` resolves, at launch, to an executable on the Job's sanitised `PATH` or is an absolute path inside the image
  or the tree (agent-runner checks with the same lookup the CLI will use);
- `env` values are literal (no `${…}` expansion) and no name is reserved: the Job's reserved names, the image ENV
  prefixes (`PATCHY_`, `ANTHROPIC_`, `CLAUDE_`, `CODEX_`, `OPENAI_`), the proxy variables in any case, `PATH`, `HOME`,
  `LD_*` and `NODE_OPTIONS`;
- at most 8 servers.

Admitted servers are written to `/workspace/.patchy/mcp.json` and rendered per harness:

- claude: `--mcp-config /workspace/.patchy/mcp.json` beside the existing `--strict-mcp-config`, and `mcp__<server>`
  added to `--allowedTools` for each admitted server, because `dontAsk` denies a tool no rule allows _(probe: the pinned
  CLI starts the server and allows its tools; nothing in the tree's `.mcp.json` starts besides them)_.
- codex: `-c mcp_servers.<server>.command=…`, `args`, `env` _(probe: the same under `codex exec` 0.153.4)_.

A server reaches the network only through the Job's network profile, as every other process does. An admitted server
that fails to start is reported by the CLI's own init event (`system/init` lists `mcp_servers` with a status on claude),
which agent-runner copies into its report.

`.mcp.json` is the one declaration format for both harnesses in the first release. Codex teams declare servers in
`.codex/config.toml`, which patchy does not read (Non-goals); open question 1.

### Plugins

Both CLIs package skills, MCP servers and hooks as plugins, and their formats have converged: codex reads a portable
`plugin.json`, its own `.codex-plugin/plugin.json`, and Claude-compatible manifests; it reads a repository's
`.claude-plugin/marketplace.json` beside its own `.agents/plugins/marketplace.json`; and it sets `CLAUDE_PLUGIN_ROOT`
for Claude plugins' hooks. Patchy treats a plugin the same way on both harnesses: **it never loads one as a unit.** It
resolves the plugin to a pinned commit, takes it apart, runs each component through the rule this design already has for
it, and stages a rebuilt plugin that holds only what was admitted.

**Where plugins are enabled.** Two sources, merged into one set of `(plugin, marketplace)` references:

1. **The repository, in the files a team already commits for its laptops.**
   - Claude: `enabledPlugins` and `extraKnownMarketplaces` in `.claude/settings.json`. These are the **only** two keys
     patchy reads from that file; every other key stays unread, as in Non-goals.
   - Codex: `[plugins."<plugin>@<marketplace>"] enabled = true` tables in `.codex/config.toml` (the only keys read from
     it), and entries with `policy.installation: INSTALLED_BY_DEFAULT` in the repository's
     `.agents/plugins/marketplace.json` or `.claude-plugin/marketplace.json`. Whether a project file can register a
     remote marketplace the way Claude's `extraKnownMarketplaces` does is _(probe)_; a marketplace file in the tree
     covers the common case regardless.
   - A plugin enabled in either form is offered to both harnesses, so a team that uses both CLIs declares it once.
2. **The operator, for every repository** (the operator-level guidance of open question 4): a pinned list in the chart.
   An operator plugin and a repository plugin of the same name keep the operator's; the repository's is reported
   `ShadowedByOperator`.

**Resolution and pinning (source-controller).** Beside the image declaration, source-controller reads the plugin
references out of the stored tarball and resolves each one exactly once per `resolvedSHA`, recording the result on the
Repository (`status.plugins`: plugin, marketplace, source, commit, content digest, or a refusal reason):

- **Marketplace sources it fetches:** a GitHub repository (Claude `github`, codex `url`/`git-subdir` on github.com) at a
  commit. A `ref` is resolved to a commit at pin time and recorded, so a moving marketplace branch changes what runs
  only when the repository's own head moves, and the report says which commit ran. Operator plugins name a commit, never
  a ref.
- **In-tree sources:** a marketplace whose entry is a local path (`directory` / `local`) is read from the same tarball.
- **Refused in the first release:** npm sources, arbitrary URL marketplaces and non-GitHub git hosts
  (`UnsupportedSource`).
- **The marketplace allowlist.** A repository plugin is fetched only from a marketplace on the operator's allowlist
  (`MarketplaceNotAllowed` otherwise), matched on `host/owner[/repo]` boundaries, as image registries are. Fetching
  arbitrary repositories into agent runs on a committer's say-so is the supply-chain path this closes.
- **Credentials.** A marketplace on a host a `Forge` covers is fetched with that forge's read token, through the
  existing forge seam. An allowlisted public marketplace on a host no `Forge` covers is fetched anonymously.
- The plugin's directory is stored content-addressed in the artifact store, beside the workspace blobs, and the prepare
  init fetches it by digest, credential-free, as it fetches the tree.

**Decomposition (`internal/repoext`).** The manifest is read in any of the three forms. Component paths must start with
`./` and resolve inside the plugin root (no `..`, no symlink out), as both CLIs require. Then each component goes
through its existing rule, or is refused by name:

| Component                                            | Read-only stage            | Writable stage                                            |
| ---------------------------------------------------- | -------------------------- | --------------------------------------------------------- |
| skills, commands (as skills)                         | normalised, as repo skills | normalised, as repo skills                                |
| stdio MCP servers (`.mcp.json`, `mcp.json`, inline)  | refused                    | admitted by the MCP rules (repository image)              |
| `bin/` executables                                   | refused                    | on the Bash `PATH`                                        |
| subagents (`agents/`)                                | refused (open question 2)  | refused (open question 2)                                 |
| LSP servers                                          | refused (open question 3)  | refused (open question 3)                                 |
| remote MCP servers, codex `apps`, browser extensions | refused                    | refused (open question 5)                                 |
| hooks, monitors, `settings.json`, output styles      | **never**                  | **never**                                                 |
| workflows, themes, channels                          | **never**                  | **never**                                                 |
| anything using `userConfig` or needing auth          | refused                    | refused: `-p` cannot prompt; no credential enters the pod |

A plugin's `settings.json` is never honoured because its `agent` key replaces the main-thread agent, which would change
patchy's prompt and posture; output styles rewrite the system prompt for the same effect.

**Staging a rebuilt plugin.** agent-runner writes a fresh plugin directory holding only the admitted components and a
manifest patchy wrote (name, version, and the component paths it admitted), so namespacing (`<plugin>:<skill>`) keeps
working:

- claude: `/workspace/.patchy/plugins/<plugin>/` passed with `--plugin-dir`, one per plugin, plus allow rules for each
  admitted plugin MCP server's tools _(probe: `--plugin-dir` loads under `--setting-sources user` and `-p`; it loads
  only what the directory holds; `--strict-mcp-config` admits a plugin's servers or patchy lists them in `--mcp-config`;
  the plugin MCP tool names the allow rules must match)_. If `--plugin-dir` does not hold up, the fallback flattens the
  plugin: skills staged as personal skills named `<plugin>-<skill>`, servers merged into the MCP file.
- codex: the rebuilt plugin under `$HOME/.codex/plugins/cache/patchy/<plugin>/local/`, a personal marketplace named
  `patchy` in `$HOME/.agents/plugins/marketplace.json` listing it as a local source,
  `-c plugins."<plugin>@patchy".enabled=true`, and `features.plugins=true` only when at least one plugin was admitted
  _(probe: `codex exec` loads a plugin from a local personal marketplace with nothing fetched; the repository's own
  marketplace files and OpenAI's curated directory install nothing when plugins are on; a plugin's hooks stay
  untrusted)_. With no admitted plugin, today's `features.plugins=false` stays.

**What this adds to the threat model.** A new principal: the authors of an allowlisted marketplace. Their code can run
only in writable stages (`bin/`, stdio MCP servers), the same class as a dependency fetched through the egress proxy,
and only at a commit patchy pinned and reported. Their text reaches every stage as normalised skills, like a
`CLAUDE.md`. Nothing of theirs runs without a tool call, because hooks and monitors are never loaded.

### The report

The report is what makes this intuitive. It has three layers, each reusing a surface that exists.

1. **The run (authoritative).** agent-runner emits one `PATCHY-EVENT:` event, `extensions`, before the CLI starts: per
   skill its name, source directory, `loaded` or `refused`, the dropped keys and a fixed reason; per server its name,
   `admitted` or `refused` and a reason; after the CLI's init event, each admitted server's start status. It is bounded
   (names and reasons only, a fixed cap on entries, the rest counted). `agentresult` records it on the run's status
   (`status.extensions`, a bounded summary), the way it records the runner image a Job ran. The envelope gains the event
   at its next version.
2. **The repository (before any run).** source-controller already opens the stored tarball to read the image
   declaration; it inventories skills and `.mcp.json` from the same stream with the same `internal/repoext` functions
   and records `status.extensions` on the Repository, pin-once beside `resolvedSHA`, as `status.runnerImage` is. This
   tells the owner what a writable and a read-only stage _would_ load before one runs. Server `command` resolution needs
   the image's filesystem and is left to the run.
3. **Where people look.**
   - The tracking issue's runner-image sticky comment (`internal/templates/runner_image_comment.md.tmpl`) gains an
     "Agent skills and tools" section, from the Repository's inventory and each run's record, with the same
     `code`/`fence` escaping it uses for image text today.
   - An intent's plan comment gains one line under patchy's header: the skills the plan run loaded and what the build
     will be offered, with refusals counted and linked to the run panel.
   - `patchy describe` on a run or a Repository prints the same table.
   - **`patchy check extensions [dir]`** (cluster-free, beside `check image`): runs `internal/repoext` over a local
     checkout and prints each skill, server and plugin component with `PASS`, `WARN` (loaded with keys dropped) or
     `FAIL` (refused) per stage posture through `checkreport`. With `--allow-marketplace` (repeatable) it also fetches
     the enabled plugins at the commits they resolve to, with the caller's own GitHub access. This is the command a team
     runs on their laptop before pushing.

### Operator control

One chart block, off by default:

```yaml
agent:
  repositoryExtensions:
    # off | all. "all" loads normalised repository skills in every stage.
    skills: off
    # off | on. "on" admits stdio MCP servers from .mcp.json in writable stages on an accepted repository image.
    mcp: off
    # off | on. "on" honours the repository's enabled plugins from allowedMarketplaces below.
    plugins: off
  plugins:
    # host/owner[/repo] prefixes repository plugins may come from; empty admits none.
    allowedMarketplaces: []
    # Operator plugins, offered to every repository; a commit, never a ref (renovate can track it).
    operator: []
    #   - plugin: house-style
    #     marketplace: github.com/acme/agent-plugins
    #     commit: 4f1c…
```

Operator plugins are on whenever listed; `repositoryExtensions.plugins` governs only the repository's own. The `mcp`
switch still governs every MCP server, a plugin's included.

It renders as `PATCHY_REPOSITORY_SKILLS` and `PATCHY_REPOSITORY_MCP` for the four job controllers (`runnercfg`), which
pass them to agent-runner as Job environment (reserved names, so neither an image nor `Config.Env` can set them), and to
source-controller and integration-controller for the inventory and the comment. With both off, the Job, the argv and
every golden are byte-identical to today's. `agent.paused` and the repository-image kill switch keep their meaning; with
repository images off, `mcp: on` admits nothing and says so in the report.

## Changes by package

- `internal/repoext` (new, pure): skill and `.mcp.json` parsing, the per-posture normaliser, the bounds, the server
  admission rules that need no filesystem, the report types. Seeded properties: normalising is idempotent; a normalised
  skill's frontmatter keys are a subset of its posture's allowlist for any input; `hooks`, `model` and `effort` never
  survive; the body is byte-identical; parse then render then parse yields the same document; no input over a bound is
  loaded. A test pins its imports.
- `internal/agentrun`: stage skills and the MCP file before the CLI starts, write the read-only settings file, emit the
  `extensions` event, read MCP start status from the init event.
- `internal/harness`: `PromptRequest` gains the MCP config path and admitted server names, and the codex skill disables;
  claude renders `--mcp-config` and the `mcp__` allow rules, codex its `-c` overrides. With neither set, argv is
  unchanged (the invocation goldens prove it).
- `internal/envelope`, `internal/agentresult`, `api/v1alpha1`: the event, the run status field, the Repository status
  field; codegen.
- `internal/controller/source`: the inventory beside `pinRunnerImage`; plugin resolution and fetching (phase 4).
- `internal/artifact`: plugin directories as content-addressed blobs; `internal/jobs`: the prepare init fetches them.
- `internal/templates`, `internal/controller/integration`, `internal/controller/intent`: the comment section and the
  plan comment line, with goldens.
- `internal/runnercfg`, `internal/jobs`, `charts/patchy`: flags, reserved env names, values and schema.
- `cmd/patchy`: `check extensions`, and `describe`.
- `internal/harness/claude_probe_test.go`: new cases (phase 0). `assertPinned` currently fails on any MCP server and any
  marker, so the new cases get their own assertions.

## Implementation phases

Branch per phase, one draft PR each, `make pr` green at each.

0. **Probes.** No product change. Extend `TestClaudePostureProbe` (`mise run claude-posture-probe`) on the pinned CLI
   and record one codex run against the fake Responses server:
   1. a personal skill under `$HOME/.claude/skills` loads under `--setting-sources user`; the tree's `.claude/skills`
      does not;
   2. `disableSkillShellExecution` from the user source replaces `` !`…` `` with the placeholder on the pinned CLI, and
      without it a read-only run aborts such a skill and runs nothing (marker file absent either way);
   3. a skill's `hooks` and `allowed-tools: Write` take effect when present raw (the control), proving the normaliser is
      what removes them;
   4. `--mcp-config` beside `--strict-mcp-config` starts only the named server and `mcp__<server>` tools are allowed
      under `dontAsk`; a tree `.mcp.json` still starts nothing;
   5. neither CLI sends a local skill to the API as a `container.skills` body field, which the broker refuses with 403
      (`internal/broker/enforce_test.go`);
   6. codex 0.153.4 under `codex exec`: whether `/workspace/repo/.agents/skills` loads today, whether
      `-c skills.config=[…]` disables it, whether `$HOME/.agents/skills` loads, whether `.codex/config.toml` and its
      hooks are skipped for the untrusted tree, and whether `-c mcp_servers.…` starts a stdio server;
   7. claude plugins: `--plugin-dir` under `--setting-sources user` and `-p` loads a rebuilt plugin's skills
      (namespaced) and nothing it does not hold; how `--strict-mcp-config` treats a plugin's MCP servers and what their
      tool names are; that a plugin with hooks removed runs no hook;
   8. codex plugins: a plugin from a local personal marketplace loads under `codex exec` with `features.plugins=true`
      and `-c plugins."<p>@patchy".enabled=true`; nothing else installs (repository marketplace files, the curated
      directory, `INSTALLED_BY_DEFAULT` entries); whether `.codex/config.toml` can register a remote marketplace.

   The probe results are recorded in this document before phase 1 starts, and any _(probe)_ claim that fails changes the
   design first.

1. **Skills.** `internal/repoext` (skills), agent-runner staging and the settings file, the codex disables, the
   `extensions` event and run status, the chart's `skills` value, `patchy check extensions`, the owner guide. Tests: the
   properties above; table tests per frontmatter key and posture; invocation goldens unchanged with the value off; e2e
   with the fake harness asserting the staged files and the run status.
2. **MCP servers.** Admission, the MCP file, both harnesses' rendering, the chart's `mcp` value. Tests: admission
   tables; invocation goldens; an e2e build on the real agent-runner driving `hack/fake-agent/claude` whose scripted
   init event lists the server.
3. **Pre-run inventory and comments.** source-controller's inventory, the Repository status field, the sticky comment
   section, the plan comment line, `describe`. Tests: reconciler tests over fixture tarballs; template goldens.
4. **Plugins.** Reference extraction from both settings forms and the in-tree marketplace files; source-controller
   resolution to commits, the marketplace allowlist, fetch through the forge seam or anonymously, content-addressed
   storage, `status.plugins`; decomposition with the manifest forms and path containment; agent-runner rebuilding and
   staging per harness; the operator list; the chart's `plugins` value; `check extensions` fetching. Tests: seeded
   properties that a rebuilt plugin holds no component the posture's table refuses, for any input manifest, and that
   every component path stays inside the plugin root; reconciler tests with a fake marketplace host (a ref moved between
   two pins records two commits; a marketplace off the allowlist is never contacted); an e2e intent build on the real
   agent-runner with one operator and one repository plugin, asserting the staged tree and the report.

### Verification and rollout

After each phase: `make e2e`, the real claude CLI through the broker, a release, and on the live cluster a Finding
remediation and an intent build against `devthenet-labs/patchy-target` carrying a skill with every dropped key, a
`` !`…` `` line, and (phase 2) an MCP server baked into its agent image, checking the report on the tracking issue and
the intent against what the run did. Rollback is `skills: off` / `mcp: off`, which restores today's argv exactly.

## Decisions needed

1. **Default.** Ship off and turn on per cluster after verification (proposed), or default `skills: all` from the first
   release since skills add no execution to read-only stages once normalised.
2. **Skills in read-only stages.** Proposed yes: a team's coding standards matter most to the planner, and with the
   normaliser and the shell setting a skill there is text, like `CLAUDE.md`. The alternative is writable stages only,
   which is simpler to argue about but leaves the planner without the guidance the build follows.
3. **Codex fallback.** If phase 0 shows codex's repository-scope skills cannot be disabled from the command line, choose
   between loading them raw (reported, not normalised; codex skills carry fewer powerful keys than claude's) and not
   offering skills on codex until it can.
4. **Codex read-only enforcement.** Separate from this design and arguably more urgent: codex plan and investigate
   stages run with a full shell today. Options are codex's own `--sandbox read-only` (it has one; patchy never renders
   it) or keeping codex out of read-only stages. This design's claim that read-only stages execute nothing holds for
   claude only until that is done.
5. **Plugin sources.** Decided 2026-10-20: the repository's own plugin settings and an operator list (both), not plugins
   baked into an image. Still open: whether `allowedMarketplaces` should ship with an entry for the CLIs' official
   marketplaces, or empty (proposed: empty, so every marketplace is the operator's explicit choice).

## Open questions

1. **Codex-native MCP declarations.** Read `[mcp_servers]` from `.codex/config.toml` as a second source, ignoring every
   other key in that file, so codex teams declare servers once.
2. **Subagents and commands.** `.claude/agents/` with a normaliser of their own (model, tools, permission mode and hooks
   dropped; the subagent inherits the stage's tool posture), and `.claude/commands/` as skills.
3. **LSP servers.** Code intelligence (go-to-definition, references) is read-only in spirit and would help the planner
   most, but a language server runs the toolchain (`gopls` runs `go list`), so it is execution. A per-language allowlist
   of known servers in writable stages first, read-only stages only after their own review.
4. **Operator-level guidance.** Mostly answered by operator plugins: an organisation's standards as skills in a plugin,
   offered to every repository. Still open: whether a plain prompt field (a chart or Project value rendered through
   `PromptRequest.SystemPromptAppend`, which no production caller sets today) is worth having beside them.
5. **Remote MCP servers** through the egress proxy, which would hold their credential and inject it only toward the
   server's origin, as it does for registries.
