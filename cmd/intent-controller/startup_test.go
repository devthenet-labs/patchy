// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
)

// resolved executes the command tree over args and returns its options.
func resolved(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts, root, _ := command(t, args...)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// openAISecret is the codex runner's default credential Secret.
func openAISecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-openai", Namespace: "patchy-agents"},
		Data:       map[string][]byte{"api-key": []byte("x")},
	}
}

// TestHarnessChoice: intents run both stages on one harness, and only on
// brokered claude or the fake harness.
func TestHarnessChoice(t *testing.T) {
	const codexModel = "openai/gpt-5.3-codex"
	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{name: "fake", args: []string{"--fake-agent-image", "fake:1"}, want: "fake"},
		{
			name: "codex is refused",
			args: []string{"--codex-agent-image", "codex:1",
				"--intent-plan-model", codexModel, "--intent-build-model", codexModel},
			wantErr: "intents run on brokered claude only, and the intent models resolve to codex",
		},
		{
			name: "stages on different harnesses",
			args: []string{"--codex-agent-image", "codex:1", "--claude-agent-image", "claude:1",
				"--broker-url", "http://broker.invalid", "--intent-build-model", codexModel},
			wantErr: "intents run both on one harness",
		},
		{
			name:    "unknown model",
			args:    []string{"--fake-agent-image", "fake:1", "--intent-plan-model", "acme/model-1"},
			wantErr: "acme/model-1",
		},
		{
			name:    "restricted to a harness without a runner",
			args:    []string{"--fake-agent-image", "fake:1", "--harnesses", "codex"},
			wantErr: `harness "codex" is enabled but has no runner image configured`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := resolved(t, tt.args...)
			runners, err := runnercfg.Runners(opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := harness(t.Context(), opts, fake.NewClientset(openAISecret()), "patchy-agents", runners)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("harness = %q, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("harness = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestHarnessAndDNS: the fake harness may run without a resolver; an
// --agent-dns value that is not a mode fails startup naming the flag, and a
// harness error comes first.
func TestHarnessAndDNS(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantMode jobs.DNSMode
		wantErr  string
	}{
		{name: "default", args: []string{"--fake-agent-image", "fake:1"}, wantMode: jobs.DNSCluster},
		{name: "none", args: []string{"--fake-agent-image", "fake:1", "--agent-dns", "none"}, wantMode: jobs.DNSNone},
		{name: "bogus mode", args: []string{"--fake-agent-image", "fake:1", "--agent-dns", "dnsmasq"},
			wantErr: "--agent-dns"},
		{name: "harness error first", args: []string{"--fake-agent-image", "fake:1", "--agent-dns", "dnsmasq",
			"--intent-build-model", "acme/x"}, wantErr: "acme/x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := resolved(t, tt.args...)
			runners, err := runnercfg.Runners(opts)
			if err != nil {
				t.Fatal(err)
			}
			id, mode, err := harnessAndDNS(t.Context(), opts, fake.NewClientset(), "patchy-agents", runners)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("harnessAndDNS error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || id != "fake" || mode != tt.wantMode {
				t.Fatalf("harnessAndDNS = %q, %q, %v; want fake, %q", id, mode, err, tt.wantMode)
			}
		})
	}
}

func TestReadJobSizes(t *testing.T) {
	const classes = `{"large":{"requests":{"cpu":"2","memory":"8Gi"},"limits":{"memory":"8Gi"}}}`
	s, err := readJobSizes(resolved(t, "--repository-images", "--agent-ephemeral-storage", "8Gi",
		"--agent-cpu-request", "500m", "--intent-resource-classes", classes))
	if err != nil {
		t.Fatalf("readJobSizes: %v", err)
	}
	if !s.repositoryImages || s.ephemeralStorage != "8Gi" {
		t.Errorf("repository images = %v, %q; want on, 8Gi", s.repositoryImages, s.ephemeralStorage)
	}
	if cr, _, _, _ := s.defaults.Strings(); cr != "500m" {
		t.Errorf("default cpu request = %q, want 500m", cr)
	}
	if len(s.classes) != 1 {
		t.Errorf("classes = %v, want the one large class", s.classes)
	}

	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "repository images without a wall", args: []string{"--repository-images"},
			wantErr: "--agent-ephemeral-storage is required"},
		{name: "bad default", args: []string{"--agent-memory-request", "lots"}, wantErr: "--agent-memory-request"},
		{name: "bad classes", args: []string{"--intent-resource-classes", "{"}, wantErr: "--intent-resource-classes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := readJobSizes(resolved(t, tt.args...)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("readJobSizes error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestSettingsRefusesNonPositive: every ceiling and interval must be
// positive, and an idle timeout may be zero but never negative.
func TestSettingsRefusesNonPositive(t *testing.T) {
	for _, flag := range []string{
		"--intent-poll-interval", "--intent-approval-poll-interval", "--intent-pr-poll-interval",
		"--intent-plan-max-turns", "--intent-plan-token-budget", "--intent-plan-timeout",
		"--intent-build-max-turns", "--intent-build-token-budget", "--intent-build-timeout",
		"--intent-revise-max-turns", "--intent-revise-token-budget", "--intent-revise-timeout",
	} {
		t.Run(flag, func(t *testing.T) {
			_, err := settings(resolved(t, flag, "0"), "patchy", "patchy-agents")
			if err == nil || err.Error() != flag+" must be positive" {
				t.Fatalf("settings with %s 0 = %v, want %q", flag, err, flag+" must be positive")
			}
		})
	}
	if _, err := settings(resolved(t, "--intent-rate-limit-floor", "-1"), "p", "a"); err == nil ||
		!strings.Contains(err.Error(), "--intent-rate-limit-floor") {
		t.Errorf("negative rate limit floor = %v, want refused", err)
	}
	for _, flag := range []string{"--intent-plan-idle-timeout", "--intent-build-idle-timeout",
		"--intent-revise-idle-timeout"} {
		if _, err := settings(resolved(t, flag, "-1s"), "p", "a"); err == nil ||
			!strings.Contains(err.Error(), flag+" must not be negative") {
			t.Errorf("settings with %s -1s = %v, want refused", flag, err)
		}
		if _, err := settings(resolved(t, flag, "0"), "p", "a"); err != nil {
			t.Errorf("settings with %s 0 = %v, want it disabled, not refused", flag, err)
		}
	}
}

func TestSettingsCarriesFlags(t *testing.T) {
	s, err := settings(resolved(t, "--intent-multi-repo", "--intent-previews-enabled",
		"--intent-rate-limit-floor", "0", "--intent-plan-max-turns", "7"), "patchy", "agents")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s.Namespace != "patchy" || s.AgentNamespace != "agents" || !s.MultiRepo || !s.Previews ||
		s.RateLimitFloor != 0 || s.Plan.MaxTurns != 7 {
		t.Errorf("settings = %+v", s)
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
		{name: "bad settings", args: with("--intent-poll-interval", "0"), wantErr: "--intent-poll-interval"},
		{name: "no runner", args: []string{"--namespace", "p"}, wantErr: "no agent runner configured"},
		{name: "namespace from the pod", env: "patchy", wantErr: "no agent runner configured"},
		{name: "bad job sizes", args: with("--intent-resource-classes", "["), wantErr: "--intent-resource-classes"},
		{name: "zero changeset cap", args: with("--changeset-max-entries", "0"),
			wantErr: "--changeset-max-entries must be positive"},
		{name: "missing kubeconfig", args: with("--kubeconfig", filepath.Join(t.TempDir(), "absent")),
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("POD_NAMESPACE", tt.env)
			}
			err := serve(t.Context(), resolved(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
