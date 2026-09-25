// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/intent"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
)

// componentConfigMap is the kustomize component's own ConfigMap; the chart's
// intent-controller ConfigMap renders the same PATCHY_INTENT_* values
// (hack/chart-render-test.sh holds the two together).
const componentConfigMap = "../../deploy/kustomize/components/intent-controller/configmap.yaml"

// command builds the binary's command tree the way main does, with serve's
// RunE replaced so executing it only resolves the flags.
func command(t *testing.T, args ...string) (*cli.Options, *cobra.Command, *cobra.Command) {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("intent-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return opts, root, serve
}

// componentData reads the component ConfigMap's data.
func componentData(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(componentConfigMap)
	if err != nil {
		t.Fatalf("read %s: %v", componentConfigMap, err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse %s: %v", componentConfigMap, err)
	}
	if len(cm.Data) == 0 {
		t.Fatalf("%s has no data", componentConfigMap)
	}
	return cm.Data
}

// flagName maps an environment key onto the flag it sets, the way
// internal/cli binds them: PATCHY_ prefix off, underscores to dashes.
func flagName(key string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, "PATCHY_"), "_", "-"))
}

// sameValue compares a ConfigMap value with a flag default in the flag's own
// type, so 60s and 1m0s are the same duration.
func sameValue(f *pflag.Flag, value string) bool {
	if f.Value.Type() == "duration" {
		want, err1 := time.ParseDuration(value)
		got, err2 := time.ParseDuration(f.DefValue)
		return err1 == nil && err2 == nil && want == got
	}
	return f.DefValue == value
}

// TestComponentConfigBindsFlags holds the deployed configuration to the
// binary: every key the component sets is a flag intent-controller binds (a
// key that is not is silently ignored, which is how a renamed flag goes
// unnoticed), and every PATCHY_INTENT_* value is that flag's default, so the
// documented defaults and the running ones cannot drift apart.
func TestComponentConfigBindsFlags(t *testing.T) {
	_, root, serve := command(t)
	for key, value := range componentData(t) {
		name := flagName(key)
		f := serve.Flags().Lookup(name)
		if f == nil {
			f = root.PersistentFlags().Lookup(name)
		}
		if f == nil {
			t.Errorf("%s sets --%s, which intent-controller does not register", key, name)
			continue
		}
		if strings.HasPrefix(key, "PATCHY_INTENT_") && !sameValue(f, value) {
			t.Errorf("%s = %q, but --%s defaults to %q", key, value, name, f.DefValue)
		}
	}
}

// TestComponentConfigReachesSettings runs the component's keys through the
// environment, the way the Deployment's envFrom delivers them, and checks the
// reconcilers' settings come out as configured.
func TestComponentConfigReachesSettings(t *testing.T) {
	data := componentData(t)
	for key, value := range data {
		t.Setenv(key, value)
	}
	// Distinct from every default, so a value that reached settings by any
	// route but the environment would show.
	t.Setenv("PATCHY_INTENT_PLAN_MAX_TURNS", "7")
	t.Setenv("PATCHY_INTENT_BUILD_TIMEOUT", "61m")

	opts, root, _ := command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	s, err := settings(opts, "patchy", "patchy-agents")
	if err != nil {
		t.Fatalf("settings refuses the component's configuration: %v", err)
	}
	want := intent.Settings{
		Namespace:            "patchy",
		AgentNamespace:       "patchy-agents",
		PollInterval:         time.Minute,
		ApprovalPollInterval: 30 * time.Second,
		PRPollInterval:       time.Minute,
		RateLimitFloor:       1000,
		MaxAttempts:          intent.DefaultMaxAttempts,
		Plan:                 intent.StageCeiling{MaxTurns: 7, TokenBudget: 200000, Timeout: 20 * time.Minute},
		Build:                intent.StageCeiling{MaxTurns: 150, TokenBudget: 800000, Timeout: 61 * time.Minute},
		Revise:               intent.StageCeiling{MaxTurns: 80, TokenBudget: 400000, Timeout: 45 * time.Minute},
	}
	if s != want {
		t.Errorf("settings = %+v, want %+v", s, want)
	}
	if got := opts.Int("intent-max-concurrent-runs"); got != 1 {
		t.Errorf("intent-max-concurrent-runs = %d, want 1", got)
	}
	if got := opts.String("harnesses"); got != "claude" {
		t.Errorf("harnesses = %q, want claude", got)
	}
}

// TestSettingsRefusesADeadlineShorterThanAStage pins the startup check the
// chart and the component document: the Job deadline bounds every stage.
func TestSettingsRefusesADeadlineShorterThanAStage(t *testing.T) {
	opts, root, _ := command(t, "--intent-job-deadline", "59m")
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := settings(opts, "patchy", "patchy-agents"); err == nil {
		t.Fatal("settings accepted a 59m Job deadline under a 60m build timeout")
	}
}

// The intent controller cannot reach the broker under its NetworkPolicy.
// Agent pods can, so its startup must validate configuration without making
// even an advisory controller-side broker request.
func TestHarnessSkipsBrokerProbe(t *testing.T) {
	var calls atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	opts, root, _ := command(t, "--claude-agent-image=claude:1", "--broker-url="+broker.URL)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := harness(context.Background(), opts, fake.NewClientset(), "patchy-agents", runners)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude" {
		t.Errorf("harness = %q, want claude", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("intent controller made %d broker readiness requests, want none", n)
	}
}
