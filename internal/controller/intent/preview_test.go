// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"reflect"
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

// The writer derives a Preview exactly when preview-controller would accept
// it (v1alpha1.DesiredPreviewComponents, the one derivation both read).
// Regression: a writer of its own created a Preview for an Intent blocked
// before review, which preview-controller then deleted, and the writer,
// requeued a second later, created again — a create/delete loop for as long
// as the block lasted. It also never previewed a one-repository Project
// written in the per-repository form.
func TestPreviewSourceSharesTheControllerDerivation(t *testing.T) {
	ctx := context.Background()
	const repo = "https://github.com/acme/preview-demo"
	sha := strings.Repeat("a", 40)
	contract := v1alpha1.ProjectPreview{
		ImageRepository: "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/preview-demo",
		Port:            8080, ReadinessPath: "/health",
	}
	shorthand := func() *v1alpha1.Project {
		return &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "patchy"},
			Spec: v1alpha1.ProjectSpec{Repositories: []v1alpha1.ProjectRepository{{Name: "demo", URL: repo}},
				Preview: &contract}}
	}
	perRepository := func(path string) *v1alpha1.Project {
		p := shorthand()
		p.Spec.Preview = nil
		p.Spec.Repositories[0].Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: contract, Path: path}
		return p
	}
	blockedFrom := func(from v1alpha1.IntentPhase) func(*v1alpha1.Intent) {
		return func(in *v1alpha1.Intent) {
			in.Status.Phase = v1alpha1.IntentBlocked
			in.Status.PhaseTimes = []v1alpha1.IntentPhaseTime{{Phase: from}, {Phase: v1alpha1.IntentBlocked}}
		}
	}
	for _, tc := range []struct {
		name    string
		project *v1alpha1.Project
		mutate  func(*v1alpha1.Intent)
		want    bool
		path    string
	}{
		{"in review", shorthand(), func(*v1alpha1.Intent) {}, true, ""},
		{"blocked from review", shorthand(), blockedFrom(v1alpha1.IntentInReview), true, ""},
		{"blocked from revising", shorthand(), blockedFrom(v1alpha1.IntentRevising), true, ""},
		{"blocked from building", shorthand(), blockedFrom(v1alpha1.IntentBuilding), false, ""},
		{"blocked with no history", shorthand(), blockedFrom(v1alpha1.IntentBlocked), false, ""},
		{"the per-repository form at the root", perRepository(""), func(*v1alpha1.Intent) {}, true, ""},
		{"the per-repository form under a path", perRepository("/app"), func(*v1alpha1.Intent) {}, true, "/app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &v1alpha1.Intent{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: "patchy", UID: types.UID("intent-uid")},
				Spec:       v1alpha1.IntentSpec{Project: tc.project.Name},
				Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
					PullRequests: []v1alpha1.IntentPullRequest{{Repository: repo, State: "open", HeadSHA: sha}}},
			}
			tc.mutate(in)
			want, wantOK := v1alpha1.DesiredPreviewComponents(tc.project, in)
			if wantOK != tc.want {
				t.Fatalf("the shared derivation says %v, the case expects %v", wantOK, tc.want)
			}
			scheme := kube.Scheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(in).
				WithObjects(tc.project, in).Build()
			r := &PreviewSourceReconciler{Client: c, APIReader: c, Scheme: scheme}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: in.Name}}
			for range 2 { // a second pass is where a churning writer would recreate
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			var list v1alpha1.PreviewList
			if err := c.List(ctx, &list); err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if len(list.Items) != 0 {
					t.Fatalf("wrote a Preview preview-controller would delete: %+v", list.Items[0].Spec)
				}
				return
			}
			if len(list.Items) != 1 || !reflect.DeepEqual(list.Items[0].Spec.Components, want) {
				t.Fatalf("previews = %+v, want one with components %+v", list.Items, want)
			}
			if got := list.Items[0].Spec.Components[0]; got.Revision != sha || got.Path != tc.path ||
				got.ImageRepository != contract.ImageRepository {
				t.Errorf("component = %+v, want revision %s at path %q", got, sha, tc.path)
			}
		})
	}
}
