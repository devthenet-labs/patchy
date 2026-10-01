// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"log/slog"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Sweeper runs even when there are no Preview events, so orphaned rendered
// objects left by a crash, old controller, or removed CR cannot keep a slot
// or ALB target alive indefinitely. It is leader-elected like Reconciler.
type Sweeper struct {
	client.Client
	Settings Settings
	Log      *slog.Logger
}

func (*Sweeper) NeedLeaderElection() bool { return true }

func (s *Sweeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Settings.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.SweepOnce(ctx); err != nil && s.Log != nil {
			s.Log.LogAttrs(ctx, slog.LevelWarn, "preview orphan sweep failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	var previews v1alpha1.PreviewList
	if err := s.List(ctx, &previews, client.InNamespace(s.Settings.Namespace)); err != nil {
		return err
	}
	live := make(map[string]int32, len(previews.Items))
	for _, p := range previews.Items {
		if p.Status.Slot != nil {
			live[string(p.UID)] = *p.Status.Slot
		}
	}
	for i := range s.Settings.SlotCount {
		ns := s.Settings.slotName(int32(i))
		if err := s.sweepSlot(ctx, ns, int32(i), live); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sweeper) sweepSlot(ctx context.Context, namespace string, slot int32, live map[string]int32) error {
	selector := client.MatchingLabels{labelManagedBy: managedBy}
	var ingresses networkingv1.IngressList
	if err := s.List(ctx, &ingresses, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range ingresses.Items {
		if err := s.deleteOrphan(ctx, &ingresses.Items[i], slot, live); err != nil {
			return err
		}
	}
	var services corev1.ServiceList
	if err := s.List(ctx, &services, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range services.Items {
		if err := s.deleteOrphan(ctx, &services.Items[i], slot, live); err != nil {
			return err
		}
	}
	var deployments appsv1.DeploymentList
	if err := s.List(ctx, &deployments, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range deployments.Items {
		if err := s.deleteOrphan(ctx, &deployments.Items[i], slot, live); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sweeper) deleteOrphan(ctx context.Context, obj client.Object, slot int32, live map[string]int32) error {
	l := obj.GetLabels()
	if !strings.HasPrefix(obj.GetName(), "preview-") || l[labelPreview] == "" ||
		l[labelPreviewUID] == "" || obj.GetName() != "preview-"+l[labelPreview] {
		return nil // do not adopt or delete an arbitrary object with a copied managed-by label
	}
	if held, ok := live[l[labelPreviewUID]]; ok && held == slot {
		return nil
	}
	if s.Log != nil {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "deleting orphan preview object",
			slog.String("kind", objectKind(obj)), slog.String("namespace", obj.GetNamespace()),
			slog.String("name", obj.GetName()))
	}
	err := s.Delete(ctx, obj)
	if kerrors.IsNotFound(err) {
		return nil
	}
	return err
}

func objectKind(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *networkingv1.Ingress:
		return "Ingress"
	default:
		return "unknown"
	}
}
