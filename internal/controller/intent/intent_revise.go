// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

const reviewQuiet = 2 * time.Minute
const reviewQuietMax = 10 * time.Minute

// Every byte a revise agent reads after the approved plan is bounded and
// visibly quoted. These bounds are on the escaped form, not only GitHub's
// original UTF-8, because one hidden rune can expand into many characters.
const (
	maxVisiblePatchBytes  = 48 << 10
	maxFeedbackCandidates = 200
)

var errInputUnavailable = errors.New("revise input unavailable")
var errNoUsableFeedback = errors.New("no usable feedback was found after filtering; no agent was launched")

// revising follows its active round while independently observing a human
// merge or close. A failed round returns to InReview; it never fails the
// Intent or silently pushes the failed agent's output. The round works on its
// own repository's pull request alone: one merged or closed under it while
// siblings stay in review (a multi-repository intent) has nothing left to
// push to, so the round is not carried on there. The run reconciler aborts
// its run (roundEnded), and the round then ends like a failed one, with its
// notice, and is never retried.
func (p *pass) revising(ctx context.Context) (bool, error) {
	if changed, err := p.review(ctx); changed || err != nil {
		return changed, err
	}
	if ok, err := p.rateOKForPullRequests(ctx); err != nil || !ok {
		return false, err
	}
	// A revise round's number is the Intent's own ordinal, never repeated
	// across repositories: the round is whichever repository's it is.
	if run := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository).latest(); run != nil &&
		run.Spec.Trigger == v1alpha1.IntentRunTriggerCommand {
		if err := p.ackPRCommand(ctx, run); err != nil {
			return false, err
		}
	}
	rs := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository)
	run := rs.latest()
	if run == nil {
		return false, fmt.Errorf("revising intent %s has no round %d run", p.in.Name, p.in.Status.Rounds)
	}
	switch run.Status.Phase {
	case v1alpha1.RunComplete:
		if err := p.finishPRRound(ctx, run); err != nil {
			return false, err
		}
		// The completed run owns one pushed commit. The phase write makes the
		// counter idempotent across reconciliation and restarts.
		return true, p.setPhase(ctx, v1alpha1.IntentInReview, func(cur *v1alpha1.Intent) {
			if run.Spec.Trigger == v1alpha1.IntentRunTriggerChecks {
				cur.Status.CheckFixes++
			} else {
				cur.Status.Revisions++
			}
			cur.Status.ActiveRun = nil
		})
	case v1alpha1.RunFailed:
		return p.failedRevise(ctx, run, rs)
	default:
		if !p.roundOpen(run) || p.leftProject(run.Spec.Repository.URL) {
			// Its pull request ended under it, or its repository left the
			// Project: the run reconciler aborts the run, and nothing is
			// created, launched or read there for it meanwhile.
			return false, nil
		}
		if blocked, err := p.missingPendingReviseBranch(ctx, run); blocked || err != nil {
			return blocked, err
		}
		if held, err := p.blockOnClass(ctx, []*v1alpha1.IntentRun{run}); held || err != nil {
			return held, err
		}
		return p.ensureActive(ctx, run)
	}
}

// missingPendingReviseBranch blocks a round not yet launched whose intent
// branch is gone from its repository before its Repository could pin it.
func (p *pass) missingPendingReviseBranch(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	if run.Status.Phase == v1alpha1.RunRunning {
		return false, nil
	}
	var repo v1alpha1.Repository
	err := p.r.Get(ctx, types.NamespacedName{Namespace: run.Namespace,
		Name: run.Spec.Repository.RepositoryRef.Name}, &repo)
	if err != nil && !kerrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && repo.Status.Artifact != nil && repo.Status.ResolvedSHA != "" {
		return false, nil
	}
	_, err = p.r.GitHub.HeadSHA(ctx, run.Spec.Repository.URL, branchName(p.in.Name))
	if ghclient.IsNotFound(err) {
		return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchMissing,
			fmt.Sprintf("the intent PR branch%s was deleted before its revision could be pinned; restore it to "+
				"resume", p.inRepository(run.Spec.Repository.URL)))
	}
	return false, err
}

