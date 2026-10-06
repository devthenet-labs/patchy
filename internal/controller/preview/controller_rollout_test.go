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
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// rollingStrategy reports whether a Deployment starts its new revision's Pod
// beside the serving one and stops the old Pod only once the new one is
// available: a rolling update with one surge Pod and none unavailable.
func rollingStrategy(s appsv1.DeploymentStrategy) bool {
	return s.Type == appsv1.RollingUpdateDeploymentStrategyType && s.RollingUpdate != nil &&
		s.RollingUpdate.MaxSurge != nil && *s.RollingUpdate.MaxSurge == intstr.FromInt32(1) &&
		s.RollingUpdate.MaxUnavailable != nil && *s.RollingUpdate.MaxUnavailable == intstr.FromInt32(0)
}

// Every component's Deployment keeps its serving Pod until the new revision's
// Pod is available. A Recreate rollout stopped it first, so the host answered
// 503 for as long as the new image took to publish and pull.
func TestRolloutKeepsTheServingPod(t *testing.T) {
	s := testSettings()
	_, single := testPreview("demo-1", time.Now())
	_, _, four := fourComponentShop(t, time.Now())
	for _, p := range []*v1alpha1.Preview{single, four} {
		for i := range p.Spec.Components {
			if strategy := s.deployment(p, i, 0).Spec.Strategy; !rollingStrategy(strategy) {
				t.Errorf("%s strategy = %+v, want a rolling update with maxSurge 1 and maxUnavailable 0",
					componentName(p, i), strategy)
			}
		}
	}
}

// The slot quota (charts/patchy/templates/preview-foundation.yaml) holds the
// surge: every component of the largest Preview runs its serving Pod and its
// new one at once. The LimitRange's per-container maximum holds each Pod.
func TestRolloutSurgeFitsTheSlotQuota(t *testing.T) {
	quota := corev1.ResourceList{
		corev1.ResourcePods:           resource.MustParse("8"),
		corev1.ResourceRequestsCPU:    resource.MustParse("1"),
		corev1.ResourceRequestsMemory: resource.MustParse("1Gi"),
		corev1.ResourceLimitsCPU:      resource.MustParse("2"),
		corev1.ResourceLimitsMemory:   resource.MustParse("2Gi"),
	}
	perContainer := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi"),
	}
	s := testSettings()
	_, _, p := fourComponentShop(t, time.Now())
	used := corev1.ResourceList{}
	add := func(name corev1.ResourceName, q resource.Quantity, n int64) {
		total := used[name]
		for range n {
			total.Add(q)
		}
		used[name] = total
	}
	for i := range p.Spec.Components {
		dep := s.deployment(p, i, 0)
		pods := int64(*dep.Spec.Replicas)
		if ru := dep.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxSurge != nil {
			pods += int64(ru.MaxSurge.IntValue())
		}
		add(corev1.ResourcePods, resource.MustParse("1"), pods)
		for _, c := range dep.Spec.Template.Spec.Containers {
			for name, q := range c.Resources.Limits {
				if limit := perContainer[name]; q.Cmp(limit) > 0 {
					t.Errorf("%s limit %s = %s, above the LimitRange maximum %s", c.Name, name, q.String(), limit.String())
				}
			}
			add(corev1.ResourceRequestsCPU, c.Resources.Requests[corev1.ResourceCPU], pods)
			add(corev1.ResourceRequestsMemory, c.Resources.Requests[corev1.ResourceMemory], pods)
			add(corev1.ResourceLimitsCPU, c.Resources.Limits[corev1.ResourceCPU], pods)
			add(corev1.ResourceLimitsMemory, c.Resources.Limits[corev1.ResourceMemory], pods)
		}
	}
	for name, hard := range quota {
		if got := used[name]; got.Cmp(hard) > 0 {
			t.Errorf("a four-component rollout needs %s %s, above the slot quota's %s", name, got.String(), hard.String())
		}
	}
}

// startPulling plays the Deployment controller and the kubelet starting
// component i's Pod at its current template while its image is not yet in
// the registry: the app's CI publishes a pull request head's runtime image
// only after the pull request's checks pass, minutes after the head is
// recorded, and the kubelet keeps retrying the pull meanwhile. The Pod was
// created after the target group binding, so it carries the gate.
func (e *testEnv) startPulling(p *v1alpha1.Preview, i int, name string) *corev1.Pod {
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
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: targetHealthGate}}
	if err := e.c.Create(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: p.Spec.Components[i].Name,
		Image: pod.Spec.Containers[0].Image, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: "Back-off pulling image " + pod.Spec.Containers[0].Image,
		}}}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	e.syncAvailability(dep.Name)
	return pod
}

