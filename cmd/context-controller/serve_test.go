// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/enhancers"
	"github.com/bitwise-media-group/patchy/pkg/enhance"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("context-controller", "test", opts)
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

func ids(chain []enhance.Enhancer) []string {
	out := make([]string, 0, len(chain))
	for _, e := range chain {
		out = append(out, e.ID())
	}
	return out
}

// TestBuildChainOrder pins the resolution order: the cloud lookups, then the
// generic fan-out, then the static file when one is configured.
func TestBuildChainOrder(t *testing.T) {
	dynamic := []string{enhancers.GoogleCloudLabelsID, enhancers.AWSTagsID, enhancers.AzureTagsID, "generic"}

	chain, cleanup, err := buildChain(chainOptions{})
	if err != nil {
		t.Fatalf("buildChain: %v", err)
	}
	cleanup()
	if got := ids(chain); !slices.Equal(got, dynamic) {
		t.Errorf("chain = %v, want %v", got, dynamic)
	}

	static := filepath.Join(t.TempDir(), "static.yaml")
	const doc = "repos:\n  acme/api:\n    owners: [alice]\n    markdown: \"owned by payments\\n\"\n"
	if err := os.WriteFile(static, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	chain, cleanup, err = buildChain(chainOptions{StaticFile: static})
	if err != nil {
		t.Fatalf("buildChain with a static file: %v", err)
	}
	defer cleanup()
	if got, want := ids(chain), append(slices.Clone(dynamic), "static-context"); !slices.Equal(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	enr, err := chain[len(chain)-1].Enhance(t.Context(), enhance.Issue{Repo: source.Repo{Owner: "acme", Name: "api"}})
	if err != nil || enr == nil || !slices.Equal(enr.Owners, []string{"alice"}) ||
		enr.CommentMarkdown != "owned by payments" {
		t.Errorf("static enhancement = %+v, %v; want the file's owners and markdown", enr, err)
	}
}

// TestBuildChainOutboundLimit: a positive --enhance-max-outbound bounds the
// generic fan-out; zero leaves it unbounded.
func TestBuildChainOutboundLimit(t *testing.T) {
	for _, tt := range []struct {
		max     int
		limited bool
	}{{0, false}, {3, true}} {
		chain, cleanup, err := buildChain(chainOptions{MaxOutbound: tt.max})
		if err != nil {
			t.Fatalf("buildChain: %v", err)
		}
		cleanup()
		gen, ok := chain[3].(*enhancers.DynamicGeneric)
		if !ok {
			t.Fatalf("chain[3] = %T, want the generic enhancer", chain[3])
		}
		if (gen.Limit != nil) != tt.limited {
			t.Errorf("MaxOutbound %d: limited = %v, want %v", tt.max, gen.Limit != nil, tt.limited)
		}
		if tt.limited && (!gen.Limit.TryAcquire(int64(tt.max)) || gen.Limit.TryAcquire(1)) {
			t.Errorf("MaxOutbound %d: the semaphore does not hold exactly %d", tt.max, tt.max)
		}
	}
}

func TestBuildChainBadStaticFile(t *testing.T) {
	for name, content := range map[string]*string{
		"missing":       nil,
		"unknown field": ptr("repos:\n  acme/api:\n    team: x\n"),
		"not yaml":      ptr("repos: [\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "static.yaml")
			if content != nil {
				if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			chain, cleanup, err := buildChain(chainOptions{StaticFile: path})
			if err == nil || !strings.Contains(err.Error(), "static context") || chain != nil || cleanup != nil {
				t.Fatalf("buildChain = %v, cleanup set %v, %v; want a static context error", chain, cleanup != nil, err)
			}
		})
	}
}

func ptr(s string) *string { return &s }

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
	bad := filepath.Join(t.TempDir(), "static.yaml")
	if err := os.WriteFile(bad, []byte("repos: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := serveOpts(t, "--kubeconfig", writeKubeconfig(t), "--health-addr", "", "--static-context-file", bad)
	if err := serve(t.Context(), opts); err == nil ||
		!strings.Contains(err.Error(), "static context") {
		t.Errorf("serve with a malformed static file = %v, want the chain's error before any controller starts", err)
	}
}

// writeKubeconfig points at a closed loopback port: building a manager from
// it contacts nothing.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	const cfg = `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
users:
  - name: test
    user:
      token: t
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