// failedRevise retries a failed round's attempt when the failure allows it,
// on the round's own repository, and otherwise ends the round. A round whose
// pull request is no longer open, or whose repository has left the Project,
// is ended, never retried: nothing more is done there, and rounds being
// serialised per Intent, a round left waiting would hold up every other pull
// request's.
func (p *pass) failedRevise(ctx context.Context, run *v1alpha1.IntentRun, rs roundRuns) (bool, error) {
	// A round whose pod no node could fit ends, as one with no image to run
	// on does: another attempt would wait the same way, and the round's
	// notice says why; a check-fix round's failures are consumed with it.
	if run.Status.Outcome == OutcomeInputUnavailable || run.Status.Outcome == OutcomeImageRequired ||
		run.Status.Outcome == OutcomeNoUsableFeedback || run.Status.Outcome == OutcomeUnschedulable ||
		!p.roundOpen(run) {
		return p.endReviseRound(ctx, run)
	}
	if p.leftProject(run.Spec.Repository.URL) {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "a failed round's repository left the project; the round ends",
			slog.String("intent", p.in.Name), slog.String("run", run.Name),
			slog.String("repository", run.Spec.Repository.URL))
		return p.endReviseRound(ctx, run)
	}
	if run.Status.Outcome == OutcomeHeadMoved && run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks &&
		rs.next() <= v1alpha1.MaxIntentRunAttempt {
		if pr := p.pullRequest(run.Spec.Repository.URL); pr != nil {
			if _, err := p.r.GitHub.HeadSHA(ctx, pr.Repository, branchName(p.in.Name)); ghclient.IsNotFound(err) {
				return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchMissing,
					fmt.Sprintf("the intent PR branch%s was deleted during revision; restore it to resume",
						p.inRepository(pr.Repository)))
			} else if err != nil {
				return false, err
			}
		}
		moves := 0
		for _, prior := range rs {
			if prior.Status.Outcome == OutcomeHeadMoved {
				moves++
			}
		}
		if moves == 1 {
			return p.retryReviseAttempt(ctx, run, rs.next(), nil)
		}
	} else if run.Status.Outcome != OutcomeHeadMoved && rs.counted(nil) < p.set.MaxAttempts &&
		rs.next() <= v1alpha1.MaxIntentRunAttempt {
		return p.retryReviseAttempt(ctx, run, rs.next(), p.previousAttempt(run))
	}
	return p.endReviseRound(ctx, run)
}

func (p *pass) endReviseRound(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	if err := p.finishPRRound(ctx, run); err != nil {
		return false, err
	}
	return true, p.setPhase(ctx, v1alpha1.IntentInReview, func(cur *v1alpha1.Intent) {
		cur.Status.ActiveRun = nil
	})
}

// finishPRRound posts the round's notice on the pull request of the round's
// own repository, once (adopting the bot's earlier one by its marker, whose
// round number never repeats across repositories): what the round pushed and
// that review is requested again, or that it ended without a push. A round
// whose repository has no recorded pull request has nowhere to say it, and
// one whose repository has left the Project says nothing there: patchy
// writes nothing more to a repository the Project no longer holds, whose
// token is no longer one Ready proved, and which it may no longer reach.
func (p *pass) finishPRRound(ctx context.Context, run *v1alpha1.IntentRun) error {
	rec := p.pullRequest(run.Spec.Repository.URL)
	if rec == nil {
		return nil
	}
	if p.leftProject(run.Spec.Repository.URL) {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "a round's repository left the project; its notice is not posted",
			slog.String("intent", p.in.Name), slog.String("run", run.Name),
			slog.String("repository", run.Spec.Repository.URL))
		return nil
	}
	pr := *rec
	marker := fmt.Sprintf("<!-- patchy:intent-pr-round:%s:%d -->", p.in.Name, run.Spec.Round)
	since := run.CreationTimestamp.Add(-clockSkew)
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, since)
	if err != nil {
		return err
	}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if markerOf(c.Body) == marker && (bot == "" || strings.EqualFold(c.UserLogin, bot)) {
			return nil
		}
	}
	tail := ""
	if pr.State == prOpen && !terminal(p.in.Status.Phase) {
		tail = " The pull request remains open for review."
	}
	// A round failed checks started says so, and which: it never reads as a
	// revision from review feedback.
	label := "Revision round"
	if run.Spec.Trigger == v1alpha1.IntentRunTriggerChecks {
		checks, err := p.roundChecks(ctx, run)
		if err != nil {
			return err
		}
		label = templates.CIFixRound(checks)
	}
	body := marker + "\n" + label + " ended without a recorded completed push." + tail
	if run.Status.Outcome == OutcomeUnschedulable {
		// The scheduler's words name the cluster's nodes: they stay on the
		// run, for the operator, and never reach the pull request.
		body = marker + "\n" + label + " stopped: no node in the cluster could fit its agent, so it never ran " +
			"and nothing was pushed. The operator can see why on the intent's run." + tail
	}
	if run.Status.Outcome == OutcomeNoUsableFeedback {
		body = marker + "\nRevision round stopped: no usable feedback was found after filtering." + tail
		if run.Status.JobRef == nil {
			body = marker + "\nRevision round stopped: no usable feedback was found after filtering. " +
				"No agent was launched for this round." + tail
		}
	}
	if run.Status.Phase == v1alpha1.RunComplete {
		body = marker + "\n" + label + " pushed commit `" + run.Status.PushedCommit + "`."
		if pr.State == prOpen && !terminal(p.in.Status.Phase) {
			if err := p.r.GitHub.RequestReviewers(ctx, pr.Repository, pr.Number,
				p.proj.Spec.Approvers.Logins); err != nil {
				return fmt.Errorf("re-request PR reviewers: %w", err)
			}
			body += " Review is requested again."
		}
	}
	_, err = p.r.GitHub.CreatePullRequestComment(ctx, pr.Repository, pr.Number, body)
	return err
}

