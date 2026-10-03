// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
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
	v1alpha1.ConditionRevisionLimitReached, v1alpha1.ConditionChecksFailing,
	v1alpha1.ConditionUnsupportedRepositories,
}

// BranchConflict reasons.
const (
	// ReasonBranchExists: patchy-intent/<intent> exists at a commit none of
	// the Intent's runs pushed.
	ReasonBranchExists = "BranchExists"
	// ReasonForeignPullRequest: an open pull request patchy did not open
	// holds patchy-intent/<intent> against the default branch.
	ReasonForeignPullRequest = "ForeignPullRequest"
	// ReasonBranchMissing: the pushed branch vanished before patchy opened its PR.
	ReasonBranchMissing = "BranchMissing"
	// ReasonBranchChanged: the branch no longer points at the build's commit.
	ReasonBranchChanged = "BranchChanged"
	// ReasonPullRequestRefused: GitHub refused to create patchy's PR.
	ReasonPullRequestRefused = "PullRequestRefused"
	// ReasonStaleRoundBranch: patchy-intent/<intent> is at a commit an
	// earlier round of this same Intent pushed (a sibling a failed round
	// left behind, say): patchy's own, so safe to delete, but never this
	// round's to build on.
	ReasonStaleRoundBranch = "StaleRoundBranch"
)

// ReasonMultiRepositoryOff is the UnsupportedRepositories reason: the
// Intent's Project lists more than one repository, and intent-controller
// runs without --intent-multi-repo.
const ReasonMultiRepositoryOff = "MultiRepositoryOff"

// multiRepoOffMessage says why an Intent of a multi-repository Project is
// held, and what lifts it.
func multiRepoOffMessage(p *v1alpha1.Project) string {
	return fmt.Sprintf("project %s lists %d repositories, and intent-controller runs intents over more than one "+
		"only with --intent-multi-repo (intentController.config.multiRepo); nothing is planned, built, pushed "+
		"or revised until it is on again or the project lists one repository", p.Name, len(p.Spec.Repositories))
}

// holdMultiRepo blocks an Intent whose Project the controller runs no intent
// of (Settings.multiRepoOff), from whatever phase it is in, so the step it
// would take is never taken. A Blocked one only gains the condition, once,
// when it lacks it. stop reports a status write.
func (p *pass) holdMultiRepo(ctx context.Context) (stop bool, err error) {
	if !p.set.multiRepoOff(p.proj) {
		return false, nil
	}
	if p.in.Status.Phase != v1alpha1.IntentBlocked {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "multi-repository intents are off; the intent is held",
			slog.String("intent", p.in.Name), slog.String("project", p.proj.Name))
		return true, p.block(ctx, v1alpha1.ConditionUnsupportedRepositories, ReasonMultiRepositoryOff,
			multiRepoOffMessage(p.proj))
	}
	if meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories) {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionUnsupportedRepositories, metav1.ConditionTrue, ReasonMultiRepositoryOff,
			multiRepoOffMessage(p.proj))
		return nil
	})
}

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
// the phase it was blocked from, in one status write that clears the
// conditions. A plan round resumes with its next attempt launched first, so
// the resumed phase never mistakes the failure it was blocked on for a new
// one; a build round with the next attempt of each repository whose build
// had no accepted image to run on, the one failure the resumed phase would
// block on again. The other repositories carry on as they stand: one still
// in flight is left to finish, one that completed keeps its result, and one
// that failed otherwise (or never launched) is launched by the resumed phase,
// which checks its branch and the spend first. When no attempt is left to
// launch it resumes all the same, and the resumed phase fails the Intent
// (Blocked has no edge to Failed). A revise round resumes with its unfinished
// run active again (block clears activeRun), so the resumed phase never takes
// the round for settled and posts its notice before it ends; a run whose
// push was held meanwhile (errRoundBlocked) pushes once the Intent is
// Revising. A suspended Project launches nothing, so
// its blocked intents wait for it to resume, and so does an Intent of a
// Project this controller runs no intent of (UnsupportedRepositories).
func (p *pass) blocked(ctx context.Context) (bool, error) {
	if p.proj.Spec.Suspend {
		return false, nil
	}
	if p.set.multiRepoOff(p.proj) {
		return p.holdMultiRepo(ctx)
	}
	holds, err := p.blockHolds(ctx)
	if err != nil {
		return false, err
	}
	if holds {
		// The flag is on again (or the Project lists one repository) while
		// another block still holds: only that one is left to explain it.
		if !meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories) {
			return false, nil
		}
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			setCondition(cur, v1alpha1.ConditionUnsupportedRepositories, metav1.ConditionFalse, "Resolved",
				"the project's intents run again")
			return nil
		})
	}
	from := v1alpha1.IntentBlockedFrom(p.in)
	var active *v1alpha1.IntentRun
	switch {
	case from == v1alpha1.IntentPlanning && p.in.Status.Input != nil:
		if active, err = p.resumePlan(ctx); err != nil {
			return false, err
		}
	case from == v1alpha1.IntentBuilding && p.in.Status.Approval != nil:
		if active, err = p.resumeBuilds(ctx); err != nil {
			return false, err
		}
	case from == v1alpha1.IntentRevising:
		active = p.unfinishedRound()
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
		if active != nil {
			cur.Status.ActiveRun = &v1alpha1.ObjectReference{Name: active.Name, UID: active.UID}
		}
	})
}

