// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// Pull request states an Intent records.
const (
	prOpen   = "open"
	prMerged = "merged"
	prClosed = "closed"
)

// ImageRequired reasons: why a build could not run on an accepted
// repository-declared image, and so what lifts the block.
const (
	ReasonNoRepositoryImage       = "NoRepositoryImage"
	ReasonRepositoryImageRejected = "RepositoryImageRejected"
	ReasonRepositoryImagesOff     = "RepositoryImagesDisabled"
	ReasonSandboxBreaker          = "SandboxBreakerTripped"
	ReasonDefaultImageRan         = "DefaultImageRan"
)

// imageReason maps a blocked build run to its ImageRequired reason. Only the
// controller writes what it reads: an image_required run's detail is the
// controller's own (a pod may not report that outcome: podOutcome), and the
// SandboxRefused condition is set from the Job's status.
func imageReason(run *v1alpha1.IntentRun) string {
	switch {
	case runnerguard.Refused(run.Status.Conditions):
		return ReasonSandboxBreaker
	case strings.HasPrefix(run.Status.Detail, runnerguard.SkipDisabled):
		return ReasonRepositoryImagesOff
	case strings.HasPrefix(run.Status.Detail, runnerguard.SkipBreaker):
		return ReasonSandboxBreaker
	case strings.HasPrefix(run.Status.Detail, runnerguard.SkipRejected):
		return ReasonRepositoryImageRejected
	case strings.HasPrefix(run.Status.Detail, runnerguard.SkipNoImage):
		return ReasonNoRepositoryImage
	}
	return ReasonDefaultImageRan
}

// building runs the approved plan's build round: a first attempt, the pull
// request once a build has pushed its branch, a block when the build had no
// accepted image to run on, and the next attempt after a failed one, until
// the attempts are spent and the Intent fails.
func (p *pass) building(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if ap == nil {
		return true, p.fail(ctx)
	}
	rs := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision)
	latest := rs.latest()
	if latest == nil {
		return p.launch(ctx, v1alpha1.IntentStageBuild, ap.PlanRevision, 1, nil)
	}
	switch latest.Status.Phase {
	case v1alpha1.RunComplete:
		return p.openPullRequest(ctx, latest)
	case v1alpha1.RunFailed:
		if imageBlocked(latest) {
			if rs.next() > v1alpha1.MaxIntentRunAttempt {
				// No attempt is left to try the image again with: a block
				// could never lift, and resuming from it would only block
				// again.
				p.r.log().LogAttrs(ctx, slog.LevelWarn, "the build's attempts are spent on image blocks; the intent fails",
					slog.String("intent", p.in.Name), slog.String("run", latest.Name))
				return true, p.fail(ctx)
			}
			return true, p.block(ctx, v1alpha1.ConditionImageRequired, imageReason(latest),
				fmt.Sprintf("the build could not run on an accepted repository image: %s", latest.Status.Detail))
		}
	default:
		return p.ensureActive(ctx, latest)
	}
	if rs.counted(nil) >= p.set.MaxAttempts {
		return true, p.fail(ctx)
	}
	return p.launch(ctx, v1alpha1.IntentStageBuild, ap.PlanRevision, rs.next(), p.previousAttempt(latest))
}

