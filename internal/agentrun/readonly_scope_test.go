// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// checkNoShell fails unless a read-only claude command has no shell: its
// tools list leaves Bash out, Bash is denied by name, and no allow rule
// names it.
func checkNoShell(t *testing.T, argv []string) {
	t.Helper()
	if got := argvFlag(t, argv, "--tools"); got != "Read,Glob,Grep,Edit,Write" {
		t.Errorf("--tools = %q, want the file tools alone", got)
	}
	if deny := strings.Fields(argvFlag(t, argv, "--disallowedTools")); !slices.Contains(deny, "Bash") {
		t.Errorf("--disallowedTools = %q, want Bash denied", deny)
	}
	for _, rule := range strings.Fields(argvFlag(t, argv, "--allowedTools")) {
		if strings.HasPrefix(rule, "Bash") {
			t.Errorf("--allowedTools allows %q in the read-only posture", rule)
		}
	}
}

// writableUnder reports whether claude's allow rules let a run write path:
// an unscoped Write or Edit, a shell, or an absolute Edit rule whose
// directory holds it.
func writableUnder(allow, path string) bool {
	for _, rule := range strings.Fields(allow) {
		switch {
		case rule == "Write", rule == "Edit", strings.HasPrefix(rule, "Bash"):
			return true
		case strings.HasPrefix(rule, "Edit(//") && strings.HasSuffix(rule, "/**)"):
			dir := strings.TrimSuffix(strings.TrimPrefix(rule, "Edit(/"), "/**)")
			if strings.HasPrefix(path, dir+"/") {
				return true
			}
		}
	}
	return false
}

// TestReadOnlyStagesWriteOnlyTheirReports: both read-only stages, the
// Finding investigation and the intent plan, run with no shell and with
// writes scoped to the reports directory. That scope leaves out the tree,
// its .git directory, and the settings the CLI reads from the user source
// under HOME (the workspace), which a report repair's resumed run would
// load.
func TestReadOnlyStagesWriteOnlyTheirReports(t *testing.T) {
	stages := []struct {
		name  string
		setup func(t *testing.T, out *bytes.Buffer) (Config, string)
		file  string
		good  string
	}{
		{"investigate", func(t *testing.T, out *bytes.Buffer) (Config, string) {
			ws := newWorkspace(t)
			return newConfig(t, ws, out), ws
		}, "investigation.md", goodInvestigation},
		{"plan", func(t *testing.T, out *bytes.Buffer) (Config, string) {
			return intentConfig(t, PhasePlan, out)
		}, "plan.md", goodPlan},
	}
	for _, st := range stages {
		t.Run(st.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := st.setup(t, &out)
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.InvestigateHarness, cfg.BrokerTokenFile = "claude", tokenFile
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"reports/" + st.file: st.good,
			}}}}
			ev := onlyEvent(t, cfg, fx, &out)
			if stage := eventStage(t, ev); stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (%s), want ok", stage.Outcome, stage.Detail)
			}
			argv := fx.specs[0].Argv
			checkNoShell(t, argv)
			allow := argvFlag(t, argv, "--allowedTools")
			if report := filepath.Join(ws, "reports", st.file); !writableUnder(allow, report) {
				t.Errorf("--allowedTools = %q cannot write the report %s", allow, report)
			}
			for _, path := range []string{
				filepath.Join(ws, ".claude", "settings.json"),       // the user source under HOME
				filepath.Join(ws, ".claude", "settings.local.json"), // never loaded; still out of scope
				filepath.Join(ws, ".claude.json"),
				filepath.Join(ws, "repo", ".git", "config"),
				filepath.Join(ws, "repo", ".git", "hooks", "pre-commit"),
				filepath.Join(ws, "repo", ".claude", "settings.json"),
				filepath.Join(ws, "repo", "main.go"),
				filepath.Join(ws, "input", "issue.md"),
				filepath.Join(ws, "commit.sh"),
			} {
				if writableUnder(allow, path) {
					t.Errorf("--allowedTools = %q lets a read-only stage write %s", allow, path)
				}
			}
		})
	}
}
