// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

func TestConfigEnvKeys(t *testing.T) {
	keys := ConfigEnvKeys()
	if !slices.IsSorted(keys) {
		t.Errorf("ConfigEnvKeys() = %v, want sorted", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "PATCHY_") {
			t.Errorf("ConfigEnvKeys() carries %q, want only PATCHY_* names", k)
		}
	}
	// The names the Job sets itself, the gateway names and the ones only an
	// image could otherwise smuggle in must all be there: the jobs package
	// blanks whatever is on this list and not on the pod.
	for _, want := range []string{
		"PATCHY_REPO", "PATCHY_PHASE", "PATCHY_FINDING", "PATCHY_BASE_SHA", "PATCHY_WORKSPACE",
		"PATCHY_INVESTIGATE_HARNESS", "PATCHY_REMEDIATE_MODEL", "PATCHY_MODEL_ALLOWLIST",
		"PATCHY_BROKER_TOKEN_FILE", "PATCHY_MODEL_MAP", "PATCHY_BIN_DIR",
		"PATCHY_GRANTED_TOKEN_BUDGET", "PATCHY_CHANGESET_MAX_BYTES", "PATCHY_CALIBRATION",
		"PATCHY_TRANSCRIPT_MAX_TOTAL_BYTES", "PATCHY_REMEDIATE_TIMEOUT",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("ConfigEnvKeys() lacks %s", want)
		}
	}

	// A fully populated, valid environment reads exactly the same keys: the
	// enumeration is the parser's real surface, not a subset the empty
	// environment happens to reach.
	full := map[string]string{"PATCHY_REPO": "acme/shop", "PATCHY_FINDING": "finding-abc123def0-1",
		"PATCHY_MODEL_MAP": "anthropic/claude-sonnet-5=claude-sonnet-5", "PATCHY_CALIBRATION": `{}`}
	seen := map[string]bool{}
	if _, err := FromEnv(func(k string) string { seen[k] = true; return full[k] }); err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	read := make([]string, 0, len(seen))
	for k := range seen {
		read = append(read, k)
	}
	slices.Sort(read)
	if !slices.Equal(read, keys) {
		t.Errorf("FromEnv read %v, ConfigEnvKeys() = %v", read, keys)
	}
}

func TestFromEnvBinDir(t *testing.T) {
	env := map[string]string{"PATCHY_REPO": "acme/shop", "PATCHY_FINDING": "f-1"}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BinDir != "" {
		t.Errorf("BinDir = %q on the default image, want empty", cfg.BinDir)
	}
	env["PATCHY_BIN_DIR"] = "/patchy/bin"
	if cfg, err = FromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if cfg.BinDir != "/patchy/bin" {
		t.Errorf("BinDir = %q, want /patchy/bin", cfg.BinDir)
	}
}

