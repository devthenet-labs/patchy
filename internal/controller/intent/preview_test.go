// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

func TestPreviewSourceUsesOnlyProjectAndRecordedPRHead(t *testing.T) {
	ctx := context.Background()
	const repo = "https://github.com/acme/preview-demo"
	project := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "patchy"},
		Spec: v1alpha1.ProjectSpec{
			Repositories: []v1alpha1.ProjectRepository{{Name: "demo", URL: repo}},
			Preview: &v1alpha1.ProjectPreview{
				ImageRepository: "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/preview-demo",
				Port:            8080, ReadinessPath: "/health",
			},
		},
	}
	sha := strings.Repeat("a", 40)
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: "patchy", UID: types.UID("intent-uid")},
		Spec: v1alpha1.IntentSpec{Project: project.Name,
			Issue: v1alpha1.IntentIssue{URL: "https://github.com/acme/intents/issues/1"}},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: repo, State: "open", HeadSHA: sha}}},
	}
	scheme := kube.Scheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(in).WithObjects(project, in).Build()
	r := &PreviewSourceReconciler{Client: c, APIReader: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: in.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	preview := &v1alpha1.Preview{}
	if err := c.Get(ctx, req.NamespacedName, preview); err != nil {
		t.Fatal(err)
	}
	if preview.Spec.IntentRef.UID != in.UID || preview.Spec.Components[0].Revision != sha ||
		preview.Spec.Components[0].ImageRepository != project.Spec.Preview.ImageRepository ||
		preview.Spec.HostLabel != in.Name || preview.Spec.TTL.Duration != 72*time.Hour ||
		len(preview.OwnerReferences) != 1 || preview.OwnerReferences[0].UID != in.UID {
		t.Fatalf("preview spec = %+v, owner references %+v", preview.Spec, preview.OwnerReferences)
	}
	// Only an observed PR-head change moves the runtime tag; changing the
	// issue body or agent output is not a source for this spec.
	in.Status.PullRequests[0].HeadSHA = strings.Repeat("b", 40)
	if err := c.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, preview); err != nil {
		t.Fatal(err)
	}
	if preview.Spec.Components[0].Revision != strings.Repeat("b", 40) {
		t.Fatalf("PR-head update did not change revision: %+v", preview.Spec.Components)
	}
	// The operator can disable previews for this Project without affecting
	// the Intent's own work; its Preview is deleted for finalizer cleanup.
	project.Spec.Preview = nil
	if err := c.Update(ctx, project); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, preview); err == nil {
		t.Fatal("opted-out Project kept its Preview")
	}
}

func TestPreviewSourceRefusesUnrecordedOrClosedPR(t *testing.T) {
	project := &v1alpha1.Project{Spec: v1alpha1.ProjectSpec{
		Repositories: []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
		Preview: &v1alpha1.ProjectPreview{
			ImageRepository: "registry.example/patchy/previews/demo", Port: 8080, ReadinessPath: "/",
		},
	}}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", UID: "intent-uid"},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: project.Spec.Repositories[0].URL,
				State: "open", HeadSHA: "short"}}}}
	if _, ok := desiredPreview(in, project); ok {
		t.Fatal("short SHA became a preview")
	}
	in.Status.PullRequests[0].HeadSHA = strings.Repeat("a", 40)
	in.Status.PullRequests[0].State = "closed"
	if _, ok := desiredPreview(in, project); ok {
		t.Fatal("closed PR became a preview")
	}
	in.Status.PullRequests[0].State = "open"
	in.Status.PullRequests[0].Repository = "https://github.com/acme/foreign"
	if _, ok := desiredPreview(in, project); ok {
		t.Fatal("foreign PR became a preview")
	}
	project.Spec.Preview = nil
	in.Status.PullRequests[0].Repository = project.Spec.Repositories[0].URL
	if _, ok := desiredPreview(in, project); ok {
		t.Fatal("non-opted-in Project became a preview")
	}
}
