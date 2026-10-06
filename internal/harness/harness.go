// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Usage is the harness-reported consumption of one agent session. Fields are
// nil where the CLI does not report them. InputTokens is the fresh (uncached)
// input only; cache reads and writes are reported separately so a multi-turn
// session's cheap cache traffic does not inflate the headline input figure.
type Usage struct {
	InputTokens         *int // fresh (uncached) input only
	CacheReadTokens     *int
	CacheCreationTokens *int
	OutputTokens        *int
	CostUSD             *float64
}

// Sandbox is the filesystem/execution posture a run requires, declared
// harness-neutrally so agentrun states intent once and each harness renders it
// in its own vocabulary: claude as an allow/deny tool grammar, codex (once the
// pod's kernel sandbox is confirmed) as a --sandbox mode backed by mounted
// execpolicy rules. A harness that has no way to express a posture renders it
// as its default — the request is advisory, not a guarantee every CLI can keep.
type Sandbox int

const (
	// SandboxDefault imposes no tool policy: the CLI's own defaults apply. It
	// is the zero value, so a bare PromptRequest stays unrestricted.
	SandboxDefault Sandbox = iota
	// SandboxReadOnly is the investigation posture: read the tree and write
	// only the report — no source edits, no mutating commands, no network tools.
	SandboxReadOnly
	// SandboxWorkspaceWrite is the remediation posture: edit the workspace
	// freely; network tools stay denied.
	SandboxWorkspaceWrite
)

// PromptRequest describes one prompted agent run, harness-agnostically; the
// harness maps it onto its CLI's flags.
type PromptRequest struct {
	Prompt             string
	Model              string   // harness-specific model id
	SystemPromptAppend string   // appended to the CLI's system prompt when set
	SessionID          string   // pre-assigned UUID → claude --session-id
	MaxTurns           int      // agent-turn ceiling; 0 leaves the CLI default
	Sandbox            Sandbox  // filesystem/exec posture; each harness renders it natively
	AddDirs            []string // extra directories the agent may access
	Env                []string // extras appended to os.Environ() by the runner
	// WriteDirs scopes a SandboxReadOnly run's writes: the directories (absolute,
	// or relative to the run's workspace) it may create and change files in,
	// and nowhere else. Empty keeps the posture's unscoped write, so a request
	// that does not set it renders exactly as before; other postures ignore it.
	// claude renders it as path-scoped edit rules; a harness with no path
	// grammar ignores it, and the pod remains its boundary.
	WriteDirs []string
}

// AgentResult is the parsed terminal result of one agent run.
type AgentResult struct {
	FinalText string
	SessionID string // the session id the CLI reports on its result event
	NumTurns  int
	Usage     *Usage // includes CostUSD from total_cost_usd; nil when unreported
	IsError   bool
	Subtype   string
	Errors    []string
}

// Harness is the required surface every agent CLI implements. A harness
// drives a model — it does not own one; the model a run targets is supplied
// as a harness-specific CLI id on the PromptRequest.
type Harness interface {
	ID() string        // registry key, e.g. "claude"
	Name() string      // human name, e.g. "Claude Code"
	CLI() []string     // runner binary candidates, in preference order
	EnvKeys() []string // credential env vars, in preference order
	// PromptSpec builds the headless command for one prompted run in workspace ws.
	PromptSpec(ws string, req PromptRequest) runner.CommandSpec
	// ParseResult extracts the terminal result from the CLI's full stdout.
	// ok is false when the output carried no terminal result event (plain
	// text, crash mid-stream); the AgentResult then carries the raw stdout as
	// FinalText and nothing else.
	ParseResult(stdout []byte) (res AgentResult, ok bool)
	// RuntimeError returns a short reason when the agent run did not complete
	// usably (auth blocked, crash, budget exhausted, error envelope), or ""
	// when it did. A run that reports an error is reported as one even if it
	// also produced text: a partial answer from a run that died is not a
	// result, and treating it as one hides the cause behind whatever the
	// stage checks next.
	RuntimeError(stdout []byte, exitCode int, timedOut bool) string
}

// BudgetReporter is the optional capability of distinguishing a run that
// exhausted its budget from one that malfunctioned. Both are runtime errors;
// only the first is the agent doing as it was told until the limit stopped
// it, and callers report them differently.
type BudgetReporter interface {
	// Exhausted reports whether stdout describes a run that hit its turn or
	// token limit rather than failing.
	Exhausted(stdout []byte) bool
}

// TerminalErrorReporter is the optional capability of reading the error a run
// ended on off the CLI's own terminal event. It reports only what the CLI
// itself said ended the run — never the model's text or a tool's output,
// which carry whatever the repository holds — so a caller matching a
// contract message against it (the egress broker's limit prefix) cannot
// have the outcome chosen by a quote of that message in the workspace, or
// by an error the run recovered from before dying of something else.
type TerminalErrorReporter interface {
	// TerminalError returns the error text the CLI's terminal event
	// reported, or "" when the run did not end in an error.
	TerminalError(stdout []byte) string
}

