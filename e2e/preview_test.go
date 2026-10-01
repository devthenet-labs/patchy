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
