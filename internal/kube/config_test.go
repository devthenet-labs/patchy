// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kube

import (
	"log/slog"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// TestRestConfigExplicitPath: an explicit kubeconfig answers with its
// current context, and the client-side rate limiter is disabled (QPS -1)
// rather than left at client-go's hidden 5 QPS default.
func TestRestConfigExplicitPath(t *testing.T) {
	cfg, err := RestConfig(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("RestConfig: %v", err)
	}
	if cfg.Host != "https://dev.example.test:6443" || cfg.BearerToken != "dev-token" {
		t.Errorf("host=%q token=%q, want the dev context", cfg.Host, cfg.BearerToken)
	}
	if cfg.QPS != -1 {
		t.Errorf("QPS = %v, want -1 (rate limiter disabled)", cfg.QPS)
	}
}

func TestRestConfigMissingExplicitPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := RestConfig(missing); err == nil {
		t.Fatal("RestConfig accepted a missing kubeconfig path")
	}
}

// TestRestConfigDefaultChain: with no explicit path the standard chain is
// used, so $KUBECONFIG answers out of cluster.
func TestRestConfigDefaultChain(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	cfg, err := RestConfig("")
	if err != nil {
		t.Fatalf("RestConfig: %v", err)
	}
	if cfg.Host != "https://dev.example.test:6443" {
		t.Errorf("host = %q, want the KUBECONFIG cluster", cfg.Host)
	}
}

// TestNewManager builds (never starts) a manager against the fixture: the
// construction contacts no API server, and the health checks register when
// a health address is set. (Per-object cache namespaces would resolve a
// REST mapping at construction, so they are covered via managerOptions.)
func TestNewManager(t *testing.T) {
	path := writeKubeconfig(t)
	for _, tt := range []struct {
		name string
		opts Options
	}{
		{"minimal", Options{Kubeconfig: path, LeaderElectionNamespace: "patchy"}},
		{"full", Options{
			Kubeconfig:              path,
			LeaderElectionID:        "patchy-test",
			LeaderElectionNamespace: "patchy",
			Namespaces:              []string{"patchy"},
			HealthAddr:              "127.0.0.1:0",
			Log:                     slog.New(slog.DiscardHandler),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := NewManager(tt.opts)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if mgr.GetScheme() == nil || !mgr.GetScheme().IsGroupRegistered("patchy.bitwisemedia.uk") {
				t.Error("manager scheme lacks the patchy API group")
			}
		})
	}
}

func TestNewManagerBadKubeconfig(t *testing.T) {
	if _, err := NewManager(Options{Kubeconfig: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("NewManager accepted a missing kubeconfig")
	}
}

// TestManagerOptions pins the remaining option mapping: metrics off unless
// an address is given, leader election only with an ID, and Secrets never
// cached; TestManagerOptionsConfigured, the namespace set and the agent
// namespace confining Jobs and Pods.
func TestManagerOptions(t *testing.T) {
	off := managerOptions(Options{})
	if off.Metrics.BindAddress != "0" || off.LeaderElection || off.Cache.DefaultNamespaces != nil ||
		off.Cache.ByObject != nil {
		t.Errorf("zero Options = metrics %q, LE %v, namespaces %v, byObject %v; want all off",
			off.Metrics.BindAddress, off.LeaderElection, off.Cache.DefaultNamespaces, off.Cache.ByObject)
	}
	if off.Client.Cache == nil || len(off.Client.Cache.DisableFor) != 1 {
		t.Fatalf("client cache = %+v, want exactly Secrets disabled", off.Client.Cache)
	}
	if _, ok := off.Client.Cache.DisableFor[0].(*corev1.Secret); !ok {
		t.Errorf("DisableFor[0] = %T, want *corev1.Secret", off.Client.Cache.DisableFor[0])
	}
	if off.Cache.DefaultTransform == nil {
		t.Error("no DefaultTransform: managedFields would be cached")
	}
}

func TestManagerOptionsConfigured(t *testing.T) {
	on := managerOptions(Options{
		MetricsAddr: ":9090", HealthAddr: ":0", LeaderElectionID: "lease", LeaderElectionNamespace: "ns",
		Namespaces: []string{"a", "b"}, AgentNamespace: "agents",
	})
	if on.Metrics.BindAddress != ":9090" || on.HealthProbeBindAddress != ":0" {
		t.Errorf("metrics=%q health=%q", on.Metrics.BindAddress, on.HealthProbeBindAddress)
	}
	if !on.LeaderElection || on.LeaderElectionID != "lease" || on.LeaderElectionNamespace != "ns" {
		t.Errorf("leader election = %v %q %q", on.LeaderElection, on.LeaderElectionID, on.LeaderElectionNamespace)
	}
	if len(on.Cache.DefaultNamespaces) != 2 {
		t.Errorf("DefaultNamespaces = %v, want a and b", on.Cache.DefaultNamespaces)
	}
	var jobs, pods bool
	for obj, by := range on.Cache.ByObject {
		_, isAgent := by.Namespaces["agents"]
		switch obj.(type) {
		case *batchv1.Job:
			jobs = isAgent && len(by.Namespaces) == 1
		case *corev1.Pod:
			pods = isAgent && len(by.Namespaces) == 1
		}
	}
	if !jobs || !pods {
		t.Errorf("Jobs confined=%v Pods confined=%v, want both on the agent namespace only", jobs, pods)
	}
}
