// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestPreviewRuntime drives the real shipped binary against the API server:
// lease a fixed slot, render a Deployment and Service, observe readiness and
// the kubelet's image ID before exposing an Ingress, withdraw that Ingress on
// a PR-head change, then finalizer-clean every object after the Intent ends.
// envtest has no Deployment controller/kubelet, so their status writes below
// stand in at exactly those edges; all Preview and workload API calls are real.
func TestPreviewRuntime(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", "preview.patchy.example.com",
		"--preview-node-pool", "patchy-preview", "--preview-node-class", "patchy-preview",
		"--preview-taint-key", "patchy.devthe.net/preview-only",
		"--preview-poll-interval", "1s")
	sha := strings.Repeat("a", 40)
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/demo",
				Port: 8080, ReadinessPath: "/health"},
		}}
	if err := cl.client.Create(ctx, project); err != nil {
		t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "demo",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{
		Repository: "https://github.com/acme/demo", Number: 1, State: "open", HeadSHA: sha,
	}}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: in.Name, UID: in.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}}},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo",
				ImageRepository: "registry.example/patchy/previews/demo", Revision: sha,
				Port: 8080, ReadinessPath: "/health"}}, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"}
	eventually(t, "preview Deployment and Service in slot 0", func() bool {
		var dep appsv1.Deployment
		var svc corev1.Service
		return cl.client.Get(ctx, key, &dep) == nil && cl.client.Get(ctx, key, &svc) == nil &&
			dep.Spec.Template.Spec.Containers[0].Image == "registry.example/patchy/previews/demo:sha-"+sha &&
			svc.Spec.Type == corev1.ServiceTypeClusterIP
	})
	var ingress networkingv1.Ingress
	if err := cl.client.Get(ctx, key, &ingress); !apierrors.IsNotFound(err) {
		t.Fatalf("Ingress existed before runtime was Ready: %v", err)
	}
	var dep appsv1.Deployment
	if err := cl.client.Get(ctx, key, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	dep.Status.AvailableReplicas = 1
	if err := cl.client.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preview-demo-1-pod", Namespace: key.Namespace,
		Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
	if err := cl.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true, ImageID: "repo@sha256:123"}}
	if err := cl.client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ready Preview and fixed Ingress", func() bool {
		var current v1alpha1.Preview
		if cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) != nil ||
			cl.client.Get(ctx, key, &ingress) != nil {
			return false
		}
		return current.Status.Phase == v1alpha1.PreviewReady &&
			current.Status.Components[0].ImageID == "repo@sha256:123" &&
			ingress.Spec.Rules[0].Host == "demo-1.preview.patchy.example.com" &&
			*ingress.Spec.IngressClassName == "alb-preview"
	})
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.PullRequests[0].HeadSHA = strings.Repeat("b", 40)
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].Revision = strings.Repeat("b", 40)
	if err := cl.client.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ingress withdrawn and Deployment moved to new PR head", func() bool {
		var current appsv1.Deployment
		return cl.client.Get(ctx, key, &ingress) != nil &&
			cl.client.Get(ctx, key, &current) == nil &&
			current.Spec.Template.Spec.Containers[0].Image == "registry.example/patchy/previews/demo:sha-"+strings.Repeat("b", 40)
	})
	// envtest has no Deployment controller to delete the synthetic Pod.
	if err := cl.client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Preview finalizer and slot cleanup", func() bool {
		var current v1alpha1.Preview
		var currentDep appsv1.Deployment
		var currentSvc corev1.Service
		return apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current)) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &currentDep)) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &currentSvc))
	})
}

func boolPtr(v bool) *bool { return &v }

