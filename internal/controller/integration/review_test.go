// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

const (
	// issueClosed and issueReopened are the tracking issue's deliveries.
	issueClosed   = `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	issueReopened = `{"action":"reopened","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	mergeSHA      = "fa82fcdc7efab2777d432ba3385517fa735e0ae0"
)

// mergedPR is acme/orders#11 as the API reports it once merged.
func mergedPR() *ghclient.PullRequest {
	return &ghclient.PullRequest{
		Number: 11, State: "closed", Merged: true,
		MergedAt: time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC), MergeCommitSHA: mergeSHA,
	}
}

// openPR is acme/orders#11 still open; its merge commit is GitHub's trial
// merge, which records nothing.
func openPR() *ghclient.PullRequest {
	return &ghclient.PullRequest{Number: 11, State: "open", MergeCommitSHA: mergeSHA}
}

// forbidden is GitHub refusing the credential a resource.
func forbidden(what string) error {
	return fmt.Errorf("ghclient: %s: %w", what,
		&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}})
}

// newReview is Signals and the projection over one fake client holding the
// finding and the tracking Integration; the projection reads pull requests
// and the tracking issue (#7, open) through the returned tracker, whose
// pulls answer by number.
func newReview(t *testing.T, fnd *v1alpha1.Finding) (*Signals, *FindingReconciler, *fakeTracker, client.Client) {
	t.Helper()
	s, c := newSignals(t, fnd, testIntegration())
	tracker := newFakeTracker()
	tracker.pulls = map[int]*ghclient.PullRequest{}
	tracker.issues[7] = &ghclient.Issue{
		Repo: ghclient.Repo{Owner: "acme", Name: "orders"}, Number: 7, State: "open", Author: botLogin,
	}
	r := &FindingReconciler{
		Client:    c,
		Namespace: "patchy",
		Now:       func() time.Time { return testClock },
		ClientFor: func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
			return tracker, nil
		},
	}
	return s, r, tracker, c
}

// handle applies one delivery through Signals.
func handle(t *testing.T, s *Signals, typ, payload string) {
	t.Helper()
	if err := s.Handle(t.Context(), testIntegration(), event(typ, payload)); err != nil {
		t.Fatalf("Handle %s: %v", typ, err)
	}
}

// closeIssue closes the tracking issue on GitHub, as a human or a merge's
// "Fixes #N" does, and delivers its close.
func closeIssue(t *testing.T, s *Signals, tracker *fakeTracker) {
	t.Helper()
	tracker.issues[7].State = "closed"
	handle(t, s, "issues", issueClosed)
}

// reopenIssue reopens the tracking issue on GitHub and delivers its reopen.
func reopenIssue(t *testing.T, s *Signals, tracker *fakeTracker) {
	t.Helper()
	tracker.issues[7].State = "open"
	handle(t, s, "issues", issueReopened)
}

// pendingReason is the reason of the finding's pending review close, ""
// when none is pending.
func pendingReason(f *v1alpha1.Finding) string {
	c := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if c == nil || c.Status != metav1.ConditionTrue {
		return ""
	}
	return c.Reason
}

