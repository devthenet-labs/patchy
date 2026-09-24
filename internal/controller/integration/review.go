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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
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

// settleReview settles a finding whose review close is pending
// (ConditionReviewClosePending) once GitHub reports the recorded PR's state
// (settlePendingClose). A read GitHub fails is returned, so the reconcile
// retries it with backoff until the PR can be read: the close is never
// guessed at, and never dropped. It reports whether it wrote the finding.
func (r *FindingReconciler) settleReview(ctx context.Context, fnd *v1alpha1.Finding) (bool, error) {
	if fnd.Status.Phase != v1alpha1.PhaseInReview ||
		!meta.IsStatusConditionTrue(fnd.Status.Conditions, v1alpha1.ConditionReviewClosePending) {
		return false, nil
	}
	pr, err := r.readRecordedPR(ctx, fnd)
	if err != nil {
		return false, fmt.Errorf("finding %s: settle review close: %w", fnd.Name, err)
	}
	wrote := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := cur.Status.DeepCopy()
		if err := settlePendingClose(&cur, pr, r.now()); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		if err := r.Status().Update(ctx, &cur); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// readRecordedPR reads the finding's recorded remediation PR with the
// credential of the Integration GitHub's deliveries are applied through —
// the one Signals is handed. GitHub redirects a renamed or transferred
// repository's old name, so the recorded one still reaches the PR. nil, nil
// means there is no PR to read: none or no repository recorded, no such
// Integration, or GitHub answers 404 or 403 (the PR gone, or the
// credential's access to it), which no retry would change.
func (r *FindingReconciler) readRecordedPR(ctx context.Context, fnd *v1alpha1.Finding) (*ghclient.PullRequest, error) {
	rec := fnd.Status.PullRequest
	repo, ok := recordedPRRepo(fnd)
	if rec == nil || !ok {
		return nil, nil
	}
	integ, err := selectIntegration(ctx, r.Client, fnd.Namespace, issuesEnabled)
	if errors.Is(err, ErrNoIntegration) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	gh, err := r.clientFor(ctx, integ, repo)
	if err != nil {
		return nil, fmt.Errorf("pull request client: %w", err)
	}
	pr, err := gh.GetPullRequest(ctx, repo, int(rec.Number))
	switch {
	case ghclient.IsNotFound(err) || ghclient.IsForbidden(err):
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

// settlePendingClose settles cur's pending review close against the
// recorded PR as GitHub reports it (nil: there is none to read). A closed PR
// settles the finding exactly as its own delivery would (settle). Otherwise
// the close turns on whose it was: the tracking issue's, with the issue
// still closed, is a human taking the finding over (HandedOff, edge 20);
// any other — the issue reopened since, or another repository's PR of the
// same number — moves nothing, and only the condition goes. A finding no
// longer in review, or with no close pending, is left as it is.
func settlePendingClose(cur *v1alpha1.Finding, pr *ghclient.PullRequest, now time.Time) error {
	pending := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if cur.Status.Phase != v1alpha1.PhaseInReview || pending == nil || pending.Status != metav1.ConditionTrue {
		return nil
	}
	if pr != nil && pr.State == "closed" {
		return prClose{merged: pr.Merged, mergedAt: pr.MergedAt, mergeCommitSHA: pr.MergeCommitSHA}.settle(cur, now)
	}
	handOff := pending.Reason == v1alpha1.ReasonTrackingIssueClosed &&
		cur.Status.Tracking != nil && cur.Status.Tracking.State == "closed"
	meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	if handOff {
		return v1alpha1.SetPhase(cur, v1alpha1.PhaseHandedOff, now)
	}
	return nil
}