// TestPreviewTargetHealthRuntime drives the shipped binary with Ready gated on
// the load balancer's target health: the Ingress comes first, the Deployment
// waits until the load balancer has admitted it, and the Preview is Ready
// only once the Pod's target-health readiness gate is True. envtest has no
// load balancer controller or kubelet; the test writes their status at
// exactly those edges.
func TestPreviewTargetHealthRuntime(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", "preview.patchy.example.com",
		"--preview-node-pool", "patchy-preview", "--preview-node-class", "patchy-preview",
		"--preview-taint-key", "patchy.devthe.net/preview-only",
		"--preview-poll-interval", "1s", "--preview-target-health")
	sha := strings.Repeat("a", 40)
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/demo",
				Port: 8080, ReadinessPath: "/health"},
		}}
	if err := cl.client.Create(ctx, project); err != nil {
		t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "demo",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{
		Repository: "https://github.com/acme/demo", Number: 1, State: "open", HeadSHA: sha,
	}}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo",
				ImageRepository: "registry.example/patchy/previews/demo", Revision: sha,
				Port: 8080, ReadinessPath: "/health"}}, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"}
	var ingress networkingv1.Ingress
	eventually(t, "the Ingress before any Pod", func() bool { return cl.client.Get(ctx, key, &ingress) == nil })
	time.Sleep(3 * time.Second) // several one-second polls
	if err := cl.client.Get(ctx, key, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Deployment created before the load balancer admitted the Ingress: %v", err)
	}
	ingress.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{Hostname: "preview.elb.example"}}
	if err := cl.client.Status().Update(ctx, &ingress); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	eventually(t, "the Deployment once the Ingress is admitted", func() bool { return cl.client.Get(ctx, key, &dep) == nil })
	const gate = corev1.PodConditionType("target-health.elbv2.k8s.aws/k8s-patchypr-previewd-0123456789")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preview-demo-1-pod", Namespace: key.Namespace,
		Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: gate}}
	if err := cl.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	// setHealth plays the load balancer reporting the target on the gate, the
	// kubelet holding Ready until the gate is True, and the Deployment
	// controller counting only a Ready Pod available.
	setHealth := func(health corev1.ConditionStatus) {
		t.Helper()
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: health}, {Type: gate, Status: health}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true, ImageID: "repo@sha256:123"}}
		if err := cl.client.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		if err := cl.client.Get(ctx, key, &dep); err != nil {
			t.Fatal(err)
		}
		ready := int32(0)
		if health == corev1.ConditionTrue {
			ready = 1
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, ready, ready
		if err := cl.client.Status().Update(ctx, &dep); err != nil {
			t.Fatal(err)
		}
	}
	setHealth(corev1.ConditionFalse)
	time.Sleep(3 * time.Second)
	var current v1alpha1.Preview
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Preview Ready while the load balancer target was unhealthy")
	}
	setHealth(corev1.ConditionTrue)
	eventually(t, "Ready once the target is healthy", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) == nil &&
			current.Status.Phase == v1alpha1.PreviewReady && current.Status.URL != ""
	})

	// A revision round pushes a new PR head whose runtime image the app's CI
	// has not published yet. The API server stores the rolling strategy, so
	// the Deployment controller keeps the serving Pod until the new one is
	// available; the Ingress stays; and the Preview reports the redeploy:
	// Deploying at the new head, with no URL.
	head := strings.Repeat("b", 40)
	headImage := "registry.example/patchy/previews/demo:sha-" + head
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.PullRequests[0].HeadSHA = head
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].Revision = head
	if err := cl.client.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a rolling redeploy to the new head, reported as Deploying", func() bool {
		if cl.client.Get(ctx, key, &dep) != nil || cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) != nil {
			return false
		}
		rolling := dep.Spec.Strategy.RollingUpdate
		return dep.Spec.Template.Spec.Containers[0].Image == headImage &&
			dep.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && rolling != nil &&
			rolling.MaxSurge != nil && rolling.MaxSurge.IntValue() == 1 &&
			rolling.MaxUnavailable != nil && rolling.MaxUnavailable.IntValue() == 0 &&
			current.Status.Phase == v1alpha1.PreviewDeploying && current.Status.URL == "" &&
			current.Status.ObservedRevision == head
	})
	if err := cl.client.Get(ctx, key, &ingress); err != nil {
		t.Fatalf("Ingress withdrawn by a redeploy with target health on: %v", err)
	}
	next := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preview-demo-1-next", Namespace: key.Namespace,
		Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
	next.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: gate}}
	if err := cl.client.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	// setNext plays the kubelet and the load balancer on the new Pod, and the
	// Deployment controller counting replicas: while the image is not
	// published, the old Pod is the one available beside the waiting new one;
	// once it is pulled and its target healthy, the old one is scaled down.
	setNext := func(pulled bool) {
		t.Helper()
		next.Status.Phase = corev1.PodPending
		next.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse}, {Type: gate, Status: corev1.ConditionFalse}}
		next.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Image: headImage,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
		replicas := int32(2)
		if pulled {
			next.Status.Phase = corev1.PodRunning
			next.Status.Conditions = []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: gate, Status: corev1.ConditionTrue}}
			next.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Image: headImage, Ready: true,
				ImageID: "repo@sha256:456", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			replicas = 1
		}
		if err := cl.client.Status().Update(ctx, next); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the Deployment's rollout status", func() bool {
			if cl.client.Get(ctx, key, &dep) != nil {
				return false
			}
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas, dep.Status.UpdatedReplicas = replicas, 1
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1
			return cl.client.Status().Update(ctx, &dep) == nil
		})
	}
	setNext(false)
	time.Sleep(3 * time.Second) // several one-second polls while the image is being published
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PreviewDeploying || current.Status.Retries != 0 || current.Status.URL != "" {
		t.Fatalf("status = %+v while the new head's image is not published, want Deploying with no retry",
			current.Status)
	}
	if err := cl.client.Get(ctx, key, &appsv1.Deployment{}); err != nil {
		t.Fatalf("Deployment gone while the previous revision served: %v", err)
	}
	setNext(true)
	eventually(t, "Ready at the new head once its target is healthy", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) == nil &&
			current.Status.Phase == v1alpha1.PreviewReady && current.Status.URL != "" &&
			len(current.Status.Components) == 1 && current.Status.Components[0].Revision == head &&
			current.Status.Components[0].ImageID == "repo@sha256:456"
	})
	// envtest has no Deployment controller or garbage collector to remove
	// the old revision's Pod or, later, the new one.
	if err := cl.client.Delete(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Preview finalizer and slot cleanup", func() bool {
		return apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{})) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &appsv1.Deployment{})) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &networkingv1.Ingress{}))
	})
}

