// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

const defaultChecksTimeout = 30 * time.Minute

// checkRound starts a check-fix round on pr when a named check failed on the
// head patchy last pushed in its repository, once the checks settled (or
// their timeout passed). Each pull request's checks are its own: observed per
// pull request, against its own repository's pushes and earlier fixes. With
// the Project's checks.rerunFailed, the first failure at a head re-runs the
// failed Actions jobs instead (rerunFailedChecks), and only a failure after
// that re-run starts the round; the checks timeout then counts from the
// re-run.
func (p *pass) checkRound(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	if len(p.proj.Spec.Checks.Fix) == 0 || pr.HeadSHA == "" || p.checksObserved(pr) {
		return false, nil
	}
	latest := p.latestPushedRun(pr.Repository)
	if latest == nil || latest.Status.PushedCommit != pr.HeadSHA {
		// A human moved the branch. Patchy may report the failure, but does
		// not launch an automatic fix on a head it did not push.
		return false, nil
	}
	failed, settled, err := p.failedNamedChecks(ctx, pr.Repository, pr.HeadSHA)
	if err != nil {
		return false, err
	}
	rerun := rerunAt(pr)
	expired := p.checksTimedOut(latest, rerun)
	if !settled && !expired {
		return false, nil
	}
	if !expired && rerunAwaited(rerun, failed) {
		// A failure the re-run re-ran is still its check's latest run:
		// GitHub has not yet replaced it with the re-run's.
		return false, nil
	}
	if len(failed.checkIDs) == 0 && len(failed.statusIDs) == 0 {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			if rec := recordedPullRequest(cur, pr.Repository); rec != nil {
				rec.ChecksObservedHeadSHA = pr.HeadSHA
				rec.ChecksObservedProjectGeneration = p.proj.Generation
			}
			return nil
		})
	}
	if p.checksConsumed(pr.Repository, failed) {
		return false, nil
	}
	if p.proj.Spec.Checks.RerunFailed && rerun == nil {
		if outcome, err := p.rerunFailedChecks(ctx, pr, failed, expired); err != nil || outcome != rerunSkipped {
			return outcome == rerunRequested, err
		}
	}
	diagnosis, err := p.checkDiagnostics(ctx, pr.Repository, pr.HeadSHA, failed)
	if err != nil {
		return false, err
	}
	return p.startCheckFix(ctx, pr, failed, diagnosis.signature)
}

// checksTimedOut reports whether the checks timeout of a head has passed:
// counted from the push that made it (latest, the run that pushed it), or
// from the re-run asked for there since, whose checks settle anew.
func (p *pass) checksTimedOut(latest *v1alpha1.IntentRun, rerun *v1alpha1.IntentChecksRerun) bool {
	deadline := latest.CreationTimestamp.Time
	if latest.Status.FinishedAt != nil {
		deadline = latest.Status.FinishedAt.Time
	}
	if rerun != nil && rerun.RequestedAt.After(deadline) {
		deadline = rerun.RequestedAt.Time
	}
	timeout := defaultChecksTimeout
	if p.proj.Spec.Checks.Timeout != nil {
		timeout = p.proj.Spec.Checks.Timeout.Duration
	}
	return !p.now.Before(deadline.Add(timeout))
}

// checksObserved reports whether pr's head had its named checks observed
// under the Project's current generation: as recorded on pr itself, or, for
// a one-pull-request intent whose pull request has no record of its own (one
// observed before the record moved onto the pull request), as the deprecated
// Intent-level pair records it. That pair is only ever read, never written,
// so an upgrade neither polls a settled head again nor starts a second fix.
func (p *pass) checksObserved(pr *v1alpha1.IntentPullRequest) bool {
	head, gen := pr.ChecksObservedHeadSHA, pr.ChecksObservedProjectGeneration
	if head == "" && len(p.in.Status.PullRequests) == 1 {
		head, gen = p.in.Status.ChecksObservedHeadSHA, p.in.Status.ChecksObservedProjectGeneration
	}
	return head == pr.HeadSHA && gen == p.proj.Generation
}