// openPullRequest opens the pull request for a build that pushed the intent
// branch, or adopts the one a failed pass opened, records it, and moves to
// InReview. Only patchy's own is adopted (ownPullRequest); an open pull
// request from the branch that is anyone else's blocks the Intent on
// BranchConflict until it is closed, since GitHub keeps one open pull request
// per head and base. The title and body are patchy's own, from the approved
// plan: the body says "Part of" the intent issue and holds no closing
// keyword.
func (p *pass) openPullRequest(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	repoURL := run.Spec.Repository.URL
	branch := branchName(p.in.Name)
	pr, own, base, err := p.findPullRequest(ctx, repoURL)
	if err != nil {
		if ghclient.IsRefused(err) {
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
				fmt.Sprintf("GitHub refused to look for the pull request from %s: %v; fix the refusal and update "+
					"the Project to retry", branch, err))
		}
		return false, err
	}
	if pr != nil && !own {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "an open pull request patchy did not open holds the intent branch",
			slog.String("intent", p.in.Name), slog.String("pullRequest", pr.HTMLURL), slog.String("author", pr.Author))
		return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonForeignPullRequest,
			fmt.Sprintf("an open pull request patchy did not open, %s (by %s, from %s into %s), already holds the "+
				"branch %s; patchy neither adopts it nor opens its own beside it. Close it to resume.",
				pr.HTMLURL, pr.Author, pr.HeadRepo, pr.Base, branch))
	}
	if pr == nil {
		// A completed run is not proof its branch still exists. A human may
		// delete or move it between the push and this pass. Do not retry a
		// doomed PR create, or open one from a different commit.
		head, headErr := p.r.GitHub.HeadSHA(ctx, repoURL, branch)
		switch {
		case ghclient.IsNotFound(headErr):
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchMissing,
				fmt.Sprintf("branch %s was deleted after the build pushed %s; restore it at that commit to resume",
					branch, run.Status.PushedCommit))
		case ghclient.IsRefused(headErr):
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
				fmt.Sprintf("GitHub refused to read branch %s: %v; fix access and update the Project to retry",
					branch, headErr))
		case headErr != nil:
			return false, fmt.Errorf("read the intent branch before opening its pull request: %w", headErr)
		case head != run.Status.PushedCommit:
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchChanged,
				fmt.Sprintf("branch %s moved from the build's commit %s to %s before patchy opened its pull request; "+
					"restore it to the build's commit to resume", branch, run.Status.PushedCommit, head))
		}
		ap, pl := p.in.Status.Approval, p.in.Status.Plan
		body, err := templates.RenderIntentPRBody(templates.IntentPRBody{
			IntentRepository: repoSlug(p.repo()), IssueNumber: p.number(), Summary: pl.Summary,
			PlanRevision: ap.PlanRevision, PlanDigest: ap.PlanDigest, ApprovedBy: ap.By,
		})
		if err != nil {
			return false, err
		}
		if pr, err = p.r.GitHub.CreatePullRequest(ctx, repoURL, ghclient.PRRequest{
			Title: templates.IntentPRTitle(p.proj.Name, pl.Summary), Head: branch, Base: base, Body: body,
		}); err != nil {
			if ghclient.IsRefused(err) {
				return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
					fmt.Sprintf("GitHub refused to open the pull request from %s: %v; fix the refusal and update the "+
						"Project to retry", branch, err))
			}
			return false, fmt.Errorf("open the pull request: %w", err)
		}
	}
	head := pr.HeadSHA
	if head == "" {
		head = run.Status.PushedCommit
	}
	rec := v1alpha1.IntentPullRequest{
		Repository: repoURL, Number: int64(pr.Number), URL: pr.HTMLURL, NodeID: pr.NodeID, HeadSHA: head,
		State: prOpen,
	}
	return true, p.setPhase(ctx, v1alpha1.IntentInReview, func(cur *v1alpha1.Intent) {
		cur.Status.Branch = branch
		cur.Status.PullRequests = []v1alpha1.IntentPullRequest{rec}
		cur.Status.ActiveRun = nil
	})
}

// findPullRequest reads the default branch of repoURL and the open pull
// request from the intent branch into it, if any, and whether it is patchy's
// own.
func (p *pass) findPullRequest(ctx context.Context, repoURL string) (pr *ghclient.PR, own bool, base string,
	err error) {
	base, err = p.r.GitHub.DefaultBranch(ctx, repoURL)
	if err != nil {
		return nil, false, "", fmt.Errorf("read the default branch: %w", err)
	}
	pr, err = p.r.GitHub.FindPullRequest(ctx, repoURL, branchName(p.in.Name), base)
	if err != nil || pr == nil {
		if err != nil {
			err = fmt.Errorf("find the pull request: %w", err)
		}
		return nil, false, base, err
	}
	own, err = p.ownPullRequest(ctx, repoURL, pr, base)
	return pr, own, base, err
}

