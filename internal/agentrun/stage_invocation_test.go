// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/runner"
)

var update = flag.Bool("update", false, "rewrite golden files")

// stageInvocation runs one stage through the claude harness — brokered, with
// a fixed session id — and renders the one command it executes (argv, the
// working directory and the extra environment) with the per-test workspace
// path replaced by /workspace, so the rendering is the same on every machine.
func stageInvocation(t *testing.T, phase Phase, s step, ws string, cfg Config) string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Phase = phase
	cfg.InvestigateHarness, cfg.RemediateHarness = "claude", "claude"
	cfg.BrokerTokenFile = tokenFile
	s.ws = ws
	fx := &fakeExec{steps: []step{s}}
	a := New(cfg, fx)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 1 {
		t.Fatalf("commands run = %d, want the one stage command", len(fx.specs))
	}
	return renderInvocation(fx.specs[0], ws)
}

// renderInvocation writes a command spec one argument per block, so a golden
// diff shows exactly which argument moved.
func renderInvocation(spec runner.CommandSpec, ws string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dir: %s\n", spec.Dir)
	for i, arg := range spec.Argv {
		fmt.Fprintf(&b, "--- argv[%d]\n%s\n", i, arg)
	}
	for _, kv := range spec.Env {
		fmt.Fprintf(&b, "--- env\n%s\n", kv)
	}
	return strings.ReplaceAll(b.String(), ws, "/workspace")
}

// goldenFile compares got with testdata/name, rewriting it under -update.
func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestFindingStageInvocationsUnchanged pins the exact command each Finding
// stage runs — the rendered prompt, every CLI flag and the brokered
// environment — so work on other stages that shares their helpers
// (templates, the harness, the budget grant, the broker env) is proven not
// to move them. The goldens were captured before any other stage existed.
func TestFindingStageInvocationsUnchanged(t *testing.T) {
	t.Run("investigate", func(t *testing.T) {
		ws := newWorkspace(t)
		var out bytes.Buffer
		got := stageInvocation(t, PhaseInvestigate, step{
			stdout: streamSuccess,
			writes: map[string]string{"reports/investigation.md": goodInvestigation},
		}, ws, newConfig(t, ws, &out))
		goldenFile(t, "invocation_investigate.txt", got)
	})
	t.Run("remediate", func(t *testing.T) {
		var out bytes.Buffer
		cfg, ws := remediateConfig(t, goodInvestigation, &out)
		cfg.GrantedMaxTurns, cfg.GrantedTokenBudget = 120, 600000
		got := stageInvocation(t, PhaseRemediate, step{
			stdout:    streamSuccess,
			writes:    map[string]string{"reports/remediation.md": goodRemediation, "commit.sh": commitScript},
			repoWrite: map[string]string{"app.js": "escaped();\n"},
		}, ws, cfg)
		goldenFile(t, "invocation_remediate.txt", got)
	})
}
