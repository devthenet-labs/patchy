// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// roundNoticeCount is how many of patchy's round notices for round sit on
// pull request n: each round says how it ended once.
func (e *env) roundNoticeCount(name string, n int64, round int32) int {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	marker := fmt.Sprintf("<!-- patchy:intent-pr-round:%s:%d -->", name, round)
	count := 0
	for _, c := range e.gh.prComments[n] {
		if markerOf(c.Body) == marker {
			count++
		}
	}
	return count
}

// holdJob keeps the Job of name's revise round on repository key running
// (not done) until releaseJob.
func (e *env) holdJob(name string, round int32, key string) string {
	job := jobs.NameFor(v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, round, key, 1), KindIntent, 1)
	e.jobs.mu.Lock()
	e.jobs.status[job] = jobs.Status{}
	e.jobs.mu.Unlock()
	return job
}

// releaseJob lets a held Job finish.
func (e *env) releaseJob(job string) {
	e.jobs.mu.Lock()
	delete(e.jobs.status, job)
	e.jobs.mu.Unlock()
}

// TestMultiRepoOffMidRoundResumesTheRound is the regression for turning
// --intent-multi-repo off and on again while a revise round runs. Off, the
// Revising intent is held Blocked and the round's finished push is held
// (PushHeld, MultiRepositoryOff). On again, whichever reconciler runs first,
// the round is the same round: the run reconciler holds the push until the
// intent is Revising again (it was read as "the intent ended", and the
// agent's changeset thrown away), and the resumed intent follows its run
// (it was taken for settled, and the round's notice posted as "ended
// without a recorded completed push" before it pushed). The round then
// pushes once, says so once on its pull request, and asks for review again.
func TestMultiRepoOffMidRoundResumesTheRound(t *testing.T) {
	for _, runFirst := range []bool{true, false} {
		order := "intent reconciled first"
		if runFirst {
			order = "run reconciled first"
		}
		t.Run(order, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.inReviewLinked()
			e.reviewOn(2, 901, "Web: show the sha.")
			e.clock.Advance(3 * time.Minute)
			job := e.holdJob(name, 1, "web")
			e.driveUntil(name, func(*v1alpha1.Intent) bool {
				r := e.reviseRun(name, 1)
				return r != nil && r.Status.JobRef != nil
			})
			commits := len(e.gh.commits)

			e.multiRepo(false)
			e.mustIntent(name)
			if in := e.get(name); in.Status.Phase != v1alpha1.IntentBlocked ||
				v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentRevising {
				t.Fatalf("phase %s with the flag off mid-round, want Blocked from Revising", in.Status.Phase)
			}
			e.releaseJob(job)
			e.runRuns()
			run := e.reviseRun(name, 1)
			held := meta.FindStatusCondition(run.Status.Conditions, v1alpha1.ConditionPushHeld)
			if run.Status.Phase != v1alpha1.RunRunning || held == nil || held.Reason != ReasonMultiRepositoryOff ||
				len(e.gh.commits) != commits {
				t.Fatalf("round %s, PushHeld %+v, %d new commits; want its push held on the flag", run.Status.Phase,
					held, len(e.gh.commits)-commits)
			}

			e.multiRepo(true)
			if runFirst {
				e.runRuns()
				if run = e.reviseRun(name, 1); run.Status.Phase != v1alpha1.RunRunning || !pushHeld(run) ||
					len(e.gh.commits) != commits {
					t.Fatalf("round %s %s %q, %d new commits; want it still held while the intent is blocked",
						run.Status.Phase, run.Status.Outcome, run.Status.Detail, len(e.gh.commits)-commits)
				}
			} else {
				e.passUntil(name, func() bool { return e.get(name).Status.Phase != v1alpha1.IntentBlocked })
				in := e.get(name)
				if in.Status.Phase != v1alpha1.IntentRevising || in.Status.ActiveRun == nil ||
					in.Status.ActiveRun.Name != run.Name {
					t.Fatalf("resumed to %s with activeRun %+v, want Revising following %s", in.Status.Phase,
						in.Status.ActiveRun, run.Name)
				}
				e.clock.Advance(2 * time.Minute) // the pull request poll is due
				e.mustIntent(name)
				if n := e.roundNoticeCount(name, 2, 1); n != 0 {
					t.Fatalf("%d round notices before the round pushed, want none: %v", n, e.roundNotices(name, 2))
				}
			}

			e.drive(name, v1alpha1.IntentInReview, repoImage)
			run = e.reviseRun(name, 1)
			if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" ||
				e.branchIn(name, webRepoURL) != run.Status.PushedCommit {
				t.Fatalf("round %s %s %q, web branch %s; want it pushed", run.Status.Phase, run.Status.Outcome,
					run.Status.Detail, e.branchIn(name, webRepoURL))
			}
			e.settleActions(name)
			want := "Revision round pushed commit `" + run.Status.PushedCommit + "`. Review is requested again."
			if n := e.roundNoticeCount(name, 2, 1); n != 1 || !strings.Contains(e.roundNotices(name, 2)[1], want) {
				t.Errorf("%d round notices %v, want one: %q", n, e.roundNotices(name, 2), want)
			}
			e.gh.mu.Lock()
			requested := len(e.gh.requestedReviewers[2])
			e.gh.mu.Unlock()
			if requested == 0 {
				t.Error("review was not requested again on the web pull request")
			}
			if in := e.get(name); in.Status.Revisions != 1 || in.Status.RoundNoticesThrough != 1 {
				t.Errorf("revisions %d, notices through %d; want 1, 1", in.Status.Revisions,
					in.Status.RoundNoticesThrough)
			}
		})
	}
}

