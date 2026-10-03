// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// targetHealthGate is the readiness gate the load balancer controller injects
// into a slot Pod created after its target group binding.
const targetHealthGate corev1.PodConditionType = "target-health.elbv2.k8s.aws/k8s-patchypr-previewd-0123456789"

// newHealthEnv is a test environment whose controller waits for target
// health before Ready.
func newHealthEnv(t *testing.T, objects ...client.Object) *testEnv {
	t.Helper()
	e := newTestEnv(t, objects...)
	e.r.Settings.TargetHealth = true
	return e
}

// admitIngress plays the load balancer controller: it has reconciled the
// Preview's Ingress (target group bindings built) and published the address.
func (e *testEnv) admitIngress(p *v1alpha1.Preview) {
	e.t.Helper()
	var ing networkingv1.Ingress
	if err := e.slotObject(resourceName(p), &ing); err != nil {
		e.t.Fatalf("Ingress to admit: %v", err)
	}
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{
		Hostname: "k8s-devthenet-preview-0123456789.us-east-1.elb.amazonaws.com",
	}}
	if err := e.c.Status().Update(context.Background(), &ing); err != nil {
		e.t.Fatal(err)
	}
}

// startPod plays the Deployment controller, the load balancer's webhook and
// the kubelet for component i: a Pod running its exact image whose
// containers are Ready, carrying the target-health gate (in state `health`)
// when `gated`, as a Pod created after the target group binding does.
func (e *testEnv) startPod(p *v1alpha1.Preview, i int, name string, gated bool,
	health corev1.ConditionStatus) *corev1.Pod {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, i), &dep); err != nil {
		e.t.Fatalf("component %d Deployment: %v", i, err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.AvailableReplicas = 1
	if err := e.c.Status().Update(ctx, &dep); err != nil {
		e.t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: dep.Namespace, Labels: maps.Clone(dep.Spec.Template.Labels),
		Annotations: map[string]string{testPodDeployment: dep.Name},
	}, Spec: *dep.Spec.Template.Spec.DeepCopy()}
	ready := corev1.ConditionTrue
	if gated {
		pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: targetHealthGate}}
		ready = health // the kubelet holds Ready until every gate is True
	}
	if err := e.c.Create(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
	if gated {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: targetHealthGate, Status: health})
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: p.Spec.Components[i].Name, Ready: true,
		ImageID: "repo@sha256:" + p.Spec.Components[i].Name}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

// setTargetHealth plays the load balancer controller reporting the Pod's
// target health on its gate, and the kubelet recomputing Ready from it.
func (e *testEnv) setTargetHealth(pod *corev1.Pod, health corev1.ConditionStatus) {
	e.t.Helper()
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: health}, {Type: targetHealthGate, Status: health},
	}
	if err := e.c.Status().Update(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
}

// The Ingress (and with it the target group binding) precedes the Pods, so
// the load balancer can inject the gate Ready waits for.
func TestTargetHealthIngressPrecedesPods(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	for range 2 {
		p = e.step(p.Name)
	}
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); err != nil {
		t.Fatalf("Ingress not created before the Pods: %v", err)
	}
	if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
		t.Fatalf("Deployment created before the load balancer admitted the Ingress: %v", err)
	}
	if p.Status.Phase == v1alpha1.PreviewReady || p.Status.URL != "" {
		t.Fatalf("status = %+v before any Pod", p.Status)
	}
	e.admitIngress(p)
	p = e.step(p.Name)
	if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); err != nil {
		t.Fatalf("Deployment not created once the Ingress was admitted: %v", err)
	}
}

// Regression for the live symptom (~15 s of empty and 404 responses right
// after Ready): Ready waits for the Pod's target-health gate, not only for
// its containers.
func TestTargetHealthReadyWaitsForTheGate(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	ctx := context.Background()
	// A Pod with Ready containers but no gate (created before the binding)
	// says nothing about the target.
	gateless := e.startPod(p, 0, "gateless", false, "")
	if p = e.step(p.Name); p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Ready on a Pod with no target-health gate")
	}
	if err := e.c.Delete(ctx, gateless); err != nil {
		t.Fatal(err)
	}
	pod := e.startPod(p, 0, "gated", true, corev1.ConditionFalse)
	if p = e.step(p.Name); p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Ready while the load balancer target is not yet healthy")
	}
	e.setTargetHealth(pod, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.URL == "" {
		t.Fatalf("status = %+v once the target is healthy, want Ready", p.Status)
	}
}

