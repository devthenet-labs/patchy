// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// restart replaces every reconciler with a fresh one, as a controller
// restart (or a leader handover) would: nothing in memory survives.
func (e *env) restart() {
	s := testSettings()
	e.nudger = NewNudger()
	e.project = &ProjectReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: s, Nudger: e.nudger,
		Now: e.clock.Now}
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: s, Images: e.intent.Images,
		Nudger: e.nudger, Now: e.clock.Now}
	e.runs = &RunReconciler{Client: e.c, APIReader: e.c, Jobs: e.jobs, GitHub: e.gh, Settings: s,
		MaxConcurrent: 1, Harness: "claude", PlanModel: e.runs.PlanModel, BuildModel: e.runs.BuildModel,
		Images: e.runs.Images, Now: e.clock.Now}
}

// tolerantDrive is drive for a world that fails: every error is tolerated
// and retried on the next round, as the controller's backoff would, and the
// reconcilers are optionally restarted before each round.
func (e *env) tolerantDrive(name string, want v1alpha1.IntentPhase, restart bool) {
	e.t.Helper()
	ctx := context.Background()
	for range 200 {
		if e.get(name).Status.Phase == want {
			return
		}
		if restart {
			e.restart()
		}
		_ = e.reconcileIntent(name)
		e.readyRepositories(repoImage)
		_, _ = e.runs.Reconcile(ctx, req(runSchedulerRequest))
		for _, r := range e.intentRuns(name) {
			_, _ = e.runs.Reconcile(ctx, req(r.Name))
		}
		e.clock.Advance(30 * time.Second)
	}
	in := e.get(name)
	e.t.Fatalf("intent %s did not reach %s: phase %s, conditions %+v", name, want, in.Status.Phase,
		in.Status.Conditions)
}

// settleEnded runs the passes an ended intent's last phase write starts,
// tolerating failures, and checks the status comment caught up with it.
func (e *env) settleEnded(name string, restart bool) {
	e.t.Helper()
	for range 6 {
		if restart {
			e.restart()
		}
		_ = e.reconcileIntent(name)
		e.clock.Advance(30 * time.Second)
	}
	st := e.gh.withMarker("patchy:intent")
	if len(st) != 1 || !strings.Contains(st[0].Body, "`"+string(e.get(name).Status.Phase)+"`") {
		e.t.Errorf("the status comment did not catch up with the phase %s", e.get(name).Status.Phase)
	}
}

// checkExactlyOnce asserts every side effect of one intent's whole life
// happened exactly once.
func (e *env) checkExactlyOnce(name string) {
	e.t.Helper()
	for key, want := range map[string]int{
		"patchy:plan":        1,
		"patchy:intent":      1,
		templates.SummaryKey: 1,
	} {
		if got := len(e.gh.withMarker(key)); got != want {
			e.t.Errorf("comments marked %q = %d, want %d", key, got, want)
		}
	}
	if len(e.gh.commits) != 1 || len(e.gh.prs) != 1 || len(e.gh.branches) != 1 {
		e.t.Errorf("commits %d, pull requests %d, branches %d; want one each",
			len(e.gh.commits), len(e.gh.prs), len(e.gh.branches))
	}
	if n := len(e.gh.closes[1]); n < 1 {
		e.t.Error("the issue was never closed")
	}
	var list v1alpha1.IntentRunList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		e.t.Fatal(err)
	}
	if len(list.Items) != 2 {
		e.t.Errorf("runs = %d, want one plan and one build", len(list.Items))
	}
	in := e.get(name)
	if in.Status.Input.Revision != 1 || in.Status.Plan.Revision != 1 {
		e.t.Errorf("input r%d plan r%d, want r1 each", in.Status.Input.Revision, in.Status.Plan.Revision)
	}
}