// ownPullRequest reports a pull request found open from the intent branch
// that patchy opened: one a pass that failed after opening it left behind.
// On a public repository anyone can open a pull request from an existing
// branch, with any title, body and base, so it is patchy's only when
// patchy's bot opened it, into the default branch, from the repository itself
// (never a fork). With a personal access token there is no bot identity (dev
// only), and only the base and the head repository are checked. Its head
// commit is not: humans may push to the branch.
func (p *pass) ownPullRequest(ctx context.Context, repoURL string, pr *ghclient.PR, base string) (bool, error) {
	if pr.Base != base || !strings.EqualFold(pr.HeadRepo, repoSlug(repoURL)) {
		return false, nil
	}
	bot, err := p.r.GitHub.BotLogin(ctx, repoURL)
	if err != nil {
		return false, fmt.Errorf("read the App's bot login: %w", err)
	}
	return bot == "" || strings.EqualFold(pr.Author, bot), nil
}

// review polls the recorded pull requests, by repository and number, and
// ends the Intent when every one has merged (Merged) or closed unmerged
// (Closed). Under the rate floor of a pull request's repository it polls
// nothing and waits a whole interval.
func (p *pass) review(ctx context.Context) (bool, error) {
	var last time.Time
	p.r.memo(func() { last = p.r.prPolled[p.in.Name] })
	if !last.IsZero() && p.now.Sub(last) < p.set.PRPollInterval {
		return false, nil
	}
	if ok, err := p.rateOKForPullRequests(ctx); err != nil || !ok {
		p.r.memo(func() { p.r.prPolled[p.in.Name] = p.now })
		return false, err
	}
	return p.reviewNow(ctx)
}

// reviewNow is review without waiting for the poll interval.
func (p *pass) reviewNow(ctx context.Context) (bool, error) {
	p.r.memo(func() { p.r.prPolled[p.in.Name] = p.now })
	prs := make([]v1alpha1.IntentPullRequest, len(p.in.Status.PullRequests))
	copy(prs, p.in.Status.PullRequests)
	if len(prs) == 0 {
		return false, nil
	}
	merged, closed, mergedAt, readable, err := p.readReviewPRStates(ctx, prs)
	if err != nil || !readable {
		return false, err
	}
	switch {
	case merged == len(prs):
		return true, p.merged(ctx, prs, mergedAt)
	case closed == len(prs):
		// Every pull request closed unmerged: patchy closes the issue.
		if err := p.r.GitHub.CloseIssue(ctx, p.repo(), p.number(), ghclient.CloseNotPlanned); err != nil {
			return false, fmt.Errorf("close the issue: %w", err)
		}
		return true, p.setPhase(ctx, v1alpha1.IntentClosed, func(cur *v1alpha1.Intent) {
			cur.Status.PullRequests = prs
		})
	}
	if !prsEqual(prs, p.in.Status.PullRequests) {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.PullRequests = prs
			return nil
		})
	}
	if changed, err := p.syncPRRoundNotices(ctx); changed || err != nil {
		return changed, err
	}
	if p.in.Status.Phase == v1alpha1.IntentInReview && len(prs) == 1 && prs[0].State == prOpen {
		if started, err := p.reviewRound(ctx, &prs[0]); started || err != nil {
			return started, err
		}
		if started, err := p.commandRound(ctx, &prs[0]); started || err != nil {
			return started, err
		}
		return p.checkRound(ctx, &prs[0])
	}
	return false, nil
}

