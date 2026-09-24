// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/go-github/v90/github"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// fakeResetClient records the demo reset's forge calls.
type fakeResetClient struct {
	deleted      []string
	closed       []string
	opened       []string
	failDelete   bool
	unauthorized bool
	gone         bool // every call answers 404, as a deleted repository does
}

func (f *fakeResetClient) DeleteIssue(_ context.Context, repo ghclient.Repo, number int) error {
	if f.failDelete {
		return errors.New("boom")
	}
	if f.gone {
		return notFoundErr(fmt.Sprintf("delete issue %s#%d", repo, number))
	}
	if f.unauthorized {
		return fmt.Errorf("delete issue %s#%d: %w", repo, number, ghclient.ErrDeleteUnauthorized)
	}
	f.deleted = append(f.deleted, fmt.Sprintf("%s#%d", repo, number))
	return nil
}

func (f *fakeResetClient) Close(_ context.Context, repo ghclient.Repo, number int) error {
	f.closed = append(f.closed, fmt.Sprintf("%s#%d", repo, number))
	return nil
}

func (f *fakeResetClient) OpenAlert(_ context.Context, repo ghclient.Repo, number int) error {
	if f.gone {
		return notFoundErr(fmt.Sprintf("open alert %s#%d", repo, number))
	}
	f.opened = append(f.opened, fmt.Sprintf("%s#%d", repo, number))
	return nil
}

// notFoundErr builds the wrapped GitHub 404 a call against a deleted or
// recreated repository returns.
func notFoundErr(op string) error {
	return fmt.Errorf("ghclient: %s: %w", op, &github.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  "Not Found",
	})
}

func newResetReconciler(
	t *testing.T, fc *fakeResetClient, objs ...client.Object,
) (*IntegrationReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Integration{}).
		Build()
	r := &IntegrationReconciler{
		Client: c,
		ClientFor: func(context.Context, *v1alpha1.Integration, ghclient.Repo) (resetClient, error) {
			return fc, nil
		},
	}
	return r, c
}

func TestRunReset(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseDismissed)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	untracked := projectable(v1alpha1.PhaseOpened)
	untracked.Name = "finding-bb-1"

	fc := &fakeResetClient{}
	r, c := newResetReconciler(t, fc,
		testIntegration(), tracked, untracked,
		&v1alpha1.Investigation{ObjectMeta: metav1.ObjectMeta{Name: "inv-1", Namespace: "patchy"}},
		&v1alpha1.Remediation{ObjectMeta: metav1.ObjectMeta{Name: "rem-1", Namespace: "patchy"}},
		findingRepository("repo-1", "finding-aa-1"),
		&v1alpha1.FindingRollup{ObjectMeta: metav1.ObjectMeta{Name: "total", Namespace: "patchy"}},
	)

	if err := r.runReset(t.Context(), "patchy"); err != nil {
		t.Fatalf("runReset() error = %v", err)
	}

	if want := []string{"acme/orders#7"}; !slices.Equal(fc.deleted, want) {
		t.Errorf("deleted issues = %v, want %v", fc.deleted, want)
	}
	// Only the dismissed finding's own alert (#42) is restored — never a
	// repository-wide sweep, and never alerts of non-dismissed findings.
	if want := []string{"acme/orders#42"}; !slices.Equal(fc.opened, want) {
		t.Errorf("reopened alerts = %v, want %v", fc.opened, want)
	}

	for _, list := range []client.ObjectList{
		&v1alpha1.FindingList{}, &v1alpha1.InvestigationList{}, &v1alpha1.RemediationList{},
		&v1alpha1.RepositoryList{}, &v1alpha1.FindingRollupList{},
	} {
		if err := c.List(t.Context(), list, client.InNamespace("patchy")); err != nil {
			t.Fatalf("list %T: %v", list, err)
		}
		if n := len(listItems(t, list)); n != 0 {
			t.Errorf("%T holds %d items after reset, want 0", list, n)
		}
	}
	var integs v1alpha1.IntegrationList
	if err := c.List(t.Context(), &integs, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list integrations: %v", err)
	}
	if len(integs.Items) != 1 {
		t.Errorf("integrations = %d, want the configuration untouched", len(integs.Items))
	}
}

// findingRepository is a Repository the investigation gate created for
// finding: it carries the Finding label.
func findingRepository(name, finding string) *v1alpha1.Repository {
	return &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "patchy", Labels: map[string]string{v1alpha1.LabelFinding: finding},
	}}
}

