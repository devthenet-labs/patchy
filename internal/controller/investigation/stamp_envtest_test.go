// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

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
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// envClient boots envtest with the shipped CRDs and returns a real client
// with the patchy namespace created. The fake client enforces none of the
// CRD schema, so a status write the API server refuses passes there.
func envClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping investigation envtest")
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

// TestEnvtestOversizedFailureDetailReachesTheRetry: a failure detail past
// the CRD's 32768-byte cap on a condition message still stamps the child
// Failed, releases the finding, and hands the retry the real failure.
// Before, the API server refused every status write that carried it, the
// run stayed Running until its Job's TTL took the pod, and the retry was
// told "agent job vanished before reporting" instead.
func TestEnvtestOversizedFailureDetailReachesTheRetry(t *testing.T) {
	c := envClient(t)
	ctx := t.Context()

	objs := investigationFixture()
	fnd, inv := objs[0].(*v1alpha1.Finding), objs[1].(*v1alpha1.Investigation)
	createWithStatus(t, c, fnd)
	// The API server mints the Finding's UID; the child must carry it.
	inv.Spec.FindingRef.UID = fnd.UID
	for _, obj := range []client.Object{inv, readyRepo(), testForge()} {
		createWithStatus(t, c, obj)
	}

	var stderr strings.Builder
	for i := 0; stderr.Len() <= 40<<10; i++ {
		fmt.Fprintf(&stderr, "claude: tool call %05d failed: permission denied reading vendor/module/file.go\n", i)
	}
	detail := "claude exited 1: " + stderr.String()
	runner := &fakeRunner{done: true, events: []envelope.Event{{
		V: envelope.Version, Type: envelope.TypeInvestigation, Finding: fndName,
		Investigation: &envelope.Investigation{Stage: envelope.Stage{
			Outcome: envelope.OutcomeRuntimeError, Detail: detail, Harness: "claude",
		}},
	}}}
	rec := &InvestigationReconciler{
		Client: c, Runner: runner, Namespace: "patchy", MaxConcurrent: 2, MaxAttempts: 2,
		ConfidenceThreshold: 0.75, Now: func() time.Time { return clock },
	}
	applyOnce(t, rec)

	var inv1 v1alpha1.Investigation
	if err := c.Get(ctx, client.ObjectKeyFromObject(inv), &inv1); err != nil {
		t.Fatal(err)
	}
	if inv1.Status.Phase != v1alpha1.RunFailed || inv1.Status.Stage == nil ||
		inv1.Status.Stage.Outcome != "runtime_error" {
		t.Fatalf("attempt 1 = %s %+v, want Failed with runtime_error", inv1.Status.Phase, inv1.Status.Stage)
	}
	if cond := meta.FindStatusCondition(inv1.Status.Conditions, v1alpha1.ConditionComplete); cond == nil ||
		!strings.HasPrefix(cond.Message, "claude exited 1: claude: tool call 00000 failed") {
		t.Errorf("Complete condition = %+v, want the head of the failure", cond)
	}
	if got := getF(t, c).Status.Phase; got != v1alpha1.PhaseEnhanced {
		t.Fatalf("phase = %q after attempt 1, want Enhanced for the retry", got)
	}

	gate := &GateReconciler{
		Client: c, Forges: forge.NewStore(c), Namespace: "patchy", MinAge: time.Hour,
		Now: func() time.Time { return clock },
	}
	gateOnce(t, gate)
	var inv2 v1alpha1.Investigation
	if err := c.Get(ctx, types.NamespacedName{Namespace: "patchy", Name: fndName + "-inv-2"}, &inv2); err != nil {
		t.Fatalf("attempt 2 not opened: %v", err)
	}
	p := inv2.Spec.PreviousAttempt
	if p == nil || p.Outcome != "runtime_error" ||
		!strings.HasPrefix(p.Detail, "claude exited 1: claude: tool call 00000 failed") {
		t.Errorf("attempt 2 previousAttempt = %.120v, want attempt 1's runtime_error and its stderr", p)
	}
}
