// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// faultPlan fails the nth API call the code under test makes (counting only
// while armed, so the test's own kubelet and garbage-collector moves never
// count) with a transient server error.
type faultPlan struct {
	armed bool
	calls int
	failN int // 0 never fails
	fired bool
}

const injected = "injected transient failure"

func (f *faultPlan) hit(verb string) error {
	if !f.armed {
		return nil
	}
	f.calls++
	if f.calls == f.failN {
		f.fired = true
		return kerrors.NewInternalError(fmt.Errorf("%s on call %d (%s)", injected, f.calls, verb))
	}
	return nil
}

func (f *faultPlan) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := f.hit("get"); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := f.hit("list"); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := f.hit("create"); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := f.hit("update"); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if err := f.hit("patch"); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := f.hit("delete"); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := f.hit("status-update"); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

// playKubelet plays the Deployment controller and kubelet for every rendered
// component that has no Pod yet, and the garbage collector for Pods whose
// Deployment is gone.
func (e *testEnv) playKubelet(p *v1alpha1.Preview) {
	e.t.Helper()
	e.collectGarbage()
	for i := range p.Spec.Components {
		var dep appsv1.Deployment
		if err := e.slotObject(componentName(p, i), &dep); err != nil {
			continue
		}
		var pod corev1.Pod
		if err := e.slotObject(dep.Name+"-pod", &pod); err == nil {
			continue
		}
		e.markReady(p, i)
	}
}

// reconcileOnce runs one reconcile with the plan armed and reports whether
// the injected failure (if it fired in this pass) was surfaced: returned as
// the error, or recorded in the Preview's status message as a spent retry.
func (e *testEnv) reconcileOnce(f *faultPlan, name string) (surfaced, firedNow bool) {
	e.t.Helper()
	before := f.fired
	f.armed = true
	_, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "patchy", Name: name}})
	f.armed = false
	firedNow = f.fired && !before
	if err != nil && !strings.Contains(err.Error(), injected) {
		e.t.Fatalf("reconcile %s: unexpected error %v", name, err)
	}
	if !firedNow {
		if err != nil {
			e.t.Fatalf("reconcile %s returned %v with no fault fired", name, err)
		}
		return true, false
	}
	if err != nil {
		return true, true
	}
	var p v1alpha1.Preview
	if gerr := e.c.Get(context.Background(), types.NamespacedName{Namespace: "patchy", Name: name}, &p); gerr == nil &&
		strings.Contains(p.Status.Message, injected) {
		return true, true
	}
	return false, true
}

// lifecycle drives one Preview from creation to Ready and then through
// deletion until its finalizer is gone, playing the cluster in between. It
// returns how many armed API calls the controller made.
func lifecycle(t *testing.T, f *faultPlan) int {
	t.Helper()
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newTestEnvWith(t, f.funcs(), in, p)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "patchy", Name: p.Name}

	check := func(stage string) {
		surfaced, fired := e.reconcileOnce(f, p.Name)
		if fired && !surfaced {
			t.Fatalf("%s: injected failure on call %d was neither returned nor recorded", stage, f.failN)
		}
	}

	ready := false
	for range 20 {
		check("deploy")
		var cur v1alpha1.Preview
		if err := e.c.Get(ctx, key, &cur); err != nil {
			t.Fatal(err)
		}
		if cur.Status.Phase == v1alpha1.PreviewFailed {
			t.Fatalf("one transient failure failed the Preview: %+v", cur.Status)
		}
		if cur.Status.Phase == v1alpha1.PreviewReady {
			ready = true
			if cur.Status.Retries > 1 {
				t.Errorf("retries = %d after one transient failure", cur.Status.Retries)
			}
			break
		}
		if cur.Status.Slot != nil {
			e.playKubelet(&cur)
		}
	}
	if !ready {
		t.Fatalf("preview never became Ready with fault on call %d", f.failN)
	}

	var cur v1alpha1.Preview
	if err := e.c.Get(ctx, key, &cur); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	gone := false
	for range 20 {
		check("finalize")
		e.collectGarbage()
		if err := e.c.Get(ctx, key, &cur); kerrors.IsNotFound(err) {
			gone = true
			break
		}
	}
	if !gone {
		t.Fatalf("preview never finalized with fault on call %d", f.failN)
	}
	for _, list := range []client.ObjectList{&appsv1.DeploymentList{}, &corev1.ServiceList{}, &corev1.PodList{}} {
		if err := e.c.List(ctx, list, client.InNamespace("patchy-preview-0")); err != nil {
			t.Fatal(err)
		}
		if n := len(listItems(list)); n != 0 {
			t.Errorf("%T holds %d objects after finalize", list, n)
		}
	}
	return f.calls
}

