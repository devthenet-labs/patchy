// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
)

// cutOff makes every call to the repository at url fail the way a
// repository no Forge covers any more does (forge.ErrNoMatch): its Forge was
// deleted, or its repository patterns no longer match it.
func (e *env) cutOff(url string) {
	e.setRepoErrs(map[string]error{
		"* " + strings.ToLower(repoSlug(url)): fmt.Errorf("resolve %s: %w", url, forge.ErrNoMatch),
	})
}

// webPRWrites counts what patchy wrote on web's pull request (2): its comments,
// and the reviews it asked for.
func (e *env) webPRWrites() int {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	return len(e.gh.prComments[2]) + len(e.gh.requestedReviewers[2])
}

// TestDepartedUnreachableRepositoryHoldsNoOtherPullRequest is the round-3
// regression of a repository that leaves the Project and that patchy can no
// longer reach (no Forge covers it). Before the fix every in-review pass read
// the rate budget of every pull request's repository, web's included, and
// returned its error: app's review never got a round, app's merge was never
// read, and a human closing the issue was never processed. Now app's round
// runs to the end and says so on app's pull request; web's pull request, last
// read open, is waited on as any open one is (the intent stays in review once
// app merges); and closing the issue ends the intent. Nothing is written to
// web.
func TestDepartedUnreachableRepositoryHoldsNoOtherPullRequest(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	webWrites, webHead := e.webPRWrites(), e.branchIn(name, webRepoURL)
	e.dropRepository(webRepoURL)
	e.cutOff(webRepoURL)

	e.reviewOn(1, 981, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.driveUntil(name, func(in *v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return in.Status.Phase == v1alpha1.IntentInReview && r != nil && r.Status.Phase == v1alpha1.RunComplete
	})
	run := e.reviseRun(name, 1)
	if !sameRepo(run.Spec.Repository.URL, appRepoURL) || e.branchIn(name, appRepoURL) != run.Status.PushedCommit {
		t.Fatalf("round 1 in %s pushed %q, app's branch at %q; want app's round pushed to app",
			run.Spec.Repository.URL, run.Status.PushedCommit, e.branchIn(name, appRepoURL))
	}
	e.settleActions(name)
	in := e.get(name)
	if in.Status.Revisions != 1 || in.Status.RoundNoticesThrough != 1 {
		t.Errorf("revisions %d, notices through %d; want app's round counted and its notice delivered",
			in.Status.Revisions, in.Status.RoundNoticesThrough)
	}
	if got := e.roundNotices(name, 1)[1]; !strings.Contains(got, "Revision round pushed commit") {
		t.Errorf("app's round notice = %q, want the push", got)
	}

	e.gh.closePRn(1, true)
	e.settleActions(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview ||
		in.Status.PullRequests[0].State != prMerged || in.Status.PullRequests[1].State != prOpen {
		t.Fatalf("phase %s with pull requests %+v; want app's merge read and the intent in review, web's "+
			"last read open", in.Status.Phase, in.Status.PullRequests)
	}

	e.gh.humanClose(1)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentClosed })
	e.settleActions(name)
	if body := e.statusBody(); !strings.Contains(body, "`Closed`") {
		t.Errorf("status comment = %q, want it to say Closed", body)
	}
	if e.webPRWrites() != webWrites || e.branchIn(name, webRepoURL) != webHead {
		t.Errorf("web's pull request writes %d -> %d, branch %s -> %s; want nothing written to web", webWrites,
			e.webPRWrites(), webHead, e.branchIn(name, webRepoURL))
	}
}

