// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// slotServiceQuota plays slot 0's ResourceQuota for Services, which the fake
// client does not enforce. Like the API server's quota admission it refuses a
// create past the limit as Forbidden, and like the quota controller it
// releases a deleted Service's share only when it next recounts (resync), not
// with the delete itself.
type slotServiceQuota struct {
	limit, used int
}

func (q *slotServiceQuota) funcs() interceptor.Funcs {
	return interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
		opts ...client.CreateOption) error {
		svc, ok := obj.(*corev1.Service)
		if !ok || svc.Namespace != "patchy-preview-0" {
			return c.Create(ctx, obj, opts...)
		}
		if q.used >= q.limit {
			return kerrors.NewForbidden(corev1.Resource("services"), svc.Name, fmt.Errorf(
				"exceeded quota: preview-quota, requested: services=1, used: services=%d, limited: services=%d",
				q.used, q.limit))
		}
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		q.used++
		return nil
	}}
}

func (q *slotServiceQuota) resync(e *testEnv) {
	e.t.Helper()
	var services corev1.ServiceList
	if err := e.c.List(context.Background(), &services, client.InNamespace("patchy-preview-0")); err != nil {
		e.t.Fatal(err)
	}
	q.used = len(services.Items)
}

// fourComponentShop is shopProject previewing four repositories, the most a
// Preview has, and an Intent of it in review: pull requests in web and api,
// recorded preview bases for docs and admin.
func fourComponentShop(t *testing.T, at time.Time) (*v1alpha1.Project, *v1alpha1.Intent, *v1alpha1.Preview) {
	t.Helper()
	project := shopProject()
	for _, key := range []string{"docs", "admin"} {
		project.Spec.Repositories = append(project.Spec.Repositories, v1alpha1.ProjectRepository{
			Name: key, URL: "https://github.com/acme/" + key,
			Preview: &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
				ImageRepository: testSettings().ImagePrefix + "acme-" + key, Port: 8080, ReadinessPath: "/healthz",
			}, Path: "/" + key},
		})
	}
	in, p := multiPreview(t, at)
	for _, key := range []string{"docs", "admin"} {
		in.Status.PreviewBases = append(in.Status.PreviewBases, v1alpha1.IntentPreviewBase{
			Repository: "https://github.com/acme/" + key, SHA: webSHA,
		})
	}
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok || len(components) != v1alpha1.MaxPreviewComponents {
		t.Fatalf("derived %+v, %v; want four components", components, ok)
	}
	p.Spec.Components = components
	return project, in, p
}

// Regression: a renamed component's Service was created before the old one
// was pruned. Slot 0's quota holds exactly four components' Services and the
// placeholder, so with four components the create was refused, the refusal
// was spent as a retry (restarting every component), and prune, which came
// after, was never reached: the Preview failed after its retries. Pruning now
// comes first, and a quota refusal waits for the deleted Service's share to
// be released rather than spending a retry.
func TestRenamedComponentFitsTheSlotQuota(t *testing.T) {
	for _, targetHealth := range []bool{false, true} {
		t.Run(fmt.Sprintf("targetHealth=%v", targetHealth), func(t *testing.T) {
			at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
			project, in, p := fourComponentShop(t, at)
			placeholder := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: "patchy-preview-placeholder", Namespace: "patchy-preview-0",
			}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}
			quota := &slotServiceQuota{limit: 5}
			e := newTestEnvWith(t, quota.funcs(), project, in, p, placeholder)
			e.r.Settings.TargetHealth = targetHealth
			quota.resync(e)
			p = e.untilSlot(p.Name)
			if *p.Status.Slot != 0 {
				t.Fatalf("slot = %d, want 0", *p.Status.Slot)
			}
			p = e.step(p.Name) // every Service (and with target health, the Ingress)
			if targetHealth {
				e.admitIngress(p)
				p = e.step(p.Name)
			}
			for i := range p.Spec.Components {
				e.startPod(p, i, p.Spec.Components[i].Name, targetHealth, corev1.ConditionTrue)
			}
			if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
				t.Fatalf("status = %+v, want Ready", p.Status)
			}
			if quota.used != 5 {
				t.Fatalf("quota used = %d, want the slot full (5)", quota.used)
			}

			// The operator renames the admin repository's key: its component
			// moves to a new name, and so does its Service.
			ctx := context.Background()
			if err := e.c.Get(ctx, client.ObjectKeyFromObject(project), project); err != nil {
				t.Fatal(err)
			}
			project.Spec.Repositories[4].Name = "console"
			if err := e.c.Update(ctx, project); err != nil {
				t.Fatal(err)
			}
			components, ok := v1alpha1.DesiredPreviewComponents(project, in)
			if !ok {
				t.Fatal("renamed Project derives no Preview")
			}
			p.Spec.Components = components
			p.Generation++
			if err := e.c.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			const stale, renamed = "preview-shop-1-admin", "preview-shop-1-console"
			for range 3 {
				p = e.step(p.Name)
				e.collectGarbage()
			}
			if p.Status.Retries != 0 || p.Status.Phase == v1alpha1.PreviewFailed {
				t.Fatalf("status = %+v: the quota refusal was spent as a retry", p.Status)
			}
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}} {
				if err := e.slotObject(stale, obj); !kerrors.IsNotFound(err) {
					t.Errorf("stale component's %T not pruned before the renamed one was created: %v", obj, err)
				}
			}
			// The quota controller recounts, releasing the pruned share.
			quota.resync(e)
			for range 2 {
				p = e.step(p.Name)
			}
			if err := e.slotObject(renamed, &corev1.Service{}); err != nil {
				t.Fatalf("renamed component's Service not created once the quota had room: %v", err)
			}
			if p.Status.Retries != 0 || p.Status.Phase == v1alpha1.PreviewFailed {
				t.Fatalf("status = %+v, want still deploying with no retry", p.Status)
			}
		})
	}
}