func listItems(list client.ObjectList) []any {
	switch l := list.(type) {
	case *appsv1.DeploymentList:
		out := make([]any, len(l.Items))
		for i := range l.Items {
			out[i] = l.Items[i]
		}
		return out
	case *corev1.ServiceList:
		out := make([]any, len(l.Items))
		for i := range l.Items {
			out[i] = l.Items[i]
		}
		return out
	case *corev1.PodList:
		out := make([]any, len(l.Items))
		for i := range l.Items {
			out[i] = l.Items[i]
		}
		return out
	}
	return nil
}

// TestEveryTransientAPIFailureIsSurfacedAndRecovered fails each API call of a
// full Preview lifecycle in turn, once. Every failure must reach the caller
// (as the reconcile's error, or as the reason a retry was spent), never fail
// the Preview, and the controller must still bring it to Ready and then
// finalize it with nothing left in the slot.
func TestEveryTransientAPIFailureIsSurfacedAndRecovered(t *testing.T) {
	clean := &faultPlan{}
	total := lifecycle(t, clean)
	if total < 10 {
		t.Fatalf("lifecycle made only %d API calls", total)
	}
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call-%d", n), func(t *testing.T) {
			f := &faultPlan{failN: n}
			lifecycle(t, f)
			if !f.fired {
				t.Fatalf("fault on call %d never fired (lifecycle took %d calls)", n, f.calls)
			}
		})
	}
}

// TestFinalizeWithoutOwnFinalizerDoesNothing: a deleting Preview that no
// longer carries this controller's finalizer is left alone.
func TestFinalizeWithoutOwnFinalizerDoesNothing(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	p.Finalizers = []string{"example.com/other"}
	e := newTestEnv(t, in, p)
	ctx := context.Background()
	if err := e.c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	var got v1alpha1.Preview
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != "example.com/other" {
		t.Errorf("finalizers = %v, want the foreign one untouched", got.Finalizers)
	}
}

// TestMissingPreviewIsNotAnError: a request for a Preview already gone ends
// quietly.
func TestMissingPreviewIsNotAnError(t *testing.T) {
	e := newTestEnv(t)
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "patchy", Name: "nope"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
}

// TestMissingProjectFailsThePreview: a Preview whose Intent's Project is gone
// fails with a reason naming it.
func TestMissingProjectFailsThePreview(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	in.Spec.Project = "gone"
	e := newTestEnv(t, in, p)
	got := e.step(p.Name) // adds the finalizer
	got = e.step(got.Name)
	if got.Status.Phase != v1alpha1.PreviewFailed || got.Status.Message != "Project no longer exists" {
		t.Fatalf("status = %+v, want Failed / Project no longer exists", got.Status)
	}
	// Failed without a slot is terminal: another pass changes nothing.
	again := e.step(got.Name)
	if again.Status.Phase != v1alpha1.PreviewFailed || again.ResourceVersion != got.ResourceVersion {
		t.Errorf("second pass rewrote a settled Failed preview: %+v", again.Status)
	}
}

// TestSlotOutsidePoolFails: a Preview holding a slot beyond the configured
// pool fails rather than rendering into a namespace the chart never made, and
// its retired slot blocks release until the pool is restored.
func TestSlotOutsidePoolFails(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	p.Finalizers = []string{finalizer}
	e := newTestEnv(t, in, p)
	ctx := context.Background()
	var cur v1alpha1.Preview
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &cur); err != nil {
		t.Fatal(err)
	}
	slot := int32(7)
	cur.Status.Slot = &slot
	cur.Status.ObservedGeneration = cur.Generation
	cur.Status.Phase = v1alpha1.PreviewDeploying
	if err := e.c.Status().Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	got := e.step(p.Name)
	if got.Status.Phase != v1alpha1.PreviewFailed || got.Status.Message != "slot outside configured pool" {
		t.Fatalf("status = %+v", got.Status)
	}
	_, err := e.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err == nil || !strings.Contains(err.Error(), "retired slot 7") {
		t.Fatalf("release of a retired slot = %v, want a retired-slot error", err)
	}
}