// writeTool drops an executable script named name into dir.
func writeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestPreflightWithFakeCLI runs the real executor against stand-in claude,
// git and bash binaries, the way a repository image would present (or
// fail to present) them.
func TestPreflightWithFakeCLI(t *testing.T) {
	tests := []struct {
		name    string
		bin     map[string]string // injected bin dir: name → script body
		path    map[string]string // the image's PATH dir: name → script body
		wantCLI bool
		wantErr []string
	}{
		{
			name:    "everything runs",
			bin:     map[string]string{"claude": "echo 2.1.263"},
			path:    map[string]string{"git": "echo git version 2.49.0", "bash": "exit 0"},
			wantCLI: true,
		},
		{
			name:    "claude was not injected",
			bin:     map[string]string{},
			path:    map[string]string{"git": "exit 0", "bash": "exit 0", "claude": "echo image-claude"},
			wantErr: []string{"no claude binary in", "prepare step injects"},
		},
		{
			name:    "claude cannot run on this libc",
			bin:     map[string]string{"claude": "echo 'claude: error while loading shared libraries: libc.so.6' >&2\nexit 127"},
			path:    map[string]string{"git": "exit 0", "bash": "exit 0"},
			wantErr: []string{"`", "/claude --version` failed", "exit status 127", "libc.so.6"},
		},
		{
			name:    "git is missing",
			bin:     map[string]string{"claude": "exit 0"},
			path:    map[string]string{"bash": "exit 0"},
			wantErr: []string{"`git --version` could not start"},
		},
		{
			name:    "bash is missing",
			bin:     map[string]string{"claude": "exit 0"},
			path:    map[string]string{"git": "exit 0"},
			wantErr: []string{"`bash -c true` could not start"},
		},
		{
			name:    "bash is busybox's stub",
			bin:     map[string]string{"claude": "exit 0"},
			path:    map[string]string{"git": "exit 0", "bash": "echo 'bash: applet not found' >&2\nexit 1"},
			wantErr: []string{"`bash -c true` failed", "applet not found"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir, pathDir := t.TempDir(), t.TempDir()
			for name, body := range tt.bin {
				writeTool(t, binDir, name, body)
			}
			for name, body := range tt.path {
				writeTool(t, pathDir, name, body)
			}
			t.Setenv("PATH", pathDir)

			cfg := newConfig(t, t.TempDir(), &bytes.Buffer{})
			cfg.BinDir = binDir
			a := New(cfg, &runner.Exec{})
			cli, err := a.preflight(context.Background(), harness.NewClaude())
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("preflight error = %v, want nil", err)
				}
				if want := filepath.Join(binDir, "claude"); cli != want {
					t.Errorf("preflight cli = %q, want the injected %q", cli, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflight = (%q, nil), want an error mentioning %v", cli, tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("preflight error = %q, want it to mention %q", err, want)
				}
			}
			if !strings.HasPrefix(err.Error(), "preflight: ") {
				t.Errorf("preflight error = %q, want the preflight: prefix", err)
			}
		})
	}
}

// injectedFakeCLI stages the fake harness's CLI in a bin dir so the
// preflight resolves it without a network or a real CLI.
func injectedFakeCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "cat", "exit 0")
	return dir
}

func TestPreflightSkippedOnDefaultImage(t *testing.T) {
	// newConfig leaves BinDir empty; the scripted executor sees exactly one
	// command, the stage, and would fail on any unscripted preflight step.
	ev := investigateRun(t)
	if ev.Investigation.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %q, want ok with no preflight on the default image", ev.Investigation.Outcome)
	}
}

func TestPreflightFailureEmitsImageIncompatible(t *testing.T) {
	failures := []struct {
		name   string
		result runner.Result
		err    error
		want   []string
	}{
		{"loader failure", runner.Result{ExitCode: 127, StderrTail: "not found"}, nil,
			[]string{"preflight:", "--version` failed", "exit status 127", "stderr: not found"}},
		{"unstartable", runner.Result{}, os.ErrPermission,
			[]string{"preflight:", "could not start", "permission denied"}},
		{"hang", runner.Result{TimedOut: true}, nil,
			[]string{"preflight:", "timed out"}},
	}
	for _, phase := range []Phase{PhaseInvestigate, PhaseRemediate} {
		for _, tt := range failures {
			t.Run(string(phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := remediateConfig(t, goodInvestigation, &out)
				cfg.Phase = phase
				cfg.BinDir = injectedFakeCLI(t)
				fx := &fakeExec{steps: []step{{ws: ws, result: tt.result, err: tt.err}}}

				if err := New(cfg, fx).Run(context.Background()); err != nil {
					t.Fatalf("Run() error = %v", err)
				}
				evs := events(t, out.String())
				if len(evs) != 1 {
					t.Fatalf("events = %d, want 1:\n%s", len(evs), out.String())
				}
				var outcome envelope.Outcome
				var detail string
				if phase == PhaseInvestigate {
					outcome, detail = evs[0].Investigation.Outcome, evs[0].Investigation.Detail
				} else {
					outcome, detail = evs[0].Remediation.Outcome, evs[0].Remediation.Detail
				}
				if outcome != envelope.OutcomeImageIncompatible {
					t.Errorf("outcome = %q, want image_incompatible (detail %q)", outcome, detail)
				}
				for _, want := range tt.want {
					if !strings.Contains(detail, want) {
						t.Errorf("detail = %q, want it to mention %q", detail, want)
					}
				}
				// The stage never ran: the one scripted command was the
				// failing preflight step.
				if len(fx.specs) != 1 {
					t.Errorf("executor saw %d commands, want only the failing preflight", len(fx.specs))
				}
			})
		}
	}
}