// TestDepartedRepositoryMergedBeforeItLeftStillMerges: web's pull request
// merged, and was read merged, before web left the Project and patchy lost
// its reach there. Once app's merges the intent is Merged: the merge it read
// still counts, and the repository it can no longer read holds nothing.
func TestDepartedRepositoryMergedBeforeItLeftStillMerges(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.gh.closePRn(2, true)
	e.driveUntil(name, func(in *v1alpha1.Intent) bool { return in.Status.PullRequests[1].State == prMerged })
	e.dropRepository(webRepoURL)
	e.cutOff(webRepoURL)

	e.gh.closePRn(1, true)
	in := e.drive(name, v1alpha1.IntentMerged, repoImage)
	if in.Status.PullRequests[0].State != prMerged || in.Status.PullRequests[1].State != prMerged {
		t.Errorf("pull requests %+v, want both merged", in.Status.PullRequests)
	}
}

// inRound drives a multi-repository intent into round 1 on pull
// request pr (its repository keyed key), its Job held running, and returns
// it Revising.
func (e *env) inRound(pr int64, key string) string {
	e.t.Helper()
	name := e.inReviewLinked()
	e.reviewOn(pr, 991, "Show the sha.")
	e.clock.Advance(3 * time.Minute)
	e.holdJob(name, key)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return r != nil && r.Status.JobRef != nil
	})
	return name
}

// cancel has the approver cancel the intent, on a poll that is due.
func (e *env) cancel(name string) {
	e.t.Helper()
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/patchy cancel")
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentClosed })
}

// statusSaysClosed reports a status comment that says the intent is Closed
// and offers no command.
func (e *env) statusSaysClosed() bool {
	body := e.statusBody()
	return strings.Contains(body, "`Closed`") && !strings.Contains(body, "/patchy cancel")
}

// TestEndedIntentOwesItsRoundNoticeOnlyWhereItIsOwed is the round-3
// regression of an intent that ends with a round notice owed while a
// repository that left the Project is out of patchy's reach. The approver
// cancels the intent while a round runs. Before the fix every ended pass read
// the rate budget of every pull request's repository first, the departed one
// included, and returned its error: the notice, the status comment and the
// trigger label's answer all waited for good. Now the notice asks only its own
// repository: delivered on the round's own pull request when that one is
// reachable, and not written (and nothing asked there) when that one is the
// repository that left. The cursor moves on either way, the status comment
// says Closed, and the trigger label is answered.
func TestEndedIntentOwesItsRoundNoticeOnlyWhereItIsOwed(t *testing.T) {
	tests := []struct {
		name string
		// pr and key are the pull request the round revises, and its
		// repository's key.
		pr  int64
		key string
		// noticed is whether the round's notice is posted on pr.
		noticed bool
	}{
		{name: "owed in app, beside web", pr: 1, key: "app", noticed: true},
		{name: "owed in web itself", pr: 2, key: "web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.inRound(tt.pr, tt.key)
			e.dropRepository(webRepoURL)
			e.cutOff(webRepoURL)
			e.cancel(name)
			e.settleActions(name)

			in := e.get(name)
			if in.Status.Rounds != 1 || in.Status.RoundNoticesThrough != 1 {
				t.Errorf("rounds %d, notices through %d; want round 1's notice settled", in.Status.Rounds,
					in.Status.RoundNoticesThrough)
			}
			if got := e.roundNoticeCount(name, tt.pr, 1); got != map[bool]int{true: 1}[tt.noticed] {
				t.Errorf("%d round notices on pull request %d, want noticed=%v", got, tt.pr, tt.noticed)
			}
			if !e.statusSaysClosed() {
				t.Errorf("status comment = %q, want it to say Closed, offering no command", e.statusBody())
			}
			e.gh.mu.Lock()
			e.gh.issues[1].state = "open" // reopened
			e.gh.mu.Unlock()
			e.gh.removeTrigger()
			e.clock.Advance(time.Minute)
			event := e.gh.label(1, "patchy:target", approver)
			e.reconcileProject()
			e.passUntil(name, func() bool {
				lt := e.get(name).Status.LastTrigger
				return lt != nil && lt.EventID == event
			})
			if e.gh.hasLabel("patchy:target") {
				t.Error("the trigger label re-applied to the closed intent was not answered")
			}
		})
	}
}

