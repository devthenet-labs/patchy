// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// checkTurns compares the transcript-visible fields of two turn lists.
func checkTurns(t *testing.T, name string, got, want []transcript.Turn) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d turns, want %d: %+v", name, len(got), len(want), got)
		return
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Kind != want[i].Kind ||
			got[i].Tool != want[i].Tool || got[i].Text != want[i].Text {
			t.Errorf("%s: turn %d = %+v, want %+v", name, i, got[i], want[i])
		}
	}
}

// TestCodexScanTurnsEdges covers the shapes TestCodexScanTurns leaves out:
// malformed lines, empty bodies that carry nothing worth a turn, todo lists,
// MCP and search tools, and the argument fallbacks.
func TestCodexScanTurnsEdges(t *testing.T) {
	tool := func(name, text string) transcript.Turn {
		return transcript.Turn{Role: transcript.RoleAssistant, Kind: transcript.KindToolUse, Tool: name, Text: text}
	}
	tests := []struct {
		name string
		line string
		want []transcript.Turn
	}{
		{name: "not json", line: `codex: warming up`},
		{name: "thread without id", line: `{"type":"thread.started"}`},
		{name: "blank agent message", line: `{"type":"item.completed","item":{"type":"agent_message","text":"  "}}`},
		{name: "blank reasoning", line: `{"type":"item.completed","item":{"type":"reasoning","text":""}}`},
		{name: "blank todo list", line: `{"type":"item.completed","item":{"type":"todo_list","text":" "}}`},
		{name: "turn failed without error", line: `{"type":"turn.failed"}`},
		{name: "turn failed with blank message", line: `{"type":"turn.failed","error":{"message":"  "}}`},
		{
			name: "todo list",
			line: `{"type":"item.completed","item":{"type":"todo_list","text":"1. read\n2. fix"}}`,
			want: []transcript.Turn{{Role: transcript.RoleAssistant, Kind: transcript.KindNotice, Text: "1. read\n2. fix"}},
		},
		{
			name: "mcp tool names server and tool",
			line: `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"gh","tool":"search","query":"q"}}`,
			want: []transcript.Turn{tool("gh search", "q")},
		},
		{
			name: "mcp tool without a tool name stays MCP",
			line: `{"type":"item.completed","item":{"type":"mcp_tool_call","text":"payload"}}`,
			want: []transcript.Turn{tool("MCP", "payload")},
		},
		{
			name: "web search uses the query",
			line: `{"type":"item.completed","item":{"type":"web_search","query":"  CVE-2026-1  "}}`,
			want: []transcript.Turn{tool("WebSearch", "CVE-2026-1")},
		},
		{
			name: "file change without arguments falls back to the item type",
			line: `{"type":"item.completed","item":{"type":"file_change"}}`,
			want: []transcript.Turn{tool("Edit", "file_change")},
		},
		{
			name: "blank output carries no result turn",
			line: `{"type":"item.completed","item":{"type":"command_execution","command":"true","aggregated_output":"  "}}`,
			want: []transcript.Turn{tool("Bash", "true")},
		},
		{
			name: "output without an exit code is not marked an error",
			line: `{"type":"item.completed","item":{"type":"command_execution","command":"ls","aggregated_output":"a\n"}}`,
			want: []transcript.Turn{
				tool("Bash", "ls"),
				{Role: transcript.RoleUser, Kind: transcript.KindToolResult, Text: "a"},
			},
		},
	}
	c := NewCodex()
	for _, tt := range tests {
		checkTurns(t, tt.name, c.ScanTurns([]byte(tt.line)), tt.want)
	}
}

func TestCopilotScanTurnsEdges(t *testing.T) {
	c := NewCopilot()
	for name, line := range map[string]string{
		"not json":            `Error: not authenticated`,
		"session without id":  `{"type":"session.start"}`,
		"blank message":       `{"type":"assistant.message","data":{"content":"   "}}`,
		"blank session error": `{"type":"session.error","data":{"message":""}}`,
		"unknown event":       `{"type":"tool.execution_start","data":{"content":"x"}}`,
	} {
		if got := c.ScanTurns([]byte(line)); len(got) != 0 {
			t.Errorf("%s: got %+v, want no turns", name, got)
		}
	}
}

// TestCopilotRuntimeErrorReportsPlainTextHead: copilot's plain-text failures
// (an auth refusal before any event) reach the operator as the first
// non-blank line, capped so a dumped page cannot flood the status.
func TestCopilotRuntimeErrorReportsPlainTextHead(t *testing.T) {
	c := NewCopilot()
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"first non-blank line", "\n\n  Error: token rejected  \nsecond line\n",
			"copilot produced no result event: Error: token rejected"},
		{"long line capped", strings.Repeat("x", 250),
			"copilot produced no result event: " + strings.Repeat("x", 200) + "…"},
	}
	for _, tt := range tests {
		if got := c.RuntimeError([]byte(tt.stdout), 1, false); got != tt.want {
			t.Errorf("%s: RuntimeError = %q, want %q", tt.name, got, tt.want)
		}
	}
	if got := firstLine([]byte("\n \n\t\n")); got != "no output" {
		t.Errorf("firstLine(blank) = %q, want no output", got)
	}
}