// startCheckFix starts the check-fix round of pr's repository, unless a
// fix there already failed the same way (RepeatedFailure) or the Intent's
// check-fix rounds are spent. Both blocks hold the whole Intent, and name the
// repository whose checks failed.
func (p *pass) startCheckFix(ctx context.Context, pr *v1alpha1.IntentPullRequest,
	failed failedChecks, signature string) (bool, error) {
	for _, prior := range p.runs {
		if prior.Spec.Trigger != v1alpha1.IntentRunTriggerChecks || prior.Status.Phase != v1alpha1.RunComplete ||
			!sameRepo(prior.Spec.Repository.URL, pr.Repository) {
			continue
		}
		var cm corev1.ConfigMap
		if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: prior.Namespace,
			Name: prior.Spec.Inputs.ConfigMap}, &cm); err != nil {
			return false, err
		}
		if cm.Data[keyCheckSignature] == signature {
			return true, p.block(ctx, v1alpha1.ConditionChecksFailing, "RepeatedFailure",
				fmt.Sprintf("project-generation=%d: a named check%s failed again with the same diagnostic "+
					"after a check-fix round; review the PR manually", p.proj.Generation,
					p.inRepository(pr.Repository)))
		}
	}
	limit := v1alpha1.DefaultMaxCheckFixes
	if p.proj.Spec.Limits.MaxCheckFixes != nil {
		limit = *p.proj.Spec.Limits.MaxCheckFixes
	}
	if p.checkFixRounds() >= limit || p.in.Status.Rounds >= v1alpha1.MaxIntentRound {
		return true, p.block(ctx, v1alpha1.ConditionChecksFailing, "MaxCheckFixes",
			fmt.Sprintf("%d of %d check-fix rounds used%s; raise limits.maxCheckFixes to retry",
				p.checkFixRounds(), limit, failedIn(p.inRepository(pr.Repository))))
	}
	run, err := p.createReviseRun(ctx, pr, p.in.Status.Rounds+1, v1alpha1.IntentRunTriggerChecks,
		nil, failed.checkIDs, failed.statusIDs, 0)
	if err != nil {
		return false, err
	}
	return true, p.enterRevising(ctx, run)
}

type failedChecks struct {
	checkIDs  []int64
	statusIDs []int64
	// runs are the failed check runs, by id, as GitHub listed them.
	runs map[int64]ghclient.CheckRun
}

// rerunAt is the re-run recorded on pr's head, nil when there is none: one
// recorded on an earlier head says nothing about this one.
func rerunAt(pr *v1alpha1.IntentPullRequest) *v1alpha1.IntentChecksRerun {
	if pr.ChecksRerun == nil || pr.ChecksRerun.HeadSHA != pr.HeadSHA {
		return nil
	}
	return pr.ChecksRerun
}

// rerunAwaited reports a failure rerun re-ran that is still its check's
// latest run: the re-run's own check run has not been reported yet. With no
// re-run there is none.
func rerunAwaited(rerun *v1alpha1.IntentChecksRerun, failed failedChecks) bool {
	return rerun != nil &&
		slices.ContainsFunc(failed.checkIDs, func(id int64) bool { return slices.Contains(rerun.CheckRunIDs, id) })
}

// rerunOutcome is what rerunFailedChecks did.
type rerunOutcome int

const (
	// rerunSkipped re-ran nothing: the check-fix round starts.
	rerunSkipped rerunOutcome = iota
	// rerunWaiting re-ran nothing yet: an Actions run behind a failure is
	// still running, and GitHub re-runs only a completed run.
	rerunWaiting
	// rerunRequested re-ran the failed jobs and recorded it on the Intent.
	rerunRequested
)

// maxRerunRuns bounds the Actions runs one re-run asks GitHub for, as
// status.pullRequests[].checksRerun.workflowRunIDs is bounded.
const maxRerunRuns = 32