// unfinishedRound is the current revise round's latest run while it has not
// settled (Complete or Failed), else nil: the run a Revising intent follows.
func (p *pass) unfinishedRound() *v1alpha1.IntentRun {
	latest := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository).latest()
	if latest == nil || latest.Status.Phase == v1alpha1.RunComplete || latest.Status.Phase == v1alpha1.RunFailed {
		return nil
	}
	return latest
}

// roundInFlight reports that the current revise round has not ended: the
// Intent is Revising, or Blocked from Revising (the round resumes with it).
// Its notice is the round's own end to post, never a settled round's.
func (p *pass) roundInFlight() bool {
	switch p.in.Status.Phase {
	case v1alpha1.IntentRevising:
		return true
	case v1alpha1.IntentBlocked:
		return v1alpha1.IntentBlockedFrom(p.in) == v1alpha1.IntentRevising
	}
	return false
}

// resumePlan launches the plan round's next attempt for a resume, unless its
// latest run is still in flight (the block held it, never failed it: it is
// the run the resumed phase follows) or completed (its plan is written back
// or refused by the resumed phase), and returns the run that resumes active.
func (p *pass) resumePlan(ctx context.Context) (*v1alpha1.IntentRun, error) {
	rs := p.round(v1alpha1.IntentStagePlan, p.in.Status.Input.Revision, anyRepository)
	latest := rs.latest()
	switch {
	case latest != nil && latest.Status.Phase == v1alpha1.RunComplete:
		return nil, nil
	case latest != nil && latest.Status.Phase != v1alpha1.RunFailed:
		return latest, nil
	case rs.counted(p.planRefused) >= p.set.MaxAttempts || rs.next() > v1alpha1.MaxIntentRunAttempt:
		return nil, nil
	}
	return p.createRun(ctx, v1alpha1.IntentStagePlan, p.proj.Spec.Repositories[0], p.in.Status.Input.Revision,
		rs.next(), p.previousAttempt(latest))
}

