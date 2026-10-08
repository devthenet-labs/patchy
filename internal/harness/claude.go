// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Claude drives the `claude` CLI (Claude Code).
type Claude struct {
	base
}

// EnforcesReadOnly reports whether h holds a SandboxReadOnly run to the
// posture itself: no shell, and writes refused outside the request's
// WriteDirs (claudeTools). Only the claude harness does; codex and copilot
// cannot express the posture and leave it to the pod (their PromptSpec
// docs), and the fake harness runs no agent. A prompt states the read-only
// tool surface as fact only when this holds.
func EnforcesReadOnly(h Harness) bool {
	_, ok := h.(*Claude)
	return ok
}

// NewClaude returns the builtin Claude Code harness.
func NewClaude() *Claude {
	return &Claude{base: base{
		id:   "claude",
		name: "Claude Code",
		clis: []string{"claude"},
		// Credentials the claude CLI itself authenticates with. Both an
		// API-key and an OAuth-token form are accepted.
		envKeys: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"},
	}}
}

// claudeTools renders each sandbox posture into Claude Code's tool flags.
// Network tools stay denied in both postures — the pod has no egress and the
// stages never fetch.
//
// The read-only posture has no shell. Its tools list (--tools) is the whole
// built-in tool set the run has: Read, Glob and Grep to find and read files,
// and Write and Edit for the report. Bash is absent from it, and denied by
// name as well, so neither an arbitrary command nor the CLI's built-in set of
// read-only shell commands (which it otherwise runs without asking) is
// available; subagents, notebooks and every other built-in tool are absent
// too. Naming Glob and Grep in --tools is also what gives a Linux run those
// two tools, which the CLI otherwise leaves out in favour of find and grep
// through Bash. Edit is left out of the allow list and Write is allowed only
// as the request's WriteDirs scope it (claudeAllow; in -p mode a tool call
// that is not allowed is refused), so a stage that sets WriteDirs can write
// nowhere but its report directory: not the tree, not its .git directory, and
// not the settings the CLI reads (claudeSettingSources), which a report
// repair's resumed run would load. The CLI's own exceptions for files it
// manages itself (its memory and scratch files) are the one way around the
// scope; a read-only run turns auto memory off (claudeNoAutoMemory), so
// nothing written there is loaded again. A request without WriteDirs keeps
// an unscoped Write; both read-only stages set it.
//
// The workspace-write posture renders no tools list and keeps the CLI's
// built-in set with Bash allowed. SandboxDefault is absent by design: an
// unset posture imposes no grammar and leaves the CLI's defaults, settings
// sources included (claudeSettingSources).
//
// A multi-repository intent's planner reads its other repositories' trees
// with the same read-only tools: they sit under the workspace the stage adds
// as a directory.
var claudeTools = map[Sandbox]struct{ tools, allow, deny []string }{
	SandboxReadOnly: {
		tools: []string{"Read", "Glob", "Grep", "Edit", "Write"},
		allow: []string{"Read", "Glob", "Grep", "Write"},
		deny:  []string{"WebFetch", "WebSearch", "Task", "Bash"},
	},
	SandboxWorkspaceWrite: {
		allow: []string{"Read", "Glob", "Grep", "Edit", "Write", "NotebookEdit", "Bash"},
		deny:  []string{"WebFetch", "WebSearch"},
	},
}