// A new PR head keeps the Ingress (the new Pod needs its binding for the
// gate) and is Ready only once the new Pod's target is healthy.
func TestTargetHealthHeadUpdateKeepsIngress(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	old := e.startPod(p, 0, "old", true, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	ctx := context.Background()
	head := strings.Repeat("b", 40)
	e.updatePRHead(in, head)
	p.Spec.Components[0].Revision = head
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewPending || p.Status.URL != "" {
		t.Fatalf("head update status = %+v", p.Status)
	}
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); err != nil {
		t.Fatalf("Ingress withdrawn on a head update with target health on: %v", err)
	}
	p = e.step(p.Name) // the Deployment rolls to the new head
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; !strings.HasSuffix(got, "sha-"+head) {
		t.Fatalf("Deployment image = %s, want the new head", got)
	}
	if p = e.step(p.Name); p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("the old revision's Pod made the new head Ready")
	}
	if err := e.c.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	pod := e.startPod(p, 0, "new", true, corev1.ConditionFalse)
	if p = e.step(p.Name); p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Ready before the new target is healthy")
	}
	e.setTargetHealth(pod, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.Components[0].Revision != head {
		t.Fatalf("status = %+v, want Ready at the new head", p.Status)
	}
}

// A retry restarts every component but keeps the Ingress: the retried Pods
// need its binding for their gates.
func TestTargetHealthRetryKeepsIngress(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	e := newHealthEnv(t, shopProject(), in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	e.startPod(p, 0, "web", true, corev1.ConditionTrue)
	e.startPod(p, 1, "api", true, corev1.ConditionFalse) // never healthy
	e.now = e.now.Add(11 * time.Minute)
	p = e.step(p.Name)
	if p.Status.Retries != 1 || !strings.Contains(p.Status.Message, "component api did not become Ready") {
		t.Fatalf("status = %+v, want one retry naming api", p.Status)
	}
	for i := range p.Spec.Components {
		if err := e.slotObject(componentName(p, i), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
			t.Errorf("component %d Deployment survived the retry: %v", i, err)
		}
	}
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); err != nil {
		t.Fatalf("Ingress withdrawn by a retry with target health on: %v", err)
	}
}

// An Ingress the load balancer never admits is retried, and named.
func TestTargetHealthIngressNeverAdmitted(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.now = e.now.Add(11 * time.Minute)
	p = e.step(p.Name)
	if p.Status.Retries != 1 || !strings.Contains(p.Status.Message, "load balancer did not admit the Ingress") {
		t.Fatalf("status = %+v, want a retry naming the unadmitted Ingress", p.Status)
	}
}

// A Preview already Ready when target health is switched on keeps serving on
// its Pod: it is not restarted to grow a gate it was created without.
func TestTargetHealthKeepsAReadyPreviewServing(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	slot := int32(0)
	p.Finalizers = []string{finalizer}
	p.Status = v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, ObservedGeneration: p.Generation, Slot: &slot,
		URL: "https://demo-1." + testSettings().HostSuffix, ObservedRevision: testSHA,
		AttemptStartedAt: &metav1.Time{Time: at}, LastDeployedAt: &metav1.Time{Time: at},
		Components: []v1alpha1.PreviewComponentStatus{{Name: "demo", ImageID: "repo@sha256:demo"}}}
	live := renderSingle(testSettings(), p, slot)
	// Not even the Ingress's load balancer address is waited for again.
	e := newHealthEnv(t, append([]client.Object{in, p}, live...)...)
	e.startPod(p, 0, "serving", false, "")
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatal(err)
	}
	before := dep.ResourceVersion
	for range 3 {
		p = e.step(p.Name)
	}
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Retries != 0 {
		t.Fatalf("status = %+v, want still Ready with no retry", p.Status)
	}
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatalf("serving Deployment removed: %v", err)
	}
	if dep.ResourceVersion != before {
		t.Errorf("serving Deployment rewritten (resourceVersion %s -> %s)", before, dep.ResourceVersion)
	}
}
