// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// These cases need the real API server: the fake client neither enforces
// the CRD schema (a condition message over 32768 bytes fails the whole
// status write with 422) nor skips a no-op update (a status write that
// changes nothing leaves the resourceVersion alone, so it raises no watch
// event and re-queues nothing).

// envReconciler boots envtest with the shipped CRDs and returns a
// runner-image reconciler over a real client, plus that client.
func envReconciler(t *testing.T, gh *fakeForgeClient) (*RepositoryReconciler, client.Client) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping repository envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "patchy"}},
		testForge("gh"),
		testRepository(),
	} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	store, err := artifact.NewStore(t.TempDir(), "http://arts.local")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := runnerimage.NewPolicy([]string{"ghcr.io/acme/"})
	if err != nil {
		t.Fatal(err)
	}
	r := &RepositoryReconciler{
		Client:    c,
		Forges:    forge.NewStore(c),
		Artifacts: store,
		ClientFor: func(context.Context, *forge.Resolved) (forgeClient, error) { return gh, nil },
		Images:    &RunnerImages{Policy: policy, Resolver: &fakeResolver{}, OnReject: OnRejectHandoff},
	}
	return r, c
}

func TestRunnerImageEnvtestLongRejectionRecorded(t *testing.T) {
	long := "ghcr.io/acme/" + strings.Repeat("a", 40000) + " x"
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{runnerimage.AgentYAMLPath: "image: '" + long + "'\n"})}
	r, c := envReconciler(t, gh)

	if _, err := reconcileErr(t, r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	repo := getRepo(t, c)
	stalled := meta.FindStatusCondition(repo.Status.Conditions, v1alpha1.ConditionStalled)
	if stalled == nil || stalled.Reason != v1alpha1.ReasonRunnerImageRejected {
		t.Fatalf("Stalled = %+v, want the rejection recorded", stalled)
	}
	if repo.Status.RunnerImage == nil || repo.Status.RunnerImage.Rejected != "InvalidReference" ||
		repo.Status.ResolvedSHA != "abc123" || repo.Status.Artifact == nil {
		t.Errorf("status = %+v, want the rejection, SHA and artifact persisted", repo.Status)
	}
}

func TestRunnerImageEnvtestUndeclaredSettles(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123",
		tarball: tarball(t, map[string]string{"README.md": "hi"})}
	r, c := envReconciler(t, gh)
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return clock }

	reconcile(t, r)
	first := getRepo(t, c)
	if !meta.IsStatusConditionTrue(first.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatalf("Ready = %+v, want True", first.Status.Conditions)
	}
	// The next pass comes more than a second later, as it does behind a
	// slow archive walk: its status write must still change nothing.
	clock = clock.Add(2 * time.Second)
	reconcile(t, r)
	if second := getRepo(t, c); second.ResourceVersion != first.ResourceVersion {
		t.Errorf("resourceVersion moved %s -> %s: the second pass wrote a change, whose watch event re-queues "+
			"the Repository", first.ResourceVersion, second.ResourceVersion)
	}
}
