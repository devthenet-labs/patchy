// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// laggingCache stands in for an informer cache that has fallen behind the
// reconciler's own writes. It snapshots the Finding every time the tracker
// takes a post; once lagging, every Get of the Finding returns the last
// snapshot — the Finding without anything the projection wrote after that
// post — until the reconciler next writes, whose conflict is when a real
// reconciler would re-read and find the newer object. Every other read
// passes through. So a projection can learn what it already posted only by
// reading past the cache, through its APIReader.
type laggingCache struct {
	client.Client
	key      client.ObjectKey
	snapshot *v1alpha1.Finding
	lagging  bool
}

// posted snapshots the Finding as the tracker takes a post.
func (l *laggingCache) posted() {
	var f v1alpha1.Finding
	if err := l.Client.Get(context.Background(), l.key, &f); err == nil {
		l.snapshot = &f
	}
}

func (l *laggingCache) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if f, ok := obj.(*v1alpha1.Finding); ok && l.lagging && key == l.key {
		l.snapshot.DeepCopyInto(f)
		return nil
	}
	return l.Client.Get(ctx, key, obj, opts...)
}

func (l *laggingCache) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	l.lagging = false
	return l.Client.Update(ctx, obj, opts...)
}

func (l *laggingCache) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	l.lagging = false
	return l.Client.Patch(ctx, obj, patch, opts...)
}

func (l *laggingCache) Status() client.SubResourceWriter {
	return &laggingStatus{SubResourceWriter: l.Client.Status(), cache: l}
}

// laggingStatus is the status writer of a laggingCache: its writes catch the
// cache up, as the main resource's do.
type laggingStatus struct {
	client.SubResourceWriter
	cache *laggingCache
}

func (s *laggingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.cache.lagging = false
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

func (s *laggingStatus) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
) error {
	s.cache.lagging = false
	return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// linkedFinding is a projectable finding at phase, accumulated, whose
// tracking issue 7 is open.
func linkedFinding(phase v1alpha1.Phase) *v1alpha1.Finding {
	fnd := projectable(phase)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{
		Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
	}
	meta.SetStatusCondition(&fnd.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionAccumulationComplete, Status: metav1.ConditionTrue, Reason: "WindowElapsed",
	})
	return fnd
}

// reportedRemediation is attempt 1's Remediation, finished with a report.
func reportedRemediation() *v1alpha1.Remediation {
	return &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finding-aa-1-rem-1", Namespace: "patchy",
			Labels: map[string]string{v1alpha1.LabelFinding: "finding-aa-1"},
		},
		Spec:   v1alpha1.RemediationSpec{FindingRef: v1alpha1.ObjectReference{Name: "finding-aa-1"}, Attempt: 1},
		Status: v1alpha1.RemediationStatus{Report: "Bumped the dependency to the patched release."},
	}
}

// countComments counts the comments posted on issue 7 containing needle.
func countComments(tracker *fakeTracker, needle string) int {
	n := 0
	for _, cm := range tracker.issueComments[7] {
		if strings.Contains(cm.Body, needle) {
			n++
		}
	}
	return n
}

