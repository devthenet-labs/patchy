// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// building runs the approved plan's build round: one build per approved
// repository (approvedRepositories, in plan order), each on its own
// repository's pin and accepted image, all created in one pass and launched
// by the run pool as its slots free. A repository's next attempt follows its
// failed one. A repository whose attempts are spent fails the whole Intent at
// once: its siblings' runs then abort and their Jobs are deleted, since the
// Intent has ended, and the branches they pushed stay, with no pull request.
// One whose build had no accepted image to run on blocks the Intent, naming
// the repository, while the siblings already running finish and push. The
// pull requests open only once every approved repository's build is
// Complete, so a Failed intent never leaves one behind.
func (p *pass) building(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if ap == nil {
		return true, p.fail(ctx)
	}
	repos, gone := p.approvedRepositories()
	if gone {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "the approved plan's repository left the project; the intent fails",
			slog.String("intent", p.in.Name))
		return true, p.fail(ctx)
	}
	var (
		launches []buildLaunch
		inFlight []*v1alpha1.IntentRun
		blocked  *v1alpha1.IntentRun
		complete int
	)
	for _, repo := range repos {
		rs := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, repo.URL)
		latest := rs.latest()
		switch {
		case latest == nil:
			launches = append(launches, buildLaunch{repo: repo, attempt: 1})
		case latest.Status.Phase == v1alpha1.RunComplete:
			complete++
		case latest.Status.Phase != v1alpha1.RunFailed:
			inFlight = append(inFlight, latest)
		case imageBlocked(latest):
			if rs.next() > v1alpha1.MaxIntentRunAttempt {
				// No attempt is left to try the image again with: a block
				// could never lift, and resuming from it would only block
				// again.
				p.r.log().LogAttrs(ctx, slog.LevelWarn, "the build's attempts are spent on image blocks; the intent fails",
					slog.String("intent", p.in.Name), slog.String("run", latest.Name))
				return true, p.fail(ctx)
			}
			if blocked == nil {
				blocked = latest
			}
		case rs.counted(nil) >= p.set.MaxAttempts:
			return true, p.fail(ctx)
		default:
			launches = append(launches, buildLaunch{repo: repo, attempt: rs.next(), prev: p.previousAttempt(latest)})
		}
	}
	if blocked != nil {
		return true, p.block(ctx, v1alpha1.ConditionImageRequired, imageReason(blocked),
			fmt.Sprintf("the build%s could not run on an accepted repository image: %s",
				p.inRepository(blocked.Spec.Repository.URL), blocked.Status.Detail))
	}
	if complete == len(repos) {
		return p.openPullRequests(ctx, ap.PlanRevision, repos)
	}
	if len(launches) > 0 {
		runs, stop, err := p.launchBuilds(ctx, ap.PlanRevision, launches)
		if errors.Is(err, errRunNameTaken) {
			return true, p.blockKeyChanged(ctx, err)
		}
		if stop || err != nil {
			return stop, err
		}
		inFlight = append(inFlight, runs...)
	}
	return p.ensureBuilds(ctx, inFlight)
}

// ensureBuilds makes sure every unfinished build of the round has its input
// ConfigMap and Repository, separately from the one status write that
// records which of them is the Intent's activeRun. In Building activeRun is
// sticky: it names one in-flight build and is rewritten only once that run
// has settled, so a fan-out's siblings never make the status flip between
// them, and a pass writes it at most once.
func (p *pass) ensureBuilds(ctx context.Context, runs []*v1alpha1.IntentRun) (bool, error) {
	for _, run := range runs {
		if err := p.ensureRunChildren(ctx, run); err != nil {
			if errors.Is(err, errPlanChanged) {
				return true, p.fail(ctx)
			}
			return false, err
		}
	}
	if len(runs) == 0 {
		return false, nil
	}
	if ar := p.in.Status.ActiveRun; ar != nil {
		for _, run := range runs {
			if ar.Name == run.Name && ar.UID == run.UID {
				return false, nil
			}
		}
	}
	run := runs[0]
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.ActiveRun = &v1alpha1.ObjectReference{Name: run.Name, UID: run.UID}
		return nil
	})
}

// inRepository names repoURL in a message about one of a multi-repository
// Project's repositories (" in owner/name"); for a one-repository Project it
// is empty, so its messages read as they always have.
func (p *pass) inRepository(repoURL string) string {
	if len(p.proj.Spec.Repositories) < 2 {
		return ""
	}
	return " in " + repoSlug(repoURL)
}

