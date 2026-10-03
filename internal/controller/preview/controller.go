// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Reconciler reads and writes Preview and Intent in the release namespace,
// and only Deployments, Services, Ingresses, Pods and ReplicaSets in slots.
// Client is deliberately uncached for slots and lease decisions. The chart's
// Roles, not this Go client, enforce the cross-namespace boundary.
type Reconciler struct {
	client.Client
	Settings Settings
	Now      func() time.Time
	Log      *slog.Logger
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) wait() ctrl.Result { return ctrl.Result{RequeueAfter: r.Settings.PollInterval} }

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Preview
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &p)
	}
	if !controllerutil.ContainsFinalizer(&p, finalizer) {
		controllerutil.AddFinalizer(&p, finalizer)
		return ctrl.Result{}, r.Update(ctx, &p)
	}
	return r.reconcilePresent(ctx, &p)
}

func (r *Reconciler) reconcilePresent(ctx context.Context, p *v1alpha1.Preview) (ctrl.Result, error) {
	if err := r.Settings.validatePreview(p); err != nil {
		return r.fail(ctx, p, err.Error())
	}
	var in v1alpha1.Intent
	err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.IntentRef.Name}, &in)
	if err != nil && !kerrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if kerrors.IsNotFound(err) || in.UID != p.Spec.IntentRef.UID || !v1alpha1.IntentWantsPreview(&in) {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, p))
	}
	var project v1alpha1.Project
	if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: in.Spec.Project}, &project); err != nil {
		if kerrors.IsNotFound(err) {
			return r.fail(ctx, p, "Project no longer exists")
		}
		return ctrl.Result{}, err
	}
	if !matchesApprovedPreview(p, &in, &project) {
		return r.fail(ctx, p, "Preview spec is not the operator Project and recorded PR head")
	}
	return r.reconcileActive(ctx, p)
}

// matchesApprovedPreview re-derives the Preview from the operator's Project
// and the Intent's recorded state through the same function the writer uses,
// and accepts the spec only if every component matches it, in order: name,
// image repository, revision, port, readiness path and route path. A forged
// or stale spec renders nothing.
func matchesApprovedPreview(p *v1alpha1.Preview, in *v1alpha1.Intent, project *v1alpha1.Project) bool {
	want, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok || len(want) != len(p.Spec.Components) {
		return false
	}
	for i, w := range want {
		got := p.Spec.Components[i]
		if got.Name != w.Name || got.ImageRepository != w.ImageRepository || got.Revision != w.Revision ||
			got.Port != w.Port || got.ReadinessPath != w.ReadinessPath ||
			v1alpha1.PreviewComponentPath(got) != v1alpha1.PreviewComponentPath(w) {
			return false
		}
	}
	return true
}