// UsageScanner is the optional capability of reading token usage off the live
// output stream; it powers the caller's output-token budget kill switch.
type UsageScanner interface {
	// ScanUsage returns the output-token count reported by one stream line
	// (claude: assistant events' message.usage.output_tokens), ok=false
	// otherwise.
	ScanUsage(line []byte) (outputTokens int, ok bool)
}

// StreamUsageReporter is the optional capability of reading what a run spent
// off its stream when it ended without the terminal event that totals it: a
// timeout, the budget kill switch, a crash, a cancelled pod. Every model call
// is billed whether or not the run finishes, so a stage records this rather
// than nothing; a cost ceiling summed from what stages record cannot see a
// run that records zero.
type StreamUsageReporter interface {
	// StreamUsage returns the usage the stream reported per model call,
	// summed, or nil when it reported none. CostUSD stays nil: no CLI
	// reports a cost before its terminal event, so the controller prices the
	// tokens at the model's rates (agentresult.stageCost).
	StreamUsage(stdout []byte) *Usage
}

// TurnScanner is the optional capability of projecting the live output stream
// onto the harness-neutral conversation vocabulary; it powers the transcript
// the status page replays. A harness without it simply produces no transcript.
//
// Each CLI's stream is shaped differently — claude nests content blocks under
// an assistant message, codex flattens them into typed items, copilot emits one
// event per message — so normalising here is the only place the difference can
// be absorbed once.
type TurnScanner interface {
	// ScanTurns returns the turns one stream line carries, in order, or nil
	// for a line that carries none. One line can yield several: a single
	// claude assistant event may hold text, thinking, and tool-use blocks.
	//
	// It reports raw text; bounding, redaction, and sequencing belong to the
	// caller's transcript.Recorder.
	ScanTurns(line []byte) []transcript.Turn
}

// TaskWatcher is the optional capability of following a command the agent
// runs while it runs: reading the stream's command events, and knowing where
// the CLI keeps a running command's output. It powers the live command output
// agentrun shows beside the transcript. A harness without it shows a command's
// output only when its tool result arrives.
type TaskWatcher interface {
	// ScanTasks returns the command events one stream line carries, in
	// order, or nil for a line that carries none. Only foreground shell
	// commands are reported started: a backgrounded one outlives the call
	// that ran it, and other task types write no output file.
	ScanTasks(line []byte) []TaskEvent
	// TaskOutputGlob returns the filepath.Glob pattern matching the file a
	// running command's output is written to, for command task of session,
	// run by a CLI with environment env (its effective one: the last
	// assignment of a name wins) as user uid. "" when there is none to look
	// for. The file exists only while the command runs.
	TaskOutputGlob(env []string, uid int, session, task string) string
}

// TaskEventKind says what a TaskEvent reports.
type TaskEventKind int

// The command events.
const (
	// TaskSession: the run's session started; Session names it.
	TaskSession TaskEventKind = iota + 1
	// TaskStarted: a foreground shell command started. Task names it,
	// ToolUse the tool call that runs it, and Session its session when the
	// line names one.
	TaskStarted
	// TaskEnded: a command ended. Task names it, ToolUse its tool call.
	TaskEnded
	// TaskAnswered: a tool call was answered; ToolUse names it. A command's
	// call is answered once the command has ended, so this ends a command
	// whose own end was never reported.
	TaskAnswered
)

// TaskEvent is one command event off a CLI's stream.
type TaskEvent struct {
	Kind    TaskEventKind
	Session string
	Task    string
	ToolUse string
}

// Resumer is the optional capability of continuing a session an earlier run
// left behind, with one more prompt. agentrun uses it to have the agent that
// wrote a report patchy refused repair it in the same conversation, which
// still holds everything it did. A harness without it has no repair.
type Resumer interface {
	// ResumeSpec builds the headless command that continues session
	// sessionID in workspace ws with req.Prompt as the next message, under
	// the rest of req exactly as PromptSpec renders it: model, turn cap,
	// sandbox posture, directories, environment. req.SessionID is ignored;
	// sessionID names the session.
	ResumeSpec(ws, sessionID string, req PromptRequest) runner.CommandSpec
}

// All returns the builtin harness set.
func All() []Harness {
	return []Harness{NewClaude(), NewCodex(), NewCopilot(), NewFake()}
}

// ByID returns the builtin harness with the given id, if any.
func ByID(id string) (Harness, bool) {
	for _, h := range All() {
		if h.ID() == id {
			return h, true
		}
	}
	return nil, false
}

// Available finds the first of the harness's runner binaries on PATH.
func Available(h Harness) (path string, ok bool) {
	for _, name := range h.CLI() {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// AvailableIn finds the harness's runner binary under dir alone, never
// falling back to PATH: an injected CLI that is missing must be reported,
// not quietly replaced by the image's own. It is how a pod running a
// repository-declared image resolves the CLI patchy injected (agentrun's
// preflight, whose result the stage then runs by absolute path).
func AvailableIn(h Harness, dir string) (path string, ok bool) {
	for _, name := range h.CLI() {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// base carries the descriptive fields shared by all harnesses.
type base struct {
	id      string
	name    string
	clis    []string
	envKeys []string
}

func (b base) ID() string        { return b.id }
func (b base) Name() string      { return b.name }
func (b base) CLI() []string     { return b.clis }
func (b base) EnvKeys() []string { return b.envKeys }