// roundChecks are the names of the failed checks a check-fix round fixed,
// as its input recorded them (keyCheckNames); none when the input is not
// the run's own, was never written, or predates the record.
func (p *pass) roundChecks(ctx context.Context, run *v1alpha1.IntentRun) ([]string, error) {
	var cm corev1.ConfigMap
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Inputs.ConfigMap}, &cm)
	switch {
	case kerrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the check-fix round's input: %w", err)
	case !controlledBy(cm.OwnerReferences, run.UID) || cm.Data[keyCheckNames] == "":
		return nil, nil
	}
	return strings.Split(cm.Data[keyCheckNames], "\n"), nil
}

// retryReviseAttempt leases one fresh attempt. A head move re-clones the
// branch and re-renders its diff; an agent failure carries PreviousAttempt.
func (p *pass) retryReviseAttempt(ctx context.Context, run *v1alpha1.IntentRun, attempt int32,
	previous *v1alpha1.PreviousAttempt) (bool, error) {
	repository, ok := p.approvedRepository(run.Spec.Repository.URL)
	if !ok {
		return false, errRepositoryGone
	}
	name := v1alpha1.IntentRunName(p.in.Name, v1alpha1.IntentStageRevise, run.Spec.Round,
		repository.Name, attempt)
	spec := *run.Spec.DeepCopy()
	spec.Attempt = attempt
	spec.Repository.RepositoryRef.Name = runRepositoryName(name)
	spec.Inputs.ConfigMap = runInputName(name)
	spec.PreviousAttempt = previous
	with := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: p.in.Namespace,
		Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
		Finalizers:      []string{v1alpha1.FinalizerJobs},
		OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
	}, Spec: spec}
	if err := p.r.Create(ctx, with); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return false, err
		}
		var existing v1alpha1.IntentRun
		if err := p.r.APIReader.Get(ctx, client.ObjectKeyFromObject(with), &existing); err != nil {
			return false, err
		}
		if existing.Spec.IntentRef.UID != p.in.UID || !reflect.DeepEqual(existing.Spec, spec) {
			return false, fmt.Errorf("revise retry %s: %w", name, errNotOwned)
		}
		with = &existing
	}
	p.runs = append(p.runs, with)
	return p.ensureActive(ctx, with)
}

// ensureReviseChildren creates the branch-pinned Repository before the
// immutable handoff: the handoff's compare patch must match that exact tree.
func (p *pass) ensureReviseChildren(ctx context.Context, run *v1alpha1.IntentRun) error {
	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name}
	var repo v1alpha1.Repository
	switch err := p.r.Get(ctx, key, &repo); {
	case err == nil:
		if !controlledBy(repo.OwnerReferences, run.UID) || repo.Spec.Ref.Branch != branchName(p.in.Name) ||
			!sameRepo(repo.Spec.URL, run.Spec.Repository.URL) {
			return fmt.Errorf("revise Repository %s: %w", key.Name, errNotOwned)
		}
	case kerrors.IsNotFound(err):
		repo = v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name, v1alpha1.LabelIntentRun: run.Name},
			OwnerReferences: []metav1.OwnerReference{runOwner(run)},
		}, Spec: v1alpha1.RepositorySpec{URL: run.Spec.Repository.URL,
			Ref: v1alpha1.RepositoryRef{Branch: branchName(p.in.Name)}}}
		if err := p.r.Create(ctx, &repo); err != nil && !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("create revise Repository %s: %w", key.Name, err)
		}
		return nil
	default:
		return err
	}
	if repo.Status.Artifact == nil || repo.Status.ResolvedSHA == "" {
		return nil
	}
	cmKey := types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Inputs.ConfigMap}
	var cm corev1.ConfigMap
	switch err := p.r.Get(ctx, cmKey, &cm); {
	case err == nil:
		if !controlledBy(cm.OwnerReferences, run.UID) {
			return fmt.Errorf("revise ConfigMap %s: %w", cmKey.Name, errNotOwned)
		}
		return nil
	case !kerrors.IsNotFound(err):
		return err
	}
	data, err := p.runInput(ctx, run)
	if errors.Is(err, errNoUsableFeedback) {
		data = map[string]string{keyNoFeedback: err.Error()}
	} else if errors.Is(err, errInputUnavailable) {
		data = map[string]string{keyInputRefusal: capVisible(err.Error(), 512)}
	} else if err != nil {
		return err
	}
	_, err = p.ensureConfigMap(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: cmKey.Name, Namespace: cmKey.Namespace,
		Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name, v1alpha1.LabelIntentRun: run.Name},
		OwnerReferences: []metav1.OwnerReference{runOwner(run)},
	}, Immutable: new(true), Data: data}, run.UID)
	return err
}