// resumeBuilds launches, for a resume, the next attempt of each approved
// repository whose latest build had no accepted image to run on (while it
// has attempts left), and returns the run that resumes active: the first so
// launched, else the first build still in flight.
func (p *pass) resumeBuilds(ctx context.Context) (*v1alpha1.IntentRun, error) {
	repos, gone := p.approvedRepositories()
	if gone {
		return nil, nil // the resumed phase fails the intent
	}
	round := p.in.Status.Approval.PlanRevision
	var launched, inFlight *v1alpha1.IntentRun
	for _, repo := range repos {
		rs := p.round(v1alpha1.IntentStageBuild, round, repo.URL)
		latest := rs.latest()
		switch {
		case latest == nil || latest.Status.Phase == v1alpha1.RunComplete:
		case latest.Status.Phase != v1alpha1.RunFailed:
			if inFlight == nil {
				inFlight = latest
			}
		case imageBlocked(latest) && rs.counted(nil) < p.set.MaxAttempts && rs.next() <= v1alpha1.MaxIntentRunAttempt:
			run, err := p.createRun(ctx, v1alpha1.IntentStageBuild, repo, round, rs.next(), p.previousAttempt(latest))
			if err != nil {
				return nil, err
			}
			if launched == nil {
				launched = run
			}
		}
	}
	if launched != nil {
		return launched, nil
	}
	return inFlight, nil
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
	if meta.IsStatusConditionTrue(p.in.Status.Conditions, v1alpha1.ConditionRevisionLimitReached) &&
		p.revisionRounds() >= maxRevisions(p.proj) {
		return true, nil
	}
	if c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionChecksFailing); c != nil &&
		c.Status == metav1.ConditionTrue {
		limit := v1alpha1.DefaultMaxCheckFixes
		if p.proj.Spec.Limits.MaxCheckFixes != nil {
			limit = *p.proj.Spec.Limits.MaxCheckFixes
		}
		if c.Reason == "MaxCheckFixes" && p.checkFixRounds() >= limit {
			return true, nil
		}
		if c.Reason == "RepeatedFailure" {
			var blockedGeneration int64
			_, _ = fmt.Sscanf(c.Message, "project-generation=%d", &blockedGeneration)
			if blockedGeneration == p.proj.Generation {
				return true, nil
			}
		}
	}
	if holds, err := p.branchBlockHolds(ctx); holds || err != nil {
		return holds, err
	}
	return p.imageBlockHolds(ctx)
}

// branchBlockHolds reports a BranchConflict block still in force: read only
// when the poll is due, and under each app repository's rate floor, as every
// poll is; until then the block holds. Each repository the block can be
// about is checked on its own: a branch that is not this round's (or a
// stale one of an earlier round) where a build still has to launch, and a
// foreign pull request, or a branch deleted or moved, where a build
// completed and its pull request is still to open. A block from Revising
// is about the round's own repository.
func (p *pass) branchBlockHolds(ctx context.Context) (bool, error) {
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionBranchConflict)
	if c == nil || c.Status != metav1.ConditionTrue {
		return false, nil
	}
	if c.Reason == ReasonPullRequestRefused {
		var gen int64
		var known bool
		p.r.memo(func() { gen, known = p.r.blockedAt[p.in.Name] })
		return known && gen == p.proj.Generation, nil
	}
	if v1alpha1.IntentBlockedFrom(p.in) == v1alpha1.IntentRevising {
		return p.reviseBranchHolds(ctx, c.Reason)
	}
	repos, gone := p.approvedRepositories()
	ap := p.in.Status.Approval
	if gone || ap == nil {
		return false, nil // the resumed phase fails the intent
	}
	if !p.polled {
		return true, nil
	}
	for _, repo := range repos {
		check := p.branchHoldCheck(ctx, c.Reason, ap.PlanRevision, repo)
		if check == nil {
			continue
		}
		if ok, err := p.rateOK(ctx, repo.URL); err != nil || !ok {
			return true, err
		}
		if holds, err := check(); holds || err != nil {
			return true, err
		}
	}
	return false, nil
}

// branchHoldCheck is the read that tells whether a BranchConflict block of
// reason still holds in repo, or nil when the block cannot be about repo: a
// foreign pull request, or a branch deleted or moved, only where the build
// completed and its pull request is still to open; a branch that is not the
// round's own (or a stale one of an earlier round) only where a build has
// still to launch.
func (p *pass) branchHoldCheck(ctx context.Context, reason string, round int32,
	repo v1alpha1.ProjectRepository) func() (bool, error) {
	latest := p.round(v1alpha1.IntentStageBuild, round, repo.URL).latest()
	complete := latest != nil && latest.Status.Phase == v1alpha1.RunComplete
	toOpen := complete && p.pullRequest(repo.URL) == nil
	switch reason {
	case ReasonForeignPullRequest:
		if !toOpen {
			return nil
		}
		return func() (bool, error) {
			pr, own, _, err := p.findPullRequest(ctx, repo.URL)
			return pr != nil && !own, err
		}
	case ReasonBranchMissing, ReasonBranchChanged:
		if !toOpen {
			return nil
		}
		return func() (bool, error) { return p.branchRestoreHolds(ctx, latest) }
	}
	if latest != nil && latest.Status.Phase != v1alpha1.RunFailed {
		return nil // complete, or still in flight: its branch is its own
	}
	return func() (bool, error) {
		sha, _, err := p.branchConflict(ctx, repo.URL, round)
		return sha != "", err
	}
}

