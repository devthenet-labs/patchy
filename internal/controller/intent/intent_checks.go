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

func (p *pass) checkRound(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	if len(p.proj.Spec.Checks.Fix) == 0 || pr.HeadSHA == "" {
		return false, nil
	}
	latest := p.latestPushedRun()
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
		return false, nil
	}
	if p.checksConsumed(failed) {
		return false, nil
	}
	_, signature, err := p.checkDiagnostics(ctx, pr.Repository, pr.HeadSHA, failed)
	if err != nil {
		return false, err
	}
	for _, prior := range p.runs {
		if prior.Spec.Trigger != v1alpha1.IntentRunTriggerChecks || prior.Status.Phase != v1alpha1.RunComplete {
			continue
		}
		var cm corev1.ConfigMap
		if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: prior.Namespace,
			Name: prior.Spec.Inputs.ConfigMap}, &cm); err != nil {
			return false, err
		}
		if cm.Data[keyCheckSignature] == signature {
			return true, p.block(ctx, v1alpha1.ConditionChecksFailing, "RepeatedFailure",
				fmt.Sprintf("project-generation=%d: a named check failed again with the same diagnostic after a check-fix round; review the PR manually", p.proj.Generation))
		}
	}
	limit := v1alpha1.DefaultMaxCheckFixes
	if p.proj.Spec.Limits.MaxCheckFixes != nil {
		limit = *p.proj.Spec.Limits.MaxCheckFixes
	}
	if p.checkFixRounds() >= limit || p.in.Status.Rounds >= v1alpha1.MaxIntentRound {
		return true, p.block(ctx, v1alpha1.ConditionChecksFailing, "MaxCheckFixes",
			fmt.Sprintf("%d of %d check-fix rounds used; raise limits.maxCheckFixes to retry", p.checkFixRounds(), limit))
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

func (p *pass) latestPushedRun() *v1alpha1.IntentRun {
	var latest *v1alpha1.IntentRun
	for _, run := range p.runs {
		if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" {
			continue
		}
		if latest == nil || run.Status.FinishedAt != nil &&
			(latest.Status.FinishedAt == nil || run.Status.FinishedAt.After(latest.Status.FinishedAt.Time)) {
			latest = run
		}
	}
	return latest
}

func (p *pass) checksConsumed(f failedChecks) bool {
	for _, run := range p.runs {
		if run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
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

// checkDiagnostics collects only the failed named checks recorded in a run
// spec. Output, annotations and Actions log tails are untrusted GitHub data:
// each goes through visible escaping and a dynamic fence, with a 48-KiB
// aggregate cap. The signature excludes GitHub IDs and head SHA, so the
// same failure after a successful patch is recognised across commits.
func (p *pass) checkDiagnostics(ctx context.Context, repo, sha string, f failedChecks) (string, string, error) {
	runs, err := p.r.GitHub.ListCheckRuns(ctx, repo, sha)
	if err != nil {
		return "", "", err
	}
	statuses, err := p.r.GitHub.ListCommitStatuses(ctx, repo, sha)
	if err != nil {
		return "", "", err
	}
	selectedChecks := map[int64]bool{}
	selectedStatuses := map[int64]bool{}
	for _, id := range f.checkIDs {
		selectedChecks[id] = true
	}
	for _, id := range f.statusIDs {
		selectedStatuses[id] = true
	}
	var parts []string
	for _, r := range runs {
		if !selectedChecks[r.ID] || r.HeadSHA != sha || !failedConclusion(r.Conclusion) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Check %s: %s\nTitle: %s\nSummary: %s\nText: %s\n", r.Name, r.Conclusion,
			r.Output.Title, r.Output.Summary, r.Output.Text)
		annotations, err := p.r.GitHub.ListCheckAnnotations(ctx, repo, r.ID, 50)
		if err != nil {
			return "", "", err
		}
		for _, a := range annotations {
			fmt.Fprintf(&b, "Annotation %s:%d: %s\n", a.Path, a.Line, a.Message)
		}
		if strings.EqualFold(r.AppSlug, "github-actions") {
			if runID := actionRunID(r.DetailsURL); runID != 0 {
				jobs, err := p.r.GitHub.ListWorkflowJobs(ctx, repo, runID)
				if err != nil {
					return "", "", err
				}
				for _, job := range jobs {
					if job.CheckRunID != r.ID || job.HeadSHA != sha || !failedConclusion(job.Conclusion) {
						continue
					}
					log, err := p.r.GitHub.GetJobLogTail(ctx, repo, job.ID, 32<<10)
					if err != nil {
						return "", "", err
					}
					fmt.Fprintf(&b, "Actions job %s log tail:\n%s\n", job.Name, log)
				}
			}
		}
		parts = append(parts, capVisible(visibleFeedback(b.String()), 48<<10))
	}
	for _, s := range statuses {
		if !selectedStatuses[s.ID] || (s.State != "failure" && s.State != "error") {
			continue
		}
		parts = append(parts, capVisible(visibleFeedback(fmt.Sprintf("Commit status %s: %s\n%s",
			s.Context, s.State, s.Description)), 48<<10))
	}
	if len(parts) == 0 {
		return "", "", fmt.Errorf("the failed checks recorded for %s vanished before the round's handoff", sha)
	}
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
	visible := capVisible(b.String(), 48<<10)
	return visible, digest([]byte(visible)), nil
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