// pullRequestsOpened reports whether the pull requests recorded are every one
// the intent opens: so in every phase but Building (and Blocked from it),
// since the record that completes the approved set is written with the move
// to InReview. A one-repository intent's one record is always that write.
func (p *pass) pullRequestsOpened() bool {
	phase := p.in.Status.Phase
	return phase != v1alpha1.IntentBuilding &&
		(phase != v1alpha1.IntentBlocked || v1alpha1.IntentBlockedFrom(p.in) != v1alpha1.IntentBuilding)
}

// pullRequest is the recorded pull request in repoURL, or nil.
func (p *pass) pullRequest(repoURL string) *v1alpha1.IntentPullRequest {
	return recordedPullRequest(p.in, repoURL)
}

// recordedPullRequest is in's recorded pull request in repoURL, or nil: how
// every round finds the one pull request it works on, by its run's
// repository.
func recordedPullRequest(in *v1alpha1.Intent, repoURL string) *v1alpha1.IntentPullRequest {
	for i := range in.Status.PullRequests {
		if sameRepo(in.Status.PullRequests[i].Repository, repoURL) {
			return &in.Status.PullRequests[i]
		}
	}
	return nil
}

// roundOpen reports whether the pull request of run's repository is
// recorded open: a round on one merged or closed has nothing left to push
// to.
func (p *pass) roundOpen(run *v1alpha1.IntentRun) bool {
	pr := p.pullRequest(run.Spec.Repository.URL)
	return pr != nil && pr.State == prOpen
}

// openPullRequests opens the pull request of the first approved repository,
// in plan order, that has none recorded: one per pass, while the Intent stays
// Building, the last one's record moving it to InReview.
func (p *pass) openPullRequests(ctx context.Context, round int32, repos []v1alpha1.ProjectRepository) (bool, error) {
	for _, repo := range repos {
		if p.pullRequest(repo.URL) != nil {
			continue
		}
		return p.openPullRequest(ctx, p.round(v1alpha1.IntentStageBuild, round, repo.URL).latest(), repos)
	}
	return true, p.setPhase(ctx, v1alpha1.IntentInReview, func(cur *v1alpha1.Intent) {
		cur.Status.ActiveRun = nil
	})
}

// openPullRequest opens the pull request for a build that pushed the intent
// branch, or adopts the one a failed pass opened, and records it, upserted
// by repository; the record that completes the approved set moves the Intent
// to InReview in the same write. Only patchy's own is adopted
// (ownPullRequest); an open pull request from the branch that is anyone
// else's blocks the Intent on BranchConflict until it is closed, since GitHub
// keeps one open pull request per head and base. A block names the
// repository, and on resume patchy carries on with the rest. The title and
// body are patchy's own, from the approved plan: the body says "Part of" the
// intent issue and holds no closing keyword; with more than one repository
// it also says which, and that the intent completes when every one merges.
func (p *pass) openPullRequest(ctx context.Context, run *v1alpha1.IntentRun,
	repos []v1alpha1.ProjectRepository) (bool, error) {
	repoURL := run.Spec.Repository.URL
	in := p.inRepository(repoURL)
	branch := branchName(p.in.Name)
	pr, own, base, err := p.findPullRequest(ctx, repoURL)
	if err != nil {
		if ghclient.IsRefused(err) {
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
				fmt.Sprintf("GitHub refused to look for the pull request from %s%s: %v; fix the refusal and update "+
					"the Project to retry", branch, in, err))
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
				fmt.Sprintf("branch %s%s was deleted after the build pushed %s; restore it at that commit to resume",
					branch, in, run.Status.PushedCommit))
		case ghclient.IsRefused(headErr):
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
				fmt.Sprintf("GitHub refused to read branch %s%s: %v; fix access and update the Project to retry",
					branch, in, headErr))
		case headErr != nil:
			return false, fmt.Errorf("read the intent branch before opening its pull request: %w", headErr)
		case head != run.Status.PushedCommit:
			return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchChanged,
				fmt.Sprintf("branch %s%s moved from the build's commit %s to %s before patchy opened its pull request; "+
					"restore it to the build's commit to resume", branch, in, run.Status.PushedCommit, head))
		}
		ap, pl := p.in.Status.Approval, p.in.Status.Plan
		body, err := templates.RenderIntentPRBody(templates.IntentPRBody{
			IntentRepository: repoSlug(p.repo()), IssueNumber: p.number(), Summary: pl.Summary,
			PlanRevision: ap.PlanRevision, PlanDigest: ap.PlanDigest, ApprovedBy: ap.By,
			Repositories: repositorySlugs(repos),
		})
		if err != nil {
			return false, err
		}
		if pr, err = p.r.GitHub.CreatePullRequest(ctx, repoURL, ghclient.PRRequest{
			Title: templates.IntentPRTitle(p.proj.Name, pl.Summary), Head: branch, Base: base, Body: body,
		}); err != nil {
			if ghclient.IsRefused(err) {
				return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonPullRequestRefused,
					fmt.Sprintf("GitHub refused to open the pull request from %s%s: %v; fix the refusal and update the "+
						"Project to retry", branch, in, err))
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
	prs := upsertPullRequest(p.in.Status.PullRequests, rec)
	if !everyRecorded(prs, repos) {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Branch = branch
			cur.Status.PullRequests = prs
			return nil
		})
	}
	return true, p.setPhase(ctx, v1alpha1.IntentInReview, func(cur *v1alpha1.Intent) {
		cur.Status.Branch = branch
		cur.Status.PullRequests = prs
		cur.Status.ActiveRun = nil
	})
}

