// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
)

// resourceOpts builds the binary's command tree the way main does, serve's
// RunE replaced so executing it only resolves the flags.
func resourceOpts(t *testing.T) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("investigation-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs([]string{"serve"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// TestAgentResourcesDefaultToNone: with none of the agent resource flags
// set, the agent Jobs this controller builds get no CPU or memory at all,
// exactly as before the flags existed; set through the environment, as the
// chart delivers them, they reach the Jobs' Config.
func TestAgentResourcesDefaultToNone(t *testing.T) {
	r, err := runnercfg.AgentResources(resourceOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsZero() {
		t.Errorf("agent resources with no flags = %s, want none", r)
	}

	t.Setenv("PATCHY_AGENT_CPU_REQUEST", "250m")
	t.Setenv("PATCHY_AGENT_MEMORY_LIMIT", "2Gi")
	r, err = runnercfg.AgentResources(resourceOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if cr, mr, cl, ml := r.Strings(); cr != "250m" || mr != "" || cl != "" || ml != "2Gi" {
		t.Errorf("agent resources = %q %q %q %q, want 250m, none, none, 2Gi", cr, mr, cl, ml)
	}
}

// TestAgentDNSFlag: the binary registers --agent-dns, defaulting to the
// cluster resolver, so the chart's PATCHY_AGENT_DNS reaches the Jobs it
// builds. Viper reads the environment for an unregistered name too, so the
// flag lookup, not the value, is the proof.
func TestAgentDNSFlag(t *testing.T) {
	f := newServeCmd(cli.NewOptions()).Flags().Lookup("agent-dns")
	if f == nil {
		t.Fatal("--agent-dns is not registered")
	}
	if f.DefValue != "cluster" {
		t.Errorf("--agent-dns defaults to %q, want cluster", f.DefValue)
	}
}
