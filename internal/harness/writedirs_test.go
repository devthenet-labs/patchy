// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"slices"
	"testing"
)

// TestClaudePromptSpecReadOnlyWriteDirs pins how a read-only run's write
// scope renders: the bare Write is replaced, at its place in the allow list,
// by one absolute Edit rule per directory (an Edit rule covers the Write
// tool too), and the deny list and the settings pin are untouched.
func TestClaudePromptSpecReadOnlyWriteDirs(t *testing.T) {
	const gitRO = "Bash(git log:*) Bash(git show:*) Bash(git blame:*) Bash(git diff:*)"
	tests := []struct {
		name string
		dirs []string
		want string
	}{
		{"one directory", []string{"/workspace/reports"},
			"Read Glob Grep Edit(//workspace/reports/**) " + gitRO},
		{"two directories, in order", []string{"/workspace/reports", "/scratch"},
			"Read Glob Grep Edit(//workspace/reports/**) Edit(//scratch/**) " + gitRO},
		{"a trailing slash is cleaned", []string{"/workspace/reports/"},
			"Read Glob Grep Edit(//workspace/reports/**) " + gitRO},
		{"a relative directory resolves against the workspace", []string{"../reports"},
			"Read Glob Grep Edit(//work/reports/**) " + gitRO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := NewClaude().PromptSpec("/work/repo", PromptRequest{
				Prompt: "plan", Model: "m", Sandbox: SandboxReadOnly, WriteDirs: tt.dirs,
			})
			want := []string{
				"claude", "-p", "plan", "--model", "m", "--output-format", "stream-json", "--verbose",
				"--allowedTools", tt.want,
				"--disallowedTools", "WebFetch WebSearch Task",
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
