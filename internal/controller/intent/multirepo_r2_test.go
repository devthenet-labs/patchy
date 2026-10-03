// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestFailedRoundWhoseRepositoryLeftEndsTheRound is the round-2 regression of
// a round in flight in a repository the operator then removes from the
// Project. web's round 1 is running when web leaves; its attempt then fails
// in a way that would otherwise be retried (a runtime error). The round ends
// instead of retrying in a repository the Project no longer holds, the intent
// returns to InReview, and app's review gets round 2, in app, to the end.
// Before the fix the retry returned errRepositoryGone at every pass, the
// intent stayed Revising with no condition saying why, and, rounds being
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

	e.dropRepository(webRepoURL)
	e.jobs.output = failingBuild
	e.releaseJob(job)
	e.runRuns()
	e.jobs.output = multiOutput
	if run := e.reviseRun(name, 1); run.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("web's round 1 is %s, want it failed", run.Status.Phase)
	}

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