// claudeSettingSources is the one settings source a postured run reads, so
// that the posture is patchy's alone. claude -p never asks whether to trust
// the directory it runs in, and from the working tree's .claude/settings.json,
// .claude/settings.local.json and .mcp.json it would still take hooks, the
// env block and helper commands, a local file's allow rules, a defaultMode
// such as acceptEdits, project skills with their allowed-tools, and MCP
// servers: each runs a command or grants a tool the grammar above withholds.
// The user source is the settings under HOME, which in the agent pod is the
// workspace root, where no tree is unpacked. A read-only stage cannot write
// there, since its writes are scoped to its report directory (claudeTools)
// and the CLI's memory, which it may write regardless, is off for it
// (claudeNoAutoMemory), so a report repair, which resumes the stage's session
// with these same sources, reads only what the image and the prepare step put
// there. The
// settings location is not moved somewhere unwritable instead
// (CLAUDE_CONFIG_DIR): the CLI keeps its sessions in the same directory and
// writes them as the agent's own process, a resume reads them back, and a
// writable stage is meant to keep the location it has. Leaving out the project source
// leaves out .mcp.json too, and --strict-mcp-config, passed beside it,
// refuses every MCP server not named by --mcp-config, which patchy never
// passes.
//
// The project source also carries the tree's CLAUDE.md, which the stages
// want for its build and test guidance. The CLI reads CLAUDE.md,
// .claude/CLAUDE.md and .claude/rules from an --add-dir directory when
// claudeMDFromAddDirs is in its environment, so the run adds its own working
// directory (ws, the tree) with --add-dir too. From such a directory the CLI
// reads skills, commands and subagents only through the project source, and
// from its settings only the plugin keys, with no plugin installed in the pod
// to enable. What the tree loses is the CLAUDE.md of a subdirectory, which
// the CLI loads on demand through the project source, and CLAUDE.local.md,
// which needs the local one.
const claudeSettingSources = "user"

// claudeMDFromAddDirs is the environment entry that has the CLI load
// CLAUDE.md from its --add-dir directories; see claudeSettingSources.
const claudeMDFromAddDirs = "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1"

// claudeNoAutoMemory turns the CLI's auto memory off for a read-only run.
// The CLI lets the agent write Markdown files into its own memory directory
// (and a few other files it manages for itself, such as its scratchpad)
// whatever the allow rules say, and that directory sits under HOME, the
// workspace root. With auto memory on, a report repair's resumed run would
// load what the first run wrote there; with it off nothing loads it, so the
// repair reads only patchy's prompt and the files it chooses to read. The
// writable posture keeps the CLI's default, since its stage may write
// anywhere in the workspace in any case.
const claudeNoAutoMemory = "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"

// PromptSpec builds the headless claude invocation for one prompted run.
// stream-json with --verbose emits one JSON event per line, which is what
// ParseResult and ScanUsage parse. Optional request fields append their flag
// only when set, in a stable order. A posture also pins where the run's
// settings come from (claudeSettingSources); the request's Env is never
// written through.
func (c *Claude) PromptSpec(ws string, req PromptRequest) runner.CommandSpec {
	argv := []string{
		"claude", "-p", req.Prompt,
		"--model", req.Model,
		"--output-format", "stream-json",
		"--verbose",
	}
	if req.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	env := req.Env
	if t, ok := claudeTools[req.Sandbox]; ok {
		if len(t.tools) > 0 {
			argv = append(argv, "--tools", strings.Join(t.tools, ","))
		}
		argv = append(argv, "--allowedTools", strings.Join(claudeAllow(ws, req, t.allow), " "))
		argv = append(argv, "--disallowedTools", strings.Join(t.deny, " "))
		argv = append(argv, "--setting-sources", claudeSettingSources, "--strict-mcp-config", "--add-dir", ws)
		env = append(slices.Clip(req.Env), claudeMDFromAddDirs)
		if req.Sandbox == SandboxReadOnly {
			env = append(env, claudeNoAutoMemory)
		}
	}
	for _, dir := range req.AddDirs {
		argv = append(argv, "--add-dir", dir)
	}
	if req.SessionID != "" {
		argv = append(argv, "--session-id", req.SessionID)
	}
	if req.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", req.SystemPromptAppend)
	}
	return runner.CommandSpec{Argv: argv, Dir: ws, Env: env}
}

