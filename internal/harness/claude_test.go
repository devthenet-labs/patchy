// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"slices"
	"testing"
)

// Claude Code emits one JSON event per line under --output-format stream-json
// --verbose: assistant events carry message.usage, and a terminal
// type:"result" event carries the final answer, session id, turn count,
// usage, cost, and error envelope. These fixtures mirror that captured shape.
const (
	claudeSessionID = "5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c"

	claudeStreamSuccess = `{"type":"system","subtype":"init","session_id":"` + claudeSessionID + `"}` + "\n" +
		`{"type":"assistant","message":{"usage":{"output_tokens":12},` +
		`"content":[{"type":"text","text":"Working."}]}}` + "\n" +
		`{"type":"assistant","message":{"usage":{"output_tokens":9},` +
		`"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test"}}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"Done.",` +
		`"session_id":"` + claudeSessionID + `","num_turns":7,"total_cost_usd":0.0123,` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":20,` +
		`"cache_read_input_tokens":50,"output_tokens":30}}`

	// A max-turns run with a populated result: the CLI flags it is_error, but
	// the partial answer is usable.
	claudeStreamMaxTurnsPartial = `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"Partial fix applied.",` +
		`"session_id":"` + claudeSessionID + `","num_turns":30,"errors":["hit max turns"]}`

	// A max-turns run that produced nothing: not usable.
	claudeStreamMaxTurnsEmpty = `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","errors":["hit max turns"]}`

	// A crash mid-run with an error envelope: not usable.
	claudeStreamExecError = `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"",` +
		`"errors":["tool Bash crashed"," ","API overloaded"]}`
)

func TestClaudePromptSpecAllFlags(t *testing.T) {
	c := NewClaude()
	spec := c.PromptSpec("/work/ws", PromptRequest{
		Prompt:             "fix the finding",
		Model:              "claude-fable-5",
		SystemPromptAppend: "never push",
		SessionID:          claudeSessionID,
		MaxTurns:           30,
		Sandbox:            SandboxWorkspaceWrite,
		AddDirs:            []string{"/scratch", "/fixtures"},
		Env:                []string{"ANTHROPIC_API_KEY=k"},
	})
	want := []string{
		"claude", "-p", "fix the finding",
		"--model", "claude-fable-5",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "30",
		"--allowedTools", "Read Glob Grep Edit Write NotebookEdit Bash",
		"--disallowedTools", "WebFetch WebSearch",
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--add-dir", "/work/ws",
		"--add-dir", "/scratch",
		"--add-dir", "/fixtures",
		"--session-id", claudeSessionID,
		"--append-system-prompt", "never push",
	}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
	}
	if spec.Dir != "/work/ws" {
		t.Errorf("Dir = %q, want the workspace", spec.Dir)
	}
	if want := []string{"ANTHROPIC_API_KEY=k", claudeMDFromAddDirs}; !slices.Equal(spec.Env, want) {
		t.Errorf("Env = %q, want the request env and then %q", spec.Env, want)
	}
}

func TestClaudePromptSpecMinimal(t *testing.T) {
	c := NewClaude()
	spec := c.PromptSpec("/ws", PromptRequest{Prompt: "hi", Model: "m"})
	want := []string{"claude", "-p", "hi", "--model", "m", "--output-format", "stream-json", "--verbose"}
	// SandboxDefault (the zero value) imposes no tool grammar.
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv = %q, want no optional flags: %q", spec.Argv, want)
	}
	if spec.Env != nil {
		t.Errorf("Env = %q, want none: an unset posture leaves the CLI's defaults", spec.Env)
	}
}

// TestClaudePromptSpecReadOnly: the read-only posture has no shell. Its
// tools list is the whole built-in set the run has (no Bash, so none of the
// CLI's built-in read-only shell commands either), Bash is denied by name
// beside it, and no allow rule names Bash.
func TestClaudePromptSpecReadOnly(t *testing.T) {
	c := NewClaude()
	spec := c.PromptSpec("/ws", PromptRequest{Prompt: "look", Model: "m", Sandbox: SandboxReadOnly})
	want := []string{
		"claude", "-p", "look", "--model", "m", "--output-format", "stream-json", "--verbose",
		"--tools", "Read,Glob,Grep,Edit,Write",
		"--allowedTools", "Read Glob Grep Write",
		"--disallowedTools", "WebFetch WebSearch Task Bash",
		"--setting-sources", "user",
		"--strict-mcp-config",
		"--add-dir", "/ws",
	}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
	}
}

