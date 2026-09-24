// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/webhook"
)

// trackedFinding is a Finding in the given phase whose tracking issue URL is
// linked, so the URL index resolves it.
func trackedFinding(phase v1alpha1.Phase) *v1alpha1.Finding {
	const name = "finding-aa-1"
	return &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"},
			Source:         "ghas",
			Advisories:     []string{"CVE-2026-0001"},
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://github.com/acme/orders",
				Name: "acme/orders",
			},
		},
		Status: v1alpha1.FindingStatus{
			Phase: phase,
			Tracking: &v1alpha1.TrackingStatus{
				Integration: "gh",
				IssueNumber: 7,
				URL:         "https://github.com/acme/orders/issues/7",
				State:       "open",
			},
		},
	}
}

func newSignals(t *testing.T, objs ...client.Object) (*Signals, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithIndex(&v1alpha1.Finding{}, TrackingURLIndex, func(obj client.Object) []string {
			f := obj.(*v1alpha1.Finding)
			if f.Status.Tracking == nil {
				return nil
			}
			return []string{f.Status.Tracking.URL}
		}).
		Build()
	return &Signals{
		Client:    c,
		Namespace: "patchy",
		Now:       func() time.Time { return testClock },
	}, c
}

func event(typ, payload string) webhook.Event {
	return webhook.Event{Type: typ, Payload: []byte(payload)}
}

func TestSignalsIssueClosedHandsOff(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff", f.Status.Phase)
	}
	if f.Status.Tracking.State != "closed" {
		t.Errorf("tracking state = %q, want closed", f.Status.Tracking.State)
	}
}

func TestSignalsIssueClosedTerminalNoop(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseRemediated))
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseRemediated {
		t.Errorf("phase = %q, want Remediated unchanged", got)
	}
}