// rerunFailedChecks re-runs the failed jobs of the GitHub Actions runs behind
// failed, the failures of pr's head, and records the re-run on the pull
// request so that it is asked for once per head (checks.rerunFailed). A
// flaky check then costs a re-run, not a check-fix round.
//
// Only a failure every part of which can be re-run is: a commit status, or a
// check run another App reports, has no Actions run behind it, so the round
// starts at once (rerunSkipped), as it does when a failure's check suite has
// no completed, failed Actions run at this head, or GitHub refuses the
// re-run (a run too old to re-run, an App without actions write). A run
// still running (other jobs of its workflow) is waited for until the
// checks timeout passes (rerunWaiting). Listing the runs failing is a
// transient error, retried with the pass.
func (p *pass) rerunFailedChecks(ctx context.Context, pr *v1alpha1.IntentPullRequest, failed failedChecks,
	expired bool) (rerunOutcome, error) {
	if len(failed.statusIDs) > 0 || len(failed.checkIDs) == 0 {
		return rerunSkipped, nil
	}
	suites := map[int64]bool{}
	names := make([]string, 0, len(failed.checkIDs))
	for _, id := range failed.checkIDs {
		r := failed.runs[id]
		if !strings.EqualFold(r.AppSlug, "github-actions") || r.CheckSuiteID == 0 {
			return rerunSkipped, nil
		}
		suites[r.CheckSuiteID] = true
		names = append(names, r.Name)
	}
	var runIDs []int64
	for _, suite := range slices.Sorted(maps.Keys(suites)) {
		runs, err := p.r.GitHub.ListWorkflowRuns(ctx, pr.Repository, suite)
		if err != nil {
			return rerunSkipped, err
		}
		rerunnable := false
		for _, run := range runs {
			if run.HeadSHA != pr.HeadSHA {
				continue
			}
			if !strings.EqualFold(run.Status, "completed") {
				if expired {
					return rerunSkipped, nil
				}
				return rerunWaiting, nil
			}
			if failedConclusion(run.Conclusion) {
				runIDs = append(runIDs, run.ID)
				rerunnable = true
			}
		}
		if !rerunnable {
			return rerunSkipped, nil
		}
	}
	slices.Sort(runIDs)
	if runIDs = slices.Compact(runIDs); len(runIDs) > maxRerunRuns {
		return rerunSkipped, nil
	}
	log := p.r.log().With(slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
		slog.String("head", pr.HeadSHA))
	for _, id := range runIDs {
		if err := p.r.GitHub.RerunFailedJobs(ctx, pr.Repository, id); err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "GitHub refused to re-run failed checks; the check-fix round starts",
				slog.Int64("workflowRun", id), slog.Any("error", err))
			return rerunSkipped, nil
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	log.LogAttrs(ctx, slog.LevelInfo, "re-ran failed checks before a check-fix round",
		slog.Any("checks", names), slog.Any("workflowRuns", runIDs))
	return rerunRequested, p.update(ctx, func(cur *v1alpha1.Intent) error {
		if rec := recordedPullRequest(cur, pr.Repository); rec != nil {
			rec.ChecksRerun = &v1alpha1.IntentChecksRerun{HeadSHA: pr.HeadSHA,
				CheckRunIDs: slices.Clone(failed.checkIDs), Checks: names, WorkflowRunIDs: runIDs,
				RequestedAt: metav1.NewTime(p.now)}
		}
		return nil
	})
}

func (p *pass) failedNamedChecks(ctx context.Context, repo, sha string) (failedChecks, bool, error) {
	runs, err := p.r.GitHub.ListCheckRuns(ctx, repo, sha)
	if err != nil {
		return failedChecks{}, false, err
	}
	statuses, err := p.r.GitHub.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return failedChecks{}, false, err
	}
	byName := map[string]ghclient.CheckRun{}
	for _, r := range runs {
		if r.HeadSHA != sha {
			continue
		}
		if current, ok := byName[r.Name]; !ok || r.ID > current.ID {
			byName[r.Name] = r
		}
	}
	byContext := map[string]ghclient.CommitStatus{}
	for _, s := range statuses {
		if current, ok := byContext[s.Context]; !ok || s.ID > current.ID {
			byContext[s.Context] = s
		}
	}
	settled := true
	failures := failedChecks{runs: map[int64]ghclient.CheckRun{}}
	for _, name := range p.proj.Spec.Checks.Fix {
		if r, ok := byName[name]; ok {
			if !strings.EqualFold(r.Status, "completed") {
				settled = false
			} else if failedConclusion(r.Conclusion) {
				failures.checkIDs = append(failures.checkIDs, r.ID)
				failures.runs[r.ID] = r
			}
			continue
		}
		if s, ok := byContext[name]; ok {
			switch strings.ToLower(s.State) {
			case "failure", "error":
				failures.statusIDs = append(failures.statusIDs, s.ID)
			case "success":
			default:
				settled = false
			}
			continue
		}
		settled = false
	}
	slices.Sort(failures.checkIDs)
	slices.Sort(failures.statusIDs)
	if len(failures.checkIDs) > 32 {
		failures.checkIDs = failures.checkIDs[:32]
	}
	if len(failures.statusIDs) > 32 {
		failures.statusIDs = failures.statusIDs[:32]
	}
	return failures, settled, nil
}