// TestEndedIntentRetriesANoticeOwedInAnUnreachableRepository: a round notice
// owed in a repository the Project still holds is never discarded, whatever
// GitHub or the Forges answer (TestPendingRoundNoticeDoesNotBlockMergeOrExpire
// holds the refusal case): while no Forge covers app, the ended intent's
// passes fail and the cursor stays. The retry holds back nothing else: the
// status comment says the intent is Closed meanwhile. Once app is covered
// again the notice is delivered, once.
func TestEndedIntentRetriesANoticeOwedInAnUnreachableRepository(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inRound(1, "app")
	e.cutOff(appRepoURL)
	e.cancel(name)
	for range 3 {
		if err := e.reconcileIntent(name); err == nil {
			t.Fatal("an ended pass succeeded while its owed notice could not be delivered")
		}
		e.clock.Advance(time.Minute)
	}
	if in := e.get(name); in.Status.RoundNoticesThrough != 0 || e.roundNoticeCount(name, 1, 1) != 0 {
		t.Fatalf("notices through %d, %d notices on app's pull request; want the notice still owed",
			in.Status.RoundNoticesThrough, e.roundNoticeCount(name, 1, 1))
	}
	if !e.statusSaysClosed() {
		t.Errorf("status comment = %q while the notice is retried, want it to say Closed", e.statusBody())
	}

	e.gh.mu.Lock()
	delete(e.gh.repoErrs, "* acme/app")
	e.gh.mu.Unlock()
	e.settleActions(name)
	if in := e.get(name); in.Status.RoundNoticesThrough != 1 || e.roundNoticeCount(name, 1, 1) != 1 {
		t.Errorf("notices through %d, %d notices on app's pull request; want it delivered once",
			in.Status.RoundNoticesThrough, e.roundNoticeCount(name, 1, 1))
	}
}

// TestRoundBlockedInADepartedRepositoryLifts: web's round is blocked because
// its branch was deleted before the round could pin it, and web then leaves
// the Project and patchy's reach. Before the fix the block's check read web's
// branch, under web's rate budget, at every pass, and its error held the
// intent Blocked for good. Now the block no longer holds there: the round,
// its run aborted, ends unretried, and app's review gets the next round.
func TestRoundBlockedInADepartedRepositoryLifts(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(2, 995, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentRevising })
	e.gh.mu.Lock()
	delete(e.gh.branches, fakeRef(webRepoURL, branchName(name)))
	e.gh.mu.Unlock()
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentBlocked })
	if c := meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionBranchConflict); c == nil ||
		c.Reason != ReasonBranchMissing || !strings.Contains(c.Message, webSlug) {
		t.Fatalf("BranchConflict = %+v, want web's branch missing", c)
	}

	e.dropRepository(webRepoURL)
	e.cutOff(webRepoURL)
	e.driveUntil(name, func(in *v1alpha1.Intent) bool { return in.Status.Phase == v1alpha1.IntentInReview })
	if run := e.reviseRun(name, 1); run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeAborted {
		t.Fatalf("web's round = %s %s, want it aborted", run.Status.Phase, run.Status.Outcome)
	}

	e.reviewOn(1, 996, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 2)
		return r != nil && r.Status.Phase == v1alpha1.RunComplete
	})
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.settleActions(name)
	if web := e.revisesIn(name, webRepoURL); len(web) != 1 {
		t.Errorf("%d runs in web, which left the project; want the aborted one, never retried", len(web))
	}
	if in := e.get(name); in.Status.RoundNoticesThrough != 2 || e.roundNoticeCount(name, 1, 2) != 1 ||
		e.roundNoticeCount(name, 2, 1) != 0 {
		t.Errorf("notices through %d; app's round-2 notices %d, web's round-1 notices %d; want app's alone, once",
			in.Status.RoundNoticesThrough, e.roundNoticeCount(name, 1, 2), e.roundNoticeCount(name, 2, 1))
	}
}
