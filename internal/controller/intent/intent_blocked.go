// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// blockingConditions are the conditions that hold an Intent Blocked.
var blockingConditions = []string{
	v1alpha1.ConditionBudgetExhausted, v1alpha1.ConditionImageRequired, v1alpha1.ConditionBranchConflict,
}

// ReasonBranchExists is the BranchConflict reason for patchy-intent/<intent>
// existing at a commit none of the Intent's runs pushed.
const ReasonBranchExists = "BranchExists"

// block moves the Intent to Blocked with the condition saying why, in one
// status write, and remembers the Project generation it was blocked under.
func (p *pass) block(ctx context.Context, condition, reason, msg string) error {
	err := p.setPhase(ctx, v1alpha1.IntentBlocked, func(cur *v1alpha1.Intent) {
		setCondition(cur, condition, metav1.ConditionTrue, reason, msg)
		cur.Status.ActiveRun = nil
	})
	if err == nil {
		p.r.memo(func() { p.r.blockedAt[p.in.Name] = p.proj.Generation })
	}
	return err
}

// fail ends the Intent Failed: the trigger label is removed first, so no
// failed intent leaves its trigger in place and completedAt, from which the
// TTL counts, is never set while it still is. An approver applying the label
// again revives the intent.
func (p *pass) fail(ctx context.Context) error {
	if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), p.trigger()); err != nil {
		return fmt.Errorf("remove the trigger label: %w", err)
	}
	return p.setPhase(ctx, v1alpha1.IntentFailed, func(cur *v1alpha1.Intent) { cur.Status.ActiveRun = nil })
}

// blocked re-evaluates a Blocked Intent. Once no block holds it resumes to
// the phase it was blocked from, launching that round's next attempt first
// (so the resumed phase never mistakes the failure it was blocked on for a
// new one), in one status write that clears the conditions. When no attempt
// is left to launch it resumes all the same, and the resumed phase fails the
// Intent (Blocked has no edge to Failed). A suspended Project launches
// nothing, so its blocked intents wait for it to resume.
func (p *pass) blocked(ctx context.Context) (bool, error) {
	if p.proj.Spec.Suspend {
		return false, nil
	}
	holds, err := p.blockHolds(ctx)
	if err != nil || holds {
		return false, err
	}
	from := v1alpha1.IntentBlockedFrom(p.in)
	var run *v1alpha1.IntentRun
	var stage v1alpha1.IntentStage
	var round int32
	switch {
	case from == v1alpha1.IntentPlanning && p.in.Status.Input != nil:
		stage, round = v1alpha1.IntentStagePlan, p.in.Status.Input.Revision
	case from == v1alpha1.IntentBuilding && p.in.Status.Approval != nil:
		stage, round = v1alpha1.IntentStageBuild, p.in.Status.Approval.PlanRevision
	}
	if stage != "" {
		rs := p.round(stage, round)
		refused := p.planRefused
		if stage == v1alpha1.IntentStageBuild {
			refused = nil
		}
		// A round whose latest run completed (a build blocked opening its
		// pull request) resumes to that run's result, with no new one.
		done := rs.latest() != nil && rs.latest().Status.Phase == v1alpha1.RunComplete
		if !done && rs.counted(refused) < p.set.MaxAttempts && rs.next() <= v1alpha1.MaxIntentRunAttempt {
			run, err = p.createRun(ctx, stage, round, rs.next(), p.previousAttempt(rs.latest()))
			switch {
			case errors.Is(err, errRepositoryGone):
				run = nil // the resumed phase fails the intent
			case err != nil:
				return false, err
			}
		}
	}
	if from == "" {
		from = v1alpha1.IntentPending
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "intent block lifted",
		slog.String("intent", p.in.Name), slog.String("resume", string(from)))
	return true, p.setPhase(ctx, from, func(cur *v1alpha1.Intent) {
		for _, c := range blockingConditions {
			if meta.IsStatusConditionTrue(cur.Status.Conditions, c) {
				setCondition(cur, c, metav1.ConditionFalse, "Resolved", "the block no longer holds")
			}
		}
		if run != nil {
			cur.Status.ActiveRun = &v1alpha1.ObjectReference{Name: run.Name, UID: run.UID}
		}
	})
}

