// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestResumers pins which harnesses can continue a session. agentrun repairs
// a refused report only on a Resumer, so codex and copilot keep a refused
// report refused, exactly as before repair existed.
func TestResumers(t *testing.T) {
	want := map[string]bool{"claude": true, "fake": true, "codex": false, "copilot": false}
	for _, h := range All() {
		_, ok := h.(Resumer)
		if w, known := want[h.ID()]; !known || ok != w {
			t.Errorf("harness %q implements Resumer = %v, want %v", h.ID(), ok, w)
		}
	}
}

// TestClaudeResumeSpec: a resumed run renders every flag the first run had —
// the CLI keeps none of them, and a plan resumed without its tool grammar
// would no longer be read-only, nor one without its settings pin confined to
// patchy's posture — with --resume in place of --session-id, which the CLI
// refuses beside it.
func TestClaudeResumeSpec(t *testing.T) {
	c := NewClaude()
	for _, sandbox := range []Sandbox{SandboxReadOnly, SandboxWorkspaceWrite} {
		req := PromptRequest{
			Prompt:    "fix the report",
			Model:     "claude-fable-5",
			SessionID: "ignored-on-resume",
			MaxTurns:  6,
			Sandbox:   sandbox,
			AddDirs:   []string{"/workspace"},
			Env:       []string{"ANTHROPIC_CUSTOM_HEADERS=x-patchy-broker-token: t"},
		}
		got := c.ResumeSpec("/workspace/repo", claudeSessionID, req)

		first := req
		first.SessionID = ""
		want := c.PromptSpec("/workspace/repo", first)
		want.Argv = append(want.Argv, "--resume", claudeSessionID)
		if !slices.Equal(got.Argv, want.Argv) {
			t.Errorf("sandbox %d: Argv =\n%q\nwant\n%q", sandbox, got.Argv, want.Argv)
		}
		if slices.Contains(got.Argv, "--session-id") || slices.Contains(got.Argv, "ignored-on-resume") {
			t.Errorf("sandbox %d: Argv %q carries --session-id beside --resume", sandbox, got.Argv)
		}
		for _, flag := range []string{
			"--allowedTools", "--disallowedTools", "--setting-sources", "--strict-mcp-config",
			"--add-dir", "--max-turns", "--model",
		} {
			if !slices.Contains(got.Argv, flag) {
				t.Errorf("sandbox %d: Argv %q lacks %s", sandbox, got.Argv, flag)
			}
		}
		// The read-only posture's tools list, which is what leaves it no
		// shell, is rendered again on resume like every other flag.
		if hasTools := slices.Contains(got.Argv, "--tools"); hasTools != (sandbox == SandboxReadOnly) {
			t.Errorf("sandbox %d: --tools rendered = %v in %q, want it exactly for the read-only posture",
				sandbox, hasTools, got.Argv)
		}
		wantEnv := append(slices.Clone(req.Env), claudeMDFromAddDirs)
		if sandbox == SandboxReadOnly {
			wantEnv = append(wantEnv, claudeNoAutoMemory)
		}
		if got.Dir != "/workspace/repo" || !slices.Equal(got.Env, wantEnv) {
			t.Errorf("sandbox %d: spec = %+v, want the first run's directory and the request env", sandbox, got)
		}
	}
}

// The result events claude 2.1.291 printed when probed: a first run, its
// resumption, and a resumption of a session that does not exist. Usage and
// num_turns count the invocation alone; total_cost_usd counts the session.
const (
	probedFirst = `{"type":"result","subtype":"success","is_error":false,"result":"done",` +
		`"session_id":"` + claudeSessionID + `","num_turns":2,"total_cost_usd":0.0439327,` +
		`"usage":{"input_tokens":18,"cache_creation_input_tokens":19351,"cache_read_input_tokens":41677,` +
		`"output_tokens":209}}`
	probedResumed = `{"type":"result","subtype":"success","is_error":false,"result":"bye",` +
		`"session_id":"` + claudeSessionID + `","num_turns":1,"total_cost_usd":0.0485374,` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":560,"cache_read_input_tokens":33147,` +
		`"output_tokens":32}}`
	probedNoSession = `{"type":"result","subtype":"error_during_execution","is_error":true,` +
		`"session_id":"00000000-0000-4000-8000-0000000000aa","num_turns":0,"total_cost_usd":0,` +
		`"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},` +
		`"result":null,"errors":["No conversation found with session ID: 00000000-0000-4000-8000-0000000000aa"]}`
)

// TestClaudeResumedResult pins how the probed shapes parse. A resumed run's
// usage is its own, so a stage sums it; its cost is not, so a stage never
// sums that. A session the CLI cannot find is a runtime error naming it.
func TestClaudeResumedResult(t *testing.T) {
	c := NewClaude()
	first, ok := c.ParseResult([]byte(probedFirst))
	if !ok {
		t.Fatal("first run: no result event parsed")
	}
	resumed, ok := c.ParseResult([]byte(probedResumed))
	if !ok {
		t.Fatal("resumed run: no result event parsed")
	}
	if resumed.SessionID != first.SessionID || resumed.NumTurns != 1 {
		t.Errorf("resumed = session %q, %d turns; want the first run's session and its own one turn",
			resumed.SessionID, resumed.NumTurns)
	}
	if got := derefInt(resumed.Usage.CacheReadTokens); got != 33147 {
		t.Errorf("resumed cache reads = %d, want the invocation's own 33147", got)
	}
	if *resumed.Usage.CostUSD <= *first.Usage.CostUSD {
		t.Errorf("resumed cost %v not above the first run's %v: the probe found it cumulative",
			*resumed.Usage.CostUSD, *first.Usage.CostUSD)
	}

	const want = "claude run error (error_during_execution): No conversation found with session ID: " +
		"00000000-0000-4000-8000-0000000000aa"
	if got := c.RuntimeError([]byte(probedNoSession), 1, false); got != want {
		t.Errorf("RuntimeError(no session) = %q, want %q", got, want)
	}
}

func TestFakeResumeSpec(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	t.Setenv(FakeFixtureEnv, fixture)
	f := NewFake()
	req := PromptRequest{Prompt: "repair", Env: []string{"A=b"}}
	got, want := f.ResumeSpec("/ws", claudeSessionID, req), f.PromptSpec("/ws", req)
	if !slices.Equal(got.Argv, want.Argv) || got.Dir != want.Dir || !slices.Equal(got.Env, want.Env) {
		t.Errorf("ResumeSpec = %+v, want the fixture replayed as PromptSpec does: %+v", got, want)
	}
}