// ResumeSpec continues session sessionID: PromptSpec's own command for req,
// so every flag the first run had is rendered again — the CLI keeps none of
// them, and a resumed run without its tool grammar would leave its sandbox
// posture — with --resume in place of --session-id, which the CLI refuses
// beside it. It is meant to run in the first run's directory, where the CLI
// filed the session.
//
// What claude 2.1.291 does on a resumed run (probed; production pins
// 2.1.263): the result event repeats the same session_id; num_turns and
// usage count this invocation alone, while total_cost_usd is cumulative
// over the session's invocations; --max-turns caps this invocation alone,
// not counting earlier turns; and an unknown session id ends at once with
// an error result event (error_during_execution, "No conversation found
// with session ID: <id>") and exit status 1.
func (c *Claude) ResumeSpec(ws, sessionID string, req PromptRequest) runner.CommandSpec {
	req.SessionID = ""
	spec := c.PromptSpec(ws, req)
	spec.Argv = append(spec.Argv, "--resume", sessionID)
	return spec
}

// claudeAllow is the posture's allow list for one request. A SandboxReadOnly
// request with WriteDirs swaps the bare Write, at its place in the list, for
// one Edit rule per directory: in Claude Code's grammar an Edit rule covers
// every file-editing tool (Write and Edit alike), and a "//" prefix makes its
// gitignore-style pattern absolute. The agent can then write and fix up files
// in those directories and is refused everywhere else. The scope has to be an
// allow rule: a deny overrides any allow, so a deny on the tree would stop
// the report as well. A relative directory is resolved against ws, the
// directory the CLI runs in. Every other request keeps the posture's list.
func claudeAllow(ws string, req PromptRequest, allow []string) []string {
	if req.Sandbox != SandboxReadOnly || len(req.WriteDirs) == 0 {
		return allow
	}
	scoped := make([]string, 0, len(allow)+len(req.WriteDirs))
	for _, tool := range allow {
		if tool != "Write" {
			scoped = append(scoped, tool)
			continue
		}
		for _, dir := range req.WriteDirs {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(ws, dir)
			}
			scoped = append(scoped, "Edit(/"+filepath.Clean(dir)+"/**)")
		}
	}
	return scoped
}

// ParseResult reads the terminal result event from claude's stream-json
// output; see parseStreamResult.
func (c *Claude) ParseResult(stdout []byte) (AgentResult, bool) {
	return parseStreamResult(stdout)
}

// RuntimeError classifies claude's output; see streamRuntimeError.
func (c *Claude) RuntimeError(stdout []byte, exitCode int, timedOut bool) string {
	return streamRuntimeError(stdout, exitCode, timedOut)
}

// ScanUsage reads the output-token count off one live stream line; see
// scanStreamUsage.
func (c *Claude) ScanUsage(line []byte) (int, bool) {
	return scanStreamUsage(line)
}