// blockHolds reports whether any block still holds: the spend still at the
// ceiling; the intent branch still not patchy's to use; a repository-image
// block still in force (the Project still requires the image, and repository
// images are still off or the breaker still tripped, or else neither the
// Project nor the default branch changed since the block).
func (p *pass) blockHolds(ctx context.Context) (bool, error) {
	if meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) &&
		p.in.Status.Usage.CostMicroUSD >= maxCostMicroUSD(p.proj) {
		return true, nil
	}
	if holds, err := p.branchBlockHolds(ctx); holds || err != nil {
		return holds, err
	}
	return p.imageBlockHolds(ctx)
}

// branchBlockHolds reports a BranchConflict block still in force: read only
// when the poll is due, and under the app repository's rate floor, as every
// poll is; until then the block holds.
func (p *pass) branchBlockHolds(ctx context.Context) (bool, error) {
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionBranchConflict)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false, nil
	}
	repo, ok := p.runRepository(v1alpha1.IntentStageBuild)
	if !ok {
		return false, nil // the resumed phase fails the intent
	}
	if !p.polled {
		return true, nil
	}
	if ok, err := p.rateOK(ctx, repo.URL); err != nil || !ok {
		return true, err
	}
	sha, err := p.branchConflict(ctx, repo.URL)
	return sha != "", err
}

// imageBlockHolds reports an ImageRequired block still in force.
func (p *pass) imageBlockHolds(ctx context.Context) (bool, error) {
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionImageRequired)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false, nil
	}
	if !requireRepositoryImage(p.proj) {
		// Opted out: the build runs on the default image, whatever kept the
		// repository's from being usable (images off, the breaker tripped).
		return false, nil
	}
	switch c.Reason {
	case ReasonRepositoryImagesOff:
		return !p.r.Images.Enabled, nil
	case ReasonSandboxBreaker:
		return p.r.Images.Breaker.Tripped(), nil
	}
	var gen int64
	var known bool
	p.r.memo(func() { gen, known = p.r.blockedAt[p.in.Name] })
	if !known || gen != p.proj.Generation {
		// The Project changed since the block (or this process never saw
		// it blocked): try once more.
		p.r.memo(func() { p.r.blockedAt[p.in.Name] = p.proj.Generation })
		return false, nil
	}
	if !p.polled {
		return true, nil
	}
	return p.headUnmoved(ctx)
}

// branchConflict is the commit patchy-intent/<intent> points at in repoURL
// when none of this Intent's runs pushed it, or "": the branch is absent, or
// it is this Intent's own. Nothing deletes an intent's branch when it ends,
// and an Intent's name is reused once the TTL deletes it (a reopened issue
// labelled again), so an earlier Intent's branch can still stand; someone
// with write access may also have created it. A build would spend its whole
// grant and then fail branch_exists, since the branch is never forced.
func (p *pass) branchConflict(ctx context.Context, repoURL string) (string, error) {
	branch := branchName(p.in.Name)
	head, err := p.r.GitHub.HeadSHA(ctx, repoURL, branch)
	switch {
	case ghclient.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the branch %s: %w", branch, err)
	}
	for _, run := range p.runs {
		if run.Status.PushedCommit != "" && run.Status.PushedCommit == head {
			return "", nil
		}
	}
	return head, nil
}

// headUnmoved reports whether the default branch still points where the
// blocked build pinned it: a new commit may declare an image the pin lacked.
// Under the app repository's rate floor it reads nothing, and the block
// holds.
func (p *pass) headUnmoved(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if ap == nil {
		return true, nil
	}
	latest := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision).latest()
	if latest == nil {
		return false, nil
	}
	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: latest.Namespace, Name: latest.Spec.Repository.RepositoryRef.Name}
	if err := p.r.Get(ctx, key, &repo); err != nil || repo.Status.ResolvedSHA == "" {
		return true, client.IgnoreNotFound(err)
	}
	url := latest.Spec.Repository.URL
	if ok, err := p.rateOK(ctx, url); err != nil || !ok {
		return true, err
	}
	branch, err := p.r.GitHub.DefaultBranch(ctx, url)
	if err != nil {
		return true, fmt.Errorf("read the default branch: %w", err)
	}
	head, err := p.r.GitHub.HeadSHA(ctx, url, branch)
	if err != nil {
		return true, fmt.Errorf("read the default branch head: %w", err)
	}
	return head == repo.Status.ResolvedSHA, nil
}
