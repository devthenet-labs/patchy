// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// ownedSelector lists the objects that claim to be the Preview's; ownedBy
// then checks each one's name label too.
func ownedSelector(p *v1alpha1.Preview) client.MatchingLabels {
	return client.MatchingLabels{labelPreviewUID: string(p.UID), labelManagedBy: managedBy}
}

func (r *Reconciler) ensureService(ctx context.Context, p *v1alpha1.Preview, i int, slot int32) error {
	desired := r.Settings.service(p, i, slot)
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
		reflect.DeepEqual(live.Spec.Ports, desired.Spec.Ports) && live.Spec.Type == desired.Spec.Type &&
		maps.Equal(live.Labels, desired.Labels) && maps.Equal(live.Annotations, desired.Annotations) {
		return nil
	}
	old := live.DeepCopy()
	live.Spec.Selector = desired.Spec.Selector
	live.Spec.Ports = desired.Spec.Ports
	live.Spec.Type = desired.Spec.Type
	live.Labels = desired.Labels
	live.Annotations = desired.Annotations
	return r.Patch(ctx, &live, client.MergeFrom(old))
}

// ensureDeployment renders component i. A Deployment's selector is
// immutable, so one whose selector differs (a component that moved to or from
// being the only one) is deleted and created anew rather than patched.
func (r *Reconciler) ensureDeployment(ctx context.Context, p *v1alpha1.Preview, i int, slot int32) error {
	desired := r.Settings.deployment(p, i, slot)
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
	if !reflect.DeepEqual(live.Spec.Selector, desired.Spec.Selector) {
		if err := r.Delete(ctx, &live); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
		return errDeleting
	}
	if reflect.DeepEqual(live.Spec, desired.Spec) && maps.Equal(live.Labels, desired.Labels) {
		return nil
	}
	old := live.DeepCopy()
	live.Spec = desired.Spec
	live.Labels = desired.Labels
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

// prune deletes the Preview's Deployments and Services in the slot that no
// current component renders: a component the Project no longer previews, or
// one whose name moved. Each is found by the Preview's own labels, never by
// name alone.
func (r *Reconciler) prune(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	desired := make(map[string]bool, len(p.Spec.Components))
	for i := range p.Spec.Components {
		desired[componentName(p, i)] = true
	}
	ns := client.InNamespace(r.Settings.slotName(slot))
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, ns, ownedSelector(p)); err != nil {
		return err
	}
	var services corev1.ServiceList
	if err := r.List(ctx, &services, ns, ownedSelector(p)); err != nil {
		return err
	}
	stale := make([]client.Object, 0)
	for i := range deployments.Items {
		stale = append(stale, &deployments.Items[i])
	}
	for i := range services.Items {
		stale = append(stale, &services.Items[i])
	}
	for _, obj := range stale {
		if desired[obj.GetName()] || !ownedBy(obj, p) || !obj.GetDeletionTimestamp().IsZero() {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// readyImage reports component i's ready image ID: its Deployment has rolled
// out and a Pod running the component's exact image is Ready — and, when
// gated, its load balancer target is healthy too (targetHealthy).
func (r *Reconciler) readyImage(ctx context.Context, p *v1alpha1.Preview, i int, slot int32,
	gated bool) (string, bool, error) {
	c := p.Spec.Components[i]
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: r.Settings.slotName(slot), Name: componentName(p, i)}
	if err := r.Get(ctx, key, &dep); err != nil {
		return "", false, err
	}
	if dep.Status.ObservedGeneration < dep.Generation || dep.Status.AvailableReplicas < 1 {
		return "", false, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(key.Namespace),
		client.MatchingLabels(componentLabels(p, i))); err != nil {
		return "", false, err
	}
	for _, pod := range pods.Items {
		if !pod.DeletionTimestamp.IsZero() || len(pod.Spec.Containers) != 1 ||
			pod.Spec.Containers[0].Image != image(c) {
			continue
		}
		if !podCondition(&pod, corev1.PodReady) {
			continue
		}
		if gated && !targetHealthy(&pod) {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == c.Name && status.Ready && status.ImageID != "" {
				return status.ImageID, true, nil
			}
		}
	}
	return "", false, nil
}

// podCondition reports whether the Pod's condition of type t is True.
func podCondition(pod *corev1.Pod, t corev1.PodConditionType) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == t {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// targetHealthy reports whether the load balancer has found the Pod's target
// healthy: the Pod carries a readiness gate, which only the load balancer
// controller injects into a slot Pod, and every gate's condition is True. A
// Pod created before its target group binding existed has no gate, and so
// never counts.
func targetHealthy(pod *corev1.Pod) bool {
	if len(pod.Spec.ReadinessGates) == 0 {
		return false
	}
	for _, gate := range pod.Spec.ReadinessGates {
		if !podCondition(pod, gate.ConditionType) {
			return false
		}
	}
	return true
}

// ingressAdmitted reports whether the load balancer controller has reconciled
// the Preview's Ingress (it publishes the load balancer's address only after
// building its target group bindings), which a Pod must follow to have its
// target-health readiness gate injected.
func (r *Reconciler) ingressAdmitted(ctx context.Context, p *v1alpha1.Preview, slot int32) (bool, error) {
	var live networkingv1.Ingress
	key := types.NamespacedName{Namespace: r.Settings.slotName(slot), Name: resourceName(p)}
	if err := r.Get(ctx, key, &live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	for _, lb := range live.Status.LoadBalancer.Ingress {
		if lb.Hostname != "" || lb.IP != "" {
			return true, nil
		}
	}
	return false, nil
}

// cleanup never deletes a foreign object, even one squatting on a rendered
// name: it lists each kind by the Preview's own UID label, so it finds every
// component's objects whatever their names, an older controller's included.
// It scans all configured slots so a lost status.slot or interrupted
// assignment cannot leave a workload behind. Pod/ReplicaSet checks keep the
// slot held until background garbage collection has truly removed the code.
func (r *Reconciler) cleanup(ctx context.Context, p *v1alpha1.Preview) (bool, error) {
	remaining := false
	for slot := range r.Settings.SlotCount {
		ns := client.InNamespace(r.Settings.slotName(int32(slot)))
		var ingresses networkingv1.IngressList
		if err := r.List(ctx, &ingresses, ns, ownedSelector(p)); err != nil {
			return false, err
		}
		var services corev1.ServiceList
		if err := r.List(ctx, &services, ns, ownedSelector(p)); err != nil {
			return false, err
		}
		var deployments appsv1.DeploymentList
		if err := r.List(ctx, &deployments, ns, ownedSelector(p)); err != nil {
			return false, err
		}
		objects := make([]client.Object, 0, len(ingresses.Items)+len(services.Items)+len(deployments.Items))
		for i := range ingresses.Items {
			objects = append(objects, &ingresses.Items[i])
		}
		for i := range services.Items {
			objects = append(objects, &services.Items[i])
		}
		for i := range deployments.Items {
			objects = append(objects, &deployments.Items[i])
		}
		for _, obj := range objects {
			if !ownedBy(obj, p) {
				return false, fmt.Errorf("foreign %T %s/%s carries preview %s's UID", obj,
					obj.GetNamespace(), obj.GetName(), p.Name)
			}
			remaining = true
			if err := r.Delete(ctx, obj); err != nil && !kerrors.IsNotFound(err) {
				return false, err
			}
		}
		selector := client.MatchingLabels(labelsFor(p))
		var pods corev1.PodList
		if err := r.List(ctx, &pods, ns, selector); err != nil {
			return false, err
		}
		var replicas appsv1.ReplicaSetList
		if err := r.List(ctx, &replicas, ns, selector); err != nil {
			return false, err
		}
		if len(pods.Items) > 0 || len(replicas.Items) > 0 {
			remaining = true
		}
	}
	return !remaining, nil
}
