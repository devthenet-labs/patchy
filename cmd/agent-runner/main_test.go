// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/sandboxprobe"
)

// TestRunDispatchesSandboxProbe: the argument internal/jobs writes into the
// prepare script selects the probe, not a stage. Both sides use
// sandboxprobe.Command; this pins the binary's side (internal/jobs pins the
// script's), with a configuration the probe rejects so no network is
// touched and the two paths are told apart by what they complain about.
func TestRunDispatchesSandboxProbe(t *testing.T) {
	env := map[string]string{sandboxprobe.TimeoutEnv: "not-a-duration"}
	getenv := func(k string) string { return env[k] }

	var probe bytes.Buffer
	code := run(context.Background(), []string{"agent-runner", sandboxprobe.Command}, getenv, io.Discard, &probe)
	if code != 2 ||
		!strings.Contains(probe.String(), "invalid sandbox probe configuration") {
		t.Errorf("run(%s) = %d, stderr %q; want the probe's configuration error", sandboxprobe.Command, code,
			probe.String())
	}

	var stage bytes.Buffer
	if code := run(context.Background(), []string{"agent-runner"}, getenv, io.Discard, &stage); code != 2 ||
		!strings.Contains(stage.String(), "PATCHY_REPO is required") || strings.Contains(stage.String(), "sandbox") {
		t.Errorf("run() = %d, stderr %q; want the stage's configuration error, not the probe", code, stage.String())
	}
}

// TestRunDispatchesPreflight: `agent-runner preflight` runs the preflight
// alone, not a stage — the stage would demand PATCHY_REPO — and prints its
// verdict on stdout with the exit status the workstation check reads.
func TestRunDispatchesPreflight(t *testing.T) {
	binDir, pathDir := t.TempDir(), t.TempDir()
	for dir, tools := range map[string][]string{binDir: {"claude"}, pathDir: {"git", "bash"}} {
		for _, tool := range tools {
			if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", pathDir)
	env := map[string]string{agentrun.BinDirEnv: binDir, "PATCHY_WORKSPACE": t.TempDir()}
	getenv := func(k string) string { return env[k] }

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"agent-runner", agentrun.PreflightCommand}, getenv, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "preflight passed") {
		t.Errorf("run(%s) = %d, stdout %q, stderr %q; want the passing verdict", agentrun.PreflightCommand, code,
			stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	delete(env, agentrun.BinDirEnv)
	code = run(context.Background(), []string{"agent-runner", agentrun.PreflightCommand}, getenv, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), agentrun.BinDirEnv+" is required") ||
		strings.Contains(stderr.String(), "PATCHY_REPO") {
		t.Errorf("run(%s) without %s = %d, stderr %q; want the preflight's configuration error, not the stage's",
			agentrun.PreflightCommand, agentrun.BinDirEnv, code, stderr.String())
	}
}
