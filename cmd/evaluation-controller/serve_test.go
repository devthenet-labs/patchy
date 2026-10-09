// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("evaluation-controller", "test", opts)
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

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeAPIServer answers GETs of Secrets in the agents namespace from
// secrets (name -> data keys); anything else is a 404 Status, as the API
// server answers a missing object.
func fakeAPIServer(t *testing.T, secrets map[string][]string) string {
	t.Helper()
	const prefix = "/api/v1/namespaces/patchy-agents/secrets/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name, ok := strings.CutPrefix(r.URL.Path, prefix)
		keys, found := secrets[name]
		if !ok || r.Method != http.MethodGet || !found {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
				Status:   metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
			})
			return
		}
		data := map[string][]byte{}
		for _, k := range keys {
			data[k] = []byte("x")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "Secret", "apiVersion": "v1",
			"metadata": map[string]any{"name": name, "namespace": "patchy-agents"}, "data": data,
		})
	}))
	t.Cleanup(srv.Close)
	return writeKubeconfig(t, srv.URL)
}

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	return writeFile(t, "kubeconfig", `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster:
      server: `+server+`
contexts:
  - name: test
    context:
      cluster: test
      user: test
users:
  - name: test
    user:
      token: t
`)
}

func TestServiceURL(t *testing.T) {
	if got := serviceURL(serveOpts(t), "artifact-base-url", "patchy", 9790); got !=
		"http://patchy-source-controller.patchy.svc.cluster.local:9790" {
		t.Errorf("default serviceURL = %q", got)
	}
	if got := serviceURL(serveOpts(t, "--artifact-base-url", "http://artifacts.test/"), "artifact-base-url",
		"patchy", 9790); got != "http://artifacts.test" {
		t.Errorf("configured serviceURL = %q, want the trailing slash trimmed", got)
	}
}

func TestUploadClient(t *testing.T) {
	c, err := uploadClient(serveOpts(t), "patchy")
	if err != nil || c.BaseURL != "http://patchy-source-controller.patchy.svc.cluster.local:9791" || c.Token != "" {
		t.Errorf("default uploadClient = %+v, %v", c, err)
	}
	tok := writeFile(t, "tok", " s3cret\n")
	c, err = uploadClient(serveOpts(t, "--internal-upload-token-file", tok,
		"--artifact-upload-url", "http://upload.test:1/"), "patchy")
	if err != nil || c.BaseURL != "http://upload.test:1" || c.Token != "s3cret" {
		t.Errorf("configured uploadClient = %+v, %v", c, err)
	}
	if _, err := uploadClient(serveOpts(t, "--internal-upload-token-file",
		filepath.Join(t.TempDir(), "absent")), "patchy"); err == nil ||
		!strings.Contains(err.Error(), "internal-upload-token-file") {
		t.Errorf("uploadClient with a missing token file = %v, want an error naming the flag", err)
	}
}

func TestResolveFleet(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	withSecret := map[string][]string{"patchy-openai": {"api-key"}}
	for _, tt := range []struct {
		name    string
		args    func(t *testing.T) []string
		want    []string
		wantErr string
	}{
		{
			name: "fake runner needs no cluster read",
			args: func(t *testing.T) []string {
				return []string{"--evolve-fake-image", "fake:1", "--kubeconfig", writeKubeconfig(t, "https://127.0.0.1:1")}
			},
			want: []string{"fake"},
		},
		{
			name: "codex with its credential",
			args: func(t *testing.T) []string {
				return []string{"--evolve-codex-image", "codex:1", "--kubeconfig", fakeAPIServer(t, withSecret)}
			},
			want: []string{"codex"},
		},
		{
			name: "codex without its credential",
			args: func(t *testing.T) []string {
				return []string{"--evolve-codex-image", "codex:1", "--kubeconfig", fakeAPIServer(t, nil)}
			},
			wantErr: "no evolve runner enabled",
		},
		{
			name: "codex credential lacking its key",
			args: func(t *testing.T) []string {
				return []string{"--evolve-codex-image", "codex:1", "--kubeconfig",
					fakeAPIServer(t, map[string][]string{"patchy-openai": {"other"}})}
			},
			wantErr: `has no key "api-key"`,
		},
		{
			name: "codex cannot run without a resolver",
			args: func(t *testing.T) []string {
				return []string{"--evolve-codex-image", "codex:1", "--agent-dns", "none",
					"--kubeconfig", fakeAPIServer(t, withSecret)}
			},
			wantErr: "needs a resolver",
		},
		{
			name: "resources checked before any cluster call",
			args: func(t *testing.T) []string {
				return []string{"--evolve-fake-image", "fake:1", "--agent-cpu-request", "lots",
					"--kubeconfig", filepath.Join(t.TempDir(), "absent")}
			},
			wantErr: "--agent-cpu-request",
		},
		{
			name: "missing kubeconfig",
			args: func(t *testing.T) []string {
				return []string{"--evolve-fake-image", "fake:1", "--kubeconfig", filepath.Join(t.TempDir(), "absent")}
			},
			wantErr: "kubeconfig",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := serveOpts(t, tt.args(t)...)
			runners, err := runnercfg.EvolveRunners(opts)
			if err != nil {
				t.Fatal(err)
			}
			runner, enabled, err := resolveFleet(t.Context(), opts, "patchy-agents", runners, log)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveFleet error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFleet: %v", err)
			}
			if runner == nil || !slices.Equal(enabled, tt.want) {
				t.Errorf("resolveFleet = %v, %v; want a runner and %v", runner, enabled, tt.want)
			}
		})
	}
}

// TestServeStartupErrors: each configuration serve refuses fails startup
// with its cause, in order, before any controller is registered.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	none := writeFile(t, "auth.yaml", "mode: none\n")
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no namespace", wantErr: "namespace is required"},
		{name: "no auth config", args: []string{"--namespace", "p"}, wantErr: "auth config path is required"},
		{name: "unknown auth mode", args: []string{"--namespace", "p", "--auth-config",
			writeFile(t, "auth.yaml", "mode: basic\n")}, wantErr: `unknown auth mode "basic"`},
		{name: "oidc without issuer", args: []string{"--namespace", "p", "--auth-config",
			writeFile(t, "auth.yaml", "mode: oidc\n")}, wantErr: "requires oidc.issuerURL"},
		{name: "no runner", args: []string{"--namespace", "p", "--auth-config", none},
			wantErr: "no evolve runner configured"},
		{name: "missing kubeconfig", args: []string{"--namespace", "p", "--auth-config", none,
			"--evolve-fake-image", "fake:1", "--kubeconfig", filepath.Join(t.TempDir(), "absent")},
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