// TestPreflightPinsInjectedCLI: after a clean preflight the stage runs the
// injected CLI by absolute path, and the three checks ran in order first —
// in both stages, the remediation one above all, since it is the stage
// that produces the changeset.
func TestPreflightPinsInjectedCLI(t *testing.T) {
	for _, phase := range []Phase{PhaseInvestigate, PhaseRemediate} {
		t.Run(string(phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := remediateConfig(t, goodInvestigation, &out)
			cfg.Phase = phase
			cfg.BinDir = injectedFakeCLI(t)
			stage := step{ws: ws, stdout: streamSuccess,
				writes: map[string]string{"reports/investigation.md": goodInvestigation}}
			if phase == PhaseRemediate {
				stage = step{ws: ws, stdout: streamSuccess,
					writes:    map[string]string{"reports/remediation.md": goodRemediation, "commit.sh": commitScript},
					repoWrite: map[string]string{"app.js": "escaped();\n"},
				}
			}
			ok := step{ws: ws, result: runner.Result{Elapsed: time.Millisecond}}
			fx := &fakeExec{steps: []step{ok, ok, ok, stage}}

			if err := New(cfg, fx).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			evs := events(t, out.String())
			if len(evs) != 1 {
				t.Fatalf("events = %+v, want one", evs)
			}
			if phase == PhaseInvestigate && evs[0].Investigation.Outcome != envelope.OutcomeOK {
				t.Fatalf("investigation = %+v, want ok", evs[0].Investigation)
			}
			if phase == PhaseRemediate && (evs[0].Remediation.Outcome != envelope.OutcomeOK || !evs[0].Remediation.Success) {
				t.Fatalf("remediation = %+v, want a successful ok", evs[0].Remediation)
			}
			if len(fx.specs) != 4 {
				t.Fatalf("executor saw %d commands, want 3 preflight checks and the stage", len(fx.specs))
			}
			injected := filepath.Join(cfg.BinDir, "cat")
			wantArgv := [][]string{{injected, "--version"}, {"git", "--version"}, {"bash", "-c", "true"}}
			for i, want := range wantArgv {
				if got := fx.specs[i].Argv; !slices.Equal(got, want) {
					t.Errorf("preflight command %d = %v, want %v", i, got, want)
				}
				if fx.specs[i].Dir != ws {
					t.Errorf("preflight command %d ran in %q, want the workspace", i, fx.specs[i].Dir)
				}
			}
			if got := fx.specs[3].Argv[0]; got != injected {
				t.Errorf("stage argv[0] = %q, want the injected CLI %q", got, injected)
			}
		})
	}
}

func TestStageArgvUnpinnedOnDefaultImage(t *testing.T) {
	ws := newWorkspace(t)
	var out bytes.Buffer
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"reports/investigation.md": goodInvestigation}},
	}}
	if err := New(newConfig(t, ws, &out), fx).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := fx.specs[0].Argv[0]; got != "cat" {
		t.Errorf("stage argv[0] = %q, want the bare CLI name for PATH resolution", got)
	}
}

func TestPinCLI(t *testing.T) {
	spec := runner.CommandSpec{Argv: []string{"claude", "-p", "hi"}, Dir: "/w"}
	if got := pinCLI(spec, ""); !slices.Equal(got.Argv, spec.Argv) {
		t.Errorf("pinCLI(spec, \"\") = %v, want unchanged", got.Argv)
	}
	got := pinCLI(spec, "/patchy/bin/claude")
	if !slices.Equal(got.Argv, []string{"/patchy/bin/claude", "-p", "hi"}) || got.Dir != "/w" {
		t.Errorf("pinCLI = %+v", got)
	}
	if spec.Argv[0] != "claude" {
		t.Error("pinCLI mutated the caller's argv")
	}
}

