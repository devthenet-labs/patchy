// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestReadOnlyPromptsStateTheToolSurface: the two read-only stages' prompts
// say what their sandbox actually gives them, no shell and writes to the
// report alone, and neither still offers the git commands an earlier posture
// allowed.
func TestReadOnlyPromptsStateTheToolSurface(t *testing.T) {
	investigate, err := RenderInvestigatePrompt(InvestigatePrompt{
		IssuePath:        "/workspace/input/issue.md",
		ReportPath:       "/workspace/reports/investigation.md",
		ReadOnlyEnforced: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := renderTestPlanPrompt(testPlanRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, got string
		want      []string
	}{
		{"investigate", investigate, []string{
			"It has no shell: do not try to run commands, tests, builds or git.",
			"no file except your report, `/workspace/reports/investigation.md`: a write anywhere\nelse is refused",
		}},
		{"plan", plan, []string{
			"It has no shell: you cannot run commands, tests, builds,\npackage managers, scripts or git.",
			"no file except your report, `/workspace/reports/plan.md`: a write anywhere else is\nrefused",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, want := range tt.want {
				if !strings.Contains(tt.got, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			for _, gone := range []string{"`git log`", "`git show`", "`git blame`", "`git diff`", "shell commands allowed"} {
				if strings.Contains(tt.got, gone) {
					t.Errorf("prompt still offers %s", gone)
				}
			}
		})
	}
}

// TestInvestigatePromptUnenforcedClaimsNoSandbox: on a harness that leaves
// the read-only posture to the pod (codex, copilot), the investigation
// prompt still asks for it but does not claim a tool surface the run does
// not have: no "no shell", no refused writes.
func TestInvestigatePromptUnenforcedClaimsNoSandbox(t *testing.T) {
	got, err := RenderInvestigatePrompt(InvestigatePrompt{
		IssuePath:  "/workspace/input/issue.md",
		ReportPath: "/workspace/reports/investigation.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "This stage is read-only. Do not run commands, tests, builds or git. " +
		"Find and read files with your file search and read\n" +
		"tools, and create or change no file except your report, `/workspace/reports/investigation.md`.\n"
	if !strings.Contains(got, want) {
		t.Errorf("prompt lacks the unenforced read-only paragraph %q:\n%s", want, got)
	}
	for _, claim := range []string{"no shell", "is refused", "`git log`"} {
		if strings.Contains(got, claim) {
			t.Errorf("prompt claims %q on a harness that does not enforce it", claim)
		}
	}
	if !strings.Contains(got, "described below.\n\nThis stage is read-only.") {
		t.Errorf("the read-only paragraph is not its own paragraph:\n%s", got)
	}
}