func (p *pass) reviseInput(ctx context.Context, run *v1alpha1.IntentRun, plan []byte) (map[string]string, error) {
	var repo v1alpha1.Repository
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace,
		Name: run.Spec.Repository.RepositoryRef.Name}, &repo); err != nil {
		return nil, err
	}
	if !controlledBy(repo.OwnerReferences, run.UID) || repo.Status.ResolvedSHA == "" {
		return nil, fmt.Errorf("revise Repository %s lacks an owned SHA pin", repo.Name)
	}
	// The round reads the feedback on, and the patch of, its own
	// repository's pull request alone.
	rec := p.pullRequest(run.Spec.Repository.URL)
	if rec == nil {
		return nil, fmt.Errorf("revise round %s has no recorded pull request in %s", run.Name,
			repoSlug(run.Spec.Repository.URL))
	}
	pr := *rec
	in, err := p.roundFeedback(ctx, run, pr, repo.Status.ResolvedSHA)
	if err != nil {
		if ghclient.IsRefused(err) || ghclient.IsNotFound(err) {
			return nil, fmt.Errorf("%w: GitHub refused the round's feedback: %v", errInputUnavailable, err)
		}
		return nil, err
	}
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks && in.feedback == "" {
		return nil, errNoUsableFeedback
	}
	// The compare base is the build of this round's own repository.
	build := p.round(v1alpha1.IntentStageBuild, run.Spec.Inputs.PlanRevision, run.Spec.Repository.URL).latest()
	if build == nil || build.Status.BaseSHA == "" {
		return nil, errors.New("revise round lacks the completed build's base SHA")
	}
	patch, err := p.r.GitHub.ComparePatch(ctx, pr.Repository, build.Status.BaseSHA, repo.Status.ResolvedSHA)
	if err != nil {
		if ghclient.IsRefused(err) || ghclient.IsNotFound(err) {
			return nil, fmt.Errorf("%w: GitHub refused the pinned compare patch: %v", errInputUnavailable, err)
		}
		return nil, err
	}
	round := roundText(run, repo.Status.ResolvedSHA, p.previousOutcome(run), in.earlier, in.feedback,
		visibleFeedback(patch))
	data := map[string]string{keyIssue: "", keyInvestigation: string(plan) + round,
		keyApprovedPlan: string(plan), keyCheckSignature: in.signature}
	if len(in.checks) > 0 {
		data[keyCheckNames] = strings.Join(in.checks, "\n")
	}
	return data, nil
}

// The first attempt leases the feedback time window for all retries. A later
// comment or edit cannot expand the agent's authority mid-round. The window
// is the round's own repository's: it opens after that repository's build,
// or its previous round, never after a sibling's round, so feedback left on
// one pull request while a round worked on another is read by its own next
// round rather than dropped.
func (p *pass) reviseWindow(run *v1alpha1.IntentRun) (time.Time, time.Time) {
	upper := run.CreationTimestamp.Time
	if leased := p.roundLeasedAt(run.Spec.Round); !leased.IsZero() {
		upper = leased
	}
	var cutoff time.Time
	if build := p.round(v1alpha1.IntentStageBuild, run.Spec.Inputs.PlanRevision,
		run.Spec.Repository.URL).latest(); build != nil &&
		build.Status.FinishedAt != nil {
		cutoff = build.Status.FinishedAt.Time
	}
	for _, older := range p.runs {
		if older.Spec.Stage == v1alpha1.IntentStageRevise && older.Spec.Round < run.Spec.Round &&
			sameRepo(older.Spec.Repository.URL, run.Spec.Repository.URL) {
			if leased := p.roundLeasedAt(older.Spec.Round); leased.After(cutoff) {
				cutoff = leased
			}
		}
	}
	return cutoff, upper
}

func (p *pass) verifyPRCommand(ctx context.Context, run *v1alpha1.IntentRun,
	pr v1alpha1.IntentPullRequest, cutoff, upper time.Time) (*ghclient.Comment, error) {
	if run.Spec.Inputs.CommandID == 0 {
		return nil, nil
	}
	c, err := p.r.GitHub.GetPullRequestComment(ctx, pr.Repository, run.Spec.Inputs.CommandID)
	if ghclient.IsNotFound(err) {
		return nil, fmt.Errorf("%w: PR command %d vanished", errInputUnavailable, run.Spec.Inputs.CommandID)
	}
	if err != nil {
		return nil, err
	}
	if c.NodeID == "" || c.CreatedAt.Before(cutoff) || c.CreatedAt.After(upper) {
		return nil, fmt.Errorf("%w: PR command %d changed", errInputUnavailable, c.ID)
	}
	approved, _, err := p.authorizeIn(ctx, pr.Repository, c.Author())
	if err != nil {
		return nil, err
	}
	if !approved {
		return nil, fmt.Errorf("%w: PR command %d is no longer from an approver", errInputUnavailable, c.ID)
	}
	wasEdited, err := p.r.GitHub.PullRequestCommentEdited(ctx, pr.Repository, c.NodeID)
	if errors.Is(err, ghclient.ErrNodeNotFound) || wasEdited {
		return nil, fmt.Errorf("%w: PR command %d was edited", errInputUnavailable, c.ID)
	}
	if err != nil {
		return nil, err
	}
	parsed, ok := prCommandParser.Parse(c.Body)
	if !ok || parsed.Verb != action.VerbRevise && parsed.Verb != action.VerbRetry {
		return nil, fmt.Errorf("%w: PR command %d no longer asks for a revision", errInputUnavailable, c.ID)
	}
	return c, nil
}