// TestReviewIssueClosed: the remediation PR's body says "Fixes #N", so
// merging it closes the tracking issue too, and deliveries are handled
// unordered — the issue's close can land first. The delivery settles
// nothing itself: it keeps the close pending, and the projection reads the
// PR. Merged settles as the merge does, closed unmerged as that close does;
// a PR still open means a human closed the issue on purpose, and so does
// one GitHub will never show (gone, or beyond the credential) — each with
// the issue still closed as GitHub reports it.
func TestReviewIssueClosed(t *testing.T) {
	cases := []struct {
		name      string
		pr        *ghclient.PullRequest // nil: GitHub answers 404
		err       error                 // answered instead, when set
		wantPhase v1alpha1.Phase
		wantState string
		wantSHA   string
	}{
		{name: "merged PR remediates", pr: mergedPR(),
			wantPhase: v1alpha1.PhaseRemediated, wantState: "merged", wantSHA: mergeSHA},
		{name: "closed unmerged PR fails", pr: &ghclient.PullRequest{Number: 11, State: "closed", MergeCommitSHA: mergeSHA},
			wantPhase: v1alpha1.PhaseFailed, wantState: "closed"},
		{name: "open PR hands off", pr: openPR(), wantPhase: v1alpha1.PhaseHandedOff, wantState: "open"},
		{name: "PR gone hands off", wantPhase: v1alpha1.PhaseHandedOff, wantState: "open"},
		{name: "PR forbidden hands off", err: forbidden("get PR acme/orders#11"),
			wantPhase: v1alpha1.PhaseHandedOff, wantState: "open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, inReview())
			if tc.pr != nil {
				tracker.pulls[11] = tc.pr
			}
			if tc.err != nil {
				tracker.pullErrs = []error{tc.err}
			}

			closeIssue(t, s, tracker)
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != v1alpha1.PhaseInReview || pendingReason(f) != v1alpha1.ReasonTrackingIssueClosed {
				t.Fatalf("after the delivery: phase %q, pending %q; want InReview with the issue close pending",
					f.Status.Phase, pendingReason(f))
			}
			if f.Status.Tracking.State != "closed" {
				t.Errorf("tracking state = %q, want closed", f.Status.Tracking.State)
			}
			if len(tracker.pullReads) != 0 {
				t.Errorf("the delivery read %v; Signals must not call GitHub", tracker.pullReads)
			}

			reconcileFinding(t, r)
			f = get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if want := []string{"acme/orders#11"}; !slices.Equal(tracker.pullReads, want) {
				t.Errorf("read %v, want %v", tracker.pullReads, want)
			}
			if f.Status.PullRequest.State != tc.wantState {
				t.Errorf("pr state = %q, want %q", f.Status.PullRequest.State, tc.wantState)
			}
			if got := f.Status.PullRequest.MergeCommitSHA; got != tc.wantSHA {
				t.Errorf("mergeCommitSHA = %q, want %q", got, tc.wantSHA)
			}
			if tc.wantSHA != "" && (f.Status.PullRequest.MergedAt == nil ||
				!f.Status.PullRequest.MergedAt.Equal(&metav1.Time{Time: mergedPR().MergedAt})) {
				t.Errorf("mergedAt = %v, want %v", f.Status.PullRequest.MergedAt, mergedPR().MergedAt)
			}
			if got := pendingReason(f); got != "" {
				t.Errorf("pending close %q left after settling", got)
			}
		})
	}
}

// TestReviewIssueClosedLookupFails: a human closes the issue while GitHub
// cannot say whether the PR merged. The delivery is never redelivered once
// answered, so the close is kept on the finding and the PR read again on
// every retry of the reconcile until GitHub answers — never guessed at, and
// never dropped with the finding left in review.
func TestReviewIssueClosedLookupFails(t *testing.T) {
	s, r, tracker, c := newReview(t, inReview())
	tracker.pulls[11] = openPR()
	tracker.pullErrs = []error{badGateway("get PR acme/orders#11"), badGateway("get PR acme/orders#11")}

	closeIssue(t, s, tracker)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
	for i := range 2 {
		if _, err := r.Reconcile(t.Context(), req); err == nil {
			t.Fatalf("reconcile %d: nil error, want the lookup failure (so the reconcile backs off and retries)", i+1)
		}
		f := get(t, c, "finding-aa-1")
		if f.Status.Phase != v1alpha1.PhaseInReview || pendingReason(f) != v1alpha1.ReasonTrackingIssueClosed {
			t.Fatalf("reconcile %d: phase %q, pending %q; want InReview with the close still pending",
				i+1, f.Status.Phase, pendingReason(f))
		}
	}

	reconcileFinding(t, r)
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff once GitHub answers the PR is open", f.Status.Phase)
	}
	if got := pendingReason(f); got != "" {
		t.Errorf("pending close %q left after settling", got)
	}
	if len(tracker.pullReads) != 3 {
		t.Errorf("read the PR %d times, want 3 (two failures, then the answer)", len(tracker.pullReads))
	}
}