func (p *pass) readReviewPRStates(ctx context.Context, prs []v1alpha1.IntentPullRequest) (
	merged, closed int, mergedAt time.Time, readable bool, err error) {
	for i := range prs {
		rec := &prs[i]
		ok, err := p.readPullRequest(ctx, rec)
		if err != nil || !ok {
			return 0, 0, time.Time{}, false, err
		}
		switch rec.State {
		case prMerged:
			merged++
			if rec.MergedAt != nil && (mergedAt.IsZero() || rec.MergedAt.Time.Before(mergedAt)) {
				mergedAt = rec.MergedAt.Time
			}
		case prClosed:
			closed++
		}
	}
	return merged, closed, mergedAt, true, nil
}

// readPullRequest reads one recorded pull request by its repository and
// number and updates rec from it. readable is false when GitHub will not
// show it (404, 403) or it is not the pull request recorded (another node
// id): the Intent waits rather than guess.
func (p *pass) readPullRequest(ctx context.Context, rec *v1alpha1.IntentPullRequest) (readable bool, err error) {
	pr, err := p.r.GitHub.GetPullRequest(ctx, rec.Repository, rec.Number)
	if ghclient.IsNotFound(err) || ghclient.IsForbidden(err) {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "recorded pull request unreadable; the intent waits",
			slog.String("intent", p.in.Name), slog.String("repository", rec.Repository),
			slog.Int64("number", rec.Number), slog.Any("error", err))
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read pull request %s#%d: %w", rec.Repository, rec.Number, err)
	}
	if rec.NodeID != "" && pr.NodeID != "" && pr.NodeID != rec.NodeID {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "pull request is not the one recorded; the intent waits",
			slog.String("intent", p.in.Name), slog.String("repository", rec.Repository),
			slog.Int64("number", rec.Number), slog.String("recorded", rec.NodeID), slog.String("found", pr.NodeID))
		return false, nil
	}
	if pr.HeadSHA != "" {
		rec.HeadSHA = pr.HeadSHA
	}
	switch {
	case pr.Merged:
		rec.State = prMerged
		rec.MergeCommitSHA = pr.MergeCommitSHA
		if !pr.MergedAt.IsZero() {
			t := metav1.NewTime(pr.MergedAt)
			rec.MergedAt = &t
		}
	case pr.State == prClosed:
		rec.State = prClosed
	default:
		rec.State = prOpen
	}
	return true, nil
}

// merged completes the Intent: the summary comment once, the issue closed as
// completed, then the phase.
func (p *pass) merged(ctx context.Context, prs []v1alpha1.IntentPullRequest, mergedAt time.Time) error {
	since := mergedAt
	if since.IsZero() {
		since = p.enteredAt().Add(-clockSkew)
	}
	summary := templates.IntentSummaryComment{
		Namespace: p.in.Namespace, Intent: p.in.Name, Revisions: p.in.Status.Revisions,
		CostMicroUSD: p.in.Status.Usage.CostMicroUSD,
	}
	for _, pr := range prs {
		summary.PullRequests = append(summary.PullRequests, templates.IntentPullRequest{
			Repository: repoSlug(pr.Repository), Number: pr.Number, URL: pr.URL, State: pr.State,
		})
	}
	body, err := templates.RenderIntentSummaryComment(summary)
	if err := p.notice(ctx, templates.SummaryKey, since, body, err); err != nil {
		return err
	}
	if err := p.r.GitHub.CloseIssue(ctx, p.repo(), p.number(), ghclient.CloseCompleted); err != nil {
		return fmt.Errorf("close the issue: %w", err)
	}
	return p.setPhase(ctx, v1alpha1.IntentMerged, func(cur *v1alpha1.Intent) {
		cur.Status.PullRequests = prs
		cur.Status.ActiveRun = nil
	})
}

func prsEqual(a, b []v1alpha1.IntentPullRequest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.State != y.State || x.HeadSHA != y.HeadSHA || x.MergeCommitSHA != y.MergeCommitSHA ||
			!x.MergedAt.Equal(y.MergedAt) {
			return false
		}
	}
	return true
}
