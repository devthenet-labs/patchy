// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

const heldNotice = "the agent recommended ignore; patchy did not dismiss the alert because the run used a " +
	"repository-declared image"

// heldIgnoreFinding is a finding whose investigation, run on a
// repository-declared image, recommended ignore: investigation-controller
// held it in HandedOff instead of dismissing it. Its alerts come from
// source (a GHAS alert for "ghas", a generic one otherwise), and its
// tracking issue 7 is open.
func heldIgnoreFinding(src string) *v1alpha1.Finding {
	fnd := projectable(v1alpha1.PhaseHandedOff)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{
		Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
	}
	fnd.Status.Investigation = &v1alpha1.InvestigationSummary{
		Name: "finding-aa-1-inv-1", Attempt: 1, Outcome: "ok",
		Recommendation: v1alpha1.RecommendationIgnore,
		RunnerImage: &v1alpha1.RunnerImageRef{
			Image:  "ghcr.io/acme/go-env@sha256:" + strings.Repeat("a", 64),
			Source: v1alpha1.RunnerImageSourceRepository, Manifest: ".patchy/agent.yaml",
		},
	}
	if src != "ghas" {
		fnd.Spec.Source = src
		fnd.Spec.Alerts = []v1alpha1.Alert{{ID: "wh-1001", Source: src, URL: "https://warehouse.internal/findings/1001"}}
	}
	return fnd
}

// TestProjectHeldIgnoreNotice: the hand-off notice says why an ignore
// verdict did not dismiss the finding, and nothing is written back to the
// scanner.
func TestProjectHeldIgnoreNotice(t *testing.T) {
	tracker := newFakeTracker()
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, _ := newProjector(t, tracker, testIntegration(), heldIgnoreFinding("ghas"))
	reconcileFinding(t, r)

	found := false
	for _, cm := range tracker.comments {
		if strings.Contains(cm, "handed this finding to its human owners") && strings.Contains(cm, heldNotice) {
			found = true
		}
	}
	if !found {
		t.Errorf("comments = %q, want the held-ignore hand-off notice", tracker.comments)
	}
	if len(tracker.dismissed) != 0 || len(tracker.closed) != 0 {
		t.Errorf("dismissed/closed = %v/%v, want the alert and issue left open", tracker.dismissed, tracker.closed)
	}
}

// TestClosingHeldFindingMakesNoResolverCall: a human closing the tracking
// issue of a held ignore changes nothing — HandedOff is terminal — and the
// projection that follows writes no dismissal back to the source, because
// write-back fires only on entry to Dismissed.
func TestClosingHeldFindingMakesNoResolverCall(t *testing.T) {
	integ := testGenericIntegration("warehouse", func(g *v1alpha1.GenericIntegration) {
		g.Source.Resolver = &v1alpha1.GenericResolver{Enabled: true, URL: "https://warehouse.internal/resolve"}
	})
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(testIntegration(), integ, heldIgnoreFinding("warehouse")).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithIndex(&v1alpha1.Finding{}, TrackingURLIndex, func(obj client.Object) []string {
			if f := obj.(*v1alpha1.Finding); f.Status.Tracking != nil {
				return []string{f.Status.Tracking.URL}
			}
			return nil
		}).
		Build()
	signals := &Signals{Client: c, Namespace: "patchy", Now: func() time.Time { return testClock }}
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := signals.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff || f.Status.Tracking.State != "closed" {
		t.Fatalf("phase/tracking = %q/%q, want HandedOff with the issue closed", f.Status.Phase, f.Status.Tracking.State)
	}

	tracker := newFakeTracker()
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "closed"}
	resolver := &fakeGenericResolver{}
	r := &FindingReconciler{
		Client:    c,
		Namespace: "patchy",
		Now:       func() time.Time { return testClock },
		ClientFor: func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
			return tracker, nil
		},
		GenericResolver: func(context.Context, *v1alpha1.Integration) (source.Resolver, error) {
			return resolver, nil
		},
	}
	reconcileFinding(t, r)
	reconcileFinding(t, r)

	if resolver.calls != 0 {
		t.Errorf("resolver called %d times, want none for a held finding", resolver.calls)
	}
	if got := get(t, c, "finding-aa-1").GetAnnotations()[AnnotationResolvedSource]; got != "" {
		t.Errorf("resolved-source annotation = %q, want none", got)
	}
}

// The runner-image comment fixtures: a finding with an open tracking issue
// whose Repository ("finding-aa-1-src", controlled by the finding) carries
// the given runner-image record and conditions.
const (
	sourceFindingUID = types.UID("finding-uid-1")
	pinnedImage      = "ghcr.io/acme/go-env@sha256:" + "abababababababababababababababababababababababababababababababab"
)

