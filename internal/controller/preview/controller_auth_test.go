// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// policyDenied is the error a ValidatingAdmissionPolicy's denial reaches a
// client as: Invalid (the reason a validation without one gets), its message
// naming the policy and binding.
func policyDenied(name string) error {
	err := kerrors.NewInvalid(networkingv1.SchemeGroupVersion.WithKind("Ingress").GroupKind(), name, nil)
	err.ErrStatus.Message = fmt.Sprintf("ingresses.networking.k8s.io %q is forbidden: ValidatingAdmissionPolicy "+
		"'patchy-preview-ingresses' with binding 'patchy-preview-ingresses' denied request: only the pinned "+
		"annotations are admitted", name)
	return err
}

// ingressRefusal refuses every Ingress create and patch while *on, with
// refuse's error, as a slot policy that does not admit the write would.
func ingressRefusal(on *bool, refuse func(name string) error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*networkingv1.Ingress); ok && *on {
				return refuse(obj.GetName())
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if _, ok := obj.(*networkingv1.Ingress); ok && *on {
				return refuse(obj.GetName())
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

func TestAdmissionRefused(t *testing.T) {
	gr := networkingv1.Resource("ingresses")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"policy denial (Invalid)", policyDenied("preview-demo-1"), true},
		{"policy denial (Forbidden)", kerrors.NewForbidden(gr, "x", errors.New(
			"ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no")), true},
		{"RBAC", kerrors.NewForbidden(gr, "x", errors.New(`User "system:serviceaccount:patchy:pc" cannot patch`)), true},
		{"webhook denial", func() error {
			err := kerrors.NewBadRequest(`admission webhook "vingress.elbv2.k8s.aws" denied the request: no`)
			return err
		}(), true},
		{"wrapped", fmt.Errorf("patch: %w", policyDenied("x")), true},
		{"quota", kerrors.NewForbidden(gr, "x", errors.New("exceeded quota: preview-quota")), false},
		{"genuinely invalid", kerrors.NewInvalid(networkingv1.SchemeGroupVersion.WithKind("Ingress").GroupKind(),
			"x", nil), false},
		{"conflict", kerrors.NewConflict(gr, "x", errors.New("changed")), false},
		{"not an API error", errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := admissionRefused(tc.err); got != tc.want {
				t.Errorf("admissionRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// readyLiveAuthPreview is a single-component Preview Ready an hour ago on its
// last attempt in slot 0, with the Deployment, Service and Ingress the
// controller rendered before auth (so the Ingress carries no sign-in
// annotations), and its Pod serving; the env's controller requires auth.
func readyLiveAuthPreview(t *testing.T, targetHealth bool, funcs interceptor.Funcs) (*testEnv, *v1alpha1.Preview,
	*corev1.Pod) {
	t.Helper()
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC) // an hour before e.now
	in, p := testPreview("demo-1", at)
	slot := int32(0)
	p.Finalizers = []string{finalizer}
	p.Status = v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, ObservedGeneration: p.Generation,
		Slot: &slot, URL: "https://demo-1." + testSettings().HostSuffix, ObservedRevision: testSHA,
		Retries: testSettings().MaxRetries - 1, AttemptStartedAt: &metav1.Time{Time: at},
		LastDeployedAt: &metav1.Time{Time: at},
		Components:     []v1alpha1.PreviewComponentStatus{{Name: "demo", Revision: testSHA, ImageID: "repo@sha256:demo"}}}
	live := renderSingle(testSettings(), p, slot)
	e := newTestEnvWith(t, funcs, append([]client.Object{in, p}, live...)...)
	e.r.Settings = authSettings()
	e.r.Settings.TargetHealth = targetHealth
	e.r.Events = events.NewFakeRecorder(100)
	e.markReady(p, 0)
	return e, p, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy-preview-0",
		Name: componentName(p, 0) + "-pod"}}
}

// liveVersions are the resource versions of the Preview's Deployment and
// Service: a write to either changes them.
func (e *testEnv) liveVersions(p *v1alpha1.Preview) (string, string) {
	e.t.Helper()
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		e.t.Fatalf("Deployment gone: %v", err)
	}
	var svc corev1.Service
	if err := e.slotObject(componentName(p, 0), &svc); err != nil {
		e.t.Fatalf("Service gone: %v", err)
	}
	return dep.ResourceVersion, svc.ResourceVersion
}

