// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
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
