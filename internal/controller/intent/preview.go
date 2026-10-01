// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var previewRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PreviewSourceReconciler is the sole writer of Preview spec. It copies only
// operator Project configuration and the PR head already verified and
// recorded by the Intent reconciler; it never reads issue or agent text.
// A separate reconciler prevents preview errors from blocking an Intent's
// merge, status comment, or Finding flow. Disabled unless the operator
// explicitly enables it alongside the preview-controller.
type PreviewSourceReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

const previewTTL = 72 * time.Hour

func (r *PreviewSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var in v1alpha1.Intent
	if err := r.APIReader.Get(ctx, req.NamespacedName, &in); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var project v1alpha1.Project
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: in.Spec.Project},
		&project); err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	desired, ok := desiredPreview(&in, &project)
	var existing v1alpha1.Preview
	err := r.APIReader.Get(ctx, req.NamespacedName, &existing)
	if err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil && existing.Spec.IntentRef.UID != in.UID {
		return ctrl.Result{}, fmt.Errorf("preview %s belongs to a different Intent UID", existing.Name)
	}
	if !ok {
		if err == nil && existing.DeletionTimestamp.IsZero() {
			uid := existing.UID
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &existing,
				&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if kerrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(&in, desired, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil && !kerrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !existing.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !reflect.DeepEqual(existing.Spec.Components, desired.Spec.Components) {
		existing.Spec.Components = desired.Spec.Components
		if err := r.Update(ctx, &existing); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func desiredPreview(in *v1alpha1.Intent, project *v1alpha1.Project) (*v1alpha1.Preview, bool) {
	if project.Spec.Preview == nil || len(project.Spec.Repositories) != 1 ||
		len(in.Status.PullRequests) != 1 ||
		(in.Status.Phase != v1alpha1.IntentInReview && in.Status.Phase != v1alpha1.IntentRevising &&
			in.Status.Phase != v1alpha1.IntentBlocked) {
		return nil, false
	}
	pr := in.Status.PullRequests[0]
	if pr.State != "open" || !sameRepo(pr.Repository, project.Spec.Repositories[0].URL) ||
		!previewRevision.MatchString(pr.HeadSHA) {
		return nil, false
	}
	p := project.Spec.Preview
	return &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID},
			HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{
				Name: project.Spec.Repositories[0].Name, ImageRepository: p.ImageRepository,
				Revision: pr.HeadSHA, Port: p.Port, ReadinessPath: p.ReadinessPath,
			}},
			TTL: metav1.Duration{Duration: previewTTL},
		},
	}, true
}

// SetupWithManager watches Intents; the minute poll also picks up Project
// configuration changes without requiring a second Project informer.
func (r *PreviewSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Intent{}).
		Named("intent-preview-source").
		Complete(r)
}
