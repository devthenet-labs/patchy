// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// pendClose keeps a close seen during review on the finding
// (ConditionReviewClosePending), for settleReview to settle against the
// recorded PR. The condition lives in status, so it survives the delivery
// that saw the close — which GitHub never redelivers once answered.
func pendClose(cur *v1alpha1.Finding, reason, message string, now time.Time) {
	meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReviewClosePending,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cur.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}

// reviewCloseRecheck paces a pending review close's wait for an
// Integration to read GitHub through. Nothing watches Integrations, so a
// finding whose close waits on one re-queues itself.
const reviewCloseRecheck = 5 * time.Minute

// settleReview settles a finding whose review close is pending
// (ConditionReviewClosePending) against what GitHub reports now: the
// recorded PR, and for a tracking issue's close with that PR not closed, the
// issue (settlePendingClose). Both are read with the credential of the
// Integration GitHub's deliveries are applied through — the one Signals is
// handed. With none (suspended, its issues turned off, or deleted, perhaps
// to be recreated) the close waits, re-checked every reviewCloseRecheck
// (wait): a suspended Integration pauses reconciliation, and a close settled
// without GitHub would be a guess. A read GitHub fails is returned, so the
// reconcile retries it with backoff until GitHub answers: the close is never
// guessed at, and never dropped. The reads settle only the finding they were
// made for; one changed since (an issue reopened, a PR's own close applied)
// conflicts, and is read again for its newer version. settled reports that
// the finding was written, or that it changed under the reads, and this
// reconcile should stop.
func (r *FindingReconciler) settleReview(
	ctx context.Context, fnd *v1alpha1.Finding,
) (settled bool, wait time.Duration, err error) {
	pending := meta.FindStatusCondition(fnd.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if fnd.Status.Phase != v1alpha1.PhaseInReview || pending == nil || pending.Status != metav1.ConditionTrue {
		return false, 0, nil
	}
	integ, err := selectIntegration(ctx, r.Client, fnd.Namespace, issuesEnabled)
	if errors.Is(err, ErrNoIntegration) {
		r.log().LogAttrs(ctx, slog.LevelInfo,
			"no issues-enabled integration to read GitHub through; the review close waits",
			slog.String("finding", fnd.Name), slog.String("reason", pending.Reason),
			slog.Duration("recheck", reviewCloseRecheck))
		return false, reviewCloseRecheck, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("finding %s: settle review close: %w", fnd.Name, err)
	}
	pr, err := r.readRecordedPR(ctx, integ, fnd)
	if err != nil {
		return false, 0, fmt.Errorf("finding %s: settle review close: %w", fnd.Name, err)
	}
	issueState := ""
	if pending.Reason == v1alpha1.ReasonTrackingIssueClosed && (pr == nil || pr.State != "closed") {
		if issueState, err = r.readTrackingIssueState(ctx, integ, fnd); err != nil {
			return false, 0, fmt.Errorf("finding %s: settle review close: %w", fnd.Name, err)
		}
	}
	cur := fnd.DeepCopy()
	if err := settlePendingClose(cur, pr, issueState, r.now()); err != nil {
		return false, 0, err
	}
	if equality.Semantic.DeepEqual(fnd.Status, cur.Status) {
		return false, 0, nil
	}
	// Written over the version the reads were made for, so a delivery
	// applied since conflicts rather than being settled over.
	if err := r.Status().Update(ctx, cur); err != nil {
		switch {
		case kerrors.IsConflict(err):
			return true, time.Second, nil // re-Get and re-read on the requeue
		case kerrors.IsNotFound(err):
			return true, 0, nil // deleted meanwhile; nothing left to settle
		}
		return false, 0, err
	}
	return true, 0, nil
}

// unreadable reports a GitHub answer no retry would change: 404 or 403 (the
// resource gone, or the credential's access to it — an App no longer
// installed on the repository answers 404 before any read is made).
func unreadable(err error) bool {
	return ghclient.IsNotFound(err) || ghclient.IsForbidden(err)
}

// readRecordedPR reads the finding's recorded remediation PR through integ.
// GitHub redirects a repository renamed within its owner, so the recorded
// name still reaches the PR; after a transfer to another owner that holds
// only while integ's credential can still read the repository under the old
// one (see ReasonUnrecordedRepository). nil, nil means there is no PR to
// read: none or no repository recorded, or GitHub answers the read, or
// building its client, unreadable.
func (r *FindingReconciler) readRecordedPR(
	ctx context.Context, integ *v1alpha1.Integration, fnd *v1alpha1.Finding,
) (*ghclient.PullRequest, error) {
	rec := fnd.Status.PullRequest
	repo, ok := recordedPRRepo(fnd)
	if rec == nil || !ok {
		return nil, nil
	}
	gh, err := r.clientFor(ctx, integ, repo)
	if err != nil && !unreadable(err) {
		return nil, fmt.Errorf("pull request client: %w", err)
	}
	var pr *ghclient.PullRequest
	if err == nil {
		pr, err = gh.GetPullRequest(ctx, repo, int(rec.Number))
	}
	switch {
	case unreadable(err):
		r.log().LogAttrs(ctx, slog.LevelWarn, "recorded pull request unreadable; settling the close without it",
			slog.String("finding", fnd.Name),
			slog.String("repository", repo.String()),
			slog.Int64("number", rec.Number),
			slog.Any("error", err))
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read pull request %s#%d: %w", repo, rec.Number, err)
	}
	return pr, nil
}

// readTrackingIssueState reads the finding's tracking issue state ("open" or
// "closed") through integ, as GitHub reports it now. Deliveries are handled
// unordered, so the state the last one handled left on the finding may be
// an older one's: a close and a quick reopen can land reopen first. ""
// means it cannot be read — no tracking issue recorded, or GitHub answers
// unreadable — and the delivered state stands.
func (r *FindingReconciler) readTrackingIssueState(
	ctx context.Context, integ *v1alpha1.Integration, fnd *v1alpha1.Finding,
) (string, error) {
	tr := fnd.Status.Tracking
	if tr == nil || tr.IssueNumber == 0 {
		return "", nil
	}
	_, repo, err := forge.ParseRepoURL(tr.URL)
	if err != nil {
		return "", nil
	}
	gh, err := r.clientFor(ctx, integ, repo)
	if err != nil && !unreadable(err) {
		return "", fmt.Errorf("tracking issue client: %w", err)
	}
	var is *ghclient.Issue
	if err == nil {
		is, err = gh.GetIssue(ctx, repo, int(tr.IssueNumber))
	}
	switch {
	case unreadable(err):
		r.log().LogAttrs(ctx, slog.LevelWarn, "tracking issue unreadable; settling the close on its delivered state",
			slog.String("finding", fnd.Name),
			slog.String("repository", repo.String()),
			slog.Int64("issue", tr.IssueNumber),
			slog.Any("error", err))
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read tracking issue %s#%d: %w", repo, tr.IssueNumber, err)
	}
	return is.State, nil
}

// settlePendingClose settles cur's pending review close against the
// recorded PR as GitHub reports it (nil: there is none to read) and, when
// read, the tracking issue's state (issueState; "" when not read, and the
// delivered state stands). A closed PR settles the finding exactly as its
// own delivery would (settle). Otherwise the close turns on whose it was:
// the tracking issue's, with the issue still closed, is a human taking the
// finding over (HandedOff, edge 20); any other — the issue reopened since,
// or another repository's PR of the same number — moves nothing, and only
// the condition goes. A finding no longer in review, or with no close
// pending, is left as it is.
func settlePendingClose(cur *v1alpha1.Finding, pr *ghclient.PullRequest, issueState string, now time.Time) error {
	pending := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if cur.Status.Phase != v1alpha1.PhaseInReview || pending == nil || pending.Status != metav1.ConditionTrue {
		return nil
	}
	if pr != nil && pr.State == "closed" {
		return prClose{merged: pr.Merged, mergedAt: pr.MergedAt, mergeCommitSHA: pr.MergeCommitSHA}.settle(cur, now)
	}
	if issueState != "" && cur.Status.Tracking != nil {
		cur.Status.Tracking.State = issueState
	}
	handOff := pending.Reason == v1alpha1.ReasonTrackingIssueClosed &&
		cur.Status.Tracking != nil && cur.Status.Tracking.State == "closed"
	meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if handOff {
		return v1alpha1.SetPhase(cur, v1alpha1.PhaseHandedOff, now)
	}
	return nil
}