func TestSignalsReopenAfterDismissal(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseDismissed))
	payload := `{"action":"reopened","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff (edge 19)", got)
	}
}

func TestSignalsApprove(t *testing.T) {
	cases := []struct {
		name        string
		association string
		body        string
		wantSet     bool
	}{
		{"collaborator approves", "COLLABORATOR", "/approve", true},
		{"owner approves with note", "OWNER", "/approve ship it", true},
		{"random user ignored", "NONE", "/approve", false},
		{"non-command ignored", "OWNER", "looks fine to me", false},
		{"prefix-only word ignored", "OWNER", "/approved", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
			payload := fmt.Sprintf(
				`{"action":"created","issue":{"html_url":"https://github.com/acme/orders/issues/7"},`+
					`"comment":{"body":%q,"author_association":%q,"user":{"login":"dev"}}}`,
				tc.body, tc.association)
			if err := s.Handle(t.Context(), testIntegration(), event("issue_comment", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if got := f.Spec.Approval != nil; got != tc.wantSet {
				t.Errorf("approval set = %v, want %v", got, tc.wantSet)
			}
			if tc.wantSet && f.Spec.Approval.By != "dev" {
				t.Errorf("approval.by = %q, want dev", f.Spec.Approval.By)
			}
		})
	}
}

func TestSignalsApproveStaleHandedOff(t *testing.T) {
	approvalAt := func(at time.Time) *v1alpha1.Approval {
		return &v1alpha1.Approval{By: "old-approver", At: metav1.NewTime(at)}
	}
	completed := metav1.NewTime(testClock.Add(-time.Hour))
	cases := []struct {
		name        string
		phase       v1alpha1.Phase
		approval    *v1alpha1.Approval
		completedAt *metav1.Time
		wantBy      string
	}{
		{
			// The recorded approval predates hand-off, so it can never
			// revive the finding; a fresh /approve replaces it.
			name:        "stale approval on HandedOff replaced",
			phase:       v1alpha1.PhaseHandedOff,
			approval:    approvalAt(testClock.Add(-2 * time.Hour)),
			completedAt: &completed,
			wantBy:      "dev",
		},
		{
			name:        "fresh approval on HandedOff kept",
			phase:       v1alpha1.PhaseHandedOff,
			approval:    approvalAt(testClock.Add(-30 * time.Minute)),
			completedAt: &completed,
			wantBy:      "old-approver",
		},
		{
			name:     "existing approval outside HandedOff kept",
			phase:    v1alpha1.PhaseAwaitingApproval,
			approval: approvalAt(testClock.Add(-2 * time.Hour)),
			wantBy:   "old-approver",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fnd := trackedFinding(tc.phase)
			fnd.Spec.Approval = tc.approval
			fnd.Status.CompletedAt = tc.completedAt
			s, c := newSignals(t, fnd)
			payload := `{"action":"created","issue":{"html_url":"https://github.com/acme/orders/issues/7"},` +
				`"comment":{"body":"/approve","author_association":"OWNER","user":{"login":"dev"}}}`
			if err := s.Handle(t.Context(), testIntegration(), event("issue_comment", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := get(t, c, "finding-aa-1").Spec.Approval.By; got != tc.wantBy {
				t.Errorf("approval.by = %q, want %q", got, tc.wantBy)
			}
		})
	}
}

// inReview is trackedFinding in review of its recorded remediation PR,
// acme/orders#11.
func inReview() *v1alpha1.Finding {
	fnd := trackedFinding(v1alpha1.PhaseInReview)
	fnd.Status.PullRequest = &v1alpha1.PullRequestStatus{
		Number: 11, URL: "https://github.com/acme/orders/pull/11", State: "open",
	}
	return fnd
}

// prClosed is a pull_request.closed delivery for PR number in repo, from the
// finding's remediation branch (patchy/finding-aa-1) in headRepo.
func prClosed(repo string, number int, headRepo string, merged bool) string {
	return fmt.Sprintf(
		`{"action":"closed","pull_request":{"number":%d,"merged":%v,`+
			`"merged_at":"2026-07-21T13:00:00Z","merge_commit_sha":"fa82fcdc7efab2777d432ba3385517fa735e0ae0",`+
			`"head":{"ref":"patchy/finding-aa-1","repo":{"full_name":%q}},"base":{"ref":"main","repo":{"full_name":%q}}},`+
			`"repository":{"full_name":%q}}`,
		number, merged, headRepo, repo, repo)
}

func TestSignalsPullRequest(t *testing.T) {
	cases := []struct {
		name      string
		merged    bool
		wantPhase v1alpha1.Phase
		wantState string
	}{
		{"merged remediates", true, v1alpha1.PhaseRemediated, "merged"},
		{"closed unmerged fails", false, v1alpha1.PhaseFailed, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			payload := prClosed("acme/orders", 11, "acme/orders", tc.merged)
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if f.Status.PullRequest.State != tc.wantState {
				t.Errorf("pr state = %q, want %q", f.Status.PullRequest.State, tc.wantState)
			}
			if tc.merged && (f.Status.CompletedAt == nil || f.Status.PullRequest.MergedAt == nil) {
				t.Error("merged PR left completedAt/mergedAt unset")
			}
			// The merge commit is what ingest measures later scanner
			// observations against; an unmerged close records none.
			wantSHA := ""
			if tc.merged {
				wantSHA = "fa82fcdc7efab2777d432ba3385517fa735e0ae0"
			}
			if got := f.Status.PullRequest.MergeCommitSHA; got != wantSHA {
				t.Errorf("mergeCommitSHA = %q, want %q", got, wantSHA)
			}
		})
	}
}

// TestSignalsPullRequestNotRecorded: the head ref names the finding, but
// anyone who can open a pull request can name a branch patchy/<finding>. A
// close settles the finding only when it is the recorded remediation PR:
// the same number, in the finding's repository, from a branch there.
func TestSignalsPullRequestNotRecorded(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantPhase v1alpha1.Phase
	}{
		{"recorded PR merges", prClosed("acme/orders", 11, "acme/orders", true), v1alpha1.PhaseRemediated},
		{"repository matches case-insensitively", prClosed("Acme/Orders", 11, "Acme/Orders", true),
			v1alpha1.PhaseRemediated},
		{"another repository", prClosed("acme/billing", 11, "acme/billing", true), v1alpha1.PhaseInReview},
		{"another number", prClosed("acme/orders", 12, "acme/orders", true), v1alpha1.PhaseInReview},
		{"another number closed unmerged", prClosed("acme/orders", 12, "acme/orders", false), v1alpha1.PhaseInReview},
		{"from a fork", prClosed("acme/orders", 11, "mallory/orders", true), v1alpha1.PhaseInReview},
		{"from a deleted fork", prClosed("acme/orders", 11, "", false), v1alpha1.PhaseInReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", tc.payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if tc.wantPhase == v1alpha1.PhaseInReview && f.Status.PullRequest.State != "open" {
				t.Errorf("pr state = %q, want open (an ignored close records nothing)", f.Status.PullRequest.State)
			}
		})
	}
}

// TestSignalsPullRequestRepositoryFallback: a record without a URL is
// matched against the finding's own repository, where remediation opens
// every PR.
func TestSignalsPullRequestRepositoryFallback(t *testing.T) {
	cases := []struct {
		name      string
		repo      string
		wantPhase v1alpha1.Phase
	}{
		{"finding's repository", "acme/orders", v1alpha1.PhaseRemediated},
		{"another repository", "acme/billing", v1alpha1.PhaseInReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fnd := inReview()
			fnd.Status.PullRequest.URL = ""
			s, c := newSignals(t, fnd)
			payload := prClosed(tc.repo, 11, tc.repo, true)
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := get(t, c, "finding-aa-1").Status.Phase; got != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got, tc.wantPhase)
			}
		})
	}
}

// fakePulls is a PullRequestReader answering every lookup with pr, or
// failing with err; it records each repo#number asked for.
type fakePulls struct {
	pr    *ghclient.PullRequest
	err   error
	asked []string
}

func (f *fakePulls) GetPullRequest(
	_ context.Context, _ *v1alpha1.Integration, repo ghclient.Repo, number int,
) (*ghclient.PullRequest, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s#%d", repo, number))
	return f.pr, f.err
}

const (
	// issueClosed is the tracking issue's issues.closed delivery.
	issueClosed = `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	mergeSHA    = "fa82fcdc7efab2777d432ba3385517fa735e0ae0"
)

