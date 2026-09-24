// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// envClient boots envtest with the shipped CRDs and returns a real client
// with the patchy namespace created. The fake client enforces none of the
// CRD schema, so a status write the API server refuses passes there.
func envClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping remediation envtest")
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
	if err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "patchy"}}); err != nil {
		t.Fatal(err)
	}
	return c
}

// createWithStatus creates obj and then writes the status it was built
// with, which Create drops.
func createWithStatus(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	status := obj.DeepCopyObject().(client.Object)
	if err := c.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	status.SetResourceVersion(obj.GetResourceVersion())
	status.SetUID(obj.GetUID())
	if err := c.Status().Update(t.Context(), status); err != nil {
		t.Fatalf("status %T %s: %v", obj, obj.GetName(), err)
	}
}

// TestEnvtestOversizedFailureDetailReachesTheRetry: a commit_failed detail
// past the CRD's 32768-byte cap on a condition message — the full
// `git status --porcelain` of a formatter run over a large tree — still
// stamps the child Failed and hands the retry the real failure. Before, the
// API server refused every status write that carried it, the run stayed
// Running until its Job's TTL took the pod, and the retry was told "agent
// job vanished before reporting" instead.
func TestEnvtestOversizedFailureDetailReachesTheRetry(t *testing.T) {
	c := envClient(t)
	ctx := t.Context()

	objs := runningRemediation()
	fnd, rem, inv, repo := objs[0].(*v1alpha1.Finding), objs[1].(*v1alpha1.Remediation),
		objs[2].(*v1alpha1.Investigation), objs[3].(*v1alpha1.Repository)
	createWithStatus(t, c, fnd)
	// The API server mints the Finding's UID; the children must carry it.
	rem.Spec.FindingRef.UID = fnd.UID
	inv.Spec.FindingRef.UID = fnd.UID
	for _, obj := range []client.Object{rem, inv, repo} {
		createWithStatus(t, c, obj)
	}

	var status strings.Builder
	for i := 0; status.Len() <= 40<<10; i++ {
		fmt.Fprintf(&status, " M src/generated/module_%05d.go\n", i)
	}
	detail := "working tree not clean after commit.sh:\n" + status.String()
	runner := &fakeCRRunner{done: true, events: []envelope.Event{{
		V: envelope.Version, Type: envelope.TypeRemediation, Finding: "finding-aa-1",
		Remediation: &envelope.Remediation{Stage: envelope.Stage{
			Outcome: envelope.OutcomeCommitFailed, Detail: detail, Harness: "claude",
		}},
	}}}
	rec := &RemediationReconciler{
		Client: c, Runner: runner, Forge: &fakeForge{}, Namespace: "patchy",
		MaxConcurrent: 1, MaxAttempts: 2, Enabled: []string{"claude"},
		Now: func() time.Time { return crdClock },
	}
	remOnce(t, rec, "finding-aa-1-rem-1")

	var rem1 v1alpha1.Remediation
	if err := c.Get(ctx, client.ObjectKeyFromObject(rem), &rem1); err != nil {
		t.Fatal(err)
	}
	if rem1.Status.Phase != v1alpha1.RunFailed || rem1.Status.Stage == nil ||
		rem1.Status.Stage.Outcome != "commit_failed" {
		t.Fatalf("attempt 1 = %s %+v, want Failed with commit_failed", rem1.Status.Phase, rem1.Status.Stage)
	}
	if cond := meta.FindStatusCondition(rem1.Status.Conditions, v1alpha1.ConditionComplete); cond == nil ||
		!strings.HasPrefix(cond.Message, "working tree not clean after commit.sh:\n M src/generated/") {
		t.Errorf("Complete condition = %+v, want the head of the git status", cond)
	}
	if got := findingNow(t, c).Status.Phase; got != v1alpha1.PhaseQueued {
		t.Fatalf("phase = %q after attempt 1, want Queued for the retry", got)
	}

	sp, _ := newSpawner(t)
	sp.Client = c
	spawnOnce(t, sp)
	var rem2 v1alpha1.Remediation
	if err := c.Get(ctx, types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-2"}, &rem2); err != nil {
		t.Fatalf("attempt 2 not spawned: %v", err)
	}
	p := rem2.Spec.PreviousAttempt
	if p == nil || p.Outcome != "commit_failed" ||
		!strings.HasPrefix(p.Detail, "working tree not clean after commit.sh:\n M src/generated/") {
		t.Errorf("attempt 2 previousAttempt = %.120v, want attempt 1's commit_failed and its git status", p)
	}
}