func imageFinding() *v1alpha1.Finding {
	fnd := projectable(v1alpha1.PhaseInvestigating)
	fnd.UID = sourceFindingUID
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{
		Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
	}
	return fnd
}

func imageRepository(ri *v1alpha1.RunnerImage, conds ...metav1.Condition) *v1alpha1.Repository {
	isController := true
	return &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finding-aa-1-src", Namespace: "patchy",
			Labels: map[string]string{v1alpha1.LabelFinding: "finding-aa-1"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(), Kind: "Finding", Name: "finding-aa-1",
				UID: sourceFindingUID, Controller: &isController,
			}},
		},
		Status: v1alpha1.RepositoryStatus{RunnerImage: ri, Conditions: conds},
	}
}

func acceptedImage() *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".patchy/agent.yaml", Image: pinnedImage, Verified: true,
	}
}

func imageInvestigation(attempt int32, src string, stage *v1alpha1.StageResult,
	conds ...metav1.Condition) *v1alpha1.Investigation {
	return &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("finding-aa-1-inv-%d", attempt), Namespace: "patchy",
			Labels: map[string]string{v1alpha1.LabelFinding: "finding-aa-1"},
		},
		Spec: v1alpha1.InvestigationSpec{
			FindingRef: v1alpha1.ObjectReference{Name: "finding-aa-1", UID: sourceFindingUID}, Attempt: attempt,
		},
		Status: v1alpha1.InvestigationStatus{
			RunnerImage: &v1alpha1.RunnerImageRef{Image: pinnedImage, Source: src, Manifest: ".patchy/agent.yaml"},
			Stage:       stage, Conditions: conds,
		},
	}
}

// imageProjector is newProjector with the runner-image comment switched on
// and an open issue 7 to project onto.
func imageProjector(t *testing.T, objs ...client.Object) (*FindingReconciler, client.Client, *fakeTracker) {
	t.Helper()
	tracker := newFakeTracker()
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, c := newProjector(t, tracker, append([]client.Object{testIntegration()}, objs...)...)
	r.RunnerImages = true
	return r, c, tracker
}

// runnerImageComments returns issue 7's runner-image sticky comments.
func runnerImageComments(tracker *fakeTracker) []*ghclient.Comment {
	var out []*ghclient.Comment
	for _, cm := range tracker.issueComments[7] {
		if strings.HasPrefix(cm.Body, templates.RunnerImageMarker+"\n") {
			out = append(out, cm)
		}
	}
	return out
}

// TestProjectRunnerImageStickyComment: the comment is posted once the
// Repository records a declaration, left alone while nothing changes, and
// edited in place — never re-posted — when a run on the image adds to it.
func TestProjectRunnerImageStickyComment(t *testing.T) {
	r, c, tracker := imageProjector(t, imageFinding(), imageRepository(acceptedImage()))

	reconcileFinding(t, r)
	got := runnerImageComments(tracker)
	if len(got) != 1 {
		t.Fatalf("runner-image comments = %d, want 1 after the first projection", len(got))
	}
	for _, want := range []string{"`.patchy/agent.yaml` declares `ghcr.io/acme/go-env:1.26`", pinnedImage} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("comment missing %q:\n%s", want, got[0].Body)
		}
	}
	if ann := get(t, c, "finding-aa-1").GetAnnotations()[AnnotationProjectedRunnerImage]; ann == "" {
		t.Error("projected-runner-image annotation not recorded")
	}

	posted, edits := len(tracker.comments), tracker.commentEdits
	reconcileFinding(t, r)
	if len(tracker.comments) != posted || tracker.commentEdits != edits {
		t.Errorf("an unchanged record re-projected: comments %d->%d, edits %d->%d",
			posted, len(tracker.comments), edits, tracker.commentEdits)
	}

	// The investigation on the image fails its preflight.
	inv := imageInvestigation(1, v1alpha1.RunnerImageSourceRepository, &v1alpha1.StageResult{
		Outcome: "image_incompatible", Detail: "preflight: bash -c true: executable file not found in $PATH",
	})
	if err := c.Create(t.Context(), inv); err != nil {
		t.Fatal(err)
	}
	reconcileFinding(t, r)
	got = runnerImageComments(tracker)
	if len(got) != 1 || tracker.commentEdits != edits+1 {
		t.Fatalf("runner-image comments = %d, edits = %d; want the one comment edited in place", len(got),
			tracker.commentEdits-edits)
	}
	for _, want := range []string{"investigation (attempt 1)", "`image_incompatible`",
		"executable file not found in $PATH"} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("edited comment missing %q:\n%s", want, got[0].Body)
		}
	}
}

