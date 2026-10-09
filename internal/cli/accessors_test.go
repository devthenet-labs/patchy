// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// controllerRun builds a controller root with one subcommand carrying the
// typed extra flags, executes args through it, and returns the resolved
// Options, exactly as a binary wires them.
func controllerRun(t *testing.T, args ...string) (*Options, error) {
	t.Helper()
	opts := NewOptions()
	root := NewControllerRoot("ctl", "test", opts)
	sub := &cobra.Command{Use: "serve", RunE: func(*cobra.Command, []string) error { return nil }}
	f := sub.Flags()
	f.Duration("interval", 5*time.Second, "")
	f.Float64("rate", 1.5, "")
	f.Int("count", 3, "")
	f.Bool("enabled", false, "")
	root.AddCommand(sub)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"serve"}, args...))
	return opts, root.Execute()
}

func TestNewOptionsDefaults(t *testing.T) {
	opts := NewOptions()
	if opts.Log == nil || opts.LogLevel == nil {
		t.Fatalf("NewOptions = %+v, want a logger and a level", opts)
	}
	if opts.LogLevel.Level() != slog.LevelInfo {
		t.Errorf("initial level = %v, want the LevelVar zero value (info)", opts.LogLevel.Level())
	}
}

func TestControllerRootTypedAccessors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		interval time.Duration
		rate     float64
		count    int
		enabled  bool
	}{
		{name: "defaults", interval: 5 * time.Second, rate: 1.5, count: 3},
		{
			name:     "flags",
			args:     []string{"--interval", "2m", "--rate", "0.25", "--count", "9", "--enabled"},
			interval: 2 * time.Minute, rate: 0.25, count: 9, enabled: true,
		},
		{
			name: "env",
			env: map[string]string{
				"PATCHY_INTERVAL": "90s", "PATCHY_RATE": "4", "PATCHY_COUNT": "11", "PATCHY_ENABLED": "true",
			},
			interval: 90 * time.Second, rate: 4, count: 11, enabled: true,
		},
		{
			name:     "flag beats env",
			args:     []string{"--count", "1", "--enabled=false"},
			env:      map[string]string{"PATCHY_COUNT": "11", "PATCHY_ENABLED": "true"},
			interval: 5 * time.Second, rate: 1.5, count: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, err := controllerRun(t, tt.args...)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got := opts.Duration("interval"); got != tt.interval {
				t.Errorf("Duration = %v, want %v", got, tt.interval)
			}
			if got := opts.Float("rate"); got != tt.rate {
				t.Errorf("Float = %v, want %v", got, tt.rate)
			}
			if got := opts.Int("count"); got != tt.count {
				t.Errorf("Int = %v, want %v", got, tt.count)
			}
			if got := opts.Bool("enabled"); got != tt.enabled {
				t.Errorf("Bool = %v, want %v", got, tt.enabled)
			}
		})
	}
}

// TestControllerRootLoadsBeforeRun pins that PersistentPreRunE resolves the
// shared options, so a bad --log-level fails the command before RunE runs
// and a good one reaches the shared level.
func TestControllerRootLoadsBeforeRun(t *testing.T) {
	opts, err := controllerRun(t, "--log-level", "debug", "--listen-addr", ":0")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if opts.LogLevel.Level() != slog.LevelDebug || opts.ListenAddr != ":0" {
		t.Errorf("level=%v listen=%q, want debug and :0", opts.LogLevel.Level(), opts.ListenAddr)
	}
	if _, err := controllerRun(t, "--log-level", "chatty"); err == nil {
		t.Error("execute with --log-level chatty succeeded, want the Load error")
	}
}
