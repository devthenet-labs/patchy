// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("remediation-controller", "test", opts)
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

// TestAgentEnvDefaults pins the PATCHY_* handoff every remediation pod
// receives with no flags set. The budgets match the ones the investigation
// controller quotes in the analysis prompt by default; the allowlist is not
// in it (the spawner enforces it controller-side).
func TestAgentEnvDefaults(t *testing.T) {
	want := map[string]string{
		"PATCHY_REMEDIATE_TIMEOUT":             "45m0s",
		"PATCHY_REMEDIATE_IDLE_TIMEOUT":        "20m0s",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "80",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "400000",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "240",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "1200000",
	}
	if got := agentEnv(serveOpts(t)); !maps.Equal(got, want) {
		t.Errorf("agentEnv =\n%v\nwant\n%v", got, want)
	}
}

func TestAgentEnvFollowsFlags(t *testing.T) {
	t.Setenv("PATCHY_REMEDIATE_TIMEOUT", "2h")
	got := agentEnv(serveOpts(t, "--remediate-idle-timeout", "0", "--remediate-auto-max-turns", "1",
		"--remediate-auto-token-budget", "2", "--remediate-manual-max-turns", "3",
		"--remediate-manual-token-budget", "4"))
	want := map[string]string{
		"PATCHY_REMEDIATE_TIMEOUT":             "2h0m0s",
		"PATCHY_REMEDIATE_IDLE_TIMEOUT":        "0s",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "1",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "2",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "3",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "4",
	}
	if !maps.Equal(got, want) {
		t.Errorf("agentEnv =\n%v\nwant\n%v", got, want)
	}
}

// TestServeStartupErrors: each misconfiguration fails startup naming its
// cause, before a manager is built or any cluster call is made.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	with := func(extra ...string) []string {
		return append([]string{"--namespace", "p", "--fake-agent-image", "fake:1"}, extra...)
	}
	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "no namespace", wantErr: "namespace is required"},
		{name: "no runner", args: []string{"--namespace", "p"}, wantErr: "no agent runner configured"},
		{name: "namespace from the pod", env: "patchy", wantErr: "no agent runner configured"},
		{name: "bad copilot credential env", args: []string{"--namespace", "p", "--copilot-agent-image", "c:1",
			"--copilot-secret-env", "OPENAI_API_KEY"}, wantErr: "--copilot-secret-env"},
		{name: "unparseable disk wall", args: with("--agent-ephemeral-storage", "big"),
			wantErr: "--agent-ephemeral-storage"},
		{name: "bad agent resources", args: with("--agent-cpu-limit", "nope"), wantErr: "--agent-cpu-limit"},
		{name: "zero changeset cap", args: with("--changeset-max-entries", "0"),
			wantErr: "--changeset-max-entries must be positive"},
		{name: "negative idle timeout", args: with("--remediate-idle-timeout", "-1m"),
			wantErr: "--remediate-idle-timeout must not be negative"},
		{name: "missing kubeconfig", args: with("--kubeconfig", filepath.Join(t.TempDir(), "absent")),
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("POD_NAMESPACE", tt.env)
			}
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