// pulled plays the kubelet once the image is published: the next pull
// succeeds, the container starts and passes its readiness probe, and the load
// balancer then finds the target healthy.
func (e *testEnv) pulled(p *v1alpha1.Preview, i int, pod *corev1.Pod) {
	e.t.Helper()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: p.Spec.Components[i].Name, Ready: true,
		Image: pod.Spec.Containers[0].Image, ImageID: "repo@sha256:" + pod.Name,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	e.setTargetHealth(pod, corev1.ConditionTrue)
}

// servingPod reports whether pod still exists, not deleting, with its
// Ready condition True: a target the load balancer routes to.
func (e *testEnv) servingPod(pod *corev1.Pod) bool {
	e.t.Helper()
	var live corev1.Pod
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(pod), &live); err != nil {
		return false
	}
	return live.DeletionTimestamp.IsZero() && podCondition(&live, corev1.PodReady)
}

// redeployWhilePublishing brings a gated Preview from Ready at its first head
// through a revision round's new PR head whose runtime image is not published
// yet, as seen live on 2026-10-06: the Recreate rollout stopped the old Pod at
// once, and the host answered 503 for about two minutes until a pull retry
// found the image. It checks what must hold for the whole wait (the Preview
// reports the redeploy, Deploying at the new head with no URL, and the
// previous revision keeps serving behind the kept Ingress for minutes, with
// no retry) and returns the waiting new Pod, four minutes into the attempt.
func redeployWhilePublishing(t *testing.T) (e *testEnv, p *v1alpha1.Preview, pulling *corev1.Pod, head string) {
	t.Helper()
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	e = newHealthEnv(t, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.admitIngress(p)
	e.step(p.Name)
	old := e.startPod(p, 0, "old", true, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready", p.Status.Phase)
	}
	head = strings.Repeat("b", 40)
	p = e.newHead(in, p, head)
	if p.Status.Phase != v1alpha1.PreviewDeploying || p.Status.URL != "" ||
		p.Status.ObservedRevision != head || len(p.Status.Components) != 0 {
		t.Errorf("status = %+v, want Deploying at the new head with no URL", p.Status)
	}
	p = e.step(p.Name) // the Deployment rolls to the new head
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; !strings.HasSuffix(got, "sha-"+head) {
		t.Fatalf("Deployment image = %s, want the new head", got)
	}
	// The Deployment controller starts the new revision as the rendered
	// strategy says: a Recreate rollout stops every old Pod first, a rolling
	// one keeps it until the new Pod is available.
	if dep.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		if err := e.c.Delete(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		e.syncAvailability(dep.Name)
	}
	pulling = e.startPulling(p, 0, "new")
	// Publishing takes minutes; every poll meanwhile finds the new Pod
	// waiting for its image and the old one still serving.
	for range 4 {
		e.now = e.now.Add(time.Minute)
		p = e.step(p.Name)
		if !e.servingPod(old) {
			t.Fatalf("the previous revision stopped serving before the new one was Ready (strategy %s)",
				dep.Spec.Strategy.Type)
		}
	}
	if p.Status.Phase != v1alpha1.PreviewDeploying || p.Status.Retries != 0 || p.Status.URL != "" {
		t.Fatalf("status = %+v after four minutes, want still Deploying with no retry", p.Status)
	}
	if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); err != nil {
		t.Fatalf("Deployment gone while its new image was still being published: %v", err)
	}
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); err != nil {
		t.Fatalf("Ingress withdrawn while the previous revision served: %v", err)
	}
	return e, p, pulling, head
}

// newHead plays a revision round: the Intent records the pull request's new
// head and intent-controller projects it into the Preview's spec. It returns
// the Preview once the controller has observed the new generation.
func (e *testEnv) newHead(in *v1alpha1.Intent, p *v1alpha1.Preview, head string) *v1alpha1.Preview {
	e.t.Helper()
	e.updatePRHead(in, head)
	p.Spec.Components[0].Revision = head
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
	return e.step(p.Name)
}

// Regression for the live symptom (redeployWhilePublishing): the previous
// revision serves while the new head's image is published, and the Preview
// is Ready at the new head once its Pod pulls the image and its target is
// healthy.
func TestRedeployKeepsThePreviousRevisionServing(t *testing.T) {
	e, p, pulling, head := redeployWhilePublishing(t)
	e.pulled(p, 0, pulling)
	p = e.step(p.Name)
	want := v1alpha1.PreviewComponentStatus{Name: "demo", Revision: head, ImageID: "repo@sha256:new"}
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.URL == "" ||
		len(p.Status.Components) != 1 || p.Status.Components[0] != want {
		t.Fatalf("status = %+v, want Ready at the new head serving %+v", p.Status, want)
	}
}