// TestProjectRunnerImageOutcomes: each Repository record projects the
// outcome a repository owner needs to read — and nothing at all when the
// repository declared nothing or the comment is switched off.
func TestProjectRunnerImageOutcomes(t *testing.T) {
	stalled := metav1.Condition{Type: v1alpha1.ConditionStalled, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonRunnerImageRejected, Message: "not allowlisted"}
	rejected := &v1alpha1.RunnerImage{
		Declared: "docker.io/library/golang:1.26", Manifest: ".patchy/agent.yaml", Rejected: "NotAllowlisted",
		Message: "image `docker.io/library/golang:1.26` is not under an allowlisted registry path (ghcr.io/acme/)",
	}
	refused := metav1.Condition{Type: v1alpha1.ConditionSandboxRefused, Status: metav1.ConditionTrue,
		Reason: "SandboxUnenforced"}
	cases := []struct {
		name     string
		objs     []client.Object
		disabled bool
		want     []string // substrings; nil means no comment at all
		unwanted []string
	}{
		{"nothing declared", []client.Object{imageRepository(nil)}, false, nil, nil},
		{"no repository yet", nil, false, nil, nil},
		{"switched off", []client.Object{imageRepository(acceptedImage())}, true, nil, nil},
		{"devcontainer image", []client.Object{imageRepository(&v1alpha1.RunnerImage{
			Declared: "ghcr.io/acme/dev-env:2", Manifest: ".devcontainer/devcontainer.json", Image: pinnedImage,
		})}, false, []string{"`.devcontainer/devcontainer.json` declares `ghcr.io/acme/dev-env:2`"}, []string{"verified"}},
		{"build-based devcontainer", []client.Object{imageRepository(&v1alpha1.RunnerImage{
			Manifest: ".devcontainer/devcontainer.json",
			Message:  "`.devcontainer/devcontainer.json` builds its image (`build`); patchy does not build images.",
		})}, false, []string{"patchy did not use `.devcontainer/devcontainer.json`", "builds its image",
			"default runner image instead"}, []string{"parked"}},
		{"rejected under default", []client.Object{imageRepository(rejected)}, false,
			[]string{"could not use `docker.io/library/golang:1.26`", "(`NotAllowlisted`)",
				"not under an allowlisted registry path", "default runner image instead",
				"`patchy check image docker.io/library/golang:1.26`"}, []string{"parked"}},
		// The gate parks the finding before any investigation exists, and an
		// approval revives only a finding that has one, so the comment must
		// not offer /approve as a way out.
		{"rejected under handoff", []client.Object{imageRepository(rejected, stalled)}, false,
			[]string{"(`NotAllowlisted`)", "parked this finding for a human", "`onReject: handoff`",
				"not investigate this finding", "the fix applies to the next finding"},
			[]string{"default runner image instead", "approve"}},
		{"sandbox refused", []client.Object{imageRepository(acceptedImage()),
			imageInvestigation(1, v1alpha1.RunnerImageSourceRepository,
				&v1alpha1.StageResult{Outcome: "aborted", Detail: "SandboxUnenforced: ..."}, refused)}, false,
			[]string{"investigation (attempt 1) was refused", "`SandboxUnenforced`", "This is not a problem"},
			[]string{"image_incompatible"}},
		// A failure on the default image says nothing about the declared one.
		{"incompatible on the default image", []client.Object{imageRepository(acceptedImage()),
			imageInvestigation(1, v1alpha1.RunnerImageSourceDefault,
				&v1alpha1.StageResult{Outcome: "image_incompatible", Detail: "no bash"})}, false,
			[]string{"declares `ghcr.io/acme/go-env:1.26`"}, []string{"image_incompatible", "no bash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, tracker := imageProjector(t, append([]client.Object{imageFinding()}, tc.objs...)...)
			r.RunnerImages = !tc.disabled
			reconcileFinding(t, r)
			got := runnerImageComments(tracker)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("runner-image comment posted, want none:\n%s", got[0].Body)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("runner-image comments = %d, want 1", len(got))
			}
			for _, want := range tc.want {
				if !strings.Contains(got[0].Body, want) {
					t.Errorf("comment missing %q:\n%s", want, got[0].Body)
				}
			}
			for _, unwanted := range tc.unwanted {
				if strings.Contains(got[0].Body, unwanted) {
					t.Errorf("comment contains %q:\n%s", unwanted, got[0].Body)
				}
			}
		})
	}
}

// TestProjectRunnerImageIgnoresForeignRepository: a Repository carrying the
// finding's label but controlled by another finding of the same name (one
// deleted and re-created) is never quoted.
func TestProjectRunnerImageIgnoresForeignRepository(t *testing.T) {
	foreign := imageRepository(acceptedImage())
	foreign.OwnerReferences[0].UID = "an-earlier-finding"
	r, _, tracker := imageProjector(t, imageFinding(), foreign)
	reconcileFinding(t, r)
	if got := runnerImageComments(tracker); len(got) != 0 {
		t.Fatalf("comment projected from a foreign Repository:\n%s", got[0].Body)
	}
}