// TestFakeDelegatesToClaude: the fake harness scans usage and locates task
// output exactly as Claude does, so a replayed fixture exercises the live
// code paths.
func TestFakeDelegatesToClaude(t *testing.T) {
	fake, claude := NewFake(), NewClaude()
	for _, line := range []string{
		`{"type":"assistant","message":{"usage":{"output_tokens":42}}}`,
		`{"type":"assistant","message":{}}`,
		`{"type":"result"}`,
		`not json`,
	} {
		fn, fok := fake.ScanUsage([]byte(line))
		cn, cok := claude.ScanUsage([]byte(line))
		if fn != cn || fok != cok {
			t.Errorf("ScanUsage(%s): fake (%d,%v), claude (%d,%v)", line, fn, fok, cn, cok)
		}
	}
	if n, ok := fake.ScanUsage([]byte(`{"type":"assistant","message":{"usage":{"output_tokens":42}}}`)); !ok || n != 42 {
		t.Errorf("fake ScanUsage = (%d, %v), want (42, true)", n, ok)
	}
	for _, tt := range []struct {
		env           []string
		session, task string
	}{
		{nil, "sess-1", "task_1"},
		{[]string{"CLAUDE_CODE_TMPDIR=/var/tmp"}, "sess-1", "task_1"},
		{nil, "../evil", "task_1"},
	} {
		f := fake.TaskOutputGlob(tt.env, 1000, tt.session, tt.task)
		c := claude.TaskOutputGlob(tt.env, 1000, tt.session, tt.task)
		if f != c {
			t.Errorf("TaskOutputGlob(%v,%s,%s): fake %q, claude %q", tt.env, tt.session, tt.task, f, c)
		}
	}
}

// TestTaskOutputGlobEscapesTmpDir: a temp directory holding glob
// metacharacters is matched as itself, never as a pattern.
func TestTaskOutputGlobEscapesTmpDir(t *testing.T) {
	got := NewClaude().TaskOutputGlob([]string{"CLAUDE_CODE_TMPDIR=/tmp/a*b?[c]"}, 7, "s", "t")
	want := `/tmp/a\*b\?\[c]/claude-7/*/s/tasks/t.output`
	if got != want {
		t.Errorf("TaskOutputGlob = %q, want %q", got, want)
	}
	if got := NewClaude().TaskOutputGlob([]string{"CLAUDE_CODE_TMPDIR=relative/dir"}, 7, "s", "t"); got != "" {
		t.Errorf("TaskOutputGlob(relative tmp) = %q, want empty", got)
	}
}

// TestGlobEscapeProperty: an escaped string, used as a pattern, matches
// exactly itself.
func TestGlobEscapeProperty(t *testing.T) {
	prop := func(s string) bool {
		s = strings.ReplaceAll(s, "/", "")
		ok, err := filepath.Match(globEscape(s), s)
		return err == nil && ok
	}
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(11)), MaxCount: 500}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestRenderToolInput(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"empty", ``, ""},
		{"not an object", `"just a string"`, `"just a string"`},
		{"malformed", ` {nope `, `{nope`},
		{"identifying key", `{"command":"  go test ./...  ","other":1}`, "go test ./..."},
		{"key precedence", `{"path":"b","file_path":"a"}`, "a"},
		{"blank key skipped", `{"command":"  ","pattern":"TODO"}`, "TODO"},
		{"non-string key skipped", `{"command":7}`, `{"command":7}`},
		{"no identifying key compacts", `{ "b": 2, "a": 1 }`, `{"a":1,"b":2}`},
	}
	for _, tt := range tests {
		if got := renderToolInput(json.RawMessage(tt.raw)); got != tt.want {
			t.Errorf("%s: renderToolInput(%s) = %q, want %q", tt.name, tt.raw, got, tt.want)
		}
	}
}

func TestRenderToolResult(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"empty", ``, ""},
		{"string", `"  done  "`, "done"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`, "a\nb"},
		{"blocks without text", `[{"type":"image"}]`, `[{"type":"image"}]`},
		{"other json", `{"k":1}`, `{"k":1}`},
	}
	for _, tt := range tests {
		if got := renderToolResult(json.RawMessage(tt.raw)); got != tt.want {
			t.Errorf("%s: renderToolResult(%s) = %q, want %q", tt.name, tt.raw, got, tt.want)
		}
	}
}

// TestClaudeScanTurnsToolResults: a user message's tool results become result
// turns, a failed one marked, and its other blocks ignored.
func TestClaudeScanTurnsToolResults(t *testing.T) {
	line := `{"type":"user","message":{"content":[` +
		`{"type":"text","text":"ignored"},` +
		`{"type":"tool_result","content":"ok"},` +
		`{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"exit 1"}]}]}}`
	want := []transcript.Turn{
		{Role: transcript.RoleUser, Kind: transcript.KindToolResult, Text: "ok"},
		{Role: transcript.RoleUser, Kind: transcript.KindToolResult, Text: "[error] exit 1"},
	}
	checkTurns(t, "claude user", NewClaude().ScanTurns([]byte(line)), want)
}

// TestAvailableSearchesPATHOnly: a harness whose CLI is nowhere on PATH is
// unavailable, and one dropped into a PATH directory is found there.
func TestAvailableSearchesPATHOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if got, ok := Available(NewCodex()); ok {
		t.Errorf("Available(codex) on an empty PATH = (%q, true), want unavailable", got)
	}
	p := filepath.Join(dir, NewCodex().CLI()[0])
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := Available(NewCodex()); !ok || got != p {
		t.Errorf("Available(codex) = (%q, %v), want %q", got, ok, p)
	}
}
