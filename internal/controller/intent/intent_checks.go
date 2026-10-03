// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

const defaultChecksTimeout = 30 * time.Minute

// checkRound starts a check-fix round on pr when a named check failed on the
// head patchy last pushed in its repository, once the checks settled (or
// their timeout passed). Each pull request's checks are its own: observed per
// pull request, against its own repository's pushes and earlier fixes.
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
	deadline := latest.CreationTimestamp.Time
	if latest.Status.FinishedAt != nil {
		deadline = latest.Status.FinishedAt.Time
	}
	timeout := defaultChecksTimeout
	if p.proj.Spec.Checks.Timeout != nil {
		timeout = p.proj.Spec.Checks.Timeout.Duration
	}
	if !settled && p.now.Before(deadline.Add(timeout)) {
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
	diagnosis, err := p.checkDiagnostics(ctx, pr.Repository, pr.HeadSHA, failed)
	if err != nil {
		return false, err
	}
	return p.startCheckFix(ctx, pr, failed, diagnosis.signature)
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
	var failures failedChecks
	for _, name := range p.proj.Spec.Checks.Fix {
		if r, ok := byName[name]; ok {
			if !strings.EqualFold(r.Status, "completed") {
				settled = false
			} else if failedConclusion(r.Conclusion) {
				failures.checkIDs = append(failures.checkIDs, r.ID)
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