var prCommandParser = command.Parser{Surface: command.IntentPR}

// commandRound consumes one approver's unedited command on the intent PR.
// The PR is public, so neither a comment nor a reaction is authority by
// itself: the author must be in this Project's approvers and have write
// access to the application repository.
func (p *pass) commandRound(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	cutoff := p.reviewCutoff(pr.Repository)
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, cutoff.Add(-time.Second))
	if err != nil {
		return false, err
	}
	comments = slices.DeleteFunc(comments, func(c *ghclient.Comment) bool {
		if !isApprover(p.proj, c.UserLogin) {
			return true
		}
		parsed, ok := prCommandParser.Parse(c.Body)
		return !ok || parsed.Verb != action.VerbRevise && parsed.Verb != action.VerbRetry
	})
	if len(comments) > maxFeedbackCandidates {
		comments = comments[len(comments)-maxFeedbackCandidates:]
	}
	slices.SortFunc(comments, func(a, b *ghclient.Comment) int { return int(a.ID - b.ID) })
	for _, c := range comments {
		eligible, err := p.eligiblePRCommand(ctx, pr, c, cutoff)
		if err != nil {
			return false, err
		}
		if !eligible {
			continue
		}
		if limit := maxRevisions(p.proj); p.revisionRounds() >= limit ||
			p.in.Status.Rounds >= v1alpha1.MaxIntentRound {
			return true, p.block(ctx, v1alpha1.ConditionRevisionLimitReached, "MaxRevisions",
				fmt.Sprintf("PR command%s waits: %d of %d permitted revision rounds have started",
					p.inRepository(pr.Repository), p.revisionRounds(), limit))
		}
		run, err := p.createReviseRun(ctx, pr, p.in.Status.Rounds+1,
			v1alpha1.IntentRunTriggerCommand, nil, nil, nil, c.ID)
		if err != nil {
			return false, err
		}
		if err := p.ackPRCommand(ctx, run); err != nil {
			return false, err
		}
		return true, p.enterRevising(ctx, run)
	}
	return false, nil
}

func (p *pass) eligiblePRCommand(ctx context.Context, pr *v1alpha1.IntentPullRequest,
	c *ghclient.Comment, cutoff time.Time) (bool, error) {
	if c.ID < 1 || c.NodeID == "" || c.CreatedAt.Before(cutoff) {
		return false, nil
	}
	parsed, ok := prCommandParser.Parse(c.Body)
	if !ok || parsed.Verb != action.VerbRevise && parsed.Verb != action.VerbRetry {
		return false, nil
	}
	for _, run := range p.runs {
		if run.Spec.Inputs.CommandID == c.ID {
			return false, nil
		}
	}
	approved, _, err := p.authorizeIn(ctx, pr.Repository, c.Author())
	if err != nil || !approved {
		return false, err
	}
	wasEdited, err := p.r.GitHub.PullRequestCommentEdited(ctx, pr.Repository, c.NodeID)
	if err != nil || wasEdited {
		return false, err
	}
	if parsed.Verb == action.VerbRetry {
		// A retry retries a failed round of this pull request's own
		// repository, never a sibling's.
		for _, run := range p.runs {
			if run.Spec.Stage == v1alpha1.IntentStageRevise && run.Status.Phase == v1alpha1.RunFailed &&
				sameRepo(run.Spec.Repository.URL, pr.Repository) {
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}

// ackPRCommand writes one eyes reaction and one marker reply, adopting an
// earlier reply if a status write or restart interrupted the pass. A marker
// counts only when the App's own bot wrote it, never merely for its text.
// The command is on the pull request of the round's own repository, where
// the reply goes.
func (p *pass) ackPRCommand(ctx context.Context, run *v1alpha1.IntentRun) error {
	id := run.Spec.Inputs.CommandID
	rec := p.pullRequest(run.Spec.Repository.URL)
	if id < 1 || rec == nil || p.leftProject(run.Spec.Repository.URL) {
		// Nothing is written to a repository that left the Project.
		return nil
	}
	pr := *rec
	c, err := p.r.GitHub.GetPullRequestComment(ctx, pr.Repository, id)
	if ghclient.IsNotFound(err) {
		return nil // the immutable run input will refuse the vanished command
	}
	if err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- patchy:intent-pr-command:%s:%d -->", p.in.Name, id)
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, c.CreatedAt.Add(-time.Second))
	if err != nil {
		return err
	}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	if err != nil {
		return err
	}
	for _, reply := range comments {
		if markerOf(reply.Body) == marker && (bot == "" || strings.EqualFold(reply.UserLogin, bot)) {
			return nil
		}
	}
	if err := p.r.GitHub.ReactPullRequestComment(ctx, pr.Repository, id); err != nil {
		return err
	}
	_, err = p.r.GitHub.CreatePullRequestComment(ctx, pr.Repository, pr.Number,
		marker+"\nRevision round started. This command and authorised PR feedback will be treated as data, not instructions.")
	return err
}

// visibleFeedback makes untrusted GitHub text legible to both a human and
// the model. CRLF, LF and tabs keep their ordinary layout; every other
// invisible, invalid byte or non-ASCII separator becomes a printed escape.
func visibleFeedback(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			fmt.Fprintf(&b, "<0x%02X>", s[i])
			i++
			continue
		}
		switch {
		case r == '\n' || r == '\t', r == '\r' && i+1 < len(s) && s[i+1] == '\n':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) ||
			unicode.Is(unicode.Variation_Selector, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
			fmt.Fprintf(&b, "<U+%04X>", r)
		default:
			b.WriteRune(r)
		}
		i += width
	}
	return b.String()
}