// claudeUsage is the token accounting claude reports: on the terminal result
// event's top-level usage, and per assistant event under message.usage. Cache
// reads and writes are kept on their own fields; see parseStreamResult for
// why they are not folded into input.
type claudeUsage struct {
	InputTokens              int  `json:"input_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
}

// claudeEvent is one line of Claude Code's stream-json (--verbose) output.
// Assistant events carry message.usage; the terminal type:"result" event
// carries the final answer, session id, turn count, usage, cost, and the
// error envelope (is_error/subtype/errors). Each event populates only its own
// fields, so the unused ones stay zero on the others.
type claudeEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID      string        `json:"id"`
		Role    string        `json:"role"`
		Content []claudeBlock `json:"content"`
		Usage   *claudeUsage  `json:"usage"`
	} `json:"message"`
	Result       string       `json:"result"`
	SessionID    string       `json:"session_id"`
	NumTurns     int          `json:"num_turns"`
	IsError      bool         `json:"is_error"`
	Subtype      string       `json:"subtype"`
	Errors       []string     `json:"errors"`
	Usage        *claudeUsage `json:"usage"`
	TotalCostUSD *float64     `json:"total_cost_usd"`
}

// claudeBlock is one content block of an assistant or user message. Only the
// fields of its own Type are populated; Input and Content stay raw because a
// tool's arguments are free-form and a tool result is either a bare string or
// an array of blocks (see renderToolInput/renderToolResult).
type claudeBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
	IsError  bool            `json:"is_error"`
}

// ScanTurns projects one stream-json line onto the transcript vocabulary; see
// scanStreamTurns.
func (c *Claude) ScanTurns(line []byte) []transcript.Turn { return scanStreamTurns(line) }

// scanStreamTurns projects one stream-json line onto the transcript
// vocabulary. Assistant events carry the model's own content blocks (text,
// thinking, tool calls); user events carry the tool results fed back to it.
// The init event becomes a banner so a transcript opens with the session it
// belongs to.
//
// Every other event type — including the terminal result, whose text is
// already the report — yields nothing: the transcript is the conversation, not
// a second copy of the outcome.
func scanStreamTurns(line []byte) []transcript.Turn {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "system":
		if ev.Subtype != "init" || ev.SessionID == "" {
			return nil
		}
		return []transcript.Turn{{
			Role: transcript.RoleSystem,
			Kind: transcript.KindNotice,
			Text: "session " + ev.SessionID + " started",
		}}
	case "assistant":
		return claudeAssistantTurns(ev.Message.Content)
	case "user":
		return claudeUserTurns(ev.Message.Content)
	}
	return nil
}

// claudeAssistantTurns maps the model's own content blocks.
func claudeAssistantTurns(blocks []claudeBlock) []transcript.Turn {
	var turns []transcript.Turn
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			turns = append(turns, transcript.Turn{
				Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: b.Text,
			})
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			turns = append(turns, transcript.Turn{
				Role: transcript.RoleAssistant, Kind: transcript.KindThinking, Text: b.Thinking,
			})
		case "tool_use":
			turns = append(turns, transcript.Turn{
				Role: transcript.RoleAssistant, Kind: transcript.KindToolUse,
				Tool: b.Name, Text: renderToolInput(b.Input),
			})
		}
	}
	return turns
}

// claudeUserTurns maps the tool results fed back to the model. A failed tool
// is marked in the text rather than in the vocabulary: readers care that the
// command failed, not that the transport distinguished it.
func claudeUserTurns(blocks []claudeBlock) []transcript.Turn {
	var turns []transcript.Turn
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text := renderToolResult(b.Content)
		if b.IsError {
			text = "[error] " + text
		}
		turns = append(turns, transcript.Turn{
			Role: transcript.RoleUser, Kind: transcript.KindToolResult, Text: text,
		})
	}
	return turns
}

// scanEvents walks stream-json output once and returns the terminal result
// event; found is false when the output carried none (plain text, or a crash
// mid-stream). parseStreamResult and streamRuntimeError each project from it.
func scanEvents(stdout []byte) (result claudeEvent, found bool) {
	for line := range bytes.SplitSeq(stdout, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "result" {
			result, found = ev, true
		}
	}
	return result, found
}

// parseStreamResult reads the final answer, session id, turn count, and usage
// from the terminal result event of stream-json output. Cache writes and
// reads are reported on their own fields rather than folded into input: a
// multi-turn cached session re-reads the same base context every turn, so
// lumping cache reads into "input" inflates it many-fold over the (cheaply
// cached) reality. total_cost_usd still reflects everything the session
// consumed. Output with no result event (plain text, crash) returns the raw
// stdout as FinalText with ok=false.
func parseStreamResult(stdout []byte) (AgentResult, bool) {
	ev, found := scanEvents(stdout)
	if !found {
		return AgentResult{FinalText: string(stdout)}, false
	}
	res := AgentResult{
		FinalText: ev.Result,
		SessionID: ev.SessionID,
		NumTurns:  ev.NumTurns,
		IsError:   ev.IsError,
		Subtype:   ev.Subtype,
		Errors:    ev.Errors,
	}
	if ev.Usage != nil {
		in := ev.Usage.InputTokens
		cacheRead := ev.Usage.CacheReadInputTokens
		cacheCreation := ev.Usage.CacheCreationInputTokens
		res.Usage = &Usage{
			InputTokens:         &in,
			CacheReadTokens:     &cacheRead,
			CacheCreationTokens: &cacheCreation,
			OutputTokens:        ev.Usage.OutputTokens,
			CostUSD:             ev.TotalCostUSD,
		}
	}
	return res, true
}

// streamRuntimeError detects a run that produced no usable answer (auth
// blocked, init crash, budget exhausted) so it can be reported distinctly
// from a run that completed and merely needs its answer judged.
//
// An error envelope is reported whether or not the run also produced text.
// It is tempting to treat any non-empty result as usable, but the CLI reports
// a genuinely failed run — exhausted turns, an API error mid-stream — with
// is_error set and, sometimes, a partial answer alongside it. Trusting that
// text hides the failure: the stage is then judged only by whether its report
// file exists, and a run that died with its budget spent surfaces as a
// missing-file error pointing at the workspace instead of at the cause.
func streamRuntimeError(stdout []byte, exitCode int, timedOut bool) string {
	if len(bytes.TrimSpace(stdout)) == 0 {
		return "empty CLI output"
	}
	result, found := scanEvents(stdout)
	if !found {
		switch {
		case timedOut:
			return "timed out with no result event"
		case exitCode != 0:
			return "unparseable CLI output"
		}
		return "" // a clean exit with plain-text output is degenerate but usable
	}
	if result.IsError {
		return claudeErrorReason(result.Subtype, result.Errors)
	}
	return "" // success, with or without text: usable (callers inspect the workspace)
}

// exhaustedSubtypes are the claude result subtypes meaning the run hit a
// limit rather than broke. They map onto the budget-exceeded outcome so an
// exhausted run is never mistaken for a malfunctioning one.
var exhaustedSubtypes = map[string]bool{
	"error_max_turns": true,
}

// Exhausted reports whether stdout describes a run that ran out of budget.
func (c *Claude) Exhausted(stdout []byte) bool {
	result, found := scanEvents(stdout)
	return found && result.IsError && exhaustedSubtypes[result.Subtype]
}

// TerminalError returns the error the terminal result event reported: its
// errors, and the result text of a run that ended on an API error — the
// CLI relays a model API error as a synthetic assistant message and
// repeats its text as the result of an is_error run (subtype "success",
// api_error_status set). Earlier events are never consulted: they hold
// model and tool content, and an error the run survived is not what ended
// it. "" when the run did not end in an error or has no result event.
func (c *Claude) TerminalError(stdout []byte) string {
	result, found := scanEvents(stdout)
	if !found || !result.IsError {
		return ""
	}
	var parts []string
	for _, e := range result.Errors {
		if e = strings.TrimSpace(e); e != "" {
			parts = append(parts, e)
		}
	}
	if r := strings.TrimSpace(result.Result); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "; ")
}

// scanStreamUsage reads the output-token count off one live stream line. Only
// assistant events carry message.usage; the result event's top-level usage
// (and every other event) reports ok=false so a budget accumulator never
// double-counts the terminal total.
func scanStreamUsage(line []byte) (int, bool) {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil {
		return 0, false
	}
	if ev.Type != "assistant" || ev.Message.Usage == nil || ev.Message.Usage.OutputTokens == nil {
		return 0, false
	}
	return *ev.Message.Usage.OutputTokens, true
}

// StreamUsage tallies the usage claude's assistant events reported; see
// streamUsage.
func (c *Claude) StreamUsage(stdout []byte) *Usage { return streamUsage(stdout) }

// streamUsage tallies the usage claude's assistant events reported, for a run
// that ended without the result event that totals it. The CLI emits one
// assistant event per content block, and every event of one API message
// repeats that message's usage, so the tally counts each message once, keyed
// by its id, taking each field's largest value across its events; an event
// with no id has nothing to merge on and counts as its own message.
//
// The input side (fresh input, cache reads, cache writes) is what the API
// reported for each call, so it is exact. The output side is a floor: the CLI
// stamps a message's usage as its content blocks complete, before the API's
// final output count arrives, so a stream reports a few output tokens a call
// where the result event reports the true total. Nil when no assistant event
// carried usage.
func streamUsage(stdout []byte) *Usage {
	type counts struct{ in, read, write, out int }
	byMessage := map[string]counts{}
	anonymous := 0
	for line := range bytes.SplitSeq(stdout, []byte{'\n'}) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" || ev.Message.Usage == nil {
			continue
		}
		id := ev.Message.ID
		if id == "" {
			anonymous++
			id = "\x00" + strconv.Itoa(anonymous)
		}
		u, prev := ev.Message.Usage, byMessage[id]
		byMessage[id] = counts{
			in:    max(prev.in, u.InputTokens),
			read:  max(prev.read, u.CacheReadInputTokens),
			write: max(prev.write, u.CacheCreationInputTokens),
			out:   max(prev.out, intOrZero(u.OutputTokens)),
		}
	}
	if len(byMessage) == 0 {
		return nil
	}
	var in, read, write, out int
	for _, c := range byMessage {
		in, read, write, out = in+c.in, read+c.read, write+c.write, out+c.out
	}
	return &Usage{InputTokens: &in, CacheReadTokens: &read, CacheCreationTokens: &write, OutputTokens: &out}
}

// FirstEditTurn reads the turn of claude's first file edit; see
// firstEditTurn.
func (c *Claude) FirstEditTurn(stdout []byte) int { return firstEditTurn(stdout) }

// claudeEditTools are the claude tools that change a file. Bash can too, but
// a command is not told apart from a read, so it is not counted.
var claudeEditTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// firstEditTurn is the 1-based turn of the first assistant event carrying a
// tool call of claudeEditTools, or 0 when none does. A turn is one API
// message: the CLI emits one assistant event per content block, so turns
// are counted by distinct message id in stream order, the way streamUsage
// merges them; an event with no id has nothing to merge on and is its own
// turn. Only the stream it is given is read, so a stage measures its main
// run alone, never a later repair round's resumed session.
func firstEditTurn(stdout []byte) int {
	seen := map[string]bool{}
	turns := 0
	for line := range bytes.SplitSeq(stdout, []byte{'\n'}) {
		var ev claudeEvent
		if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" {
			continue
		}
		switch id := ev.Message.ID; {
		case id == "":
			turns++
		case !seen[id]:
			seen[id] = true
			turns++
		}
		for _, b := range ev.Message.Content {
			if b.Type == "tool_use" && claudeEditTools[b.Name] {
				return turns
			}
		}
	}
	return 0
}

// claudeErrorReason renders the claude error envelope into one diagnostic
// line. The claude CLI reports a failed run only on stdout: the subtype names
// the class (error_max_turns, error_during_execution) and the `errors` array
// carries the human-readable detail. Neither is ever written to stderr, so
// without lifting them here the run surfaces as a bare non-zero exit with no
// explanation.
func claudeErrorReason(subtype string, errs []string) string {
	reason := "claude run error"
	if subtype != "" {
		reason += " (" + subtype + ")"
	}
	cleaned := make([]string, 0, len(errs))
	for _, e := range errs {
		if e = strings.TrimSpace(e); e != "" {
			cleaned = append(cleaned, e)
		}
	}
	if len(cleaned) > 0 {
		reason += ": " + strings.Join(cleaned, "; ")
	}
	return reason
}