// reviseBranchHolds reports a BranchConflict block of a revise round still
// in force: the branch of the round's own repository still missing. Read
// only when the poll is due, under that repository's rate floor.
func (p *pass) reviseBranchHolds(ctx context.Context, reason string) (bool, error) {
	run := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository).latest()
	if run == nil || reason != ReasonBranchMissing {
		return false, nil
	}
	if !p.polled {
		return true, nil
	}
	if ok, err := p.rateOK(ctx, run.Spec.Repository.URL); err != nil || !ok {
		return true, err
	}
	_, err := p.r.GitHub.HeadSHA(ctx, run.Spec.Repository.URL, branchName(p.in.Name))
	if ghclient.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// branchRestoreHolds reports whether the intent branch in build's repository
// is still not at the commit build pushed: deleted, or moved.
func (p *pass) branchRestoreHolds(ctx context.Context, build *v1alpha1.IntentRun) (bool, error) {
	if build == nil || build.Status.PushedCommit == "" {
		return true, nil
	}
	head, err := p.r.GitHub.HeadSHA(ctx, build.Spec.Repository.URL, branchName(p.in.Name))
	if ghclient.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	return head != build.Status.PushedCommit, nil
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
// when the build round round did not push it there, or "": the branch is
// absent, or it is that round's own in repoURL. Nothing deletes an intent's
// branch when it ends, and an Intent's name is reused once the TTL deletes it
// (a reopened issue labelled again), so an earlier Intent's branch can still
// stand; someone with write access may also have created it. A build would
// spend its whole grant and then fail branch_exists, since the branch is
// never forced. Only the round's own builds in repoURL make the branch its
// own: a commit an earlier round of this Intent pushed (a sibling a failed
// round left behind, revived since) is stale, the run that pushed it
// returned so a block can say the branch is patchy's and safe to delete.
func (p *pass) branchConflict(ctx context.Context, repoURL string, round int32) (string, *v1alpha1.IntentRun,
	error) {
	branch := branchName(p.in.Name)
	head, err := p.r.GitHub.HeadSHA(ctx, repoURL, branch)
	switch {
	case ghclient.IsNotFound(err):
		return "", nil, nil
	case err != nil:
		return "", nil, fmt.Errorf("read the branch %s: %w", branch, err)
	}
	for _, run := range p.round(v1alpha1.IntentStageBuild, round, repoURL) {
		if run.Status.PushedCommit != "" && run.Status.PushedCommit == head {
			return "", nil, nil
		}
	}
	for _, run := range p.runs {
		if run.Status.PushedCommit != "" && run.Status.PushedCommit == head {
			return head, run, nil
		}
	}
	return head, nil, nil
}

// headUnmoved reports whether the default branch of every repository whose
// latest build was blocked on its image still points where that build pinned
// it: a new commit may declare an image the pin lacked. The block holds while
// any of them is unmoved, so the resume retries them together rather than
// spend an attempt on one whose head has not moved. Under a repository's rate
// floor it reads nothing, and the block holds.
func (p *pass) headUnmoved(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if ap == nil {
		return true, nil
	}
	repos, gone := p.approvedRepositories()
	if gone {
		return false, nil // the resumed phase fails the intent
	}
	for _, r := range repos {
		latest := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, r.URL).latest()
		if latest == nil || !imageBlocked(latest) {
			continue
		}
		if unmoved, err := p.pinUnmoved(ctx, latest); unmoved || err != nil {
			return true, err
		}
	}
	return false, nil
}

// pinUnmoved reports whether the default branch of build's repository still
// points where build's Repository pinned it.
func (p *pass) pinUnmoved(ctx context.Context, build *v1alpha1.IntentRun) (bool, error) {
	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: build.Namespace, Name: build.Spec.Repository.RepositoryRef.Name}
	if err := p.r.Get(ctx, key, &repo); err != nil || repo.Status.ResolvedSHA == "" {
		return true, client.IgnoreNotFound(err)
	}
	url := build.Spec.Repository.URL
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