// TestReviewMergeEitherOrder: a merge sends both deliveries, handled in
// either order, and the projection may read the PR before or after the
// second lands. Whichever settles the finding first settles it Remediated;
// nothing after it writes the finding again, bar the issue close's own
// tracking state, and a close already settled is never read for.
func TestReviewMergeEitherOrder(t *testing.T) {
	cases := []struct {
		name      string
		steps     []string
		wantReads int
	}{
		{"issue close first", []string{"issue", "reconcile", "pr"}, 1},
		{"PR close first", []string{"pr", "issue", "reconcile"}, 0},
		{"PR close before the projection reads", []string{"issue", "pr", "reconcile"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, inReview())
			tracker.pulls[11] = mergedPR()
			settledRV := ""
			for _, step := range tc.steps {
				switch step {
				case "issue":
					closeIssue(t, s, tracker)
				case "pr":
					handle(t, s, "pull_request", prClosed("acme/orders", 11, "acme/orders", true))
				case "reconcile":
					reconcileFinding(t, r)
				}
				f := get(t, c, "finding-aa-1")
				switch {
				case f.Status.Phase != v1alpha1.PhaseRemediated:
				case settledRV == "" || step == "issue":
					settledRV = f.ResourceVersion
				case f.ResourceVersion != settledRV:
					t.Errorf("%s wrote the settled finding (resourceVersion %s -> %s)", step, settledRV, f.ResourceVersion)
				}
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != v1alpha1.PhaseRemediated {
				t.Fatalf("phase = %q, want Remediated", f.Status.Phase)
			}
			if f.Status.PullRequest.State != "merged" || f.Status.PullRequest.MergeCommitSHA != mergeSHA {
				t.Errorf("pr = %+v, want merged at %s", f.Status.PullRequest, mergeSHA)
			}
			if got := pendingReason(f); got != "" {
				t.Errorf("pending close %q left after settling", got)
			}
			if len(tracker.pullReads) != tc.wantReads {
				t.Errorf("read the PR %d times, want %d", len(tracker.pullReads), tc.wantReads)
			}
		})
	}
}

// TestReviewIssueReopenedBeforeSettled: a human closes the issue and
// reopens it before the projection reads the still-open PR — they changed
// their mind, so the finding stays in review.
func TestReviewIssueReopenedBeforeSettled(t *testing.T) {
	s, r, tracker, c := newReview(t, inReview())
	tracker.pulls[11] = openPR()
	closeIssue(t, s, tracker)
	reopenIssue(t, s, tracker)
	reconcileFinding(t, r)

	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseInReview {
		t.Errorf("phase = %q, want InReview", f.Status.Phase)
	}
	if f.Status.Tracking.State != "open" {
		t.Errorf("tracking state = %q, want open", f.Status.Tracking.State)
	}
	if got := pendingReason(f); got != "" {
		t.Errorf("pending close %q left once the PR was read", got)
	}
}

// TestReviewIssueReopenDeliveredFirst: a human closes the issue by mistake
// and reopens it at once, and the two deliveries are handled reopen first —
// GitHub orders neither. The finding's tracking state then says closed while
// the issue is open, so the projection reads the issue as GitHub reports it
// now: the finding stays in review, its tracking state corrected.
func TestReviewIssueReopenDeliveredFirst(t *testing.T) {
	s, r, tracker, c := newReview(t, inReview())
	tracker.pulls[11] = openPR()
	tracker.issues[7].State = "open" // closed, then reopened, on GitHub
	handle(t, s, "issues", issueReopened)
	handle(t, s, "issues", issueClosed)
	if f := get(t, c, "finding-aa-1"); f.Status.Tracking.State != "closed" ||
		pendingReason(f) != v1alpha1.ReasonTrackingIssueClosed {
		t.Fatalf("after the deliveries: tracking %q, pending %q; want closed with the issue close pending",
			f.Status.Tracking.State, pendingReason(f))
	}

	reconcileFinding(t, r)
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseInReview {
		t.Errorf("phase = %q, want InReview: the issue is open on GitHub", f.Status.Phase)
	}
	if f.Status.Tracking.State != "open" {
		t.Errorf("tracking state = %q, want open as GitHub reports it", f.Status.Tracking.State)
	}
	if got := pendingReason(f); got != "" {
		t.Errorf("pending close %q left once the issue was read", got)
	}
}

