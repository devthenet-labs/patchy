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
		IssuePath:  "/workspace/input/issue.md",
		ReportPath: "/workspace/reports/investigation.md",
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