func failedConclusion(s string) bool {
	switch strings.ToLower(s) {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// failedIn is how a MaxCheckFixes block names the repository whose checks
// failed (in, from inRepository): nothing for a one-repository Project.
func failedIn(in string) string {
	if in == "" {
		return ""
	}
	return " (a named check failed" + in + ")"
}

// latestPushedRun is the run that last pushed to the intent branch in
// repoURL: its build, or a later round there. A sibling repository's later
// push says nothing about this pull request's head.
func (p *pass) latestPushedRun(repoURL string) *v1alpha1.IntentRun {
	var latest *v1alpha1.IntentRun
	for _, run := range p.runs {
		if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" ||
			!sameRepo(run.Spec.Repository.URL, repoURL) {
			continue
		}
		if latest == nil || pushedAfter(run, latest) {
			latest = run
		}
	}
	return latest
}

func pushedAfter(a, b *v1alpha1.IntentRun) bool {
	if a.Status.FinishedAt != nil && b.Status.FinishedAt != nil {
		if d := a.Status.FinishedAt.Compare(b.Status.FinishedAt.Time); d != 0 {
			return d > 0
		}
	} else if a.Status.FinishedAt != nil || b.Status.FinishedAt != nil {
		return a.Status.FinishedAt != nil
	}
	if a.Spec.Stage != b.Spec.Stage {
		return a.Spec.Stage == v1alpha1.IntentStageRevise
	}
	if a.Spec.Round != b.Spec.Round {
		return a.Spec.Round > b.Spec.Round
	}
	return a.Spec.Attempt > b.Spec.Attempt
}

// checksConsumed reports a check-fix round in repoURL that already took
// exactly these failures.
func (p *pass) checksConsumed(repoURL string, f failedChecks) bool {
	for _, run := range p.runs {
		if run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks || !sameRepo(run.Spec.Repository.URL, repoURL) {
			continue
		}
		if slices.Equal(run.Spec.Inputs.CheckRunIDs, f.checkIDs) && slices.Equal(run.Spec.Inputs.StatusIDs, f.statusIDs) {
			return true
		}
	}
	return false
}

func (p *pass) checkFixRounds() int32 {
	seen := map[int32]bool{}
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageRevise && run.Spec.Trigger == v1alpha1.IntentRunTriggerChecks {
			seen[run.Spec.Round] = true
		}
	}
	return int32(len(seen))
}

// checkDiagnosis is what a check-fix round reads of its failed checks.
type checkDiagnosis struct {
	// feedback is the diagnostics the agent is handed.
	feedback string
	// signature fingerprints the failures, compared with an earlier
	// check-fix round's to stop a repeated failure.
	signature string
	// names are the failed checks' names and status contexts, sorted, as
	// the round's pull request notice names them.
	names []string
}

// checkDiagnostics collects only the failed named checks recorded in a run
// spec. Output, annotations and Actions log tails are untrusted GitHub data:
// each goes through visible escaping and a dynamic fence, with a 48-KiB
// aggregate cap. The signature is the failures' fingerprint
// (failureSignature over their stable forms), never the diagnostic's own
// bytes: it leaves out GitHub's ids, commits, times and every other token one
// run of a check differs from the next by, so the same failure after a
// pushed fix is recognised across commits and job runs.
func (p *pass) checkDiagnostics(ctx context.Context, repo, sha string, f failedChecks) (checkDiagnosis, error) {
	runs, err := p.r.GitHub.ListCheckRuns(ctx, repo, sha)
	if err != nil {
		return checkDiagnosis{}, err
	}
	statuses, err := p.r.GitHub.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return checkDiagnosis{}, err
	}
	selectedChecks := map[int64]bool{}
	selectedStatuses := map[int64]bool{}
	for _, id := range f.checkIDs {
		selectedChecks[id] = true
	}
	for _, id := range f.statusIDs {
		selectedStatuses[id] = true
	}
	var parts, prints, names []string
	for _, r := range runs {
		if !selectedChecks[r.ID] || r.HeadSHA != sha || !failedConclusion(r.Conclusion) {
			continue
		}
		part, print, err := p.checkRunDiagnostic(ctx, repo, sha, r)
		if err != nil {
			return checkDiagnosis{}, err
		}
		parts = append(parts, part)
		prints = append(prints, print)
		names = append(names, r.Name)
	}
	for _, s := range statuses {
		if !selectedStatuses[s.ID] || (s.State != "failure" && s.State != "error") {
			continue
		}
		parts = append(parts, capVisible(fmt.Sprintf("Commit status %s: %s\n%s",
			visibleDiagnostic(s.Context, 2<<10), visibleDiagnostic(s.State, 256),
			visibleDiagnostic(s.Description, 8<<10)), 48<<10))
		prints = append(prints, statusPrint(s))
		names = append(names, s.Context)
	}
	if len(parts) == 0 {
		return checkDiagnosis{}, fmt.Errorf("%w: the failed checks recorded for %s vanished before the round's handoff",
			errInputUnavailable, sha)
	}
	slices.Sort(names)
	slices.Sort(parts)
	var b strings.Builder
	for _, item := range parts {
		fence := fencedBounded(item, 48<<10-64)
		if b.Len()+len(fence)+2 > 48<<10 {
			b.WriteString("\n[check diagnostics truncated at 48 KiB]\n")
			break
		}
		b.WriteString(fence)
		b.WriteString("\n\n")
	}
	return checkDiagnosis{feedback: capVisible(b.String(), 48<<10), signature: failureSignature(prints),
		names: slices.Compact(names)}, nil
}