// TestReviewIssueChangesWhileRead: the issue is reopened, then closed again
// while the projection reads it, and both deliveries are applied before it
// writes. The answer it read (open) is stale by then: the finding it was
// read for has changed, so nothing is settled over it, and the next
// reconcile reads the issue closed and hands the finding off.
func TestReviewIssueChangesWhileRead(t *testing.T) {
	s, r, tracker, c := newReview(t, inReview())
	tracker.pulls[11] = openPR()
	closeIssue(t, s, tracker)
	tracker.issues[7].State = "open" // reopened on GitHub; its delivery not yet handled
	tracker.onIssueRead = func() {
		handle(t, s, "issues", issueReopened)
		closeIssue(t, s, tracker)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseInReview || pendingReason(f) != v1alpha1.ReasonTrackingIssueClosed {
		t.Fatalf("phase %q, pending %q; want InReview with the close still pending, not settled on a stale read",
			f.Status.Phase, pendingReason(f))
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want a requeue to read the changed finding again")
	}

	reconcileFinding(t, r)
	f = get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff || f.Status.Tracking.State != "closed" {
		t.Errorf("phase %q, tracking %q; want HandedOff with the issue closed", f.Status.Phase, f.Status.Tracking.State)
	}
}

// TestReviewCloseWaitsForIntegration: GitHub is down, so a merge's issue
// close is pending, and the operator suspends the Integration to stop the
// retries — or turns its issues off, or deletes it to recreate it. Nothing
// can read the PR then, and the close is not settled without it: it waits,
// the finding in review, re-checked on its own clock since nothing watches
// Integrations, and settles as the merge once the Integration is back.
func TestReviewCloseWaitsForIntegration(t *testing.T) {
	cases := []struct {
		name string
		take func(*v1alpha1.Integration) // nil deletes it
	}{
		{"suspended", func(i *v1alpha1.Integration) { i.Spec.Suspend = true }},
		{"issues turned off", func(i *v1alpha1.Integration) { i.Spec.GitHub.Issues.Enabled = false }},
		{"deleted", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, inReview())
			tracker.pulls[11] = mergedPR()
			closeIssue(t, s, tracker)

			var integ v1alpha1.Integration
			key := types.NamespacedName{Namespace: "patchy", Name: "gh"}
			if err := c.Get(t.Context(), key, &integ); err != nil {
				t.Fatal(err)
			}
			if tc.take == nil {
				if err := c.Delete(t.Context(), &integ); err != nil {
					t.Fatal(err)
				}
			} else {
				tc.take(&integ)
				if err := c.Update(t.Context(), &integ); err != nil {
					t.Fatal(err)
				}
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
			res, err := r.Reconcile(t.Context(), req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != v1alpha1.PhaseInReview || pendingReason(f) != v1alpha1.ReasonTrackingIssueClosed {
				t.Fatalf("phase %q, pending %q; want InReview with the close still pending",
					f.Status.Phase, pendingReason(f))
			}
			if res.RequeueAfter != reviewCloseRecheck {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, reviewCloseRecheck)
			}
			if len(tracker.pullReads) != 0 {
				t.Errorf("read %v with no Integration to read through", tracker.pullReads)
			}

			if tc.take == nil {
				if err := c.Create(t.Context(), testIntegration()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Get(t.Context(), key, &integ); err != nil {
					t.Fatal(err)
				}
				integ.Spec = testIntegration().Spec
				if err := c.Update(t.Context(), &integ); err != nil {
					t.Fatal(err)
				}
			}
			reconcileFinding(t, r)
			f = get(t, c, "finding-aa-1")
			if f.Status.Phase != v1alpha1.PhaseRemediated || f.Status.PullRequest.MergeCommitSHA != mergeSHA {
				t.Errorf("phase %q, merge commit %q; want Remediated at %s once the Integration is back",
					f.Status.Phase, f.Status.PullRequest.MergeCommitSHA, mergeSHA)
			}
		})
	}
}

// TestReviewClientUnreadable: the client to read the recorded PR with
// cannot be built, because resolving the App's installation on the
// repository answers 404 (uninstalled, or the repository taken out of its
// selection since the close was recorded) or 403. No retry changes that,
// so the close settles on what its deliveries said, exactly as a PR read
// answering so does; a transient failure is retried instead.
func TestReviewClientUnreadable(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		delivery    string // the close's delivery: "issue" or "pr" (an unrecorded repository's)
		wantErr     bool
		wantPhase   v1alpha1.Phase
		wantPending string
	}{
		{name: "not installed: the issue close hands off", err: notFound("resolve installation for acme/orders"),
			delivery: "issue", wantPhase: v1alpha1.PhaseHandedOff},
		{name: "forbidden: the issue close hands off", err: forbidden("resolve installation for acme/orders"),
			delivery: "issue", wantPhase: v1alpha1.PhaseHandedOff},
		{name: "not installed: another repository's close moves nothing",
			err: notFound("resolve installation for acme/orders"), delivery: "pr", wantPhase: v1alpha1.PhaseInReview},
		{name: "transient: retried", err: badGateway("resolve installation for acme/orders"), delivery: "issue",
			wantErr: true, wantPhase: v1alpha1.PhaseInReview, wantPending: v1alpha1.ReasonTrackingIssueClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, inReview())
			tracker.pulls[11] = openPR()
			r.ClientFor = func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
				return nil, tc.err
			}
			if tc.delivery == "issue" {
				closeIssue(t, s, tracker)
			} else {
				handle(t, s, "pull_request", prClosed("acme/storefront", 11, "acme/storefront", true))
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
			if _, err := r.Reconcile(t.Context(), req); (err != nil) != tc.wantErr {
				t.Fatalf("Reconcile error = %v, want error %v", err, tc.wantErr)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if got := pendingReason(f); got != tc.wantPending {
				t.Errorf("pending close = %q, want %q", got, tc.wantPending)
			}
		})
	}
}