// assertUntouched fails unless the live Preview is as it was: Ready with its
// retries, its Deployment and Service unwritten, its Ingress in place and its
// Pod serving.
func (e *testEnv) assertUntouched(when string, p *v1alpha1.Preview, dep, svc string, retries int32,
	serving *corev1.Pod) {
	e.t.Helper()
	if d, s := e.liveVersions(p); d != dep || s != svc {
		e.t.Fatalf("%s: the Deployment or Service was written", when)
	}
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Retries != retries || p.Status.URL == "" ||
		p.Status.Message != "" {
		e.t.Fatalf("%s: status = %+v, want still Ready with %d retries", when, p.Status, retries)
	}
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); err != nil {
		e.t.Fatalf("%s: Ingress withdrawn: %v", when, err)
	}
	if !e.servingPod(serving) {
		e.t.Fatalf("%s: the serving Pod stopped", when)
	}
}

func drainEvents(e *testEnv) []string {
	rec := e.r.Events.(*events.FakeRecorder)
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestRefusedIngressWriteNeverRestartsALivePreview is the regression test for
// critique F2. Turning auth on patches every live Preview's Ingress; while a
// slot policy refuses that patch (a rollout before the policy admitting it is
// live, or a rollback leaving the required policy behind) the refusal was
// spent as a deploy attempt: every component's Deployment deleted, the
// Preview restarted, and Failed once its retries ran out. Now the Preview
// keeps its Deployment, its Service, its retries and its host, waits past
// its rollout deadline, and reports the refusal as a Warning Event; once the
// write is admitted the Ingress is patched to the pinned set and nothing
// else is written, so no Pod restarts.
func TestRefusedIngressWriteNeverRestartsALivePreview(t *testing.T) {
	gr := networkingv1.Resource("ingresses")
	for _, refusal := range []struct {
		name   string
		refuse func(string) error
	}{
		{"policy", policyDenied},
		{"forbidden", func(name string) error {
			return kerrors.NewForbidden(gr, name, errors.New("the annotation is not admitted"))
		}},
	} {
		for _, targetHealth := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/targetHealth=%t", refusal.name, targetHealth), func(t *testing.T) {
				refusing := true
				e, p, serving := readyLiveAuthPreview(t, targetHealth, ingressRefusal(&refusing, refusal.refuse))
				dep, svc := e.liveVersions(p)
				retries := p.Status.Retries
				for poll := range 6 {
					e.now = e.now.Add(4 * time.Minute) // well past the rollout deadline by the end
					p = e.step(p.Name)
					e.assertUntouched(fmt.Sprintf("poll %d", poll), p, dep, svc, retries, serving)
				}
				evs := drainEvents(e)
				if len(evs) == 0 || !strings.Contains(evs[0], "Warning IngressRefused") {
					t.Errorf("events = %q, want a Warning IngressRefused", evs)
				}
				// The write is admitted: the Ingress gets the slot's pinned
				// set, and nothing else changes.
				refusing = false
				p = e.step(p.Name)
				var ing networkingv1.Ingress
				if err := e.slotObject(resourceName(p), &ing); err != nil {
					t.Fatal(err)
				}
				want := maps.Clone(authSettings().Auth.Annotations["patchy-preview-0"])
				want[annotationHealthcheck] = "/health"
				if !maps.Equal(ing.Annotations, want) {
					t.Errorf("Ingress annotations = %v, want %v", ing.Annotations, want)
				}
				if d, s := e.liveVersions(p); d != dep || s != svc {
					t.Error("patching the Ingress wrote the Deployment or Service")
				}
				if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Retries != retries || !e.servingPod(serving) {
					t.Errorf("status = %+v after the patch, want still Ready and serving", p.Status)
				}
			})
		}
	}
}

