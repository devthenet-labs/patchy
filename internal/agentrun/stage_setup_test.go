// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// resultStage returns the stage fields of whichever result event a run emitted.
func resultStage(t *testing.T, ev envelope.Event) envelope.Stage {
	t.Helper()
	switch {
	case ev.Remediation != nil:
		return ev.Remediation.Stage
	case ev.Plan != nil:
		return ev.Plan.Stage
	case ev.Investigation != nil:
		return ev.Investigation.Stage
	}
	t.Fatalf("event %+v carries no stage", ev)
	return envelope.Stage{}
}

// reinitWithoutCommits replaces the workspace clone with one that has no
// commit, so HEAD cannot be resolved.
func reinitWithoutCommits(t *testing.T, ws string) {
	t.Helper()
	repo := filepath.Join(ws, "repo")
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
}

// TestStageSetupFailures: every stage that fails while setting itself up —
// before any agent runs — reports a typed outcome naming the cause, keeps its
// harness and model for the rollups, and never calls the executor.
func TestStageSetupFailures(t *testing.T) {
	setups := []struct {
		name   string
		setup  func(t *testing.T, cfg *Config, ws string)
		want   envelope.Outcome
		detail string
		phases []Phase
	}{
		{
			name:   "unknown harness",
			setup:  func(_ *testing.T, cfg *Config, _ string) { cfg.RemediateHarness = "nonesuch" },
			want:   envelope.OutcomeRuntimeError,
			detail: `unknown harness "nonesuch"`,
			phases: []Phase{PhaseRemediate},
		},
		{
			name:   "injected CLI missing",
			setup:  func(t *testing.T, cfg *Config, _ string) { cfg.BinDir = t.TempDir() },
			want:   envelope.OutcomeImageIncompatible,
			detail: "preflight: no fake binary in",
			phases: []Phase{PhaseRemediate, PhaseBuild, PhasePlan},
		},
		{
			name: "broker token unreadable",
			setup: func(t *testing.T, cfg *Config, _ string) {
				cfg.BrokerTokenFile = filepath.Join(t.TempDir(), "absent")
			},
			want:   envelope.OutcomeRuntimeError,
			detail: "broker token:",
			phases: []Phase{PhaseRemediate, PhaseBuild, PhasePlan},
		},
		{
			name: "broker token empty",
			setup: func(t *testing.T, cfg *Config, _ string) {
				p := filepath.Join(t.TempDir(), "token")
				if err := os.WriteFile(p, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.BrokerTokenFile = p
			},
			want:   envelope.OutcomeRuntimeError,
			detail: "is empty",
			phases: []Phase{PhaseRemediate, PhaseBuild, PhasePlan},
		},
		{
			name:   "clone without a commit",
			setup:  func(t *testing.T, _ *Config, ws string) { reinitWithoutCommits(t, ws) },
			want:   envelope.OutcomeRuntimeError,
			detail: "rev-parse HEAD",
			phases: []Phase{PhaseRemediate, PhaseBuild},
		},
	}
	for _, s := range setups {
		for _, phase := range s.phases {
			t.Run(s.name+"/"+string(phase), func(t *testing.T) {
				var out bytes.Buffer
				var cfg Config
				var ws string
				if phase == PhaseRemediate {
					cfg, ws = remediateConfig(t, goodInvestigation, &out)
				} else {
					cfg, ws = intentConfig(t, phase, &out)
				}
				s.setup(t, &cfg, ws)
				fx := &fakeExec{}
				st := resultStage(t, onlyEvent(t, cfg, fx, &out))
				if st.Outcome != s.want || !strings.Contains(st.Detail, s.detail) {
					t.Errorf("stage = %q / %q, want %q mentioning %q", st.Outcome, st.Detail, s.want, s.detail)
				}
				if st.Harness == "" || st.Model == "" {
					t.Errorf("stage lost its harness/model: %+v", st)
				}
				if len(fx.specs) != 0 {
					t.Errorf("executor called %d times before the stage was set up", len(fx.specs))
				}
			})
		}
	}
}

// TestRemediateSetsCommitIdentity: a clone with no commit identity of its own
// is given patchy's, so commit.sh can commit, and the changeset still lands.
func TestRemediateSetsCommitIdentity(t *testing.T) {
	// Keep the machine's own git config out of the clone's view: with no
	// global or system identity, only the clone's config can provide one.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	var out bytes.Buffer
	cfg, ws := remediateConfig(t, goodInvestigation, &out)
	repo := filepath.Join(ws, "repo")
	for _, key := range []string{"user.email", "user.name"} {
		cmd := exec.Command("git", "config", "--unset", key)
		cmd.Dir = repo
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config --unset %s: %v: %s", key, err, b)
		}
	}
	fx := &fakeExec{steps: []step{{
		ws: ws, stdout: streamSuccess,
		writes:    map[string]string{"reports/remediation.md": goodRemediation, "commit.sh": commitScript},
		repoWrite: map[string]string{"app.js": "escaped();\n"},
	}}}
	rem := onlyEvent(t, cfg, fx, &out).Remediation
	if rem == nil || rem.Outcome != envelope.OutcomeOK || !rem.Success {
		t.Fatalf("remediation = %+v, want a successful one", rem)
	}
	for key, want := range map[string]string{
		"user.email": "patchy[bot]@users.noreply.github.com",
		"user.name":  "patchy[bot]",
	} {
		cmd := exec.Command("git", "config", key)
		cmd.Dir = repo
		got, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("git config %s = %q (%v), want %q", key, got, err, want)
		}
	}
}

// TestNewDefaultsLogger: an Agent built without a logger still runs (it logs
// to the default), rather than panicking on its first log line.
func TestNewDefaultsLogger(t *testing.T) {
	var out bytes.Buffer
	cfg, _ := remediateConfig(t, goodInvestigation, &out)
	cfg.Log = nil
	cfg.RemediateHarness = "nonesuch"
	rem := onlyEvent(t, cfg, &fakeExec{}, &out).Remediation
	if rem == nil || rem.Outcome != envelope.OutcomeRuntimeError {
		t.Errorf("remediation = %+v, want a runtime error from the unknown harness", rem)
	}

}

// TestPlanUnreadableIntent: an intent handoff that exists but cannot be read
// (here a directory where the file should be) fails the plan stage before any
// agent runs, rather than planning against an empty intent.
func TestPlanUnreadableIntent(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := intentConfig(t, PhasePlan, &out)
	issue := filepath.Join(ws, "input", "issue.md")
	if err := os.Remove(issue); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(issue, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExec{}
	p := onlyEvent(t, cfg, fx, &out).Plan
	if p == nil || p.Outcome != envelope.OutcomeRuntimeError || !strings.Contains(p.Detail, "issue.md") {
		t.Errorf("plan = %+v, want a runtime error naming the intent handoff", p)
	}
	if len(fx.specs) != 0 {
		t.Errorf("executor called %d times", len(fx.specs))
	}
}