// upsertPullRequest is prs with rec in place of the record of its
// repository, or appended when there is none; prs itself is not changed.
func upsertPullRequest(prs []v1alpha1.IntentPullRequest, rec v1alpha1.IntentPullRequest) []v1alpha1.IntentPullRequest {
	out := slices.Clone(prs)
	for i := range out {
		if sameRepo(out[i].Repository, rec.Repository) {
			out[i] = rec
			return out
		}
	}
	return append(out, rec)
}

// everyRecorded reports whether prs holds a record for each of repos.
func everyRecorded(prs []v1alpha1.IntentPullRequest, repos []v1alpha1.ProjectRepository) bool {
	for _, repo := range repos {
		if !slices.ContainsFunc(prs, func(pr v1alpha1.IntentPullRequest) bool { return sameRepo(pr.Repository, repo.URL) }) {
			return false
		}
	}
	return true
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
// ends the Intent once every one has settled: Merged when every one merged,
// Closed when any closed unmerged (endReview). Under the rate floor of a pull
// request's repository it polls nothing and waits a whole interval.
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

// reviewNow is review without waiting for the poll interval. In InReview it
// then starts at most one round, on one open pull request: rounds are
// serialised per Intent, each on the pull request of one repository. A round
// already leased (its run created, its Revising write lost) is adopted first,
// whatever its repository and whatever its pull request's state, so no lease
// can keep another pull request's round waiting. The open pull requests are
// then served in turn (roundOrder), each with its review, command and checks
// round in that order.
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
	if ended, err := p.endReview(ctx, prs, merged, closed, mergedAt); ended || err != nil {
		return ended, err
	}
	if !prsEqual(prs, p.in.Status.PullRequests) {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.PullRequests = prs
			return nil
		})
	}
	if changed, err := p.recordPreviewBases(ctx); changed || err != nil {
		return changed, err
	}
	if changed, err := p.linkSiblings(ctx); changed || err != nil {
		return changed, err
	}
	if changed, err := p.syncPRRoundNotices(ctx); changed || err != nil {
		return changed, err
	}
	if p.in.Status.Phase != v1alpha1.IntentInReview {
		return false, nil
	}
	return p.startRound(ctx, prs)
}

// startRound starts at most one round: the leased one adopted first, then
// the first open pull request in roundOrder with a review, command or checks
// round due.
func (p *pass) startRound(ctx context.Context, prs []v1alpha1.IntentPullRequest) (bool, error) {
	if adopted, err := p.adoptPendingRound(ctx); adopted || err != nil {
		return adopted, err
	}
	for _, i := range p.roundOrder(prs) {
		pr := &prs[i]
		if pr.State != prOpen {
			continue
		}
		if started, err := p.reviewRound(ctx, pr); started || err != nil {
			return started, err
		}
		if started, err := p.commandRound(ctx, pr); started || err != nil {
			return started, err
		}
		if started, err := p.checkRound(ctx, pr); started || err != nil {
			return started, err
		}
	}
	return false, nil
}