// A Preview still deploying whose Ingress create is refused waits too, past
// its rollout deadline, rather than spending its attempts: the refusal is
// not its workload failing.
func TestRefusedIngressCreateNeverSpendsARetry(t *testing.T) {
	for _, targetHealth := range []bool{false, true} {
		t.Run(fmt.Sprintf("targetHealth=%t", targetHealth), func(t *testing.T) {
			refusing := true
			in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
			e := newTestEnvWith(t, ingressRefusal(&refusing, policyDenied), in, p)
			e.r.Settings = authSettings()
			e.r.Settings.TargetHealth = targetHealth
			p = e.untilSlot(p.Name)
			p = e.step(p.Name)
			if !targetHealth {
				e.markReady(p, 0) // the Ingress follows a Ready component
			}
			for range 4 {
				e.now = e.now.Add(e.r.Settings.RolloutTimeout)
				p = e.step(p.Name)
				if p.Status.Retries != 0 || p.Status.Phase != v1alpha1.PreviewDeploying {
					t.Fatalf("status = %+v, want Deploying with no retry", p.Status)
				}
			}
			if p.Status.Message != msgIngressRefused {
				t.Errorf("message = %q, want %q", p.Status.Message, msgIngressRefused)
			}
			refusing = false
			p = e.step(p.Name)
			var ing networkingv1.Ingress
			if err := e.slotObject(resourceName(p), &ing); err != nil {
				t.Fatalf("Ingress not created once admitted: %v", err)
			}
			if !e.r.Settings.authConforms("patchy-preview-0", ing.Annotations) {
				t.Errorf("created Ingress annotations = %v, want the pinned set", ing.Annotations)
			}
			if targetHealth {
				// The attempt starts afresh once admitted: waiting for the load
				// balancer is not timed from before the refusal.
				if p.Status.Message != "" || p.Status.AttemptStartedAt == nil ||
					!p.Status.AttemptStartedAt.Time.Equal(e.now) {
					t.Errorf("status = %+v, want the attempt restarted now", p.Status)
				}
				p = e.step(p.Name)
				if p.Status.Retries != 0 || p.Status.Phase != v1alpha1.PreviewDeploying {
					t.Errorf("status = %+v, want Deploying with no retry while the load balancer admits it",
						p.Status)
				}
				e.admitIngress(p)
				p = e.step(p.Name) // Deployments now
				if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); err != nil {
					t.Errorf("no Deployment after the Ingress was admitted: %v", err)
				}
			} else {
				if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Message != "" {
					t.Errorf("status = %+v, want Ready", p.Status)
				}
			}
		})
	}
}

// TestUpgradeAddsAuthToALiveIngressWithoutARestart: the controller that
// requires auth meets a live Ingress rendered before it, admitted as it is.
// It patches the Ingress to the slot's pinned set at its first poll and
// writes nothing else: the Deployment and Service keep their resource
// versions, so no Pod restarts, and the Preview stays Ready.
func TestUpgradeAddsAuthToALiveIngressWithoutARestart(t *testing.T) {
	for _, targetHealth := range []bool{false, true} {
		t.Run(fmt.Sprintf("targetHealth=%t", targetHealth), func(t *testing.T) {
			refusing := false
			e, p, serving := readyLiveAuthPreview(t, targetHealth, ingressRefusal(&refusing, policyDenied))
			dep, svc := e.liveVersions(p)
			var before networkingv1.Ingress
			if err := e.slotObject(resourceName(p), &before); err != nil {
				t.Fatal(err)
			}
			if e.r.Settings.authConforms("patchy-preview-0", before.Annotations) {
				t.Fatal("test setup: the live Ingress already carries the pinned set")
			}
			for range 3 {
				p = e.step(p.Name)
			}
			var after networkingv1.Ingress
			if err := e.slotObject(resourceName(p), &after); err != nil {
				t.Fatal(err)
			}
			if !e.r.Settings.authConforms("patchy-preview-0", after.Annotations) ||
				after.Annotations[annotationHealthcheck] != "/health" {
				t.Errorf("Ingress annotations = %v, want the pinned set beside the health check", after.Annotations)
			}
			if after.UID != before.UID {
				t.Error("the Ingress was replaced rather than patched")
			}
			if d, s := e.liveVersions(p); d != dep || s != svc {
				t.Error("the Deployment or Service was written")
			}
			if p.Status.Phase != v1alpha1.PreviewReady || !e.servingPod(serving) {
				t.Errorf("status = %+v, want still Ready and serving", p.Status)
			}
			if evs := drainEvents(e); len(evs) != 0 {
				t.Errorf("events = %q, want none", evs)
			}
		})
	}
}
