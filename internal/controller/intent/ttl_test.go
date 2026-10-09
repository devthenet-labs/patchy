// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
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

// ttlIntent is an Intent in phase, completed at completed (none when zero).
func ttlIntent(phase v1alpha1.IntentPhase, completed time.Time) *v1alpha1.Intent {
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS, UID: "intent-uid"},
		Spec:       v1alpha1.IntentSpec{Project: "target"},
		Status:     v1alpha1.IntentStatus{Phase: phase},
	}
	if !completed.IsZero() {
		at := metav1.NewTime(completed)
		in.Status.CompletedAt = &at
	}
	return in
}

// TestTTLReconcile: an ended Intent is deleted its TTL after completion and
// not before; one never ending, never completed, or with no TTL is kept and
// not requeued; a conflict on the delete is decided again shortly, and any
// other delete failure is returned.
func TestTTLReconcile(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	errDelete := errors.New("delete refused")
	gr := schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "intents"}
	cases := []struct {
		name      string
		in        *v1alpha1.Intent // nil: no Intent
		ttl       time.Duration
		deleteErr error
		wantAfter time.Duration
		wantErr   error
		wantGone  bool
	}{
		{name: "no intent", ttl: time.Hour},
		{name: "active intent kept", in: ttlIntent(v1alpha1.IntentBuilding, now.Add(-48*time.Hour)), ttl: time.Hour},
		{name: "ended without completedAt kept", in: ttlIntent(v1alpha1.IntentMerged, time.Time{}), ttl: time.Hour},
		{name: "no TTL keeps forever", in: ttlIntent(v1alpha1.IntentMerged, now.Add(-48*time.Hour))},
		{name: "not yet expired requeues", in: ttlIntent(v1alpha1.IntentClosed, now.Add(-20*time.Minute)),
			ttl: time.Hour, wantAfter: 40 * time.Minute},
		{name: "expired is deleted", in: ttlIntent(v1alpha1.IntentFailed, now.Add(-2*time.Hour)),
			ttl: time.Hour, wantGone: true},
		{name: "expired exactly now is deleted", in: ttlIntent(v1alpha1.IntentMerged, now.Add(-time.Hour)),
			ttl: time.Hour, wantGone: true},
		{name: "conflict decides again", in: ttlIntent(v1alpha1.IntentMerged, now.Add(-2*time.Hour)),
			ttl: time.Hour, deleteErr: kerrors.NewConflict(gr, "target-1", errors.New("changed")),
			wantAfter: time.Second},
		{name: "gone already is done", in: ttlIntent(v1alpha1.IntentMerged, now.Add(-2*time.Hour)),
			ttl: time.Hour, deleteErr: kerrors.NewNotFound(gr, "target-1")},
		{name: "other delete failure returned", in: ttlIntent(v1alpha1.IntentMerged, now.Add(-2*time.Hour)),
			ttl: time.Hour, deleteErr: errDelete, wantErr: errDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithStatusSubresource(&v1alpha1.Intent{})
			if tc.in != nil {
				b = b.WithObjects(tc.in)
			}
			var deletes int
			var opts client.DeleteOptions
			c := b.WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
					o ...client.DeleteOption) error {
					deletes++
					opts.ApplyOptions(o)
					if tc.deleteErr != nil {
						return tc.deleteErr
					}
					return c.Delete(ctx, obj, o...)
				},
			}).Build()
			// No APIReader: the re-read goes through the client.
			r := &TTLReconciler{Client: c, TTL: tc.ttl, Now: func() time.Time { return now }}
			res, err := r.Reconcile(t.Context(), req("target-1"))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if res.RequeueAfter != tc.wantAfter {
				t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, tc.wantAfter)
			}
			var got v1alpha1.Intent
			getErr := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "target-1"}, &got)
			if gone := kerrors.IsNotFound(getErr); gone != tc.wantGone && tc.in != nil {
				t.Errorf("intent gone = %v, want %v", gone, tc.wantGone)
			}
			if deletes > 0 {
				if opts.Preconditions == nil || opts.Preconditions.UID == nil ||
					*opts.Preconditions.UID != tc.in.UID || opts.Preconditions.ResourceVersion == nil {
					t.Errorf("delete preconditions = %+v, want bound to the UID and resourceVersion",
						opts.Preconditions)
				}
				if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
					t.Errorf("delete propagation = %v, want Foreground", opts.PropagationPolicy)
				}
			}
			wantDelete := tc.wantGone || tc.deleteErr != nil
			if (deletes > 0) != wantDelete {
				t.Errorf("deletes = %d, want a delete %v", deletes, wantDelete)
			}
		})
	}
}

// TestTTLRereadsBeforeDelete: an Intent the cache shows expired but the API
// server shows revived (no longer ended) is not deleted, and one the API
// server no longer holds is done with.
func TestTTLRereadsBeforeDelete(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	expired := ttlIntent(v1alpha1.IntentFailed, now.Add(-2*time.Hour))

	revived := ttlIntent(v1alpha1.IntentPlanning, time.Time{})
	cache := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(expired.DeepCopy()).Build()
	api := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(revived).Build()
	r := &TTLReconciler{Client: cache, APIReader: api, TTL: time.Hour, Now: func() time.Time { return now }}
	res, err := r.Reconcile(t.Context(), req("target-1"))
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want nothing to do", res, err)
	}
	if err := cache.Get(t.Context(), client.ObjectKeyFromObject(expired), &v1alpha1.Intent{}); err != nil {
		t.Errorf("a revived intent was deleted: %v", err)
	}

	// The uncached read finds it already gone.
	empty := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	r.APIReader = empty
	if res, err := r.Reconcile(t.Context(), req("target-1")); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile over a gone intent = %+v, %v; want done", res, err)
	}
	if err := cache.Get(t.Context(), client.ObjectKeyFromObject(expired), &v1alpha1.Intent{}); err != nil {
		t.Errorf("an intent the API server no longer held was deleted through the cache: %v", err)
	}
}

// TestTTLWaitsForRoundNotices: an ended Intent whose round notices are not
// yet all delivered is kept, whatever its age.
func TestTTLWaitsForRoundNotices(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	in := ttlIntent(v1alpha1.IntentMerged, now.Add(-48*time.Hour))
	in.Status.Rounds = 2
	in.Status.RoundNoticesThrough = 1
	r := &TTLReconciler{TTL: time.Hour, Now: func() time.Time { return now }}
	if _, ok := r.wait(in); ok {
		t.Error("an intent with undelivered round notices expires")
	}
	in.Status.RoundNoticesThrough = 2
	if wait, ok := r.wait(in); !ok || wait != -47*time.Hour {
		t.Errorf("wait = %s, %v; want -47h, true", wait, ok)
	}
	deleting := in.DeepCopy()
	ts := metav1.NewTime(now)
	deleting.DeletionTimestamp = &ts
	if _, ok := r.wait(deleting); ok {
		t.Error("an intent being deleted expires again")
	}
}