// TestClaudePromptSpecKeepsTheTreesSettingsOut: every posture reads settings
// from the user source alone, so nothing under the working tree's .claude/
// (hooks, env, allow rules, skills) or its .mcp.json can change what the run
// may do, and loads CLAUDE.md through the workspace added as a directory
// instead. The request's Env is extended, never written through: the
// caller's backing array keeps its spare capacity untouched.
func TestClaudePromptSpecKeepsTheTreesSettingsOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sandbox Sandbox
	}{
		{"read-only", SandboxReadOnly},
		{"workspace-write", SandboxWorkspaceWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqEnv := make([]string, 1, 4)
			reqEnv[0] = "A=b"
			spec := NewClaude().PromptSpec("/ws", PromptRequest{
				Prompt: "p", Model: "m", Sandbox: tc.sandbox, AddDirs: []string{"/workspace"}, Env: reqEnv,
			})
			argv := spec.Argv
			if n := countArg(argv, "--setting-sources"); n != 1 {
				t.Fatalf("--setting-sources given %d times, want once: %q", n, argv)
			}
			if got := argv[slices.Index(argv, "--setting-sources")+1]; got != "user" {
				t.Errorf("--setting-sources = %q, want user alone (no project, no local)", got)
			}
			if !slices.Contains(argv, "--strict-mcp-config") {
				t.Errorf("argv = %q, want --strict-mcp-config", argv)
			}
			if !hasAddDir(argv, "/ws") || !hasAddDir(argv, "/workspace") {
				t.Errorf("argv = %q, want the working tree added as a directory beside the request's", argv)
			}
			want := []string{"A=b", claudeMDFromAddDirs}
			if tc.sandbox == SandboxReadOnly {
				want = append(want, claudeNoAutoMemory)
			}
			if !slices.Equal(spec.Env, want) {
				t.Errorf("Env = %q, want %q", spec.Env, want)
			}
			if spare := reqEnv[:cap(reqEnv)][1]; spare != "" {
				t.Errorf("request Env's backing array written through: %q", spare)
			}
		})
	}
}

// countArg counts the occurrences of arg in argv.
func countArg(argv []string, arg string) int {
	n := 0
	for _, a := range argv {
		if a == arg {
			n++
		}
	}
	return n
}

// hasAddDir reports whether argv adds dir with --add-dir.
func hasAddDir(argv []string, dir string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--add-dir" && argv[i+1] == dir {
			return true
		}
	}
	return false
}

func TestClaudeParseResultSuccess(t *testing.T) {
	c := NewClaude()
	res, ok := c.ParseResult([]byte(claudeStreamSuccess))
	if !ok {
		t.Fatal("ok = false, want a parsed result event")
	}
	if res.FinalText != "Done." {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "Done.")
	}
	if res.SessionID != claudeSessionID {
		t.Errorf("SessionID = %q, want %q", res.SessionID, claudeSessionID)
	}
	if res.NumTurns != 7 {
		t.Errorf("NumTurns = %d, want 7", res.NumTurns)
	}
	if res.IsError || res.Subtype != "success" {
		t.Errorf("envelope = (%v, %q), want a clean success", res.IsError, res.Subtype)
	}
	if res.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	// Fresh input, cache read, and cache write stay on their own fields.
	if got := derefInt(res.Usage.InputTokens); got != 100 {
		t.Errorf("InputTokens = %d, want 100", got)
	}
	if got := derefInt(res.Usage.CacheReadTokens); got != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", got)
	}
	if got := derefInt(res.Usage.CacheCreationTokens); got != 20 {
		t.Errorf("CacheCreationTokens = %d, want 20", got)
	}
	if got := derefInt(res.Usage.OutputTokens); got != 30 {
		t.Errorf("OutputTokens = %d, want 30", got)
	}
	if res.Usage.CostUSD == nil || *res.Usage.CostUSD != 0.0123 {
		t.Errorf("CostUSD = %v, want 0.0123", res.Usage.CostUSD)
	}
}

func TestClaudeParseResultFallbacks(t *testing.T) {
	c := NewClaude()

	// No result event: raw stdout comes back as FinalText with ok=false.
	raw := "plain text answer\n"
	if res, ok := c.ParseResult([]byte(raw)); ok || res.FinalText != raw || res.Usage != nil {
		t.Errorf("ParseResult(plain) = (%+v, %v), want raw fallback with ok=false", res, ok)
	}

	// A max-turns envelope with a partial answer still parses fully.
	res, ok := c.ParseResult([]byte(claudeStreamMaxTurnsPartial))
	if !ok || !res.IsError || res.Subtype != "error_max_turns" {
		t.Fatalf("ParseResult(max-turns) = (%+v, %v), want the error envelope parsed", res, ok)
	}
	if res.FinalText != "Partial fix applied." || res.NumTurns != 30 {
		t.Errorf("partial = (%q, %d turns), want the partial answer and turn count", res.FinalText, res.NumTurns)
	}
	if res.Usage != nil {
		t.Errorf("Usage = %+v, want nil when the result event carries none", res.Usage)
	}
}