func capVisible(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const note = "\n[truncated]\n"
	end := maxBytes - len(note)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + note
}

// fencedBounded keeps the enclosing fence inside the item bound too. An
// attacker may send a 2-KiB run of backticks; the normal dynamic fence would
// then add two more 2-KiB lines. In that case the backticks are shown as
// visible code-point escapes and a fixed short fence suffices.
func fencedBounded(s string, maxBytes int) string {
	s = capVisible(s, maxBytes-16)
	if f := fenced(s); len(f) <= maxBytes {
		return f
	}
	s = capVisible(strings.ReplaceAll(s, "`", "<U+0060>"), maxBytes-16)
	return fenced(s)
}

// adoptPendingRound enters Revising on the round a pass created just before
// its status write failed (or the controller restarted): the run of round
// Rounds+1, whatever its repository and whatever state its pull request is
// in now. Its deterministic create is the round's lease, and it is adopted
// before any pull request's feedback is considered, so a lease left on one
// pull request never stands in the way of another's round: a round whose
// pull request has since been merged or closed is entered all the same, and
// ends there without a push (revising), freeing the Intent for the next.
// The runs it reads are the cache's; createReviseRun checks the API server
// itself before it leases a round (errRoundLeased).
func (p *pass) adoptPendingRound(ctx context.Context) (bool, error) {
	pending := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds+1, anyRepository).latest()
	if pending == nil {
		return false, nil
	}
	return true, p.adoptRound(ctx, pending)
}

// adoptRound enters Revising on run, a round already leased, acknowledging
// its command first when a command started it.
func (p *pass) adoptRound(ctx context.Context, run *v1alpha1.IntentRun) error {
	if run.Spec.Trigger == v1alpha1.IntentRunTriggerCommand {
		if err := p.ackPRCommand(ctx, run); err != nil {
			return err
		}
	}
	return p.enterRevising(ctx, run)
}

// errRoundLeased is createReviseRun's refusal to lease a round the API
// server already holds a run of: its run, whatever its repository, is the
// round, and the caller adopts it (adoptRound).
type errRoundLeased struct{ run *v1alpha1.IntentRun }

func (e *errRoundLeased) Error() string {
	return fmt.Sprintf("revise round %d is already leased by run %s", e.run.Spec.Round, e.run.Name)
}

// adoptLeased adopts the round err says is already leased; adopted is false
// for any other err, which the caller returns as it is.
func (p *pass) adoptLeased(ctx context.Context, err error) (adopted bool, _ error) {
	var leased *errRoundLeased
	if !errors.As(err, &leased) {
		return false, nil
	}
	return true, p.adoptRound(ctx, leased.run)
}

// leasedRound is this Intent's run of revise round round as the API server
// holds it, whatever its repository (its latest attempt), or nil. A round's
// number is its lease across repositories, and the run names that make the
// lease differ by repository, so only a live read can tell a round leased a
// moment ago, under another repository's name, from a free one: the cache
// the pass listed its runs from can lag that create.
func (p *pass) leasedRound(ctx context.Context, round int32) (*v1alpha1.IntentRun, error) {
	var list v1alpha1.IntentRunList
	if err := p.r.APIReader.List(ctx, &list, client.InNamespace(p.in.Namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: p.in.Name}); err != nil {
		return nil, fmt.Errorf("list the intent's runs: %w", err)
	}
	var leased *v1alpha1.IntentRun
	for i := range list.Items {
		run := &list.Items[i]
		if run.Spec.IntentRef.UID == p.in.UID && run.Spec.Stage == v1alpha1.IntentStageRevise &&
			run.Spec.Round == round && (leased == nil || run.Spec.Attempt > leased.Spec.Attempt) {
			leased = run
		}
	}
	return leased, nil
}