// streamBrokerLimit is what claude 2.1.280 printed (trimmed of fields
// nothing reads) when the egress broker answered its first request 429 at a
// per-pod limit: a synthetic assistant message the CLI flags as an API
// error, then the terminal result carrying the same text.
const streamBrokerLimit = `{"type":"system","subtype":"init",` +
	`"session_id":"d267e741-0839-4a0b-98de-58c91facde4f"}` + "\n" +
	`{"type":"assistant","message":{"id":"a7406e26-4a34-44b7-b694-ac641a2dec2b","model":"<synthetic>",` +
	`"role":"assistant","stop_reason":"stop_sequence","type":"message","usage":{"input_tokens":0,"output_tokens":0},` +
	`"content":[{"type":"text","text":"API Error: Request rejected (429) · egress broker: per-pod limit: ` +
	`tokens per pod (400000) reached"}]},"parent_tool_use_id":null,` +
	`"session_id":"d267e741-0839-4a0b-98de-58c91facde4f","error":"rate_limit","is_api_error_message":true}` + "\n" +
	`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"terminal_reason":"api_error",` +
	`"num_turns":1,"result":"API Error: Request rejected (429) · egress broker: per-pod limit: tokens per pod ` +
	`(400000) reached","session_id":"d267e741-0839-4a0b-98de-58c91facde4f","total_cost_usd":0}`

// TestStageOutcomeBrokerLimit: a run the egress broker cut off at a per-pod
// limit ends budget_exceeded when the broker's message is what ended it —
// the CLI's terminal error, the harness's reason, or the CLI's stderr — and
// never because the prefix appears somewhere else in the stream. Tool
// results and the model's own text carry whatever the repository holds
// (patchy's own source quotes the prefix), and a limit relayed earlier in a
// run that then died of something else is not what ended it.
func TestStageOutcomeBrokerLimit(t *testing.T) {
	limit := provider.LimitMessagePrefix + ": tokens_per_pod 400000 exceeded"
	errorResult := func(errs string) string {
		return `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"",` +
			`"errors":[` + errs + `]}`
	}
	unrelated := errorResult(`"API Error: 500 upstream exploded"`)
	tests := []struct {
		name string
		res  runner.Result
		want envelope.Outcome
	}{
		{"the CLI's terminal API error (claude 2.1.280)", runner.Result{ExitCode: 1, Stdout: []byte(streamBrokerLimit)},
			envelope.OutcomeBudgetExceeded},
		{"limit in the result errors", runner.Result{Stdout: []byte(errorResult(`"API Error: 429 ` + limit + `"`))},
			envelope.OutcomeBudgetExceeded},
		{"limit on stderr", runner.Result{ExitCode: 1, StderrTail: "429 " + limit},
			envelope.OutcomeBudgetExceeded},
		{"an unrelated failure stays a runtime error",
			runner.Result{Stdout: []byte(errorResult(`"API Error: 500 upstream"`))},
			envelope.OutcomeRuntimeError},
		{"a limit mentioned by a successful run is not a failure",
			runner.Result{Stdout: []byte(streamSuccess + "\n" + limit)},
			envelope.OutcomeOK},
		{"a limit quoted in a tool result, then an unrelated terminal error",
			runner.Result{ExitCode: 1, Stdout: []byte(`{"type":"user","message":{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"t1","content":"const LimitMessagePrefix = \"` + limit + `\""}]}}` +
				"\n" + unrelated)},
			envelope.OutcomeRuntimeError},
		{"a limit in the model's own text, then an unrelated terminal error",
			runner.Result{ExitCode: 1, Stdout: []byte(`{"type":"assistant","message":{"content":[{"type":"text",` +
				`"text":"The broker says \"` + limit + `\" when a pod overspends."}]}}` + "\n" + unrelated)},
			envelope.OutcomeRuntimeError},
		{"a stale limit relayed earlier, then an unrelated terminal error",
			runner.Result{ExitCode: 1, Stdout: []byte(`{"type":"assistant","message":{"model":"<synthetic>",` +
				`"content":[{"type":"text","text":"API Error: Request rejected (429) · ` + limit + `"}]},` +
				`"error":"rate_limit","is_api_error_message":true}` + "\n" + errorResult(`"boom"`))},
			envelope.OutcomeRuntimeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, detail := stageOutcome(harness.NewClaude(), tt.res, nil, nil)
			if outcome != tt.want {
				t.Errorf("stageOutcome = (%q, %q), want %q", outcome, detail, tt.want)
			}
			carries := strings.Contains(detail, provider.LimitMessagePrefix)
			if tt.want == envelope.OutcomeBudgetExceeded && !carries {
				t.Errorf("detail = %q, want it to carry the broker's message", detail)
			}
			if tt.want == envelope.OutcomeRuntimeError && carries {
				t.Errorf("detail = %q quotes text that was not the broker's terminal answer", detail)
			}
		})
	}
}

