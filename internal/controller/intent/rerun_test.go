// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"net/http"
	"slices"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// rerunEnv is an intent in review on a Project that fixes the check "test"
// and re-runs failed checks first; it returns the env, the intent and the
// pull request's head.
func rerunEnv(t *testing.T) (*env, string, string) {
	t.Helper()
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	project.Spec.Checks.RerunFailed = true
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	return e, name, in.Status.PullRequests[0].HeadSHA
}

// failActions reports the check "test" failed at head as check run id, an
// Actions job of workflow run runID in check suite suite, with the job's
// log; status is the workflow run's own.
func (e *env) failActions(head string, id, suite, runID int64, status string) {
	e.gh.checks[head] = append(e.gh.checks[head], ghclient.CheckRun{ID: id, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions", CheckSuiteID: suite,
		DetailsURL: "https://github.com/acme/app/actions/runs/" + itoa(runID) + "/job/" + itoa(id+1000),
		Output:     ghclient.CheckOutput{Title: "Go failed"}})
	e.gh.workflowJobs[runID] = append(e.gh.workflowJobs[runID], ghclient.WorkflowJob{ID: id + 1000, CheckRunID: id,
		HeadSHA: head, Name: "go test", Conclusion: "failure"})
	e.gh.jobLogs[id+1000] = "go test ./...\nFAIL flaky"
	run := ghclient.WorkflowRun{ID: runID, HeadSHA: head, Status: status}
	if status == "completed" {
		run.Conclusion = "failure"
	}
	e.gh.workflowRuns[suite] = []ghclient.WorkflowRun{run}
}

// rerunReports reports the re-run's own check run id for "test" at head,
// concluded as conclusion (a job of the suite's workflow run, with its log),
// and the workflow run completed with it.
func (e *env) rerunReports(head string, id, suite int64, conclusion string) {
	run := &e.gh.workflowRuns[suite][0]
	run.Status, run.Conclusion = "completed", conclusion
	e.gh.checks[head] = append(e.gh.checks[head], ghclient.CheckRun{ID: id, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: conclusion, AppSlug: "github-actions", CheckSuiteID: suite,
		DetailsURL: "https://github.com/acme/app/actions/runs/" + itoa(run.ID) + "/job/" + itoa(id+1000),
		Output:     ghclient.CheckOutput{Title: "Go " + conclusion}})
	e.gh.workflowJobs[run.ID] = append(e.gh.workflowJobs[run.ID], ghclient.WorkflowJob{ID: id + 1000,
		CheckRunID: id, HeadSHA: head, Name: "go test", Conclusion: conclusion})
	e.gh.jobLogs[id+1000] = "go test ./...\nFAIL for real"
}

// poll runs one intent pass a PR poll interval later.
func (e *env) poll(name string) {
	e.t.Helper()
	e.clock.Advance(2 * time.Minute)
	e.mustIntent(name)
}