// A field is cut before visible escaping, and again after it. This keeps a
// malicious or malformed provider response from expanding without bound.
func visibleDiagnostic(s string, maxBytes int) string {
	return capVisible(visibleFeedback(cutBytes(s, maxBytes)), maxBytes)
}

// checkRunDiagnostic is one failed check run's diagnostic, as the agent reads
// it, and its stable form (checkRunPrint), taken from the same answers.
func (p *pass) checkRunDiagnostic(ctx context.Context, repo, sha string, r ghclient.CheckRun) (string, string,
	error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Check %s: %s\nTitle: %s\nSummary: %s\nText: %s\n",
		visibleDiagnostic(r.Name, 2<<10), visibleDiagnostic(r.Conclusion, 256),
		visibleDiagnostic(r.Output.Title, 4<<10), visibleDiagnostic(r.Output.Summary, 8<<10),
		visibleDiagnostic(r.Output.Text, 8<<10))
	annotations, err := p.r.GitHub.ListCheckAnnotations(ctx, repo, r.ID, 50)
	if err != nil {
		return "", "", err
	}
	for _, a := range annotations {
		if b.Len() >= 40<<10 {
			break
		}
		fmt.Fprintf(&b, "Annotation %s:%d: %s\n", visibleDiagnostic(a.Path, 512), a.Line,
			visibleDiagnostic(a.Message, 1<<10))
	}
	if !strings.EqualFold(r.AppSlug, "github-actions") {
		return capVisible(b.String(), 48<<10), checkRunPrint(r, annotations, "", false), nil
	}
	runID := actionRunID(r.DetailsURL)
	if runID == 0 {
		return capVisible(b.String(), 48<<10), checkRunPrint(r, annotations, "", false), nil
	}
	jobs, err := p.r.GitHub.ListWorkflowJobs(ctx, repo, runID)
	if err != nil {
		return "", "", err
	}
	const tailBytes = 32 << 10
	var logTail string
	for _, job := range jobs {
		if job.CheckRunID != r.ID || job.HeadSHA != sha || !failedConclusion(job.Conclusion) {
			continue
		}
		if logTail, err = p.r.GitHub.GetJobLogTail(ctx, repo, job.ID, tailBytes); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&b, "Actions job %s log tail:\n%s\n", visibleDiagnostic(job.Name, 1<<10),
			visibleDiagnostic(logTail, tailBytes))
		break // one Actions job owns a check run
	}
	// A tail as long as asked for was cut from a longer log.
	return capVisible(b.String(), 48<<10), checkRunPrint(r, annotations, logTail, len(logTail) >= tailBytes), nil
}

func actionRunID(raw string) int64 {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return 0
	}
	parts := strings.Split(u.Path, "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "actions" && parts[i+1] == "runs" {
			id, _ := strconv.ParseInt(parts[i+2], 10, 64)
			return id
		}
	}
	return 0
}
