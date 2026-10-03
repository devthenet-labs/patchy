// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// checkOn sets the one named check, "test", on sha: passed, or failed with
// the same diagnostic every time.
func (e *env) checkOn(sha string, id int64, passed bool) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	run := ghclient.CheckRun{ID: id, Name: "test", HeadSHA: sha, Status: "completed", Conclusion: "success"}
	if !passed {
		run.Conclusion = "failure"
		run.Output = ghclient.CheckOutput{Title: "test failed", Summary: "TestVersion: want the sha in the body"}
	}
	e.gh.checks[sha] = []ghclient.CheckRun{run}
}

// TestCheckFixesStayInTheirRepository: each pull request's checks are its
// own. They are observed on its own record (the Intent-level pair is never
// written); a failure on the app pull request's head starts app's fix even
// after web was pushed later (regression: the head to fix was the intent's
// latest push in any repository, so app's looked moved by a human and was
// never fixed); the same failure on web then starts web's own first fix
// rather than a RepeatedFailure from app's (regression: the signature was
// compared with every repository's fixes); and only web failing again the
// same way after its fix blocks, naming web.
func TestCheckFixesStayInTheirRepository(t *testing.T) {
	p := testMultiProject()
	p.Spec.Checks.Fix = []string{"test"}
	p.Spec.Checks.Timeout = &metav1.Duration{Duration: 4 * time.Hour}
	p.Spec.Limits.MaxCheckFixes = new(int32(5))
	e := newEnv(t, p)
	e.multiRepo(true)
	e.jobs.output = multiOutput
	name := e.inReviewLinked()
	in := e.get(name)
	appHead, webHead := in.Status.PullRequests[0].HeadSHA, in.Status.PullRequests[1].HeadSHA

	e.checkOn(webHead, 1, true)
	e.settleActions(name)
	in = e.get(name)
	if in.Status.PullRequests[1].ChecksObservedHeadSHA != webHead ||
		in.Status.PullRequests[0].ChecksObservedHeadSHA != "" || in.Status.ChecksObservedHeadSHA != "" {
		t.Fatalf("observed: app %q, web %q, intent %q; want web's head on web's record alone",
			in.Status.PullRequests[0].ChecksObservedHeadSHA, in.Status.PullRequests[1].ChecksObservedHeadSHA,
			in.Status.ChecksObservedHeadSHA)
	}

	e.reviewOn(2, 1001, "Web: put it in the footer.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	webHead2 := in.Status.PullRequests[1].HeadSHA

	e.checkOn(appHead, 3, false)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	if fix := e.reviseRun(name, 2); fix == nil || fix.Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		!sameRepo(fix.Spec.Repository.URL, appRepoURL) || !slices.Equal(fix.Spec.Inputs.CheckRunIDs, []int64{3}) {
		t.Fatalf("round 2 = %+v, want app's check fix", fix)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.checkOn(in.Status.PullRequests[0].HeadSHA, 4, true)

	e.checkOn(webHead2, 5, false)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	if fix := e.reviseRun(name, 3); fix == nil || fix.Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		!sameRepo(fix.Spec.Repository.URL, webRepoURL) {
		t.Fatalf("round 3 = %+v, want web's own check fix", fix)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.checkOn(in.Status.PullRequests[1].HeadSHA, 6, false)
	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionChecksFailing)
	if c == nil || c.Reason != "RepeatedFailure" || !strings.Contains(c.Message, "check in "+webSlug+" failed again") {
		t.Fatalf("ChecksFailing = %+v, want web's repeated failure named", c)
	}
	if in.Status.CheckFixes != 2 || in.Status.Revisions != 1 {
		t.Errorf("check fixes %d, revisions %d; want 2 and 1", in.Status.CheckFixes, in.Status.Revisions)
	}
}

// TestIntentLevelChecksObservationIsHonouredOnUpgrade: a one-pull-request
// intent whose checks an earlier controller observed on the Intent (the
// deprecated pair, with nothing on the pull request) is not polled again at
// that head, so a failure GitHub still reports there starts no second fix;
// and the pair is never rewritten.
func TestIntentLevelChecksObservationIsHonouredOnUpgrade(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	head := in.Status.PullRequests[0].HeadSHA
	in.Status.ChecksObservedHeadSHA, in.Status.ChecksObservedProjectGeneration = head, 1
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	e.checkOn(head, 41, false)
	before := e.gh.calls["ListCheckRuns"]
	for range 5 {
		e.mustIntent(name)
		e.runRuns()
		e.clock.Advance(2 * time.Minute)
	}
	in = e.get(name)
	if got := e.gh.calls["ListCheckRuns"]; got != before || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Fatalf("checks read %d more times, %d rounds; want the observed head left alone", got-before,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
	if in.Status.ChecksObservedHeadSHA != head || in.Status.PullRequests[0].ChecksObservedHeadSHA != "" {
		t.Errorf("observed: intent %q, pull request %q", in.Status.ChecksObservedHeadSHA,
			in.Status.PullRequests[0].ChecksObservedHeadSHA)
	}
}

// TestMixedEndingIsClosedWithANotice: the app pull request merges while web's
// stays open: the intent stays in review, nothing is posted and nothing
// closed. Once web's closes unmerged every pull request has settled, so the
// intent is Closed: one notice on the issue naming what merged and what did
// not, with the round counts, then the issue closed as not planned. The
// intent expires once its round notices are delivered.
func TestMixedEndingIsClosedWithANotice(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(2, 1101, "Web: put it in the footer.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)

	e.gh.closePRn(1, true)
	e.settleActions(name)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.PullRequests[0].State != prMerged ||
		in.Status.PullRequests[1].State != prOpen {
		t.Fatalf("after app merged: phase %s, pull requests %+v; want in review", in.Status.Phase,
			in.Status.PullRequests)
	}
	if len(e.gh.withMarker(" partial ")) != 0 || len(e.gh.closes[1]) != 0 || e.gh.state() != "open" {
		t.Fatal("an intent with an open pull request posted its ending or closed its issue")
	}

	e.gh.closePRn(2, false)
	e.drive(name, v1alpha1.IntentClosed, repoImage)
	notice := e.gh.withMarker(" partial ")
	if len(notice) != 1 {
		t.Fatalf("partial notices = %d, want one", len(notice))
	}
	body := notice[0].Body
	merged, closed := strings.Index(body, "**Merged:**"), strings.Index(body, "**Closed without merging:**")
	app, web := strings.Index(body, "[acme/app#1]"), strings.Index(body, "["+webSlug+"#2]")
	if merged < 0 || closed < merged || app < merged || app > closed || web < closed ||
		!strings.Contains(body, "**Revisions:** 1") {
		t.Errorf("partial notice does not say app merged and web did not:\n%s", body)
	}
	if !slices.Equal(e.gh.closes[1], []string{ghclient.CloseNotPlanned}) || len(e.gh.withMarker(" summary ")) != 0 {
		t.Errorf("issue closes = %v, summaries %d; want closed not planned, no summary", e.gh.closes[1],
			len(e.gh.withMarker(" summary ")))
	}
	e.settleActions(name)
	if _, ok := e.ttl.wait(e.get(name)); !ok {
		t.Errorf("the closed intent never expires: notices %d of %d rounds", e.get(name).Status.RoundNoticesThrough,
			e.get(name).Status.Rounds)
	}
}

// TestEndingsOfSeveralPullRequests: Merged only once every pull request has
// merged, with the summary listing each; every one closed unmerged is Closed
// with the notice listing each as not merged. A one-pull-request intent's
// close still posts nothing.
func TestEndingsOfSeveralPullRequests(t *testing.T) {
	t.Run("every one merged", func(t *testing.T) {
		e := newMultiEnv(t)
		name := e.inReviewLinked()
		e.gh.closePRn(2, true)
		e.settleActions(name)
		if phase := e.get(name).Status.Phase; phase != v1alpha1.IntentInReview {
			t.Fatalf("one of two merged: phase %s, want InReview", phase)
		}
		e.gh.closePRn(1, true)
		e.drive(name, v1alpha1.IntentMerged, repoImage)
		summary := e.gh.withMarker(" summary ")
		if len(summary) != 1 || !strings.Contains(summary[0].Body, "[acme/app#1]") ||
			!strings.Contains(summary[0].Body, "["+webSlug+"#2]") {
			t.Errorf("summary = %+v, want both pull requests listed", summary)
		}
		if !slices.Equal(e.gh.closes[1], []string{ghclient.CloseCompleted}) || len(e.gh.withMarker(" partial ")) != 0 {
			t.Errorf("issue closes %v, partial notices %d", e.gh.closes[1], len(e.gh.withMarker(" partial ")))
		}
	})
	t.Run("every one closed", func(t *testing.T) {
		e := newMultiEnv(t)
		name := e.inReviewLinked()
		e.gh.closePRn(1, false)
		e.gh.closePRn(2, false)
		e.drive(name, v1alpha1.IntentClosed, repoImage)
		notice := e.gh.withMarker(" partial ")
		if len(notice) != 1 || strings.Contains(notice[0].Body, "**Merged:**") ||
			!strings.Contains(notice[0].Body, "[acme/app#1]") || !strings.Contains(notice[0].Body, "["+webSlug+"#2]") {
			t.Errorf("notice = %+v, want both listed as not merged", notice)
		}
	})
	t.Run("one pull request closed", func(t *testing.T) {
		e := newEnv(t, testProject())
		name := e.awaiting()
		e.gh.label(1, "patchy:approved", approver)
		e.drive(name, v1alpha1.IntentInReview, repoImage)
		e.gh.closePR(false)
		e.drive(name, v1alpha1.IntentClosed, repoImage)
		if len(e.gh.withMarker(" partial ")) != 0 || !slices.Equal(e.gh.closes[1], []string{ghclient.CloseNotPlanned}) {
			t.Errorf("partial notices %d, closes %v; want none and not planned", len(e.gh.withMarker(" partial ")),
				e.gh.closes[1])
		}
	})
}
