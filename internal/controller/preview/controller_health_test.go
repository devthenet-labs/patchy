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
	e.syncAvailability(dep.Name)
	return pod
}

// setTargetHealth plays the load balancer controller reporting the Pod's
// target health on its gate, the kubelet recomputing Ready from it, and the
// Deployment controller recounting what is available.
func (e *testEnv) setTargetHealth(pod *corev1.Pod, health corev1.ConditionStatus) {
	e.t.Helper()
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: health}, {Type: targetHealthGate, Status: health},
	}
	if err := e.c.Status().Update(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
	e.syncAvailability(pod.Annotations[testPodDeployment])
}

// syncAvailability plays the ReplicaSet and Deployment controllers: the
// Deployment's available replicas are its Pods whose Ready condition is
// True. A gated Pod whose target is unhealthy is not Ready, so it is never
// counted, however Ready its containers are.
func (e *testEnv) syncAvailability(deployment string) {
	e.t.Helper()
	ctx := context.Background()
	var dep appsv1.Deployment
	if err := e.slotObject(deployment, &dep); err != nil {
		e.t.Fatalf("Deployment %s: %v", deployment, err)
	}
	var pods corev1.PodList
	if err := e.c.List(ctx, &pods, client.InNamespace(dep.Namespace)); err != nil {
		e.t.Fatal(err)
	}
	var available int32
	for i := range pods.Items {
		if pods.Items[i].Annotations[testPodDeployment] == deployment && podCondition(&pods.Items[i], corev1.PodReady) {
			available++
		}
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.AvailableReplicas = available
	if err := e.c.Status().Update(ctx, &dep); err != nil {
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
	// Its containers are Ready: the message names the unhealthy target.
	if p.Status.Retries != 1 ||
		!strings.Contains(p.Status.Message, "component api's load balancer target did not become healthy") {
		t.Fatalf("status = %+v, want one retry naming api's target", p.Status)
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

// Regression: a Pod the load balancer injected no gate into is Ready, so a
// retry that said the component "did not become Ready" pointed away from the
// cause. The retry names the missing gate and how to stop waiting for it.
func TestTargetHealthNamesAMissingGate(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e := newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	e.startPod(p, 0, "gateless", false, "")
	e.now = e.now.Add(11 * time.Minute)
	p = e.step(p.Name)
	for _, want := range []string{"component demo's Ready Pod got no load-balancer readiness gate within 10m0s",
		"eks.amazonaws.com/pod-readiness-gate-inject", "targetHealth: false"} {
		if p.Status.Retries != 1 || !strings.Contains(p.Status.Message, want) {
			t.Errorf("status = %+v, want one retry whose message has %q", p.Status, want)
		}
	}
}

// Regression: a gated Pod whose target is not healthy is not Ready (the
// kubelet holds Ready until every gate is True), so its Deployment counts no
// replica available. The rollout check stopped at that count, before looking
// at the Pod, and the retry said the component "did not become Ready" though
// its containers were; it now names the target. A Pod whose containers are
// not Ready still gets the plain message.
func TestTargetHealthRetryNamesTheCause(t *testing.T) {
	const unhealthy = "component demo's load balancer target did not become healthy within 10m0s"
	for _, tc := range []struct {
		name string
		// observe plays the kubelet and the load balancer on the started,
		// gated Pod, whose target is reported unhealthy.
		observe func(*corev1.Pod)
		want    string
	}{
		{name: "unhealthy target", want: unhealthy},
		{name: "target not yet reported", observe: func(pod *corev1.Pod) {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		}, want: unhealthy},
		{name: "containers not Ready", observe: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].Ready = false
		}, want: "component demo did not become Ready within 10m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
			in, p := testPreview("demo-1", at)
			e := newHealthEnv(t, in, p)
			p = e.untilSlot(p.Name)
			e.step(p.Name)
			e.admitIngress(p)
			e.step(p.Name)
			pod := e.startPod(p, 0, "gated", true, corev1.ConditionFalse)
			if tc.observe != nil {
				tc.observe(pod)
				if err := e.c.Status().Update(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
				e.syncAvailability(componentName(p, 0))
			}
			var dep appsv1.Deployment
			if err := e.slotObject(componentName(p, 0), &dep); err != nil {
				t.Fatal(err)
			}
			if dep.Status.AvailableReplicas != 0 {
				t.Fatalf("available replicas = %d, want 0: a Pod with a gate not True is never available",
					dep.Status.AvailableReplicas)
			}
			e.now = e.now.Add(11 * time.Minute)
			p = e.step(p.Name)
			if p.Status.Retries != 1 || !strings.Contains(p.Status.Message, tc.want) {
				t.Fatalf("status = %+v, want one retry whose message has %q", p.Status, tc.want)
			}
		})
	}
}

// Regression: a component added to a Ready Preview started its Pod as soon as
// the kept Ingress was patched to route to it, on the strength of an address
// the load balancer had published for the Ingress before that route existed.
// The Pod then came up before its target group binding, got no gate, and held
// the Preview until the rollout timeout and a retry. An Ingress that lacks a
// backend is now replaced, and the new component starts only once the load
// balancer has admitted the replacement.
func TestTargetHealthAddedComponentWaitsForAFreshIngress(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	project := shopProject()
	api := project.Spec.Repositories[2].Preview
	project.Spec.Repositories[2].Preview = nil
	both := p.Spec.Components
	p.Spec.Components = both[:1]
	e := newHealthEnv(t, project, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	e.startPod(p, 0, "web", true, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	ctx := context.Background()
	// The operator previews the API too: a second component, and a route.
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(project), project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Repositories[2].Preview = api
	if err := e.c.Update(ctx, project); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components = both
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		p = e.step(p.Name)
		e.collectGarbage()
	}
	if p.Status.Phase == v1alpha1.PreviewFailed || p.Status.Retries != 0 {
		t.Fatalf("status = %+v, want still deploying", p.Status)
	}
	if err := e.slotObject(componentName(p, 1), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
		t.Fatalf("api Deployment created before the load balancer admitted an Ingress routing to it: %v", err)
	}
	var ing networkingv1.Ingress
	if err := e.slotObject(resourceName(p), &ing); err != nil {
		t.Fatalf("Ingress not recreated: %v", err)
	}
	if !ingressBackends(&ing)[componentName(p, 1)] || len(ing.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("Ingress = %+v, want a fresh, unadmitted one routing to api", ing)
	}
	e.admitIngress(p)
	for range 2 { // the web Deployment is replaced first: its selector gains the component label
		p = e.step(p.Name)
		e.collectGarbage()
	}
	if err := e.slotObject(componentName(p, 1), &appsv1.Deployment{}); err != nil {
		t.Fatalf("api Deployment not created once the fresh Ingress was admitted: %v", err)
	}
}