// TestRunResetLeavesIntentFlow: the intent flow shares the namespace, and a
// demo reset is the Finding flow's. Its Projects, Intents, IntentRuns and
// Repositories survive (intent Repositories never carry the Finding label,
// and a later round pins its image to the intent's first Repository by
// UID), and the only issue it touches is the finding's own tracking issue:
// never an intent's, which a human wrote.
func TestRunResetLeavesIntentFlow(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseOpened)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	intentRepo := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
		Name: "web-12-r0", Namespace: "patchy",
		Labels: map[string]string{v1alpha1.LabelIntent: "web-12", v1alpha1.LabelIntentRun: "web-12-plan-1"},
	}}
	intent := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "web-12", Namespace: "patchy"},
		Spec: v1alpha1.IntentSpec{
			Project: "web",
			Issue:   v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 12},
		},
	}
	fc := &fakeResetClient{}
	r, c := newResetReconciler(t, fc, testIntegration(), tracked,
		findingRepository("finding-aa-1", "finding-aa-1"), intentRepo, intent,
		&v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "web-12-plan-1", Namespace: "patchy"}},
		&v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "patchy"}},
	)

	if err := r.runReset(t.Context(), "patchy"); err != nil {
		t.Fatalf("runReset() error = %v", err)
	}

	if touched := append(slices.Clone(fc.deleted), fc.closed...); !slices.Equal(touched, []string{"acme/orders#7"}) {
		t.Errorf("issues deleted or closed = %v, want only the finding's tracking issue", touched)
	}
	var repos v1alpha1.RepositoryList
	if err := c.List(t.Context(), &repos, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list repositories: %v", err)
	}
	if len(repos.Items) != 1 || repos.Items[0].Name != intentRepo.Name {
		t.Errorf("repositories after reset = %v, want only the intent's", repos.Items)
	}
	for _, list := range []client.ObjectList{
		&v1alpha1.IntentList{}, &v1alpha1.IntentRunList{}, &v1alpha1.ProjectList{},
	} {
		if err := c.List(t.Context(), list, client.InNamespace("patchy")); err != nil {
			t.Fatalf("list %T: %v", list, err)
		}
		if n := len(listItems(t, list)); n != 1 {
			t.Errorf("%T holds %d items after reset, want 1 (untouched)", list, n)
		}
	}
}

func TestRunResetClosesWhenDeleteUnauthorized(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseDismissed)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	fc := &fakeResetClient{unauthorized: true}
	r, c := newResetReconciler(t, fc, testIntegration(), tracked)

	if err := r.runReset(t.Context(), "patchy"); err != nil {
		t.Fatalf("runReset() error = %v, want the close fallback to succeed", err)
	}
	if len(fc.deleted) != 0 {
		t.Errorf("deleted issues = %v, want none", fc.deleted)
	}
	if want := []string{"acme/orders#7"}; !slices.Equal(fc.closed, want) {
		t.Errorf("closed issues = %v, want %v", fc.closed, want)
	}

	var findings v1alpha1.FindingList
	if err := c.List(t.Context(), &findings, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings.Items) != 0 {
		t.Errorf("findings = %d after reset, want 0", len(findings.Items))
	}
}

// The benchmark repositories a demo runs against get deleted and recreated
// between runs, so the issues and alerts a reset cleans up may be gone. The
// pipeline resources must still be deleted.
func TestRunResetIgnoresMissingForgeObjects(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseDismissed)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	fc := &fakeResetClient{gone: true}
	r, c := newResetReconciler(t, fc, testIntegration(), tracked)

	if err := r.runReset(t.Context(), "patchy"); err != nil {
		t.Fatalf("runReset() error = %v, want the missing issue and alert ignored", err)
	}

	var findings v1alpha1.FindingList
	if err := c.List(t.Context(), &findings, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings.Items) != 0 {
		t.Errorf("findings = %d after reset, want 0", len(findings.Items))
	}
}

// A repository that is gone takes its credential with it: the App
// installation lookup 404s before any issue or alert call is made.
func TestRunResetIgnoresMissingRepository(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseDismissed)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	r, c := newResetReconciler(t, &fakeResetClient{}, testIntegration(), tracked)
	r.ClientFor = func(_ context.Context, _ *v1alpha1.Integration, repo ghclient.Repo) (resetClient, error) {
		return nil, notFoundErr(fmt.Sprintf("resolve installation for %s", repo))
	}

	if err := r.runReset(t.Context(), "patchy"); err != nil {
		t.Fatalf("runReset() error = %v, want the missing repository ignored", err)
	}

	var findings v1alpha1.FindingList
	if err := c.List(t.Context(), &findings, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings.Items) != 0 {
		t.Errorf("findings = %d after reset, want 0", len(findings.Items))
	}
}

func TestRunResetKeepsStateOnForgeFailure(t *testing.T) {
	tracked := projectable(v1alpha1.PhaseDismissed)
	tracked.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7}
	fc := &fakeResetClient{failDelete: true}
	r, c := newResetReconciler(t, fc, testIntegration(), tracked)

	if err := r.runReset(t.Context(), "patchy"); err == nil {
		t.Fatal("runReset() = nil, want the forge failure surfaced")
	}

	// The Findings carry the issue numbers a retry needs; they must survive.
	var findings v1alpha1.FindingList
	if err := c.List(t.Context(), &findings, client.InNamespace("patchy")); err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Errorf("findings = %d after failed reset, want 1 (kept for retry)", len(findings.Items))
	}
}

func TestConsumeResetDropsDedupAndEchoes(t *testing.T) {
	fc := &fakeResetClient{}
	r, _ := newResetReconciler(t, fc, testIntegration())
	dropped := false
	r.ResetDedup = func() { dropped = true }

	integ := testIntegration()
	at := metav1.Now()
	if err := r.consumeReset(t.Context(), integ, &v1alpha1.ActionRequest{By: "op", At: at}); err != nil {
		t.Fatalf("consumeReset() error = %v", err)
	}
	if !dropped {
		t.Error("dedup window not dropped")
	}
	if integ.Status.ResetAt == nil || !integ.Status.ResetAt.Equal(&at) {
		t.Errorf("status.resetAt = %v, want the request echoed", integ.Status.ResetAt)
	}
}

// listItems extracts the items of any ObjectList via the meta accessor.
func listItems(t *testing.T, list client.ObjectList) []client.Object {
	t.Helper()
	items, err := meta.ExtractList(list)
	if err != nil {
		t.Fatalf("extract list: %v", err)
	}
	out := make([]client.Object, 0, len(items))
	for _, it := range items {
		out = append(out, it.(client.Object))
	}
	return out
}