func (r *Reconciler) reconcileActive(ctx context.Context, p *v1alpha1.Preview) (ctrl.Result, error) {
	if p.Status.ObservedGeneration != p.Generation {
		if p.Status.Slot != nil {
			if err := r.deleteIngress(ctx, p, *p.Status.Slot); err != nil {
				return ctrl.Result{}, err
			}
		}
		p.Status.Phase = v1alpha1.PreviewPending
		p.Status.ObservedGeneration = p.Generation
		p.Status.ObservedRevision = p.Spec.Components[0].Revision
		p.Status.Retries = 0
		p.Status.URL = ""
		p.Status.Components = nil
		p.Status.Message = ""
		p.Status.LastDeployedAt = nil
		t := metav1.NewTime(r.now())
		p.Status.AttemptStartedAt = &t
		return ctrl.Result{}, r.Status().Update(ctx, p)
	}
	if p.Status.Phase == v1alpha1.PreviewFailed || p.Status.Phase == v1alpha1.PreviewExpired {
		if p.Status.Slot == nil {
			return ctrl.Result{}, nil
		}
		return r.release(ctx, p)
	}
	anchor := p.CreationTimestamp.Time
	if p.Status.AttemptStartedAt != nil && p.Status.AttemptStartedAt.After(anchor) {
		anchor = p.Status.AttemptStartedAt.Time
	}
	if p.Status.LastDeployedAt != nil {
		anchor = p.Status.LastDeployedAt.Time
	}
	if !r.now().Before(anchor.Add(p.Spec.TTL.Duration)) {
		p.Status.Phase = v1alpha1.PreviewExpired
		p.Status.Message = fmt.Sprintf("preview expired after %s without a new deployment", p.Spec.TTL.Duration)
		p.Status.URL = ""
		return ctrl.Result{}, r.Status().Update(ctx, p)
	}
	if p.Status.Slot == nil {
		slot, ok, err := r.chooseSlot(ctx, p)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			if p.Status.Phase != v1alpha1.PreviewQueued {
				p.Status.Phase = v1alpha1.PreviewQueued
				return ctrl.Result{}, r.Status().Update(ctx, p)
			}
			return r.wait(), nil
		}
		p.Status.Slot = &slot
		p.Status.Phase = v1alpha1.PreviewDeploying
		t := metav1.NewTime(r.now())
		p.Status.AttemptStartedAt = &t
		return ctrl.Result{}, r.Status().Update(ctx, p)
	}
	if *p.Status.Slot < 0 || int(*p.Status.Slot) >= r.Settings.SlotCount {
		return r.fail(ctx, p, "slot outside configured pool")
	}
	return r.deploy(ctx, p)
}