func TestClaudeRuntimeError(t *testing.T) {
	c := NewClaude()
	tests := []struct {
		name     string
		stdout   string
		exitCode int
		timedOut bool
		want     string
	}{
		{"usable result", claudeStreamSuccess, 0, false, ""},
		// A partial answer from an exhausted run is not a result. Reporting
		// it as usable is what let a starved remediation surface as a missing
		// report file instead of as the budget failure it was.
		{"max turns with partial result is an error", claudeStreamMaxTurnsPartial, 1, false,
			"claude run error (error_max_turns): hit max turns"},
		{"empty", "", 1, false, "empty CLI output"},
		{"plain text clean exit", "hello\n", 0, false, ""},
		{"plain text crash", "boom\n", 1, false, "unparseable CLI output"},
		{"timeout with no result", "partial stream\n", -1, true, "timed out with no result event"},
		{"max turns empty result", claudeStreamMaxTurnsEmpty, 1, false,
			"claude run error (error_max_turns): hit max turns"},
		{"error during execution", claudeStreamExecError, 1, false,
			"claude run error (error_during_execution): tool Bash crashed; API overloaded"},
	}
	for _, tt := range tests {
		if got := c.RuntimeError([]byte(tt.stdout), tt.exitCode, tt.timedOut); got != tt.want {
			t.Errorf("%s: RuntimeError = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestClaudeExhausted(t *testing.T) {
	c := NewClaude()
	tests := []struct {
		name   string
		stdout string
		want   bool
	}{
		{"max turns with partial answer", claudeStreamMaxTurnsPartial, true},
		{"max turns with empty result", claudeStreamMaxTurnsEmpty, true},
		{"success", claudeStreamSuccess, false},
		{"crashed mid-run", claudeStreamExecError, false},
		{"no result event", "hello\n", false},
	}
	for _, tt := range tests {
		if got := c.Exhausted([]byte(tt.stdout)); got != tt.want {
			t.Errorf("%s: Exhausted = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestClaudeScanUsage(t *testing.T) {
	c := NewClaude()
	tests := []struct {
		name string
		line string
		want int
		ok   bool
	}{
		{"assistant with usage",
			`{"type":"assistant","message":{"usage":{"output_tokens":42},"content":[]}}`, 42, true},
		{"assistant without usage",
			`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`, 0, false},
		{"result event top-level usage is not counted",
			`{"type":"result","result":"Done.","usage":{"output_tokens":30}}`, 0, false},
		{"system event", `{"type":"system","subtype":"init"}`, 0, false},
		{"garbage", "not json", 0, false},
	}
	for _, tt := range tests {
		got, ok := c.ScanUsage([]byte(tt.line))
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: ScanUsage = (%d, %v), want (%d, %v)", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func derefInt(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// TestClaudeTerminalError: only the terminal result event of a run that
// ended in an error is reported — its errors, and the result text the CLI
// repeats an API error in — never the model's text or a tool's output
// earlier in the stream.
func TestClaudeTerminalError(t *testing.T) {
	var _ TerminalErrorReporter = NewClaude()
	const apiError = "API Error: Request rejected (429) · egress broker: per-pod limit: tokens per pod (400000) reached"
	// What claude 2.1.280 printed for a 429 on its first request (trimmed).
	captured := `{"type":"system","subtype":"init","session_id":"s"}` + "\n" +
		`{"type":"assistant","message":{"model":"<synthetic>","role":"assistant",` +
		`"content":[{"type":"text","text":"` + apiError + `"}]},"error":"rate_limit","is_api_error_message":true}` + "\n" +
		`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"terminal_reason":"api_error","num_turns":1,"result":"` + apiError + `","session_id":"s"}`
	quoted := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"` +
		`egress broker: per-pod limit"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"egress broker: per-pod limit"}]}}` + "\n"
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"an API error the run ended on", captured, apiError},
		{"execution errors", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"",` +
			`"errors":[" first ","","second"]}`, "first; second"},
		{"content earlier in the stream is never read",
			quoted + `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"]}`, "boom"},
		{"a successful run", quoted + `{"type":"result","subtype":"success","is_error":false,"result":"done"}`, ""},
		{"no result event", quoted, ""},
		{"empty output", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewClaude().TerminalError([]byte(tt.stdout)); got != tt.want {
				t.Errorf("TerminalError = %q, want %q", got, tt.want)
			}
		})
	}
}