// roundOrder is the order the pull requests in prs are offered a round in:
// starting after the one the latest round worked on, and wrapping round, so
// feedback arriving on every pull request at once is served in turn rather
// than the first one's starving the rest. With no round yet it is the
// recorded (plan) order.
func (p *pass) roundOrder(prs []v1alpha1.IntentPullRequest) []int {
	start := 0
	if last := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository).latest(); last != nil {
		for i := range prs {
			if sameRepo(prs[i].Repository, last.Spec.Repository.URL) {
				start = i + 1
				break
			}
		}
	}
	order := make([]int, 0, len(prs))
	for k := range prs {
		order = append(order, (start+k)%len(prs))
	}
	return order
}

// endReview ends the Intent once every pull request in prs (read just now)
// has settled: Merged when every one merged; Closed, the issue closed as not
// planned, when any closed unmerged. A multi-repository intent's Closed
// first posts the notice naming what merged (already on its repository's
// default branch, never reverted) and what did not; a one-repository
// intent's ends as it always has, with no notice. patchy never closes one
// pull request because another closed: while any is still open, the Intent
// stays in review. While the pull requests are still being opened (Building,
// one record per pass) the ones recorded are not every one the intent opens,
// and their state ends nothing.
func (p *pass) endReview(ctx context.Context, prs []v1alpha1.IntentPullRequest, merged, closed int,
	mergedAt time.Time) (bool, error) {
	if !p.pullRequestsOpened() {
		return false, nil
	}
	switch {
	case merged == len(prs):
		return true, p.merged(ctx, prs, mergedAt)
	case merged+closed == len(prs):
		if len(prs) > 1 {
			if err := p.partialNotice(ctx, prs, mergedAt); err != nil {
				return false, err
			}
		}
		// Every pull request settled and not all merged: patchy closes the
		// issue.
		if err := p.r.GitHub.CloseIssue(ctx, p.repo(), p.number(), ghclient.CloseNotPlanned); err != nil {
			return false, fmt.Errorf("close the issue: %w", err)
		}
		return true, p.setPhase(ctx, v1alpha1.IntentClosed, func(cur *v1alpha1.Intent) {
			cur.Status.PullRequests = prs
		})
	}
	return false, nil
}

// partialNotice posts, once, the notice of a multi-repository intent that
// ended with some of its pull requests closed unmerged: which merged and
// which did not, with the round counts and the spend. It can only have been
// posted once every pull request had settled, so after the earliest merge
// when any merged, and in any case after the builds finished.
func (p *pass) partialNotice(ctx context.Context, prs []v1alpha1.IntentPullRequest, mergedAt time.Time) error {
	n := templates.IntentPartialNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Revisions: p.in.Status.Revisions,
		CheckFixes: p.in.Status.CheckFixes, CostMicroUSD: p.in.Status.Usage.CostMicroUSD,
	}
	for _, pr := range prs {
		t := templates.IntentPullRequest{Repository: repoSlug(pr.Repository), Number: pr.Number, URL: pr.URL,
			State: pr.State}
		if pr.State == prMerged {
			n.Merged = append(n.Merged, t)
		} else {
			n.Closed = append(n.Closed, t)
		}
	}
	since := mergedAt
	if since.IsZero() {
		since = p.buildsFinished().Add(-clockSkew)
	}
	body, err := templates.RenderIntentPartialNotice(n)
	return p.notice(ctx, templates.PartialKey, since, body, err)
}

// buildsFinished is when the approved round's first build finished: no pull
// request, and so nothing that answers one, is older. Without a finished
// build it is when the Intent entered its phase.
func (p *pass) buildsFinished() time.Time {
	var at time.Time
	if ap := p.in.Status.Approval; ap != nil {
		for _, run := range p.runs {
			if run.Spec.Stage == v1alpha1.IntentStageBuild && run.Spec.Round == ap.PlanRevision &&
				run.Status.FinishedAt != nil && (at.IsZero() || run.Status.FinishedAt.Time.Before(at)) {
				at = run.Status.FinishedAt.Time
			}
		}
	}
	if at.IsZero() {
		return p.enteredAt()
	}
	return at
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
		CheckFixes: p.in.Status.CheckFixes, CostMicroUSD: p.in.Status.Usage.CostMicroUSD,
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
