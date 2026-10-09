// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/integration"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("integration-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// TestServeFlagDefaults pins the defaults the chart relies on not setting,
// and that the e2e-only overrides stay hidden from --help.
func TestServeFlagDefaults(t *testing.T) {
	opts := serveOpts(t)
	if got := opts.Duration("accumulation-window"); got != time.Hour {
		t.Errorf("accumulation-window = %v, want 1h", got)
	}
	if got := opts.Int("projection-concurrency"); got != 2 {
		t.Errorf("projection-concurrency = %d, want 2", got)
	}
	if opts.Bool("repository-images") {
		t.Error("repository-images defaults on, want off")
	}
	if got := opts.Duration("stale-recheck-interval"); got != integration.DefaultStaleRecheck {
		t.Errorf("stale-recheck-interval = %v, want %v", got, integration.DefaultStaleRecheck)
	}
	if got := opts.String("google-oidc-issuer"); got != "" {
		t.Errorf("google-oidc-issuer = %q, want empty (Google)", got)
	}
	f := newServeCmd(cli.NewOptions()).Flags()
	for _, name := range []string{"google-oidc-issuer", "stale-recheck-interval"} {
		if fl := f.Lookup(name); fl == nil || !fl.Hidden {
			t.Errorf("flag %s is not hidden", name)
		}
	}
}

func TestServeFlagsFromEnv(t *testing.T) {
	t.Setenv("PATCHY_ACCUMULATION_WINDOW", "15m")
	t.Setenv("PATCHY_REPOSITORY_IMAGES", "true")
	opts := serveOpts(t)
	if got := opts.Duration("accumulation-window"); got != 15*time.Minute {
		t.Errorf("accumulation-window = %v, want 15m from the environment", got)
	}
	if !opts.Bool("repository-images") {
		t.Error("repository-images not read from PATCHY_REPOSITORY_IMAGES")
	}
}

func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	missing := filepath.Join(t.TempDir(), "absent")
	if err := serve(t.Context(), serveOpts(t)); err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("serve without a namespace = %v, want the namespace error", err)
	}
	if err := serve(t.Context(), serveOpts(t, "--namespace", "p", "--kubeconfig", missing)); err == nil ||
		!strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("serve with a missing kubeconfig = %v, want the kubeconfig error", err)
	}
	t.Setenv("POD_NAMESPACE", "patchy")
	if err := serve(t.Context(), serveOpts(t, "--kubeconfig", missing)); err == nil ||
		!strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("serve with POD_NAMESPACE = %v, want it to get past the namespace to the kubeconfig", err)
	}
}
