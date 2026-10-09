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
	root := cli.NewControllerRoot("investigation-controller", "test", opts)
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

// TestAgentEnvDefaults pins the PATCHY_* handoff every investigation pod
// receives with no flags set: the analysis budgets, and the remediation
// budgets the analysis prompt quotes, which must match the remediation
// controller's own defaults.
func TestAgentEnvDefaults(t *testing.T) {
	want := map[string]string{
		"PATCHY_MODEL_ALLOWLIST":               "anthropic/claude-sonnet-5,anthropic/claude-opus-5",
		"PATCHY_INVESTIGATE_TIMEOUT":           "15m0s",
		"PATCHY_INVESTIGATE_IDLE_TIMEOUT":      "20m0s",
		"PATCHY_INVESTIGATE_MAX_TURNS":         "25",
		"PATCHY_INVESTIGATE_TOKEN_BUDGET":      "150000",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "80",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "400000",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "240",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "1200000",
	}
	if got := agentEnv(serveOpts(t)); !maps.Equal(got, want) {
		t.Errorf("agentEnv =\n%v\nwant\n%v", got, want)
	}
}

// TestAgentEnvFollowsFlags: every value comes from its flag (or the
// PATCHY_* variable the chart renders), never a constant.
func TestAgentEnvFollowsFlags(t *testing.T) {
	t.Setenv("PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET", "7")
	got := agentEnv(serveOpts(t,
		"--model-allowlist", "anthropic/claude-opus-5",
		"--investigate-timeout", "90s", "--investigate-idle-timeout", "0",
		"--investigate-max-turns", "3", "--investigate-token-budget", "4",
		"--remediate-auto-max-turns", "5", "--remediate-auto-token-budget", "6",
		"--remediate-manual-max-turns", "8"))
	for k, v := range map[string]string{
		"PATCHY_MODEL_ALLOWLIST":               "anthropic/claude-opus-5",
		"PATCHY_INVESTIGATE_TIMEOUT":           "1m30s",
		"PATCHY_INVESTIGATE_IDLE_TIMEOUT":      "0s",
		"PATCHY_INVESTIGATE_MAX_TURNS":         "3",
		"PATCHY_INVESTIGATE_TOKEN_BUDGET":      "4",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "5",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "6",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "7",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "8",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestServeStartupErrors: each misconfiguration fails startup naming its
// cause, before a manager is built or any cluster call is made.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	fake := []string{"--namespace", "p", "--fake-agent-image", "fake:1"}
	with := func(extra ...string) []string { return append(append([]string{}, fake...), extra...) }
	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "no namespace", wantErr: "namespace is required"},
		{name: "no runner", args: []string{"--namespace", "p"}, wantErr: "no agent runner configured"},
		{name: "namespace from the pod", env: "patchy", wantErr: "no agent runner configured"},
		{name: "bad codex credential env", args: []string{"--namespace", "p", "--codex-agent-image", "c:1",
			"--codex-secret-env", "ANTHROPIC_API_KEY"}, wantErr: "--codex-secret-env"},
		{name: "repository images without a disk wall", args: with("--repository-images"),
			wantErr: "--agent-ephemeral-storage is required"},
		{name: "zero disk wall", args: with("--agent-ephemeral-storage", "0"), wantErr: "must be positive"},
		{name: "negative idle timeout", args: with("--investigate-idle-timeout", "-1s"),
			wantErr: "--investigate-idle-timeout must not be negative"},
		{name: "bad agent resources", args: with("--agent-memory-request", "4Gi", "--agent-memory-limit", "1Gi"),
			wantErr: "agent resources"},
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
