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
)

// blockingConditions are the conditions that hold an Intent Blocked.
var blockingConditions = []string{v1alpha1.ConditionBudgetExhausted, v1alpha1.ConditionImageRequired}

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
		if rs.counted(refused) < p.set.MaxAttempts && rs.next() <= v1alpha1.MaxIntentRunAttempt {
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
// ceiling; a repository-image block still in force (the Project still
// requires the image, and repository images are still off or the breaker
// still tripped, or else neither the Project nor the default branch changed
// since the block).
func (p *pass) blockHolds(ctx context.Context) (bool, error) {
	if meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) &&
		p.in.Status.Usage.CostMicroUSD >= maxCostMicroUSD(p.proj) {
		return true, nil
	}
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

// headUnmoved reports whether the default branch still points where the
// blocked build pinned it: a new commit may declare an image the pin lacked.
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
