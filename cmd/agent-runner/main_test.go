// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
	if code := run(context.Background(), []string{"agent-runner", sandboxprobe.Command}, getenv, &probe); code != 2 ||
		!strings.Contains(probe.String(), "invalid sandbox probe configuration") {
		t.Errorf("run(%s) = %d, stderr %q; want the probe's configuration error", sandboxprobe.Command, code,
			probe.String())
	}

	var stage bytes.Buffer
	if code := run(context.Background(), []string{"agent-runner"}, getenv, &stage); code != 2 ||
		!strings.Contains(stage.String(), "PATCHY_REPO is required") || strings.Contains(stage.String(), "sandbox") {
		t.Errorf("run() = %d, stderr %q; want the stage's configuration error, not the probe", code, stage.String())
	}
}
