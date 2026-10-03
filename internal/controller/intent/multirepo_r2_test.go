// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// TestFailedRoundWhoseRepositoryLeftEndsTheRound is the round-2 regression of
// a round in flight in a repository the operator then removes from the
// Project. web's round 1 fails in a way that would otherwise be retried (a
// runtime error), and web leaves before the intent reads the failure. The
// round ends instead of retrying in a repository the Project no longer holds,
// the intent returns to InReview, and app's review gets round 2, in app, to
// the end. Before the fix the retry returned errRepositoryGone at every pass,
// the intent stayed Revising with no condition saying why, and, rounds being
// serialised per intent, no other pull request's round could ever start.
func TestFailedRoundWhoseRepositoryLeftEndsTheRound(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(2, 961, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)
	job := e.holdJob(name, 1, "web")
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return r != nil && r.Status.JobRef != nil
	})
	commits, webHead := len(e.gh.commits), e.branchIn(name, webRepoURL)

	e.jobs.output = failingBuild
	e.releaseJob(job)
	e.runRuns()
	e.jobs.output = multiOutput
	if run := e.reviseRun(name, 1); run.Status.Phase != v1alpha1.RunFailed ||
		run.Status.Outcome != string(envelope.OutcomeRuntimeError) {
		t.Fatalf("web's round 1 is %s %s, want it failed with a runtime error", run.Status.Phase,
			run.Status.Outcome)
	}
	e.dropRepository(webRepoURL)

	e.reviewOn(1, 962, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 2)
		return r != nil && r.Status.Phase == v1alpha1.RunComplete
	})
	if run := e.reviseRun(name, 2); !sameRepo(run.Spec.Repository.URL, appRepoURL) {
		t.Fatalf("round 2 is in %s, want app's", run.Spec.Repository.URL)
	}
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.Rounds != 2 || in.Status.Revisions != 1 {
		t.Errorf("rounds %d, revisions %d; want web's round ended unpushed and app's pushed", in.Status.Rounds,
			in.Status.Revisions)
	}
	if web := e.revisesIn(name, webRepoURL); len(web) != 1 {
		t.Errorf("%d runs in web, which left the project; want its one failed attempt, never retried", len(web))
	}
	if e.branchIn(name, webRepoURL) != webHead {
		t.Errorf("web's branch moved from %s to %s; want nothing pushed there", webHead, e.branchIn(name, webRepoURL))
	}
	if n := len(e.gh.commits) - commits; n != 1 {
		t.Errorf("%d new commits, want app's round alone", n)
	}
}

// TestRoundInRemovedRepositoryPushesNothing is the round-2 regression of a
// push to a repository the operator has removed from the Project. web's round
// is running when web leaves; the agent then succeeds. Nothing reaches web:
// no commit, its branch unmoved, the run aborted saying why. The round ends
// unretried, and app's review gets the next round. Before the fix the run
// ended Complete with web's branch fast-forwarded, by a token for a
// repository outside what the Project's Ready proved.
func TestRoundInRemovedRepositoryPushesNothing(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(2, 971, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)
	job := e.holdJob(name, 1, "web")
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return r != nil && r.Status.JobRef != nil
	})
	commits, webHead := len(e.gh.commits), e.branchIn(name, webRepoURL)

	e.dropRepository(webRepoURL)
	e.releaseJob(job)
	e.runRuns()
	run := e.reviseRun(name, 1)
	want := "the repository " + webSlug + " left the project; nothing was pushed"
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeAborted || run.Status.Detail != want {
		t.Fatalf("round = %s %s %q, want aborted: %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail, want)
	}
	if len(e.gh.commits) != commits || e.branchIn(name, webRepoURL) != webHead {
		t.Fatalf("commits %d -> %d, web branch %s -> %s; want nothing pushed", commits, len(e.gh.commits), webHead,
			e.branchIn(name, webRepoURL))
	}

	e.reviewOn(1, 972, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 2)
		return r != nil && r.Status.Phase == v1alpha1.RunComplete
	})
	if web := e.revisesIn(name, webRepoURL); len(web) != 1 {
		t.Errorf("%d runs in web, which left the project; want the aborted one, never retried", len(web))
	}
}

// TestPushGateRefusesARepositoryThatLeft: the gate every push passes, and the
// launch check, refuse a build or revise run whose repository the Project no
// longer lists, read when they run (a decision taken earlier from a cache can
// lag the removal). A plan run, which writes nothing, is not theirs to refuse,
// and a run whose repository the Project still lists passes as before.
func TestPushGateRefusesARepositoryThatLeft(t *testing.T) {
	tests := []struct {
		name     string
		stage    v1alpha1.IntentStage
		repoURL  string
		wantLeft bool
	}{
		{name: "a build in a repository that left", stage: v1alpha1.IntentStageBuild, repoURL: webRepoURL,
			wantLeft: true},
		{name: "a revise round in a repository that left", stage: v1alpha1.IntentStageRevise, repoURL: webRepoURL,
			wantLeft: true},
		{name: "a build in a repository still listed", stage: v1alpha1.IntentStageBuild, repoURL: appRepoURL},
		{name: "a plan", stage: v1alpha1.IntentStagePlan, repoURL: webRepoURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.inReviewLinked()
			e.dropRepository(webRepoURL)
			in := e.get(name)
			run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{
				IntentRef:  v1alpha1.ObjectReference{Name: name, UID: in.UID},
				Stage:      tt.stage,
				Repository: v1alpha1.IntentRunRepository{URL: tt.repoURL},
			}}
			run.Namespace = testNS
			if got := repositoryLeft(e.getProject(), run); got != tt.wantLeft {
				t.Errorf("repositoryLeft = %v, want %v", got, tt.wantLeft)
			}
			if tt.stage == v1alpha1.IntentStagePlan {
				return
			}
			err := e.runs.pushGate(context.Background(), run)
			switch {
			case tt.wantLeft && (!errors.Is(err, errRepositoryLeft) || !errors.Is(err, errIntentEnded)):
				t.Errorf("pushGate = %v, want errRepositoryLeft, settled as an ended push (errIntentEnded)", err)
			case !tt.wantLeft && err != nil:
				t.Errorf("pushGate = %v, want the build's push let through", err)
			}
			if got := e.runs.launchable(context.Background(), run); tt.wantLeft && got {
				t.Error("launchable = true, want a run whose repository left never launched")
			}
		})
	}
}
