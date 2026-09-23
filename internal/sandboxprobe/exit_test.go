// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestExitStatus pins the process contract the prepare init relies on: 0
// hands over to the agent container, ExitUnenforced is the verdict the
// collectors map to SandboxUnenforced, and a probe that could not finish is
// neither. Each outcome prints its one line to the log.
func TestExitStatus(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		ctx  context.Context
		open func(round int, addr string) bool
		want int
		line string
	}{
		{"egress blocked hands over", context.Background(), func(int, string) bool { return false }, 0,
			"NetworkPolicy is enforced"},
		{"egress open is the unenforced verdict", context.Background(), func(int, string) bool { return true },
			ExitUnenforced, "refusing to run untrusted code"},
		{"an interrupted probe is no verdict", cancelled, func(int, string) bool { return false }, exitInterrupted,
			"sandbox probe interrupted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := newProber(newClock(), tt.open, 3*time.Second).Exit(tt.ctx, slog.New(slog.NewTextHandler(&buf, nil)))
			if got != tt.want {
				t.Errorf("Exit = %d, want %d (log %q)", got, tt.want, buf.String())
			}
			if !strings.Contains(buf.String(), tt.line) {
				t.Errorf("log = %q, want it to say %q", buf.String(), tt.line)
			}
		})
	}
}

// TestMainMisconfigured: a configuration the probe cannot run under exits
// exitMisconfigured, never 0 and never the unenforced verdict, and says why.
func TestMainMisconfigured(t *testing.T) {
	var buf bytes.Buffer
	env := map[string]string{TimeoutEnv: "twenty", "KUBERNETES_SERVICE_HOST": "10.96.0.1"}
	got := Main(context.Background(), func(k string) string { return env[k] }, slog.New(slog.NewTextHandler(&buf, nil)))
	if got != exitMisconfigured {
		t.Errorf("Main = %d, want %d", got, exitMisconfigured)
	}
	if !strings.Contains(buf.String(), "invalid sandbox probe configuration") {
		t.Errorf("log = %q, want the configuration error", buf.String())
	}
	if exitMisconfigured == 0 || exitMisconfigured == ExitUnenforced || exitInterrupted == 0 ||
		exitInterrupted == ExitUnenforced {
		t.Error("a probe that could not run must exit neither 0 nor the unenforced verdict")
	}
}