// TestForeignDeploymentFailsARetry: a retry that finds a foreign object on
// the component's Deployment name fails the Preview instead of deleting it.
func TestForeignDeploymentFailsARetry(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	e := newTestEnv(t, in, p)
	ctx := context.Background()
	cur := e.untilSlot(p.Name)
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy-preview-0",
		Name: componentName(cur, 0), Labels: map[string]string{"app": "squatter"}}}
	if err := e.c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.retryError(ctx, cur, errors.New("rollout stuck"))
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("retryError = %+v, %v", res, err)
	}
	got := e.step(p.Name)
	if got.Status.Phase != v1alpha1.PreviewFailed ||
		got.Status.Message != "deployment name is held by a foreign object" {
		t.Fatalf("status = %+v", got.Status)
	}
	var still appsv1.Deployment
	if err := e.slotObject(foreign.Name, &still); err != nil {
		t.Errorf("foreign deployment was deleted: %v", err)
	}
}

// TestSweeperStartSweepsAndStops: Start sweeps at once, logs a failed sweep
// rather than stopping, and returns when its context ends.
func TestSweeperStartSweepsAndStops(t *testing.T) {
	_, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	orphan := testSettings().deployment(p, 0, 0)
	e := newTestEnv(t, orphan)
	s := &Sweeper{Client: e.c, Settings: testSettings()}
	if !s.NeedLeaderElection() {
		t.Error("sweeper must run under leader election")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start = %v", err)
	}
	var dep appsv1.Deployment
	if err := e.slotObject(orphan.Name, &dep); !kerrors.IsNotFound(err) {
		t.Fatalf("orphan after Start = %v, want deleted", err)
	}

	var logs strings.Builder
	failing := newTestEnvWith(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver down")
		},
	})
	s = &Sweeper{Client: failing.c, Settings: testSettings(),
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start with a failing sweep = %v, want nil", err)
	}
	if out := logs.String(); !strings.Contains(out, "preview orphan sweep failed") ||
		!strings.Contains(out, "apiserver down") {
		t.Errorf("log = %q, want the sweep failure", logs.String())
	}
}

// TestSweepFaultsAreReturned: every List and Delete the sweep makes returns
// its failure, and a later clean sweep still removes every orphan.
func TestSweepFaultsAreReturned(t *testing.T) {
	_, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	s0 := testSettings()
	orphans := func() []client.Object {
		return []client.Object{s0.deployment(p, 0, 0), s0.service(p, 0, 0), s0.ingress(p, 0)}
	}
	count := &faultPlan{}
	e := newTestEnvWith(t, count.funcs(), orphans()...)
	count.armed = true
	if err := (&Sweeper{Client: e.c, Settings: s0}).SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	total := count.calls
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call-%d", n), func(t *testing.T) {
			f := &faultPlan{failN: n, armed: true}
			e := newTestEnvWith(t, f.funcs(), orphans()...)
			s := &Sweeper{Client: e.c, Settings: s0}
			err := s.SweepOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), injected) {
				t.Fatalf("SweepOnce = %v, want the injected failure", err)
			}
			f.failN = 0
			if err := s.SweepOnce(context.Background()); err != nil {
				t.Fatalf("clean sweep = %v", err)
			}
			for _, o := range orphans() {
				if err := e.slotObject(o.GetName(), o); !kerrors.IsNotFound(err) {
					t.Errorf("%T %s after clean sweep: %v", o, o.GetName(), err)
				}
			}
		})
	}
}

// TestSweepLeavesForeignAndLiveObjects: the sweep never deletes an object
// whose name the controller does not render, nor one a live Preview holds in
// that slot.
func TestSweepLeavesForeignAndLiveObjects(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	s0 := testSettings()
	squatter := s0.deployment(p, 0, 0)
	squatter.Name = "not-a-preview"
	live := s0.service(p, 0, 0)
	e := newTestEnv(t, in, p)
	ctx := context.Background()
	cur := e.untilSlot(p.Name)
	if *cur.Status.Slot != 0 {
		t.Fatalf("slot = %d", *cur.Status.Slot)
	}
	for _, o := range []client.Object{squatter, live} {
		if err := e.c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&Sweeper{Client: e.c, Settings: s0}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := e.slotObject(squatter.Name, &dep); err != nil {
		t.Errorf("foreign-named deployment deleted: %v", err)
	}
	var svc corev1.Service
	if err := e.slotObject(live.Name, &svc); err != nil {
		t.Errorf("live preview's service deleted: %v", err)
	}
}

func TestObjectKind(t *testing.T) {
	cases := []struct {
		obj  client.Object
		want string
	}{
		{&appsv1.Deployment{}, "Deployment"},
		{&corev1.Service{}, "Service"},
		{&corev1.Pod{}, "unknown"},
	}
	for _, c := range cases {
		if got := objectKind(c.obj); got != c.want {
			t.Errorf("objectKind(%T) = %q, want %q", c.obj, got, c.want)
		}
	}
}
