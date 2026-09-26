// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TTLReconciler deletes an Intent its TTL after completedAt. The delete is
// foreground: the Intent keeps its name until everything it owns is gone,
// its runs (held by the jobs finalizer until their Jobs are), their
// Repositories and ConfigMaps, so discovery never creates the issue's next
// Intent beside the remains of the last. Expiry is deletion, not a phase.
type TTLReconciler struct {
	client.Client
	// APIReader re-reads an Intent the cache shows expired before it is
	// deleted: the cache can still show a Failed intent an approver revived
	// a moment ago. nil reads through Client.
	APIReader client.Reader
	// TTL is the retention after completion; 0 keeps Intents forever.
	TTL time.Duration
	Now func() time.Time
	Log *slog.Logger
}

// wait is how long until in expires: zero or less means it has; ok is false
// when it never does (no TTL, not ended, or already being deleted).
func (r *TTLReconciler) wait(in *v1alpha1.Intent) (wait time.Duration, ok bool) {
	if r.TTL <= 0 || !in.DeletionTimestamp.IsZero() || !terminal(in.Status.Phase) || in.Status.CompletedAt == nil {
		return 0, false
	}
	if in.Status.RoundNoticesThrough < in.Status.Rounds {
		return 0, false // delivery's status write requeues the TTL reconciler
	}
	return in.Status.CompletedAt.Add(r.TTL).Sub(r.now()), true
}

func (r *TTLReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile enforces the TTL on one Intent. An Intent the cache shows
// expired is read again uncached and deleted only as that read shows it, the
// delete bound to its UID and resourceVersion, so an Intent revived just as
// its TTL fired is never deleted with its new run.
func (r *TTLReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var in v1alpha1.Intent
	if err := r.Get(ctx, req.NamespacedName, &in); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if wait, ok := r.wait(&in); !ok || wait > 0 {
		return ctrl.Result{RequeueAfter: max(wait, 0)}, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, req.NamespacedName, &in); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if wait, ok := r.wait(&in); !ok || wait > 0 {
		return ctrl.Result{RequeueAfter: max(wait, 0)}, nil
	}
	foreground := metav1.DeletePropagationForeground
	err := r.Delete(ctx, &in, &client.DeleteOptions{
		PropagationPolicy: &foreground,
		Preconditions:     &metav1.Preconditions{UID: &in.UID, ResourceVersion: &in.ResourceVersion},
	})
	switch {
	case kerrors.IsConflict(err):
		// Changed since the read: decide again from the next one.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case err != nil && !kerrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	if r.Log != nil {
		r.Log.LogAttrs(ctx, slog.LevelInfo, "intent expired", slog.String("intent", in.Name))
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the TTL loop.
func (r *TTLReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Intent{}).
		Named("intent-ttl").
		Complete(reconcile.Func(r.Reconcile))
}