// TestRunStatusFailures: every few IntentRun status writes fail. A lost
// record of the pushed commit may leave an unreferenced commit behind (the
// retry commits again), but never a second branch or pull request, and the
// intent still merges.
func TestRunStatusFailures(t *testing.T) {
	for _, every := range []int{2, 3, 5} {
		t.Run(fmt.Sprintf("every %d", every), func(t *testing.T) {
			e := newEnv(t, testProject())
			e.failRunEvery = every
			name := e.newIntent(approver)
			e.tolerantDrive(name, v1alpha1.IntentAwaitingApproval, false)
			e.clock.Advance(time.Minute)
			e.gh.label(1, "patchy:approved", approver)
			e.tolerantDrive(name, v1alpha1.IntentInReview, false)
			e.gh.closePR(true)
			e.tolerantDrive(name, v1alpha1.IntentMerged, false)
			if len(e.gh.prs) != 1 || len(e.gh.branches) != 1 || len(e.gh.commits) < 1 {
				t.Errorf("pull requests %d branches %d commits %d", len(e.gh.prs), len(e.gh.branches),
					len(e.gh.commits))
			}
			if n := len(e.gh.withMarker("patchy:plan")); n != 1 {
				t.Errorf("plan comments = %d", n)
			}
		})
	}
}

// TestRestartEveryPass: a controller restarted before every pass takes the
// intent from trigger to merge with every side effect exactly once.
func TestRestartEveryPass(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", approver)
	e.reconcileProject()
	name := v1alpha1.IntentName("target", 1)
	e.tolerantDrive(name, v1alpha1.IntentAwaitingApproval, true)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	e.tolerantDrive(name, v1alpha1.IntentInReview, true)
	e.gh.closePR(true)
	e.tolerantDrive(name, v1alpha1.IntentMerged, true)
	e.settleEnded(name, true)
	e.checkExactlyOnce(name)
}

// TestTransientFailuresEverywhere: every GitHub call fails once, and every
// third status write of the Intent fails, yet the intent reaches its merge
// with every side effect exactly once: a GitHub failure retries without
// deciding again, and a lost status write never repeats what it recorded.
func TestTransientFailuresEverywhere(t *testing.T) {
	for _, every := range []int{2, 3, 4, 5, 7} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("every %d restart %v", every, restart), func(t *testing.T) {
				transientFailures(t, every, restart)
			})
		}
	}
}

func transientFailures(t *testing.T, every int, restart bool) {
	e := newEnv(t, testProject())
	for _, m := range []string{
		"BotLogin", "Permission", "RateRemaining", "GetIssue", "ListIssueEvents", "ListIssueComments",
		"GetIssueComment", "CreateIssueComment", "EditIssueComment", "React", "RemoveLabel", "CloseIssue",
		"DefaultBranch", "CreateCommit", "CreateBranchRef", "FindPullRequest", "CreatePullRequest",
		"GetPullRequest",
	} {
		e.gh.failNext(m, errTransient)
	}
	e.failEvery = every
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", approver)
	name := v1alpha1.IntentName("target", 1)
	for range 10 {
		_, _ = e.project.Reconcile(context.Background(), req("target"))
		var in v1alpha1.Intent
		if e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, &in) == nil {
			break
		}
		e.clock.Advance(time.Minute)
	}
	e.tolerantDrive(name, v1alpha1.IntentAwaitingApproval, restart)
	e.clock.Advance(time.Minute)
	approve := e.gh.comment(approver, "/patchy approve")
	e.tolerantDrive(name, v1alpha1.IntentInReview, restart)
	e.gh.closePR(true)
	e.tolerantDrive(name, v1alpha1.IntentMerged, restart)
	e.settleEnded(name, restart)
	if e.failed == 0 {
		t.Fatal("no status write failed")
	}
	if r := e.gh.withMarker("comment-" + itoa(approve)); len(r) != 1 {
		t.Errorf("replies to the approve command = %d, want exactly one", len(r))
	}
	for m, q := range e.gh.errs {
		if len(q) > 0 {
			t.Errorf("%s was never called to fail", m)
		}
	}
	e.checkExactlyOnce(name)
}