// reviewRound starts one review-driven round on pr after approvers' reviews
// have been quiet for two minutes (at most ten from the first request).
func (p *pass) reviewRound(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	round := p.in.Status.Rounds + 1
	reviews, err := p.r.GitHub.ListPullRequestReviews(ctx, pr.Repository, pr.Number)
	if err != nil {
		return false, fmt.Errorf("list reviews on %s#%d: %w", pr.Repository, pr.Number, err)
	}
	reviews = slices.DeleteFunc(reviews, func(r ghclient.Review) bool {
		return !isApprover(p.proj, r.Author.Login)
	})
	cutoff := p.reviewCutoff(pr.Repository)
	eligible, requests, err := p.eligibleReviews(ctx, pr, reviews, cutoff)
	if err != nil {
		return false, err
	}
	if len(requests) == 0 {
		return false, nil
	}
	slices.SortFunc(requests, func(a, b ghclient.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	first, last := requests[0].SubmittedAt, requests[len(requests)-1].SubmittedAt
	quietUntil := last.Add(reviewQuiet)
	if maxUntil := first.Add(reviewQuietMax); quietUntil.After(maxUntil) {
		quietUntil = maxUntil
	}
	if p.now.Before(quietUntil) {
		return false, nil
	}
	if limit := maxRevisions(p.proj); p.revisionRounds() >= limit || round > v1alpha1.MaxIntentRound {
		return true, p.block(ctx, v1alpha1.ConditionRevisionLimitReached, "MaxRevisions",
			fmt.Sprintf("review feedback%s waits: %d of %d permitted revision rounds have started",
				p.inRepository(pr.Repository), p.revisionRounds(), limit))
	}
	slices.SortFunc(eligible, func(a, b ghclient.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	if len(eligible) > 32 {
		eligible = eligible[len(eligible)-32:]
	}
	ids := make([]int64, 0, len(eligible))
	for _, r := range eligible {
		ids = append(ids, r.ID)
	}
	run, err := p.createReviseRun(ctx, pr, round, v1alpha1.IntentRunTriggerReview, ids, nil, nil, 0)
	if err != nil {
		return false, err
	}
	return true, p.enterRevising(ctx, run)
}

func (p *pass) eligibleReviews(ctx context.Context, pr *v1alpha1.IntentPullRequest,
	reviews []ghclient.Review, cutoff time.Time) ([]ghclient.Review, []ghclient.Review, error) {
	slices.SortFunc(reviews, func(a, b ghclient.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	if len(reviews) > maxFeedbackCandidates {
		reviews = reviews[len(reviews)-maxFeedbackCandidates:]
	}
	var eligible, requests []ghclient.Review
	for _, review := range reviews {
		if review.ID < 1 || review.NodeID == "" || review.SubmittedAt.Before(cutoff) || p.reviewConsumed(review.ID) {
			continue
		}
		ok, _, err := p.authorizeIn(ctx, pr.Repository, review.Author)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		edited, err := p.r.GitHub.ReviewEdited(ctx, pr.Repository, review.NodeID)
		if err != nil {
			return nil, nil, fmt.Errorf("verify review %d was never edited: %w", review.ID, err)
		}
		if !edited {
			eligible = append(eligible, review)
			if strings.EqualFold(review.State, "CHANGES_REQUESTED") {
				requests = append(requests, review)
			}
		}
	}
	return eligible, requests, nil
}

func maxRevisions(proj *v1alpha1.Project) int32 {
	if proj.Spec.Limits.MaxRevisions != nil {
		return *proj.Spec.Limits.MaxRevisions
	}
	return v1alpha1.DefaultMaxRevisions
}

// revisionRounds counts leased review/command rounds, including agent failures.
// A round refused before launch for lack of usable feedback does not spend
// this allowance; the independent MaxIntentRound still bounds such rounds.
func (p *pass) revisionRounds() int32 {
	seen := map[int32]bool{}
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageRevise && run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
			if latest := p.round(v1alpha1.IntentStageRevise, run.Spec.Round, anyRepository).latest(); latest != nil &&
				latest.Status.Phase == v1alpha1.RunFailed && latest.Status.Outcome == OutcomeNoUsableFeedback {
				continue
			}
			seen[run.Spec.Round] = true
		}
	}
	return int32(len(seen))
}

// reviewCutoff ignores feedback on the pull request in repoURL predating its
// build, and predating its own previous round. IDs consumed by its run are
// also recorded there; the time bound covers a burst larger than the
// schema's 32 IDs without starting duplicate rounds from its tail. Only
// rounds in repoURL move it: rounds are serialised per Intent, so a review on
// one pull request may wait out a round on another, and is still read by its
// own pull request's next round.
func (p *pass) reviewCutoff(repoURL string) time.Time {
	var at time.Time
	if ap := p.in.Status.Approval; ap != nil {
		if build := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, repoURL).latest(); build != nil &&
			build.Status.FinishedAt != nil {
			at = build.Status.FinishedAt.Time
		}
	}
	for _, run := range p.runs {
		if run.Spec.Stage != v1alpha1.IntentStageRevise || !sameRepo(run.Spec.Repository.URL, repoURL) {
			continue
		}
		if t := p.roundLeasedAt(run.Spec.Round); t.After(at) {
			at = t
		}
	}
	return at
}

// roundLeasedAt is when a revise round was leased: its first attempt's
// creation. It closes the round's feedback window (reviseWindow), which
// every retry attempt reads again unchanged, and so opens the next round's
// in the same repository. A retry attempt's own, later, creation is never a
// boundary: feedback left while a failing attempt ran is after the round's
// window, and is read by the next round rather than dropped.
func (p *pass) roundLeasedAt(round int32) time.Time {
	if rs := p.round(v1alpha1.IntentStageRevise, round, anyRepository); len(rs) > 0 {
		return rs[0].CreationTimestamp.Time
	}
	return time.Time{}
}

// reviewConsumed reports a review some round already took (its ReviewIDs):
// a review is consumed once, by id, whatever its time says.
func (p *pass) reviewConsumed(id int64) bool {
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageRevise && slices.Contains(run.Spec.Inputs.ReviewIDs, id) {
			return true
		}
	}
	return false
}

func (p *pass) enterRevising(ctx context.Context, run *v1alpha1.IntentRun) error {
	return p.setPhase(ctx, v1alpha1.IntentRevising, func(cur *v1alpha1.Intent) {
		cur.Status.Rounds = run.Spec.Round
		cur.Status.ActiveRun = &v1alpha1.ObjectReference{Name: run.Name, UID: run.UID}
	})
}

// createReviseRun creates the immutable round lease. ImageFrom names the
// build round's Repository by UID, so source-controller's new pin for the
// PR head can never choose a different runner image.
func (p *pass) createReviseRun(ctx context.Context, pr *v1alpha1.IntentPullRequest, round int32,
	trigger v1alpha1.IntentRunTrigger, reviewIDs, checkIDs, statusIDs []int64,
	commandID int64) (*v1alpha1.IntentRun, error) {
	ap := p.in.Status.Approval
	if ap == nil {
		return nil, errors.New("a revise round has no approved plan")
	}
	// The round runs in its own repository's build-round image, never a
	// sibling's: the build of the pull request's repository.
	build := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, pr.Repository).latest()
	if build == nil || build.Status.Phase != v1alpha1.RunComplete {
		return nil, errors.New("a revise round has no completed build")
	}
	var imageRepo v1alpha1.Repository
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace,
		Name: build.Spec.Repository.RepositoryRef.Name}, &imageRepo); err != nil {
		return nil, fmt.Errorf("read the build round's image source: %w", err)
	}
	if !controlledBy(imageRepo.OwnerReferences, build.UID) || imageRepo.UID == "" {
		return nil, fmt.Errorf("build image Repository %s is not the build run's own", imageRepo.Name)
	}
	repo, ok := p.approvedRepository(pr.Repository)
	if !ok {
		return nil, errRepositoryGone
	}
	name := v1alpha1.IntentRunName(p.in.Name, v1alpha1.IntentStageRevise, round, repo.Name, 1)
	spec := v1alpha1.IntentRunSpec{
		IntentRef: v1alpha1.ObjectReference{Name: p.in.Name, UID: p.in.UID},
		Stage:     v1alpha1.IntentStageRevise, Trigger: trigger,
		Repository: v1alpha1.IntentRunRepository{URL: repo.URL,
			RepositoryRef: v1alpha1.LocalObjectReference{Name: runRepositoryName(name)}},
		Round: round, Attempt: 1,
		Inputs: v1alpha1.IntentRunInputs{ConfigMap: runInputName(name), InputDigest: ap.InputDigest,
			PlanRevision: ap.PlanRevision, PlanDigest: ap.PlanDigest, ReviewIDs: slices.Clone(reviewIDs),
			CheckRunIDs: slices.Clone(checkIDs), StatusIDs: slices.Clone(statusIDs), CommandID: commandID},
		ImageFrom: &v1alpha1.ObjectReference{Name: imageRepo.Name, UID: imageRepo.UID},
		Grant:     p.set.grant(p.proj, v1alpha1.IntentStageRevise),
	}
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: p.in.Namespace,
		Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
		Finalizers:      []string{v1alpha1.FinalizerJobs},
		OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
	}, Spec: spec}
	if len(p.in.Status.PullRequests) > 1 {
		// Rounds span repositories, and the run name (the lease) is the
		// repository's: a round leased under another repository's name a
		// moment ago is found only live. With one pull request every round
		// takes the same name, whose create below is the lease.
		leased, err := p.leasedRound(ctx, round)
		if err != nil {
			return nil, err
		}
		if leased != nil {
			return nil, &errRoundLeased{run: leased}
		}
	}
	if err := p.r.Create(ctx, run); err == nil {
		p.runs = append(p.runs, run)
		return run, nil
	} else if !kerrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create revise run %s: %w", name, err)
	}
	var existing v1alpha1.IntentRun
	if err := p.r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), &existing); err != nil {
		return nil, err
	}
	if existing.Spec.IntentRef.UID != p.in.UID || !reflect.DeepEqual(existing.Spec, spec) {
		return nil, fmt.Errorf("revise run %s: %w", name, errNotOwned)
	}
	return &existing, nil
}