// chooseSlot makes a stable first-come queue. A slot is held by a Preview
// with status.slot even while it is deleting or cleaning up. One leader and
// MaxConcurrentReconciles=1 serialize the status writes; the uncached list
// sees the last assignment before a later request chooses.
func (r *Reconciler) chooseSlot(ctx context.Context, p *v1alpha1.Preview) (int32, bool, error) {
	var all v1alpha1.PreviewList
	if err := r.List(ctx, &all, client.InNamespace(r.Settings.Namespace)); err != nil {
		return 0, false, err
	}
	used := make([]bool, r.Settings.SlotCount)
	waiting := make([]v1alpha1.Preview, 0)
	for _, x := range all.Items {
		if x.Status.Slot != nil {
			if *x.Status.Slot >= 0 && int(*x.Status.Slot) < len(used) {
				used[*x.Status.Slot] = true
			}
			continue
		}
		if x.DeletionTimestamp.IsZero() && x.Status.Phase != v1alpha1.PreviewFailed &&
			x.Status.Phase != v1alpha1.PreviewExpired {
			waiting = append(waiting, x)
		}
	}
	slices.SortFunc(waiting, func(a, b v1alpha1.Preview) int {
		if n := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	free := make([]int32, 0)
	for i, occupied := range used {
		if !occupied {
			// An orphan from an interrupted finalizer still occupies the
			// slot until the sweep has actually removed it. Never overlap a
			// new preview with old code just because the CR disappeared.
			orphan, err := r.slotHasManagedObjects(ctx, int32(i))
			if err != nil {
				return 0, false, err
			}
			if orphan {
				continue
			}
			free = append(free, int32(i))
		}
	}
	for i, x := range waiting {
		if x.UID == p.UID && i < len(free) {
			return free[i], true, nil
		}
	}
	return 0, false, nil
}

func (r *Reconciler) slotHasManagedObjects(ctx context.Context, slot int32) (bool, error) {
	ns := r.Settings.slotName(slot)
	selector := client.MatchingLabels{labelManagedBy: managedBy}
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	if len(deployments.Items) > 0 {
		return true, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	if len(pods.Items) > 0 {
		return true, nil
	}
	var replicas appsv1.ReplicaSetList
	if err := r.List(ctx, &replicas, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	if len(replicas.Items) > 0 {
		return true, nil
	}
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	if len(services.Items) > 0 {
		return true, nil
	}
	var ingresses networkingv1.IngressList
	if err := r.List(ctx, &ingresses, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	return len(ingresses.Items) > 0, nil
}

// deploy renders every component into the slot: it prunes the objects of
// components no longer rendered, ensures each component's Service and
// Deployment (a Deployment whose spec did not change is not touched, so it is
// not rolled), and exposes the host only once every component is Ready.
func (r *Reconciler) deploy(ctx context.Context, p *v1alpha1.Preview) (ctrl.Result, error) {
	slot := *p.Status.Slot
	if err := r.prune(ctx, p, slot); err != nil {
		return ctrl.Result{}, err
	}
	for i := range p.Spec.Components {
		if err := r.ensureService(ctx, p, i, slot); err != nil {
			if errors.Is(err, errDeleting) {
				return r.wait(), nil
			}
			return r.retryError(ctx, p, err)
		}
	}
	for i := range p.Spec.Components {
		if p.Status.Retries > 0 {
			// A retry deleted the component's Deployment; its old Pods must
			// be gone before a new one starts beside them.
			var dep appsv1.Deployment
			key := types.NamespacedName{Namespace: r.Settings.slotName(slot), Name: componentName(p, i)}
			if err := r.Get(ctx, key, &dep); kerrors.IsNotFound(err) {
				children, err := r.childrenRemain(ctx, p, i, slot)
				if err != nil {
					return ctrl.Result{}, err
				}
				if children {
					return r.wait(), nil
				}
			} else if err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.ensureDeployment(ctx, p, i, slot); err != nil {
			if errors.Is(err, errDeleting) {
				return r.wait(), nil
			}
			return r.retryError(ctx, p, err)
		}
	}
	return r.completeDeployment(ctx, p, slot)
}

func (r *Reconciler) completeDeployment(ctx context.Context, p *v1alpha1.Preview, slot int32) (ctrl.Result, error) {
	components := make([]v1alpha1.PreviewComponentStatus, 0, len(p.Spec.Components))
	for i, c := range p.Spec.Components {
		imageID, ready, err := r.readyImage(ctx, p, i, slot)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			if p.Status.AttemptStartedAt != nil &&
				r.now().Sub(p.Status.AttemptStartedAt.Time) >= r.Settings.RolloutTimeout {
				return r.retryError(ctx, p, fmt.Errorf("component %s did not become Ready within %s", c.Name,
					r.Settings.RolloutTimeout))
			}
			return r.wait(), nil
		}
		components = append(components, v1alpha1.PreviewComponentStatus{
			Name: c.Name, Revision: c.Revision, ImageID: imageID,
		})
	}
	if err := r.ensureIngress(ctx, p, slot); err != nil {
		if errors.Is(err, errDeleting) {
			return r.wait(), nil
		}
		return r.retryError(ctx, p, err)
	}
	url := "https://" + r.Settings.host(p)
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.URL != url ||
		!slices.Equal(p.Status.Components, components) {
		p.Status.Phase = v1alpha1.PreviewReady
		p.Status.URL = url
		p.Status.Message = ""
		p.Status.Components = components
		if p.Status.LastDeployedAt == nil {
			t := metav1.NewTime(r.now())
			p.Status.LastDeployedAt = &t
		}
		return ctrl.Result{}, r.Status().Update(ctx, p)
	}
	return r.wait(), nil
}

// childrenRemain reports whether component i still has Pods or ReplicaSets.
func (r *Reconciler) childrenRemain(ctx context.Context, p *v1alpha1.Preview, i int, slot int32) (bool, error) {
	ns := r.Settings.slotName(slot)
	selector := client.MatchingLabels(componentLabels(p, i))
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	var replicas appsv1.ReplicaSetList
	if err := r.List(ctx, &replicas, client.InNamespace(ns), selector); err != nil {
		return false, err
	}
	return len(pods.Items) > 0 || len(replicas.Items) > 0, nil
}

func (r *Reconciler) retryError(ctx context.Context, p *v1alpha1.Preview, cause error) (ctrl.Result, error) {
	if r.Log != nil {
		r.Log.LogAttrs(ctx, slog.LevelWarn, "preview deploy attempt failed",
			slog.String("preview", p.Name), slog.Any("error", cause))
	}
	if p.Status.Slot != nil {
		if err := r.deleteIngress(ctx, p, *p.Status.Slot); err != nil {
			return ctrl.Result{}, err
		}
		// Every component restarts: a retry deletes each one's Deployment.
		for i := range p.Spec.Components {
			var dep appsv1.Deployment
			key := types.NamespacedName{Namespace: r.Settings.slotName(*p.Status.Slot), Name: componentName(p, i)}
			if err := r.Get(ctx, key, &dep); err == nil {
				if !ownedBy(&dep, p) {
					return r.fail(ctx, p, "deployment name is held by a foreign object")
				}
				if err := r.Delete(ctx, &dep); err != nil &&
					!kerrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			} else if !kerrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	p.Status.Retries++
	p.Status.Message = cause.Error()
	if p.Status.Retries >= r.Settings.MaxRetries {
		p.Status.Phase = v1alpha1.PreviewFailed
		p.Status.URL = ""
	} else {
		p.Status.Phase = v1alpha1.PreviewDeploying
		t := metav1.NewTime(r.now())
		p.Status.AttemptStartedAt = &t
	}
	return ctrl.Result{}, r.Status().Update(ctx, p)
}

func (r *Reconciler) fail(ctx context.Context, p *v1alpha1.Preview, reason string) (ctrl.Result, error) {
	if p.Status.Slot != nil && *p.Status.Slot >= 0 && int(*p.Status.Slot) < r.Settings.SlotCount {
		if err := r.deleteIngress(ctx, p, *p.Status.Slot); err != nil {
			return ctrl.Result{}, err
		}
	}
	if p.Status.Phase == v1alpha1.PreviewFailed && p.Status.Message == reason && p.Status.URL == "" {
		if p.Status.Slot != nil {
			return r.release(ctx, p)
		}
		return ctrl.Result{}, nil
	}
	p.Status.Phase = v1alpha1.PreviewFailed
	p.Status.Message = reason
	p.Status.URL = ""
	return ctrl.Result{}, r.Status().Update(ctx, p)
}

func (r *Reconciler) release(ctx context.Context, p *v1alpha1.Preview) (ctrl.Result, error) {
	if p.Status.Slot == nil {
		return ctrl.Result{}, nil
	}
	if *p.Status.Slot < 0 || int(*p.Status.Slot) >= r.Settings.SlotCount {
		return ctrl.Result{}, fmt.Errorf("preview %s holds retired slot %d; restore slotCount before cleanup",
			p.Name, *p.Status.Slot)
	}
	done, err := r.cleanup(ctx, p)
	if err != nil || !done {
		return r.wait(), err
	}
	p.Status.Slot = nil
	return ctrl.Result{}, r.Status().Update(ctx, p)
}

func (r *Reconciler) finalize(ctx context.Context, p *v1alpha1.Preview) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(p, finalizer) {
		return ctrl.Result{}, nil
	}
	if p.Status.Slot != nil && (*p.Status.Slot < 0 || int(*p.Status.Slot) >= r.Settings.SlotCount) {
		return ctrl.Result{}, fmt.Errorf("preview %s holds retired slot %d; restore slotCount before finalizer cleanup",
			p.Name, *p.Status.Slot)
	}
	done, err := r.cleanup(ctx, p)
	if err != nil || !done {
		return r.wait(), err
	}
	controllerutil.RemoveFinalizer(p, finalizer)
	return ctrl.Result{}, r.Update(ctx, p)
}

// SetupWithManager serializes slot leases under one elected leader. A
// separate periodic sweep runs even when there are zero Preview events.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Preview{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("preview").Complete(r)
}