// TestPreviewMultiComponentRuntime drives the shipped binary through a
// two-component Preview (slice 3): a web component at / and an API at /api,
// from a Project that also has a library it never previews. Each component
// gets its own Deployment and Service, with selectors that never reach a
// sibling's Pods; the one host is exposed only once both are Ready, with the
// deeper path first; and the finalizer removes every component's objects.
func TestPreviewMultiComponentRuntime(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", "preview.patchy.example.com",
		"--preview-node-pool", "patchy-preview", "--preview-node-class", "patchy-preview",
		"--preview-taint-key", "patchy.devthe.net/preview-only",
		"--preview-poll-interval", "1s")
	webSHA, apiSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	preview := func(leaf, readiness, path string) *v1alpha1.ProjectRepositoryPreview {
		return &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
			ImageRepository: "registry.example/patchy/previews/" + leaf, Port: 8080, ReadinessPath: readiness,
		}, Path: path}
	}
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories: []v1alpha1.ProjectRepository{
				{Name: "web", URL: "https://github.com/acme/Acme.Web_App", Preview: preview("acme-web", "/healthz", "/")},
				{Name: "lib", URL: "https://github.com/acme/lib"},
				{Name: "api", URL: "https://github.com/acme/api", Preview: preview("acme-api", "/api/healthz", "/api")},
			},
		}}
	if err := cl.client.Create(ctx, project); err != nil {
		t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "shop-1", Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "shop",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{
		{Repository: "https://github.com/acme/Acme.Web_App", Number: 1, State: "open", HeadSHA: webSHA},
		{Repository: "https://github.com/acme/api", Number: 2, State: "open", HeadSHA: apiSHA},
	}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	// The Preview the writer derives, through the same function the
	// controller re-derives it with.
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok || len(components) != 2 {
		t.Fatalf("derived %+v, %v; want two components", components, ok)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: in.Name, UID: in.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}}},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, HostLabel: in.Name,
			Components: components, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	slotKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: "patchy-preview-0", Name: name}
	}
	names := []string{"preview-shop-1", "preview-shop-1-api"}
	images := []string{"registry.example/patchy/previews/acme-web:sha-" + webSHA,
		"registry.example/patchy/previews/acme-api:sha-" + apiSHA}
	eventually(t, "a Deployment and Service per component in slot 0", func() bool {
		for i, name := range names {
			var dep appsv1.Deployment
			var svc corev1.Service
			if cl.client.Get(ctx, slotKey(name), &dep) != nil || cl.client.Get(ctx, slotKey(name), &svc) != nil ||
				dep.Spec.Template.Spec.Containers[0].Image != images[i] {
				return false
			}
		}
		return true
	})
	var web, api corev1.Service
	if err := cl.client.Get(ctx, slotKey(names[0]), &web); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, slotKey(names[1]), &api); err != nil {
		t.Fatal(err)
	}
	if web.Spec.Selector["patchy.bitwisemedia.uk/preview-component"] != "web" ||
		api.Spec.Selector["patchy.bitwisemedia.uk/preview-component"] != "api" {
		t.Fatalf("component selectors overlap: web %v, api %v", web.Spec.Selector, api.Spec.Selector)
	}
	pods := make([]*corev1.Pod, 0, len(names))
	ready := func(i int) {
		t.Helper()
		var dep appsv1.Deployment
		if err := cl.client.Get(ctx, slotKey(names[i]), &dep); err != nil {
			t.Fatal(err)
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
		if err := cl.client.Status().Update(ctx, &dep); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names[i] + "-pod", Namespace: "patchy-preview-0",
			Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
		if err := cl.client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: components[i].Name, Ready: true,
			ImageID: "repo@sha256:" + components[i].Name}}
		if err := cl.client.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pods = append(pods, pod)
	}
	ready(0)
	// With one of two components Ready the host stays withdrawn, through
	// several of the controller's one-second polls.
	time.Sleep(3 * time.Second)
	var ingress networkingv1.Ingress
	if err := cl.client.Get(ctx, slotKey("preview-shop-1"), &ingress); !apierrors.IsNotFound(err) {
		t.Fatalf("Ingress existed with one of two components Ready: %v", err)
	}
	ready(1)
	eventually(t, "a Ready two-component Preview behind one host, /api first", func() bool {
		var current v1alpha1.Preview
		if cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) != nil ||
			cl.client.Get(ctx, slotKey("preview-shop-1"), &ingress) != nil {
			return false
		}
		paths := ingress.Spec.Rules[0].HTTP.Paths
		return current.Status.Phase == v1alpha1.PreviewReady && len(current.Status.Components) == 2 &&
			current.Status.Components[1].Revision == apiSHA && len(paths) == 2 &&
			paths[0].Path == "/api" && paths[0].Backend.Service.Name == "preview-shop-1-api" &&
			paths[1].Path == "/" && paths[1].Backend.Service.Name == "preview-shop-1"
	})
	// envtest has no Deployment controller or garbage collector to remove
	// the synthetic Pods.
	for _, pod := range pods {
		if err := cl.client.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "every component's objects removed and the Preview finalized", func() bool {
		if !apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{})) ||
			!apierrors.IsNotFound(cl.client.Get(ctx, slotKey("preview-shop-1"), &networkingv1.Ingress{})) {
			return false
		}
		for _, name := range names {
			if !apierrors.IsNotFound(cl.client.Get(ctx, slotKey(name), &appsv1.Deployment{})) ||
				!apierrors.IsNotFound(cl.client.Get(ctx, slotKey(name), &corev1.Service{})) {
				return false
			}
		}
		return true
	})
}
