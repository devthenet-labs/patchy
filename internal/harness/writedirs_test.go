// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestClaudePromptSpecReadOnlyWriteDirs pins how a read-only run's write
// scope renders: the bare Write is replaced, at its place in the allow list,
// by one absolute Edit rule per directory (an Edit rule covers the Write
// tool too), and the deny list and the settings pin are untouched.
func TestClaudePromptSpecReadOnlyWriteDirs(t *testing.T) {
	tests := []struct {
		name string
		dirs []string
		want string
	}{
		{"one directory", []string{"/workspace/reports"},
			"Read Glob Grep Edit(//workspace/reports/**)"},
		{"two directories, in order", []string{"/workspace/reports", "/scratch"},
			"Read Glob Grep Edit(//workspace/reports/**) Edit(//scratch/**)"},
		{"a trailing slash is cleaned", []string{"/workspace/reports/"},
			"Read Glob Grep Edit(//workspace/reports/**)"},
		{"a relative directory resolves against the workspace", []string{"../reports"},
			"Read Glob Grep Edit(//work/reports/**)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := NewClaude().PromptSpec("/work/repo", PromptRequest{
				Prompt: "plan", Model: "m", Sandbox: SandboxReadOnly, WriteDirs: tt.dirs,
			})
			want := []string{
				"claude", "-p", "plan", "--model", "m", "--output-format", "stream-json", "--verbose",
				"--tools", "Read,Glob,Grep,Edit,Write",
				"--allowedTools", tt.want,
				"--disallowedTools", "WebFetch WebSearch Task Bash",
				"--setting-sources", "user", "--strict-mcp-config", "--add-dir", "/work/repo",
			}
			if !slices.Equal(spec.Argv, want) {
				t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
			}
		})
	}
}

// TestWriteDirsOnlyScopesReadOnly: WriteDirs changes nothing outside the
// read-only posture — the workspace-write posture keeps its free writes and
// an unset posture still imposes no grammar — and changes nothing for the
// harnesses with no path grammar, which leave the pod as the boundary.
func TestWriteDirsOnlyScopesReadOnly(t *testing.T) {
	for _, h := range []Harness{NewClaude(), NewCodex(), NewCopilot()} {
		for _, sandbox := range []Sandbox{SandboxDefault, SandboxReadOnly, SandboxWorkspaceWrite} {
			if h.ID() == "claude" && sandbox == SandboxReadOnly {
				continue // the one scoped case, pinned above
			}
			req := PromptRequest{Prompt: "p", Model: "m", Sandbox: sandbox, AddDirs: []string{"/workspace"}}
			want := h.PromptSpec("/workspace/repo", req)
			req.WriteDirs = []string{"/workspace/reports"}
			if got := h.PromptSpec("/workspace/repo", req); !slices.Equal(got.Argv, want.Argv) {
				t.Errorf("%s, sandbox %v: WriteDirs changed argv:\n%q\nwant\n%q", h.ID(), sandbox, got.Argv, want.Argv)
			}
		}
	}
}

// TestClaudeReadOnlyWriteScopeExcludesSettingsAndGit: with the agent pod's
// layout (HOME and the workspace at /workspace, the tree at
// /workspace/repo, reports under /workspace/reports), a read-only run's
// write scope covers the report and leaves out the settings the CLI reads
// (claudeSettingSources: the user source under HOME), which a report
// repair's resumed run would load, the tree's .git directory and the tree
// itself; and its allow list names no shell, through which a write would
// not be held to the scope at all. On resume the same scope is rendered.
func TestClaudeReadOnlyWriteScopeExcludesSettingsAndGit(t *testing.T) {
	const ws, home = "/workspace/repo", "/workspace"
	req := PromptRequest{
		Prompt: "p", Model: "m", Sandbox: SandboxReadOnly,
		AddDirs: []string{home}, WriteDirs: []string{home + "/reports"},
	}
	c := NewClaude()
	for name, spec := range map[string][]string{
		"first run": c.PromptSpec(ws, req).Argv,
		"resume":    c.ResumeSpec(ws, claudeSessionID, req).Argv,
	} {
		t.Run(name, func(t *testing.T) {
			i := slices.Index(spec, "--allowedTools")
			if i < 0 || i+1 >= len(spec) {
				t.Fatalf("argv lacks --allowedTools: %q", spec)
			}
			var dirs []string
			for _, rule := range strings.Fields(spec[i+1]) {
				switch {
				case rule == "Write", rule == "Edit", strings.HasPrefix(rule, "Bash"):
					t.Fatalf("allow rule %q is not held to the write scope", rule)
				case strings.HasPrefix(rule, "Edit("):
					dirs = append(dirs, strings.TrimSuffix(strings.TrimPrefix(rule, "Edit(/"), "/**)"))
				}
			}
			writable := func(path string) bool {
				return slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(path, d+"/") })
			}
			if !writable(home + "/reports/investigation.md") {
				t.Errorf("scope %q does not cover the report", dirs)
			}
			for _, path := range []string{
				filepath.Join(home, ".claude", "settings.json"),
				filepath.Join(home, ".claude", "settings.local.json"),
				filepath.Join(home, ".claude.json"),
				filepath.Join(ws, ".git", "config"),
				filepath.Join(ws, ".git", "hooks", "pre-commit"),
				filepath.Join(ws, ".claude", "settings.json"),
				filepath.Join(ws, "main.go"),
			} {
				if writable(path) {
					t.Errorf("scope %q covers %s", dirs, path)
				}
			}
		})
	}
}

// TestEnforcesReadOnly: only the claude harness renders the read-only
// posture itself; the prompts state its tool surface as fact on it alone.
func TestEnforcesReadOnly(t *testing.T) {
	for _, h := range All() {
		if got, want := EnforcesReadOnly(h), h.ID() == "claude"; got != want {
			t.Errorf("EnforcesReadOnly(%s) = %v, want %v", h.ID(), got, want)
		}
	}
}

// TestClaudeReadOnlyTurnsAutoMemoryOff: the CLI may write its own memory
// files under HOME whatever the allow rules say, and loads them into a later
// run of the same project, which a report repair's resume is. A read-only run
// turns auto memory off, on its first run and on resume alike, so nothing it
// wrote there is loaded again; the writable posture and an unpostured run
// keep the CLI's default.
func TestClaudeReadOnlyTurnsAutoMemoryOff(t *testing.T) {
	c := NewClaude()
	for _, tt := range []struct {
		sandbox Sandbox
		off     bool
	}{
		{SandboxReadOnly, true},
		{SandboxWorkspaceWrite, false},
		{SandboxDefault, false},
	} {
		req := PromptRequest{
			Prompt: "p", Model: "m", Sandbox: tt.sandbox,
			AddDirs: []string{"/workspace"}, WriteDirs: []string{"/workspace/reports"},
		}
		for name, env := range map[string][]string{
			"first run": c.PromptSpec("/workspace/repo", req).Env,
			"resume":    c.ResumeSpec("/workspace/repo", claudeSessionID, req).Env,
		} {
			if got := slices.Contains(env, claudeNoAutoMemory); got != tt.off {
				t.Errorf("sandbox %v, %s: %s in env = %v, want %v (env %q)",
					tt.sandbox, name, claudeNoAutoMemory, got, tt.off, env)
			}
		}
	}
}