// TestProjectLaggingCacheDoesNotRepost is the duplicate-comment regression:
// a projection posts, and the reconcile right behind it reads the Finding
// from a cache that has seen none of that projection's writes after the
// post, while GitHub's comment list does not yet show the new comment
// either. The second reconcile must still post nothing — and open no second
// issue.
func TestProjectLaggingCacheDoesNotRepost(t *testing.T) {
	tests := []struct {
		name  string
		objs  func() []client.Object
		count func(*fakeTracker) int
	}{
		{
			name: "remediation report",
			objs: func() []client.Object {
				fnd := linkedFinding(v1alpha1.PhaseInReview)
				fnd.Status.Remediation = &v1alpha1.RemediationSummary{
					Name: "finding-aa-1-rem-1", Attempt: 1, Outcome: "ok", Success: true,
				}
				return []client.Object{fnd, reportedRemediation()}
			},
			count: func(tr *fakeTracker) int { return countComments(tr, "<!-- patchy:report Remediation/1 -->") },
		},
		{
			name: "enrichment sticky",
			objs: func() []client.Object {
				fnd := linkedFinding(v1alpha1.PhaseEnhanced)
				fnd.Status.Enrichments = []v1alpha1.Enrichment{{
					Enhancer: "static-context", Markdown: "owned by team-payments", AppliedAt: metav1.NewTime(testClock),
				}}
				return []client.Object{fnd}
			},
			count: func(tr *fakeTracker) int { return countComments(tr, "patchy:enrichment static-context") },
		},
		{
			name:  "failed notice",
			objs:  func() []client.Object { return []client.Object{linkedFinding(v1alpha1.PhaseFailed)} },
			count: func(tr *fakeTracker) int { return countComments(tr, "could not remediate this finding") },
		},
		{
			name:  "approval notice",
			objs:  func() []client.Object { return []client.Object{linkedFinding(v1alpha1.PhaseAwaitingApproval)} },
			count: func(tr *fakeTracker) int { return countComments(tr, "holding this remediation") },
		},
		{
			name: "tracking issue",
			objs: func() []client.Object { return []client.Object{projectable(v1alpha1.PhaseOpened)} },
			count: func(tr *fakeTracker) int {
				n := 0
				for _, is := range tr.issues {
					if strings.Contains(is.Body, "patchy:finding patchy/finding-aa-1") {
						n++
					}
				}
				return n
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newFakeTracker()
			tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
			tracker.nextNumber = 8
			tracker.listLag = true
			r, c := newProjector(t, tracker, append([]client.Object{testIntegration()}, tt.objs()...)...)
			cache := &laggingCache{Client: c, key: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
			r.Client, r.APIReader = cache, c
			tracker.onPost = cache.posted

			reconcileFinding(t, r)
			if got := tt.count(tracker); got != 1 {
				t.Fatalf("after the first projection: %d, want 1", got)
			}

			cache.lagging = true
			reconcileFinding(t, r)
			if got := tt.count(tracker); got != 1 {
				t.Errorf("after a projection on a lagging cache: %d, want still 1; comments = %q", got, tracker.comments)
			}
		})
	}
}

// TestProjectTrackedComments: a marker-headed comment is recorded by id on
// status.tracking.comments, edited by that id without listing the issue's
// comments, adopted by marker when no id is recorded, and posted afresh —
// keeping the issue linked — when the recorded one was deleted.
func TestProjectTrackedComments(t *testing.T) {
	const marker = "<!-- patchy:enrichment static-context -->"
	enriched := func(markdown string) *v1alpha1.Finding {
		fnd := linkedFinding(v1alpha1.PhaseEnhanced)
		fnd.Status.Enrichments = []v1alpha1.Enrichment{{
			Enhancer: "static-context", Markdown: markdown, AppliedAt: metav1.NewTime(testClock),
		}}
		return fnd
	}
	tests := []struct {
		name string
		// fnd is the finding; existing are the comments already on issue 7.
		fnd      func() *v1alpha1.Finding
		existing []*ghclient.Comment
		// Wanted afterwards.
		posts, edits, lists int
		recorded            int64
	}{
		{
			name:     "first projection posts and records",
			fnd:      func() *v1alpha1.Finding { return enriched("owned by team-payments") },
			posts:    1,
			lists:    1,
			recorded: 1,
		},
		{
			name: "recorded id is edited without listing",
			fnd: func() *v1alpha1.Finding {
				fnd := enriched("owned by team-checkout")
				fnd.Status.Tracking.Comments = []v1alpha1.TrackedComment{{Marker: marker, ID: 40, Digest: "stale"}}
				return fnd
			},
			existing: []*ghclient.Comment{{ID: 40, Body: marker + "\nowned by team-payments"}},
			edits:    1,
			recorded: 40,
		},
		{
			name:     "unrecorded comment is adopted by marker",
			fnd:      func() *v1alpha1.Finding { return enriched("owned by team-checkout") },
			existing: []*ghclient.Comment{{ID: 40, Body: marker + "\nowned by team-payments"}},
			edits:    1,
			lists:    1,
			recorded: 40,
		},
		{
			name: "deleted comment is posted afresh",
			fnd: func() *v1alpha1.Finding {
				fnd := enriched("owned by team-checkout")
				fnd.Status.Tracking.Comments = []v1alpha1.TrackedComment{{Marker: marker, ID: 40, Digest: "stale"}}
				return fnd
			},
			posts:    1,
			lists:    1,
			recorded: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newFakeTracker()
			tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
			tracker.issueComments[7] = tt.existing
			r, c := newProjector(t, tracker, testIntegration(), tt.fnd())

			reconcileFinding(t, r)

			if len(tracker.comments) != tt.posts || tracker.commentEdits != tt.edits || tracker.lists != tt.lists {
				t.Errorf("posts/edits/lists = %d/%d/%d, want %d/%d/%d",
					len(tracker.comments), tracker.commentEdits, tracker.lists, tt.posts, tt.edits, tt.lists)
			}
			if got := countComments(tracker, "patchy:enrichment static-context"); got != 1 {
				t.Errorf("enrichment comments on the issue = %d, want 1", got)
			}
			tr := get(t, c, "finding-aa-1").Status.Tracking
			if tr == nil || tr.IssueNumber != 7 {
				t.Fatalf("tracking = %+v, want issue 7 still linked", tr)
			}
			if len(tr.Comments) != 1 || tr.Comments[0].Marker != marker || tr.Comments[0].ID != tt.recorded {
				t.Errorf("tracked comments = %+v, want %s recorded as id %d", tr.Comments, marker, tt.recorded)
			}

			// Nothing moved: the next projection writes nothing.
			posts, edits := len(tracker.comments), tracker.commentEdits
			reconcileFinding(t, r)
			if len(tracker.comments) != posts || tracker.commentEdits != edits {
				t.Errorf("idempotent reprojection wrote: posts %d→%d, edits %d→%d",
					posts, len(tracker.comments), edits, tracker.commentEdits)
			}
		})
	}
}

// TestSetupRequiresAPIReader: every post is confirmed through the API
// reader, and without one those reads would come silently from the cache —
// the very lag they exist to see past. Wiring the reconciler without one is
// refused, not defaulted.
func TestSetupRequiresAPIReader(t *testing.T) {
	r := &FindingReconciler{Namespace: "patchy"}
	if err := r.SetupWithManager(nil); err == nil || !strings.Contains(err.Error(), "APIReader") {
		t.Errorf("SetupWithManager without an APIReader: err = %v, want it refused", err)
	}
}
