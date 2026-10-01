// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var errDeleting = errors.New("rendered object is still deleting")

func ownedBy(obj client.Object, p *v1alpha1.Preview) bool {
	l := obj.GetLabels()
	return l[labelPreview] == p.Name && l[labelPreviewUID] == string(p.UID) &&
		l[labelManagedBy] == managedBy
}

func (r *Reconciler) ensureService(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	desired := r.Settings.service(p, slot)
	var live corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &live)
	if kerrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ownedBy(&live, p) {
		return fmt.Errorf("service %s is foreign", desired.Name)
	}
	if !live.DeletionTimestamp.IsZero() {
		return errDeleting
	}
	if reflect.DeepEqual(live.Spec.Selector, desired.Spec.Selector) &&
		reflect.DeepEqual(live.Spec.Ports, desired.Spec.Ports) && live.Spec.Type == desired.Spec.Type {
		return nil
	}
	old := live.DeepCopy()
	live.Spec.Selector = desired.Spec.Selector
	live.Spec.Ports = desired.Spec.Ports
	live.Spec.Type = desired.Spec.Type
	return r.Patch(ctx, &live, client.MergeFrom(old))
}

func (r *Reconciler) ensureDeployment(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	desired := r.Settings.deployment(p, slot)
	var live appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &live)
	if kerrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ownedBy(&live, p) {
		return fmt.Errorf("deployment %s is foreign", desired.Name)
	}
	if !live.DeletionTimestamp.IsZero() {
		return errDeleting
	}
	if reflect.DeepEqual(live.Spec, desired.Spec) {
		return nil
	}
	old := live.DeepCopy()
	live.Spec = desired.Spec
	return r.Patch(ctx, &live, client.MergeFrom(old))
}

func (r *Reconciler) ensureIngress(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	desired := r.Settings.ingress(p, slot)
	var live networkingv1.Ingress
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &live)
	if kerrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ownedBy(&live, p) {
		return fmt.Errorf("ingress %s is foreign", desired.Name)
	}
	if !live.DeletionTimestamp.IsZero() {
		return errDeleting
	}
	if reflect.DeepEqual(live.Spec, desired.Spec) &&
		reflect.DeepEqual(live.Annotations, desired.Annotations) {
		return nil
	}
	old := live.DeepCopy()
	live.Spec = desired.Spec
	live.Annotations = desired.Annotations
	return r.Patch(ctx, &live, client.MergeFrom(old))
}

func (r *Reconciler) deleteIngress(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	var live networkingv1.Ingress
	key := types.NamespacedName{Namespace: r.Settings.slotName(slot), Name: resourceName(p)}
	if err := r.Get(ctx, key, &live); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedBy(&live, p) {
		return fmt.Errorf("ingress %s is foreign", key)
	}
	return client.IgnoreNotFound(r.Delete(ctx, &live))
}

func (r *Reconciler) readyImage(ctx context.Context, p *v1alpha1.Preview, slot int32) (string, bool, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: r.Settings.slotName(slot), Name: resourceName(p)}
	if err := r.Get(ctx, key, &dep); err != nil {
		return "", false, err
	}
	if dep.Status.ObservedGeneration < dep.Generation || dep.Status.AvailableReplicas < 1 {
		return "", false, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(key.Namespace), client.MatchingLabels(labelsFor(p))); err != nil {
		return "", false, err
	}
	for _, pod := range pods.Items {
		if !pod.DeletionTimestamp.IsZero() || len(pod.Spec.Containers) != 1 ||
			pod.Spec.Containers[0].Image != image(p.Spec.Components[0]) {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == p.Spec.Components[0].Name && status.Ready && status.ImageID != "" {
				return status.ImageID, true, nil
			}
		}
	}
	return "", false, nil
}

// cleanup never deletes a foreign object, even one squatting on a rendered
// name. It scans all configured slots so a lost status.slot or interrupted
// assignment cannot leave a workload behind. Pod/ReplicaSet checks keep the
// slot held until background garbage collection has truly removed the code.
func (r *Reconciler) cleanup(ctx context.Context, p *v1alpha1.Preview) (bool, error) {
	remaining := false
	for slot := range r.Settings.SlotCount {
		ns := r.Settings.slotName(int32(slot))
		key := types.NamespacedName{Namespace: ns, Name: resourceName(p)}
		for _, obj := range []client.Object{&networkingv1.Ingress{}, &corev1.Service{}, &appsv1.Deployment{}} {
			if err := r.Get(ctx, key, obj); kerrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return false, err
			}
			if !ownedBy(obj, p) {
				return false, fmt.Errorf("foreign %T holds %s", obj, key)
			}
			remaining = true
			if err := r.Delete(ctx, obj); err != nil &&
				!kerrors.IsNotFound(err) {
				return false, err
			}
		}
		selector := client.MatchingLabels(labelsFor(p))
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(ns), selector); err != nil {
			return false, err
		}
		var replicas appsv1.ReplicaSetList
		if err := r.List(ctx, &replicas, client.InNamespace(ns), selector); err != nil {
			return false, err
		}
		if len(pods.Items) > 0 || len(replicas.Items) > 0 {
			remaining = true
		}
	}
	return !remaining, nil
}