// TestFlakyCheckPassesOnRerunWithoutAFixRound is the field note's case: a
// flaky named check fails once on patchy's head. The failed Actions jobs are
// re-run once, recorded on the pull request, and while GitHub has not yet
// reported the re-run's check run nothing more happens; the re-run passing
// settles the head with no check-fix round spent and nothing re-run again.
func TestFlakyCheckPassesOnRerunWithoutAFixRound(t *testing.T) {
	e, name, head := rerunEnv(t)
	e.failActions(head, 71, 61, 81, "completed")
	e.poll(name)
	if !slices.Equal(e.gh.reruns, []int64{81}) {
		t.Fatalf("re-runs = %v, want the failed check's Actions run 81", e.gh.reruns)
	}
	want := v1alpha1.IntentChecksRerun{HeadSHA: head, CheckRunIDs: []int64{71}, Checks: []string{"test"},
		WorkflowRunIDs: []int64{81}}
	got := e.get(name).Status.PullRequests[0].ChecksRerun
	if got == nil || got.HeadSHA != want.HeadSHA || !slices.Equal(got.CheckRunIDs, want.CheckRunIDs) ||
		!slices.Equal(got.Checks, want.Checks) || !slices.Equal(got.WorkflowRunIDs, want.WorkflowRunIDs) ||
		!got.RequestedAt.Time.Equal(e.clock.Now()) {
		t.Fatalf("recorded re-run = %+v, want %+v requested now", got, want)
	}
	// GitHub has queued the re-run but still lists the failure as the
	// check's latest run: neither a round nor a second re-run, across a
	// restart too, since the record is on the Intent.
	for range 3 {
		e.restart()
		e.poll(name)
	}
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 0 || len(e.gh.reruns) != 1 {
		t.Fatalf("while the re-run reports: revise runs %d, re-runs %v; want none and one", len(runs), e.gh.reruns)
	}
	e.rerunReports(head, 72, 61, "success")
	e.poll(name)
	pr := e.get(name).Status.PullRequests[0]
	if pr.ChecksObservedHeadSHA != head || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 ||
		len(e.gh.reruns) != 1 {
		t.Errorf("after a green re-run: observed %q, revise runs %d, re-runs %v; want %s settled, none, one",
			pr.ChecksObservedHeadSHA, len(e.runsOf(name, v1alpha1.IntentStageRevise)), e.gh.reruns, head)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview || in.Status.CheckFixes != 0 {
		t.Errorf("intent = %s with %d check fixes, want in review with none", in.Status.Phase, in.Status.CheckFixes)
	}
}

// TestCheckFailingAgainAfterRerunStartsTheFixRound: a re-run that fails
// again at the same head starts the check-fix round on the re-run's own
// failure, without a second re-run; the round's new head is re-run once of
// its own.
func TestCheckFailingAgainAfterRerunStartsTheFixRound(t *testing.T) {
	e, name, head := rerunEnv(t)
	e.failActions(head, 71, 61, 81, "completed")
	e.poll(name)
	e.rerunReports(head, 72, 61, "failure")
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		!slices.Equal(runs[0].Spec.Inputs.CheckRunIDs, []int64{72}) {
		t.Fatalf("check-fix runs = %+v, want one on the re-run's failure 72", runs)
	}
	if !slices.Equal(e.gh.reruns, []int64{81}) {
		t.Errorf("re-runs = %v, want 81 once", e.gh.reruns)
	}
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	newHead := in.Status.PullRequests[0].HeadSHA
	if newHead == head {
		t.Fatal("the check-fix round pushed no new head")
	}
	e.failActions(newHead, 73, 62, 82, "completed")
	// The round's own notice and status writes take passes of their own.
	for range 5 {
		e.poll(name)
	}
	if !slices.Equal(e.gh.reruns, []int64{81, 82}) || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 {
		t.Errorf("on the round's new head: re-runs %v, revise runs %d; want 81 then 82, still one round",
			e.gh.reruns, len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
}

// TestRerunGoesStraightToTheFixRound: a failure no re-run can serve starts
// the check-fix round at once: a failed commit status (no Actions run), a
// check run another App reports, a check suite with no completed, failed
// Actions run at the head, and GitHub refusing the re-run. Without
// checks.rerunFailed nothing is ever re-run.
func TestRerunGoesStraightToTheFixRound(t *testing.T) {
	tests := []struct {
		name  string
		off   bool
		setup func(e *env, head string)
		lists bool // ListWorkflowRuns is asked
		asks  bool // RerunFailedJobs is asked
	}{
		{name: "re-runs off", off: true, setup: func(e *env, head string) {
			e.failActions(head, 71, 61, 81, "completed")
		}},
		{name: "a failed commit status", setup: func(e *env, head string) {
			e.gh.statuses[head] = []ghclient.CommitStatus{{ID: 91, Context: "test", State: "failure",
				Description: "tests failed"}}
		}},
		{name: "another App's check run", setup: func(e *env, head string) {
			e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head, Status: "completed",
				Conclusion: "failure", AppSlug: "circleci", CheckSuiteID: 61,
				Output: ghclient.CheckOutput{Title: "CI failed"}}}
		}},
		{name: "no Actions run at the head", lists: true, setup: func(e *env, head string) {
			e.failActions(head, 71, 61, 81, "completed")
			e.gh.workflowRuns[61][0].HeadSHA = baseSHA
		}},
		{name: "a run that did not fail", lists: true, setup: func(e *env, head string) {
			e.failActions(head, 71, 61, 81, "completed")
			e.gh.workflowRuns[61][0].Conclusion = "success"
		}},
		{name: "GitHub refuses the re-run", lists: true, asks: true, setup: func(e *env, head string) {
			e.failActions(head, 71, 61, 81, "completed")
			e.gh.failNext("RerunFailedJobs", ghError(http.StatusForbidden, "Resource not accessible by integration"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := testProject()
			project.Spec.Checks.Fix = []string{"test"}
			project.Spec.Checks.RerunFailed = !tt.off
			e := newEnv(t, project)
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			head := e.drive(name, v1alpha1.IntentInReview, repoImage).Status.PullRequests[0].HeadSHA
			tt.setup(e, head)
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStageRevise)
			if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
				t.Fatalf("revise runs = %+v, want one check-fix round", runs)
			}
			if len(e.gh.reruns) != 0 || e.get(name).Status.PullRequests[0].ChecksRerun != nil {
				t.Errorf("re-ran %v (recorded %+v), want nothing", e.gh.reruns,
					e.get(name).Status.PullRequests[0].ChecksRerun)
			}
			if lists := e.gh.calls["ListWorkflowRuns"] > 0; lists != tt.lists {
				t.Errorf("listed the Actions runs: %v, want %v", lists, tt.lists)
			}
			if asks := e.gh.calls["RerunFailedJobs"] > 0; asks != tt.asks {
				t.Errorf("asked for a re-run: %v, want %v", asks, tt.asks)
			}
		})
	}
}

// TestRerunWaitsForTheRunToComplete: GitHub re-runs only a completed run, so
// a failed named check whose workflow run is still running other jobs waits
// for it, then re-runs; one still running when the checks timeout passes
// starts the check-fix round instead.
func TestRerunWaitsForTheRunToComplete(t *testing.T) {
	t.Run("completes", func(t *testing.T) {
		e, name, head := rerunEnv(t)
		e.failActions(head, 71, 61, 81, "in_progress")
		e.poll(name)
		e.poll(name)
		if len(e.gh.reruns) != 0 || e.gh.calls["RerunFailedJobs"] != 0 ||
			len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
			t.Fatalf("while the run runs: re-runs %v, revise runs %d; want neither", e.gh.reruns,
				len(e.runsOf(name, v1alpha1.IntentStageRevise)))
		}
		e.gh.workflowRuns[61][0].Status, e.gh.workflowRuns[61][0].Conclusion = "completed", "failure"
		e.poll(name)
		if !slices.Equal(e.gh.reruns, []int64{81}) {
			t.Errorf("once the run completed: re-runs %v, want 81", e.gh.reruns)
		}
	})
	t.Run("times out", func(t *testing.T) {
		e, name, head := rerunEnv(t)
		e.failActions(head, 71, 61, 81, "in_progress")
		e.poll(name)
		e.clock.Advance(defaultChecksTimeout)
		e.drive(name, v1alpha1.IntentRevising, repoImage)
		if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 1 || len(e.gh.reruns) != 0 {
			t.Errorf("past the timeout: revise runs %d, re-runs %v; want one round, no re-run", len(runs),
				e.gh.reruns)
		}
	})
}

// TestRerunThatNeverReportsStartsTheFixRound: a re-run GitHub accepted but
// whose check run never replaces the failure starts the check-fix round on
// that failure once the checks timeout, counted from the re-run, passes.
func TestRerunThatNeverReportsStartsTheFixRound(t *testing.T) {
	e, name, head := rerunEnv(t)
	e.failActions(head, 71, 61, 81, "completed")
	e.poll(name)
	e.clock.Advance(defaultChecksTimeout - 3*time.Minute)
	e.mustIntent(name)
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 0 {
		t.Fatalf("before the timeout counted from the re-run: %d revise runs, want none", len(runs))
	}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || !slices.Equal(runs[0].Spec.Inputs.CheckRunIDs, []int64{71}) ||
		!slices.Equal(e.gh.reruns, []int64{81}) {
		t.Errorf("revise runs %+v, re-runs %v; want one round on 71 and the one re-run", runs, e.gh.reruns)
	}
}
