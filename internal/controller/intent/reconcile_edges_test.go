// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestReconcileForgetsADeletedIntent: once an Intent is gone, the
// reconciler drops everything it remembered of it in memory, and a
// reconcile of it is no error.
func TestReconcileForgetsADeletedIntent(t *testing.T) {
	e := newEnv(t, testProject())
	now := e.clock.Now()
	e.intent.memo(func() {
		e.intent.polled["gone"] = now
		e.intent.prPolled["gone"] = now
		e.intent.blockedAt["gone"] = 3
		e.intent.siblings["gone"] = &siblingLinks{}
		e.intent.departed["gone"] = map[string]time.Time{appRepoURL: now}
		e.intent.previews["gone"] = &previewNotes{}
		e.intent.polled["kept"] = now
	})
	res, err := e.intent.Reconcile(context.Background(), req("gone"))
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile of a gone intent = %+v, %v; want nothing", res, err)
	}
	e.intent.memo(func() {
		for name, m := range map[string]bool{
			"polled": hasKey(e.intent.polled, "gone"), "prPolled": hasKey(e.intent.prPolled, "gone"),
			"blockedAt": hasKey(e.intent.blockedAt, "gone"), "siblings": hasKey(e.intent.siblings, "gone"),
			"departed": hasKey(e.intent.departed, "gone"), "previews": hasKey(e.intent.previews, "gone"),
		} {
			if m {
				t.Errorf("%s still remembers the deleted intent", name)
			}
		}
		if !hasKey(e.intent.polled, "kept") {
			t.Error("another intent's memory was dropped")
		}
	})
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

// TestReconcileLeavesSuspendedAndDeletingIntents: a suspended Intent, or one
// being deleted, is not written to nor touched on GitHub.
func TestReconcileLeavesSuspendedAndDeletingIntents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Intent)
	}{
		{"suspended", func(in *v1alpha1.Intent) { in.Spec.Suspend = true }},
		{"deleting", func(in *v1alpha1.Intent) {
			in.Finalizers = []string{"test/hold"}
			ts := metav1.NewTime(time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC))
			in.DeletionTimestamp = &ts
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &v1alpha1.Intent{
				ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS},
				Spec: v1alpha1.IntentSpec{Project: "target",
					Issue: v1alpha1.IntentIssue{Repository: intentRepoURL, Number: 1}},
			}
			tc.mutate(in)
			e := newEnv(t, testProject(), in)
			res, err := e.intent.Reconcile(context.Background(), req("target-1"))
			if err != nil || res.RequeueAfter != 0 {
				t.Fatalf("Reconcile = %+v, %v; want nothing", res, err)
			}
			if got := e.get("target-1"); got.Status.Phase != "" {
				t.Errorf("phase = %q, want unwritten", got.Status.Phase)
			}
			if e.writes != 0 {
				t.Errorf("%d status writes, want none", e.writes)
			}
		})
	}
}

// TestReconcileWaitsForItsProject: an Intent whose Project is gone waits a
// poll interval without writing, and a failed read of the Project is
// returned to be retried.
func TestReconcileWaitsForItsProject(t *testing.T) {
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS},
		Spec:       v1alpha1.IntentSpec{Project: "target"},
	}
	e := newEnv(t, in.DeepCopy())
	res, err := e.intent.Reconcile(context.Background(), req("target-1"))
	if err != nil || res.RequeueAfter != testSettings().PollInterval {
		t.Fatalf("Reconcile with no Project = %+v, %v; want a wait of %s", res, err, testSettings().PollInterval)
	}
	if got := e.get("target-1"); got.Status.Phase != "" {
		t.Errorf("phase = %q, want unwritten", got.Status.Phase)
	}

	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(in.DeepCopy(), testProject()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.Project); ok {
					return errTransient
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &IntentReconciler{Client: c, APIReader: c, GitHub: e.gh, Settings: testSettings(), Now: e.clock.Now}
	if _, err := r.Reconcile(context.Background(), req("target-1")); !errors.Is(err, errTransient) {
		t.Errorf("Reconcile with a failing Project read = %v, want %v", err, errTransient)
	}
	// A failing read of the Intent itself is returned too.
	failing := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errTransient
			},
		}).Build()
	r = &IntentReconciler{Client: failing, APIReader: failing, GitHub: e.gh, Settings: testSettings()}
	if _, err := r.Reconcile(context.Background(), req("target-1")); !errors.Is(err, errTransient) {
		t.Errorf("Reconcile with a failing Intent read = %v, want %v", err, errTransient)
	}
}

// TestReconcileStatusWriteFailures: a status write that meets a newer
// version ends the pass with a short requeue and no error; any other failed
// write is returned, naming the intent.
func TestReconcileStatusWriteFailures(t *testing.T) {
	gr := schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "intents"}
	e := newEnv(t, testProject())
	e.reconcileProject()
	name := e.newIntent(approver)

	e.failStatus = []error{kerrors.NewConflict(gr, name, errors.New("newer"))}
	res, err := e.intent.Reconcile(context.Background(), req(name))
	if err != nil || res.RequeueAfter != time.Second {
		t.Fatalf("Reconcile on a conflict = %+v, %v; want a 1s requeue", res, err)
	}
	if got := e.get(name); got.Status.Phase != "" {
		t.Errorf("phase = %q after a refused write, want unwritten", got.Status.Phase)
	}

	e.failStatus = []error{errTransient}
	_, err = e.intent.Reconcile(context.Background(), req(name))
	if !errors.Is(err, errTransient) || !strings.Contains(err.Error(), name) {
		t.Fatalf("Reconcile on a failed write = %v; want %v naming %s", err, errTransient, name)
	}

	// The next pass writes the first phase.
	e.mustIntent(name)
	if got := e.get(name); got.Status.Phase != v1alpha1.IntentPending {
		t.Errorf("phase = %q, want %s", got.Status.Phase, v1alpha1.IntentPending)
	}
}