// TestReviewRevivedWithIssueStillClosed: a finding handed off by a closed
// issue and revived by /approve comes back to review with its issue still
// closed. That old close is not one seen during this review: nothing is
// pending, nothing is read, and the finding stays in review.
func TestReviewRevivedWithIssueStillClosed(t *testing.T) {
	fnd := inReview()
	fnd.Status.Tracking.State = "closed"
	_, r, tracker, c := newReview(t, fnd)
	tracker.pulls[11] = openPR()
	reconcileFinding(t, r)

	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseInReview {
		t.Errorf("phase = %q, want InReview", got)
	}
	if len(tracker.pullReads) != 0 {
		t.Errorf("read %v, want nothing read", tracker.pullReads)
	}
}

// TestReviewUnrecordedRepository: the recorded repository is renamed or
// transferred during review, so the merge's delivery carries a name the
// record does not. The delivery settles nothing itself — another
// repository's PR of the same number looks the same — so the projection
// reads the recorded PR under its recorded name, which GitHub redirects.
func TestReviewUnrecordedRepository(t *testing.T) {
	cases := []struct {
		name      string
		pr        *ghclient.PullRequest // nil: GitHub answers 404
		wantPhase v1alpha1.Phase
		wantState string
	}{
		{"renamed: recorded PR merged", mergedPR(), v1alpha1.PhaseRemediated, "merged"},
		{"renamed: recorded PR closed unmerged", &ghclient.PullRequest{Number: 11, State: "closed"},
			v1alpha1.PhaseFailed, "closed"},
		{"another repository's PR: recorded PR open", openPR(), v1alpha1.PhaseInReview, "open"},
		{"recorded PR gone", nil, v1alpha1.PhaseInReview, "open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, inReview())
			if tc.pr != nil {
				tracker.pulls[11] = tc.pr
			}
			handle(t, s, "pull_request", prClosed("acme/storefront", 11, "acme/storefront", true))
			if got := pendingReason(get(t, c, "finding-aa-1")); got != v1alpha1.ReasonUnrecordedRepository {
				t.Fatalf("pending close = %q, want %s", got, v1alpha1.ReasonUnrecordedRepository)
			}

			reconcileFinding(t, r)
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if f.Status.PullRequest.State != tc.wantState {
				t.Errorf("pr state = %q, want %q", f.Status.PullRequest.State, tc.wantState)
			}
			if want := []string{"acme/orders#11"}; !slices.Equal(tracker.pullReads, want) {
				t.Errorf("read %v, want the recorded %v", tracker.pullReads, want)
			}
			if got := pendingReason(f); got != "" {
				t.Errorf("pending close %q left once the PR was read", got)
			}
		})
	}
}

// TestReviewUnrecordedRepositoryKeepsIssueClose: another repository's PR
// of the same number closing does not demote an issue close already
// pending — with the recorded PR still open, that close is a hand-off.
func TestReviewUnrecordedRepositoryKeepsIssueClose(t *testing.T) {
	s, r, tracker, c := newReview(t, inReview())
	tracker.pulls[11] = openPR()
	closeIssue(t, s, tracker)
	handle(t, s, "pull_request", prClosed("acme/billing", 11, "acme/billing", false))
	if got := pendingReason(get(t, c, "finding-aa-1")); got != v1alpha1.ReasonTrackingIssueClosed {
		t.Fatalf("pending close = %q, want %s kept", got, v1alpha1.ReasonTrackingIssueClosed)
	}
	reconcileFinding(t, r)
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff", got)
	}
}

// TestReviewIssueClosedOutsideReview: only a finding in review has a PR
// whose merge could have closed its issue; any other hands off at once.
func TestReviewIssueClosedOutsideReview(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
	tracker.pulls[11] = mergedPR()
	closeIssue(t, s, tracker)
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff || pendingReason(f) != "" {
		t.Errorf("phase %q, pending %q; want HandedOff with nothing pending", f.Status.Phase, pendingReason(f))
	}
	reconcileFinding(t, r)
	if len(tracker.pullReads) != 0 {
		t.Errorf("read %v, want nothing read", tracker.pullReads)
	}
}