// mergedPR is acme/orders#11 as the API reports it once merged.
func mergedPR() *ghclient.PullRequest {
	return &ghclient.PullRequest{
		Number: 11, State: "closed", Merged: true,
		MergedAt: time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC), MergeCommitSHA: mergeSHA,
	}
}

// TestSignalsIssueClosedDuringReview: the remediation PR's body says
// "Fixes #N", so merging it closes the tracking issue too, and deliveries
// are handled unordered — the issue's close can land first. The PR's live
// state decides: merged settles as the merge does, closed unmerged as that
// close does, and only a PR still open means a human closed the issue on
// purpose.
func TestSignalsIssueClosedDuringReview(t *testing.T) {
	cases := []struct {
		name      string
		pr        *ghclient.PullRequest
		wantPhase v1alpha1.Phase
		wantState string
		wantSHA   string
	}{
		{"merged PR remediates", mergedPR(), v1alpha1.PhaseRemediated, "merged", mergeSHA},
		{
			"closed unmerged PR fails",
			&ghclient.PullRequest{Number: 11, State: "closed", MergeCommitSHA: mergeSHA},
			v1alpha1.PhaseFailed, "closed", "",
		},
		{
			"open PR hands off",
			&ghclient.PullRequest{Number: 11, State: "open", MergeCommitSHA: mergeSHA},
			v1alpha1.PhaseHandedOff, "open", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			pulls := &fakePulls{pr: tc.pr}
			s.PullRequests = pulls
			if err := s.Handle(t.Context(), testIntegration(), event("issues", issueClosed)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if want := []string{"acme/orders#11"}; !slices.Equal(pulls.asked, want) {
				t.Errorf("looked up %v, want %v", pulls.asked, want)
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
			if f.Status.Tracking.State != "closed" {
				t.Errorf("tracking state = %q, want closed", f.Status.Tracking.State)
			}
		})
	}
}

// TestSignalsMergeEitherOrder: a merge sends both deliveries; whichever is
// handled first settles the finding Remediated, and the second changes
// nothing — the pull_request close arriving second writes nothing at all.
func TestSignalsMergeEitherOrder(t *testing.T) {
	prMerged := event("pull_request", prClosed("acme/orders", 11, "acme/orders", true))
	cases := []struct {
		name   string
		first  webhook.Event
		second webhook.Event
	}{
		{"issue close first", event("issues", issueClosed), prMerged},
		{"PR close first", prMerged, event("issues", issueClosed)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			s.PullRequests = &fakePulls{pr: mergedPR()}
			if err := s.Handle(t.Context(), testIntegration(), tc.first); err != nil {
				t.Fatalf("Handle first: %v", err)
			}
			settled := get(t, c, "finding-aa-1")
			if err := s.Handle(t.Context(), testIntegration(), tc.second); err != nil {
				t.Fatalf("Handle second: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != v1alpha1.PhaseRemediated {
				t.Fatalf("phase = %q, want Remediated", f.Status.Phase)
			}
			if f.Status.PullRequest.State != "merged" || f.Status.PullRequest.MergeCommitSHA != mergeSHA {
				t.Errorf("pr = %+v, want merged at %s", f.Status.PullRequest, mergeSHA)
			}
			if !f.Status.CompletedAt.Equal(settled.Status.CompletedAt) {
				t.Errorf("completedAt moved from %v to %v", settled.Status.CompletedAt, f.Status.CompletedAt)
			}
			if tc.second.Type == "pull_request" && f.ResourceVersion != settled.ResourceVersion {
				t.Errorf("the PR close arriving second wrote the finding (resourceVersion %s -> %s)",
					settled.ResourceVersion, f.ResourceVersion)
			}
		})
	}
}

// TestSignalsIssueClosedLookupFails: when GitHub cannot say whether the PR
// merged, the close is not guessed at — the finding stays in review for the
// PR's own delivery to settle, rather than handed off with its merge lost.
func TestSignalsIssueClosedLookupFails(t *testing.T) {
	s, c := newSignals(t, inReview())
	s.PullRequests = &fakePulls{err: errors.New("github unavailable")}
	if err := s.Handle(t.Context(), testIntegration(), event("issues", issueClosed)); err == nil {
		t.Error("Handle: nil error, want the lookup failure")
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseInReview {
		t.Errorf("phase = %q, want InReview", f.Status.Phase)
	}
	if f.Status.Tracking.State != "open" {
		t.Errorf("tracking state = %q, want open (nothing recorded)", f.Status.Tracking.State)
	}
}

// TestSignalsIssueClosedOutsideReviewNoLookup: only a finding in review has
// a PR whose merge could have closed its issue.
func TestSignalsIssueClosedOutsideReviewNoLookup(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
	pulls := &fakePulls{pr: mergedPR()}
	s.PullRequests = pulls
	if err := s.Handle(t.Context(), testIntegration(), event("issues", issueClosed)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff", got)
	}
	if len(pulls.asked) != 0 {
		t.Errorf("looked up %v, want no lookup", pulls.asked)
	}
}

func TestSignalsForeignIssueIgnored(t *testing.T) {
	s, _ := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
	payload := `{"action":"closed","issue":{"number":99,"html_url":"https://github.com/acme/other/issues/99"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
