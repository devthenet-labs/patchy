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

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// captureStdout points os.Stdout, where a stage writes its envelope stream,
// at a file for the duration of the test and returns a reader of it.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = orig
		_ = f.Close()
	})
	return func() string {
		raw, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
}

// TestRunStageFailureExitsTwo: a valid configuration whose workspace holds
// no repository clone fails the stage: the failure is emitted as a fatal
// envelope event on stdout (what the controller collects) and the process
// exits 2, so the Job is marked failed.
func TestRunStageFailureExitsTwo(t *testing.T) {
	stdout := captureStdout(t)
	env := map[string]string{
		"PATCHY_WORKSPACE": t.TempDir(),
		"PATCHY_REPO":      "acme/api",
		"PATCHY_FINDING":   "finding-1",
		"PATCHY_PHASE":     "investigate",
	}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"agent-runner"}, func(k string) string { return env[k] },
		io.Discard, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent run failed") ||
		!strings.Contains(stderr.String(), "repository clone missing") {
		t.Errorf("stderr = %q, want the run failure naming the missing clone", stderr.String())
	}

	var fatal *envelope.Event
	for line := range strings.Lines(stdout()) {
		if ev, ok := envelope.Decode([]byte(line)); ok && ev.Type == envelope.TypeFatal {
			fatal = &ev
		}
	}
	if fatal == nil || fatal.Repo != "acme/api" || fatal.Finding != "finding-1" ||
		!strings.Contains(fatal.Error, "repository clone missing") {
		t.Errorf("fatal event = %+v, want one for acme/api finding-1 naming the missing clone", fatal)
	}
}

// TestRunRejectsBadPhase: an unknown phase is a configuration error (exit
// 2) before any stage starts, so nothing is emitted on stdout.
func TestRunRejectsBadPhase(t *testing.T) {
	stdout := captureStdout(t)
	env := map[string]string{"PATCHY_REPO": "acme/api", "PATCHY_FINDING": "f", "PATCHY_PHASE": "deploy"}
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"agent-runner"}, func(k string) string { return env[k] },
		io.Discard, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "invalid configuration") ||
		!strings.Contains(stderr.String(), `PATCHY_PHASE=\"deploy\"`) {
		t.Errorf("run = %d, stderr %q; want the phase configuration error", code, stderr.String())
	}
	if out := stdout(); strings.Contains(out, envelope.Prefix) {
		t.Errorf("stdout = %q, want no envelope event for a configuration error", out)
	}
}
