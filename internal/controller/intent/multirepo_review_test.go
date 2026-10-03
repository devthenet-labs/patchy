// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// In the multi-repository review tests, pull request 1 is acme/app's and 2
// is acme/Acme.Web_App's: they open in plan order.

// revisesIn are the Intent's revise runs in repoURL, by name.
func (e *env) revisesIn(name, repoURL string) []v1alpha1.IntentRun {
	var out []v1alpha1.IntentRun
	for _, r := range e.runsOf(name, v1alpha1.IntentStageRevise) {
		if sameRepo(r.Spec.Repository.URL, repoURL) {
			out = append(out, r)
		}
	}
	return out
}

// branchIn is the intent branch's head in repoURL, "" when there is none.
func (e *env) branchIn(name, repoURL string) string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	return e.gh.branches[fakeRef(repoURL, branchName(name))]
}

// inReviewLinked drives a new multi-repository intent to InReview and lets
// its cross-links settle, so later passes are about the rounds alone.
func (e *env) inReviewLinked() string {
	e.t.Helper()
	name := e.inReviewMulti()
	e.settleActions(name)
	return name
}

// TestReviewRevisesItsOwnRepository: a review on the web pull request starts
// a round in web alone. The round runs in web's own build image (never the
// app build's), pushes to web's branch past pushGate while the app pull
// request is also open, and posts its notice, and asks for review again, on
// the web pull request; the app pull request and branch are untouched.
func TestReviewRevisesItsOwnRepository(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	appHead := e.branchIn(name, appRepoURL)
	e.reviewOn(2, 501, "Show the sha in a <code> element.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)

	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || !sameRepo(runs[0].Spec.Repository.URL, webRepoURL) ||
		!slices.Equal(runs[0].Spec.Inputs.ReviewIDs, []int64{501}) {
		t.Fatalf("revise runs = %+v, want one in web consuming review 501", runs)
	}
	webBuild := e.buildsIn(name, webRepoURL)[0]
	webImage := e.repository(webBuild.Spec.Repository.RepositoryRef.Name)
	appImage := e.repository(e.buildsIn(name, appRepoURL)[0].Spec.Repository.RepositoryRef.Name)
	if ref := runs[0].Spec.ImageFrom; ref == nil || ref.Name != webImage.Name || ref.UID != webImage.UID ||
		ref.UID == appImage.UID {
		t.Fatalf("imageFrom = %+v, want web's build Repository %s, never app's %s", ref, webImage.Name, appImage.Name)
	}
	checkWebRound(t, e, name, appHead)
}

// checkWebRound: the web round of TestReviewRevisesItsOwnRepository, driven
// to its end, pushed to web's branch alone, ran in web on the repository
// image, and said so on web's pull request alone.
func checkWebRound(t *testing.T, e *env, name, appHead string) {
	t.Helper()
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" ||
		e.branchIn(name, webRepoURL) != run.Status.PushedCommit {
		t.Fatalf("round = %s %q, web branch %q; want web's branch advanced to its push", run.Status.Phase,
			run.Status.PushedCommit, e.branchIn(name, webRepoURL))
	}
	if e.branchIn(name, appRepoURL) != appHead || in.Status.PullRequests[0].HeadSHA != appHead {
		t.Errorf("the app branch moved: %s, want %s", e.branchIn(name, appRepoURL), appHead)
	}
	for _, s := range e.jobs.launched() {
		if s.Finding == run.Name && (s.Repo != webSlug || s.RunnerImage != repoImage) {
			t.Errorf("round Job ran in %s on %q, want %s on the repository image", s.Repo, s.RunnerImage, webSlug)
		}
	}
	want := "Revision round pushed commit `" + run.Status.PushedCommit + "`. Review is requested again."
	if got := e.roundNotices(name, 2)[1]; !strings.Contains(got, want) {
		t.Errorf("web round notice = %q, want %q", got, want)
	}
	if got := e.roundNotices(name, 1); len(got) != 0 {
		t.Errorf("app pull request round notices = %v, want none", got)
	}
	if len(e.gh.requestedReviewers[2]) == 0 || len(e.gh.requestedReviewers[1]) != 0 {
		t.Errorf("review re-requested on %v, want web's pull request alone", e.gh.requestedReviewers)
	}
	e.settleActions(name) // the notice cursor follows on a later pass
	in = e.get(name)
	if in.Status.Revisions != 1 || in.Status.Rounds != 1 || in.Status.RoundNoticesThrough != 1 {
		t.Errorf("counters = revisions %d, rounds %d, notices %d; want 1 each", in.Status.Revisions,
			in.Status.Rounds, in.Status.RoundNoticesThrough)
	}
}

// TestPRCommandIsAnsweredOnItsPullRequest: /patchy revise on the app pull
// request starts a round in app and is acknowledged there, once, with the
// eyes reaction; the web pull request hears nothing of it.
func TestPRCommandIsAnsweredOnItsPullRequest(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.gh.mu.Lock()
	e.gh.prComments[1] = append(e.gh.prComments[1], &ghclient.Comment{ID: 940, NodeID: "comment-940",
		UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
		Body: "/patchy revise Return the build time too.", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
	e.gh.mu.Unlock()
	e.clock.Advance(time.Second)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || !sameRepo(runs[0].Spec.Repository.URL, appRepoURL) || runs[0].Spec.Inputs.CommandID != 940 {
		t.Fatalf("revise runs = %+v, want the command's round in app", runs)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	acks := func(n int64) int {
		e.gh.mu.Lock()
		defer e.gh.mu.Unlock()
		count := 0
		for _, c := range e.gh.prComments[n] {
			if strings.Contains(c.Body, "patchy:intent-pr-command:") {
				count++
			}
		}
		return count
	}
	if acks(1) != 1 || acks(2) != 0 || e.gh.reactions[940] != 1 {
		t.Errorf("command replies app %d, web %d, reactions %d; want one on app's pull request", acks(1), acks(2),
			e.gh.reactions[940])
	}
	if len(e.roundNotices(name, 1)) != 1 || len(e.roundNotices(name, 2)) != 0 {
		t.Errorf("round notices app %v, web %v; want the one on app's", e.roundNotices(name, 1),
			e.roundNotices(name, 2))
	}
}

// TestReviewsWaitingOutASiblingRoundAreConsumed: rounds are serialised per
// intent, so feedback on one pull request can wait out a round on another.
// Each pull request's review cutoff is its own: a review on web submitted
// before the app round started is still consumed by web's next round
// (regression: the cutoff was the newest round in any repository, so it was
// dropped). The pull requests take turns: with a second app review arriving
// during app's round, web's waiting review is served next, and app's after
// it.
func TestReviewsWaitingOutASiblingRoundAreConsumed(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(1, 601, "App: name the field sha.")
	e.reviewOn(2, 602, "Web: show it in the footer.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.reviewOn(1, 603, "App: and the build time.")
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)

	for _, want := range []struct {
		round   int32
		repo    string
		reviews []int64
	}{{1, appRepoURL, []int64{601}}, {2, webRepoURL, []int64{602}}, {3, appRepoURL, []int64{603}}} {
		run := e.reviseRun(name, want.round)
		if run == nil || !sameRepo(run.Spec.Repository.URL, want.repo) ||
			!slices.Equal(run.Spec.Inputs.ReviewIDs, want.reviews) || run.Status.Phase != v1alpha1.RunComplete {
			t.Errorf("round %d = %+v, want a completed round in %s consuming %v", want.round, run, want.repo,
				want.reviews)
		}
	}
	if in := e.get(name); in.Status.Revisions != 3 {
		t.Errorf("revisions = %d, want 3", in.Status.Revisions)
	}
}

// TestRoundLeaseOnAnEndedPullRequestNeverDeadlocks is the critique's
// deadlock: a round on app is leased (its run created) and the status write
// that would enter Revising is lost; then app's pull request merges, and a
// review arrives on web. The lease is adopted all the same, its run aborted
// before anything launched since its pull request is no longer open, and its
// round ends with a notice on the app pull request; web's review then gets
// its round. Nothing waits forever, and no round number is used twice.
func TestRoundLeaseOnAnEndedPullRequestNeverDeadlocks(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(1, 701, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.failStatusIf = func(in *v1alpha1.Intent) bool { return in.Status.Phase == v1alpha1.IntentRevising }
	for e.reviseRun(name, 1) == nil {
		_ = e.reconcileIntent(name)
		e.clock.Advance(time.Minute)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview || in.Status.Rounds != 0 || e.failed != 1 {
		t.Fatalf("after the lost write: phase %s, rounds %d, failures %d", in.Status.Phase, in.Status.Rounds, e.failed)
	}
	launched := len(e.jobs.launched())
	e.gh.closePRn(1, true)
	e.reviewOn(2, 702, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)

	e.driveUntil(name, func(in *v1alpha1.Intent) bool {
		r := e.reviseRun(name, 2)
		return in.Status.Phase == v1alpha1.IntentInReview && r != nil && r.Status.Phase == v1alpha1.RunComplete
	})
	lease := e.reviseRun(name, 1)
	if lease.Status.Phase != v1alpha1.RunFailed || lease.Status.Outcome != OutcomeAborted ||
		!strings.Contains(lease.Status.Detail, "pull request in acme/app was merged or closed") {
		t.Fatalf("the app lease = %s %s %q, want aborted for its merged pull request", lease.Status.Phase,
			lease.Status.Outcome, lease.Status.Detail)
	}
	if len(e.revisesIn(name, appRepoURL)) != 1 {
		t.Errorf("app rounds = %d, want the lease alone, never retried", len(e.revisesIn(name, appRepoURL)))
	}
	if got := len(e.jobs.launched()) - launched; got != 1 {
		t.Errorf("%d Jobs launched since the lease, want web's round alone", got)
	}
	web := e.reviseRun(name, 2)
	if !sameRepo(web.Spec.Repository.URL, webRepoURL) || !slices.Equal(web.Spec.Inputs.ReviewIDs, []int64{702}) {
		t.Errorf("round 2 = %+v, want web's review round", web.Spec)
	}
	if !strings.Contains(e.roundNotices(name, 1)[1], "ended without a recorded completed push") ||
		!strings.Contains(e.roundNotices(name, 2)[2], "pushed commit") {
		t.Errorf("notices app %v, web %v", e.roundNotices(name, 1), e.roundNotices(name, 2))
	}
	e.settleActions(name) // the notice cursor follows on a later pass
	in := e.get(name)
	if in.Status.Rounds != 2 || in.Status.Revisions != 1 || in.Status.RoundNoticesThrough != 2 {
		t.Errorf("rounds %d, revisions %d, notices %d; want 2, 1, 2", in.Status.Rounds, in.Status.Revisions,
			in.Status.RoundNoticesThrough)
	}
}

// driveUntil runs the intent, its runs and its repositories until done
// holds of the Intent, as drive does for a phase.
func (e *env) driveUntil(name string, done func(*v1alpha1.Intent) bool) {
	e.t.Helper()
	for range 60 {
		if done(e.get(name)) {
			return
		}
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	in := e.get(name)
	e.t.Fatalf("intent %s: the condition never held: phase %s, conditions %+v", name, in.Status.Phase,
		in.Status.Conditions)
}

// TestPushGateRefusesARoundWhosePullRequestEnded: web's round finishes after
// its pull request merged on GitHub (the intent has not read it yet). The
// push gate reads the pull request live and pushes nothing: no commit, the
// branch unmoved, the run aborted saying why. The round then ends without a
// retry, the intent stays in review for the app pull request, and merges
// once that one does.
func TestPushGateRefusesARoundWhosePullRequestEnded(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(2, 801, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)
	job := jobs.NameFor(v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, "web", 1), KindIntent, 1)
	e.jobs.mu.Lock()
	e.jobs.status[job] = jobs.Status{}
	e.jobs.mu.Unlock()
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return r != nil && r.Status.JobRef != nil
	})
	commits, webHead := len(e.gh.commits), e.branchIn(name, webRepoURL)

	e.gh.closePRn(2, true)
	e.jobs.mu.Lock()
	delete(e.jobs.status, job)
	e.jobs.mu.Unlock()
	e.runRuns()
	run := e.reviseRun(name, 1)
	want := "the pull request in " + webSlug + " was merged or closed before the round ended; nothing was pushed"
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeAborted || run.Status.Detail != want {
		t.Fatalf("round = %s %s %q, want aborted: %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail, want)
	}
	if len(e.gh.commits) != commits || e.branchIn(name, webRepoURL) != webHead {
		t.Fatalf("commits %d -> %d, web branch %s -> %s; want nothing pushed", commits, len(e.gh.commits), webHead,
			e.branchIn(name, webRepoURL))
	}

	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.settleActions(name)
	if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 || e.get(name).Status.Phase != v1alpha1.IntentInReview {
		t.Fatalf("revise runs %d, phase %s; want the one round ended, the intent in review",
			len(e.runsOf(name, v1alpha1.IntentStageRevise)), e.get(name).Status.Phase)
	}
	if in.Status.PullRequests[1].State != prMerged || in.Status.PullRequests[0].State != prOpen {
		t.Errorf("pull requests = %+v, want web merged and app open", in.Status.PullRequests)
	}
	if !strings.Contains(e.roundNotices(name, 2)[1], "ended without a recorded completed push") {
		t.Errorf("web round notice = %q", e.roundNotices(name, 2)[1])
	}
	e.gh.closePRn(1, true)
	e.drive(name, v1alpha1.IntentMerged, repoImage)
}

// TestBlocksNameTheirRepository: a review limit reached on the web pull
// request blocks the whole intent, the block saying which pull request's
// feedback waits.
func TestBlocksNameTheirRepository(t *testing.T) {
	p := testMultiProject()
	p.Spec.Limits.MaxRevisions = new(int32)
	e := newEnv(t, p)
	e.multiRepo(true)
	e.jobs.output = multiOutput
	name := e.inReviewLinked()
	e.reviewOn(2, 901, "Web: one more thing.")
	e.clock.Advance(3 * time.Minute)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionRevisionLimitReached)
	if c == nil || !strings.Contains(c.Message, "review feedback in "+webSlug+" waits") {
		t.Fatalf("RevisionLimitReached = %+v, want web named", c)
	}
	if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Error("a round started past the limit")
	}
}