// TestPreflightMain pins the subcommand's contract with the workstation
// check: the verdict line on out, 0 for a compatible image,
// ExitPreflightFailed for an incompatible one, and a different status,
// logged, when no verdict could be reached.
func TestPreflightMain(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		bin      map[string]string
		path     map[string]string
		noBinDir bool
		want     int
		out      string
		log      string
	}{
		{name: "compatible image", bin: map[string]string{"claude": "exit 0"},
			path: map[string]string{"git": "exit 0", "bash": "exit 0"}, want: 0, out: "preflight passed"},
		{name: "named harness", args: []string{"claude"}, bin: map[string]string{"claude": "exit 0"},
			path: map[string]string{"git": "exit 0", "bash": "exit 0"}, want: 0, out: "/claude --version`"},
		{name: "no bash", bin: map[string]string{"claude": "exit 0"}, path: map[string]string{"git": "exit 0"},
			want: ExitPreflightFailed, out: "preflight: `bash -c true` could not start"},
		{name: "claude not injected", path: map[string]string{"git": "exit 0", "bash": "exit 0"},
			want: ExitPreflightFailed, out: "preflight: no claude binary in"},
		{name: "no bin dir", noBinDir: true, want: exitPreflightMisconfigured, log: BinDirEnv + " is required"},
		{name: "unknown harness", args: []string{"nope"}, want: exitPreflightMisconfigured,
			log: `unknown harness \"nope\"`},
		{name: "too many arguments", args: []string{"claude", "extra"}, want: exitPreflightMisconfigured,
			log: "usage: agent-runner preflight [harness]"},
	}
	if ExitPreflightFailed == 0 || ExitPreflightFailed == exitPreflightMisconfigured {
		t.Fatal("the incompatible verdict must be distinguishable from success and from no verdict")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir, pathDir := t.TempDir(), t.TempDir()
			for name, body := range tt.bin {
				writeTool(t, binDir, name, body)
			}
			for name, body := range tt.path {
				writeTool(t, pathDir, name, body)
			}
			t.Setenv("PATH", pathDir)
			env := map[string]string{BinDirEnv: binDir, "PATCHY_WORKSPACE": t.TempDir()}
			if tt.noBinDir {
				delete(env, BinDirEnv)
			}
			var out, log bytes.Buffer
			got := PreflightMain(context.Background(), tt.args, func(k string) string { return env[k] },
				&runner.Exec{}, &out, slog.New(slog.NewTextHandler(&log, nil)))
			if got != tt.want {
				t.Errorf("PreflightMain = %d, want %d (out %q, log %q)", got, tt.want, out.String(), log.String())
			}
			if !strings.Contains(out.String(), tt.out) {
				t.Errorf("out = %q, want it to mention %q", out.String(), tt.out)
			}
			if !strings.Contains(log.String(), tt.log) {
				t.Errorf("log = %q, want it to mention %q", log.String(), tt.log)
			}
		})
	}
}