// TestSwappedKeysNeverAdoptAnotherRepositorysBuild: build runs are named by
// the Project's repository key and read by URL, so once the keys of app and
// web are swapped mid-build, web's next attempt takes the name of app's
// second attempt. That run is app's, and is never adopted as web's build
// (which then never ran, the intent building forever with nothing said): the
// intent blocks, naming the run and both repositories, and builds web once
// the keys are restored.
func TestSwappedKeysNeverAdoptAnotherRepositorysBuild(t *testing.T) {
	e := newMultiEnv(t)
	e.runs.MaxConcurrent = 2
	appA2 := v1alpha1.IntentRunName("target-1", v1alpha1.IntentStageBuild, 1, "app", 2)
	failing := map[string]bool{
		v1alpha1.IntentRunName("target-1", v1alpha1.IntentStageBuild, 1, "app", 1): true,
		v1alpha1.IntentRunName("target-1", v1alpha1.IntentStageBuild, 1, "web", 1): true,
	}
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "build" && failing[spec.Finding] {
			return failingBuild(spec)
		}
		return multiOutput(spec)
	}
	e.jobs.status[buildJob("web")] = jobs.Status{}
	e.jobs.status[jobs.NameFor(appA2, KindIntent, 2)] = jobs.Status{}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.driveUntil(name, func(*v1alpha1.Intent) bool { return len(e.buildsIn(name, appRepoURL)) == 2 })

	swap := func() {
		p := e.getProject()
		p.Spec.Repositories[0].Name, p.Spec.Repositories[1].Name = p.Spec.Repositories[1].Name,
			p.Spec.Repositories[0].Name
		p.Generation++
		if err := e.c.Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	swap()
	e.releaseJob(buildJob("web"))
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonRepositoryKeyChanged ||
		!strings.Contains(c.Message, appA2) || !strings.Contains(c.Message, webSlug) ||
		!strings.Contains(c.Message, "acme/app ") {
		t.Fatalf("UnsupportedRepositories = %+v, want the run %s and both repositories named", c, appA2)
	}
	if web := e.buildsIn(name, webRepoURL); len(web) != 1 {
		t.Fatalf("web builds = %d, want only its failed first attempt", len(web))
	}
	for range 3 {
		e.mustIntent(name)
		e.clock.Advance(time.Minute)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentBlocked {
		t.Fatalf("phase %s while the keys stay swapped, want Blocked", in.Status.Phase)
	}

	swap()
	e.releaseJob(jobs.NameFor(appA2, KindIntent, 2))
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	web := e.buildsIn(name, webRepoURL)
	if len(web) != 2 || web[1].Name != v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, "web", 2) ||
		web[1].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("web builds = %+v, want its second attempt built under its own key", web)
	}
	if len(in.Status.PullRequests) != 2 {
		t.Errorf("pull requests = %+v, want both", in.Status.PullRequests)
	}
}
