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
// branch, or adopts the one a failed pass opened (found by its head, in the
// repository itself, never a fork), records it, and moves to InReview. The
// title and body are patchy's own, from the approved plan: the body says
// "Part of" the intent issue and holds no closing keyword.
func (p *pass) openPullRequest(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	repoURL := run.Spec.Repository.URL
	branch := branchName(p.in.Name)
	pr, err := p.r.GitHub.FindPullRequest(ctx, repoURL, branch)
	if err != nil {
		return false, fmt.Errorf("find the pull request: %w", err)
	}
	if pr == nil {
		base, err := p.r.GitHub.DefaultBranch(ctx, repoURL)
		if err != nil {
			return false, fmt.Errorf("read the default branch: %w", err)
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

// review polls the recorded pull requests, by repository and number, and
// ends the Intent when every one has merged (Merged) or closed unmerged
// (Closed).
func (p *pass) review(ctx context.Context) (bool, error) {
	var last time.Time
	p.r.memo(func() { last = p.r.prPolled[p.in.Name] })
	if !last.IsZero() && p.now.Sub(last) < p.set.PRPollInterval {
		return false, nil
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
	merged, closed := 0, 0
	var mergedAt time.Time
	for i := range prs {
		rec := &prs[i]
		readable, err := p.readPullRequest(ctx, rec)
		if err != nil || !readable {
			return false, err
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
	return false, nil
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