// A new head whose image is never published spends the rollout deadline
// while the previous revision serves, then fails as before: each retry
// restarts every component by deleting its Deployment (the previous revision
// with them), and after the last the Preview is Failed and its slot released.
func TestRedeployOfAnUnpublishedImageFailsAsBefore(t *testing.T) {
	e, p, _, _ := redeployWhilePublishing(t)
	e.now = e.now.Add(7 * time.Minute) // past the 10-minute deadline from the new head's attempt
	p = e.step(p.Name)
	if p.Status.Retries != 1 || !strings.Contains(p.Status.Message, "component demo did not become Ready") {
		t.Fatalf("status = %+v, want one retry naming the component", p.Status)
	}
	if err := e.slotObject(componentName(p, 0), &appsv1.Deployment{}); !kerrors.IsNotFound(err) {
		t.Fatalf("Deployment survived the retry: %v", err)
	}
	for p.Status.Phase != v1alpha1.PreviewFailed {
		e.collectGarbage()
		e.step(p.Name) // the retried Deployment
		e.now = e.now.Add(11 * time.Minute)
		if p = e.step(p.Name); p.Status.Retries > testSettings().MaxRetries {
			t.Fatalf("retries = %d, past the bound", p.Status.Retries)
		}
	}
	if p.Status.Retries != testSettings().MaxRetries || p.Status.URL != "" {
		t.Fatalf("status = %+v, want Failed after %d attempts", p.Status, testSettings().MaxRetries)
	}
	e.collectGarbage()
	for range 4 {
		if p = e.step(p.Name); p.Status.Slot == nil {
			break
		}
	}
	if p.Status.Slot != nil {
		t.Fatal("failed preview did not release its slot")
	}
}

// The rolling update keeps the previous Pod Ready beside the new one, so a
// spec change that keeps the image (the operator's new readiness path) must
// not be made Ready by the previous Pod: only a Pod of the current template
// counts.
func TestSpecChangeKeepingTheImageWaitsForItsOwnPod(t *testing.T) {
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
	var project v1alpha1.Project
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: "patchy", Name: "demo"}, &project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Preview.ReadinessPath = "/ready"
	if err := e.c.Update(ctx, &project); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].ReadinessPath = "/ready"
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		p = e.step(p.Name)
	}
	if !e.servingPod(old) {
		t.Fatal("test setup: the previous Pod should still be serving")
	}
	if p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatalf("status = %+v: the previous Pod, probed at the old readiness path, made the new spec Ready",
			p.Status)
	}
	e.startPod(p, 0, "new", true, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("status = %+v, want Ready once the new spec's own Pod is", p.Status)
	}
}

// A Deployment the previous controller rendered with the Recreate strategy is
// moved to the rolling one in place. Its Pod template is untouched, so the
// Deployment controller starts no new ReplicaSet and a live preview's Pods
// keep running across the upgrade.
func TestUpgradeMovesALiveDeploymentToRollingWithoutARestart(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := testPreview("demo-1", at)
	slot := int32(0)
	p.Finalizers = []string{finalizer}
	p.Status = v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, ObservedGeneration: p.Generation, Slot: &slot,
		URL: "https://demo-1." + testSettings().HostSuffix, ObservedRevision: testSHA,
		AttemptStartedAt: &metav1.Time{Time: at}, LastDeployedAt: &metav1.Time{Time: at},
		Components: []v1alpha1.PreviewComponentStatus{{Name: "demo", Revision: testSHA, ImageID: "repo@sha256:demo"}}}
	live := renderSingle(testSettings(), p, slot)
	previous := live[0].(*appsv1.Deployment)
	previous.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	template := previous.Spec.Template.DeepCopy()
	e := newTestEnv(t, append([]client.Object{in, p}, live...)...)
	e.markReady(p, 0)
	for range 2 {
		p = e.step(p.Name)
	}
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(p, 0), &dep); err != nil {
		t.Fatal(err)
	}
	if !rollingStrategy(dep.Spec.Strategy) {
		t.Errorf("strategy = %+v, want the rolling update", dep.Spec.Strategy)
	}
	if !equality.Semantic.DeepEqual(dep.Spec.Template, *template) {
		t.Errorf("Pod template changed across the upgrade, which restarts the preview:\n got %+v\nwant %+v",
			dep.Spec.Template, *template)
	}
	if p.Status.Phase != v1alpha1.PreviewReady {
		t.Errorf("phase = %s, want still Ready", p.Status.Phase)
	}
}
