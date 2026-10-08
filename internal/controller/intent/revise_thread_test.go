// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// prComment adds a conversation comment by login, posted now, to the
// intent's pull request (number 1).
func (e *env) prComment(id int64, login, userType, body string) {
	const n = 1
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.prComments[n] = append(e.gh.prComments[n], &ghclient.Comment{ID: id,
		NodeID: fmt.Sprintf("comment-%d", id), UserLogin: login, UserID: actorOf(login).ID, UserType: userType,
		Body: body, CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
}

// secondRound is the investigation.md of round 2's first attempt.
func (e *env) secondRound(name string) string {
	e.t.Helper()
	run := e.reviseRun(name, 2)
	if run == nil {
		e.t.Fatal("no round 2")
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		e.t.Fatal(err)
	}
	return cm.Data[keyInvestigation]
}

// section is the text under heading in a round's input, up to the next
// level-3 heading; ok is false when there is no such heading.
func section(input, heading string) (string, bool) {
	_, rest, ok := strings.Cut(input, "\n### "+heading+"\n\n")
	if !ok {
		return "", false
	}
	body, _, _ := strings.Cut(rest, "\n\n### ")
	return body, true
}

// TestFailedRoundFeedbackIsNewAgain: the feedback a round was given, and
// the feedback posted while it ran, reach the next round as new feedback
// when the round failed, with the failure itself. Regression: the next
// round read only what came after the failed round's lease, so what the
// failed round never acted on was lost.
func TestFailedRoundFeedbackIsNewAgain(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if strings.Contains(spec.Finding, "-rev1-") {
			return failingBuild(spec)
		}
		return defaultOutput(spec)
	}
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewOn(1, 3101, "FIRST-REVIEW: name the field sha.")
	e.prComment(3102, approver, "User", "FIRST-COMMENT: and keep it short.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.clock.Advance(time.Second)
	e.prComment(3103, approver, "User", "DURING-COMMENT: also the build time.")
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.round(name, 1).latest(); r == nil || r.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("round 1 = %+v, want it failed", r)
	}
	e.reviewOn(1, 3104, "SECOND-REVIEW: and a test.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.round(name, 2).latest(); r.Status.Phase != v1alpha1.RunComplete {
		t.Errorf("round 2 = %s %s %q", r.Status.Phase, r.Status.Outcome, r.Status.Detail)
	}
	input := e.secondRound(name)
	fresh, ok := section(input, "Approver feedback")
	if !ok {
		t.Fatalf("round 2 has no approver feedback: %q", input)
	}
	for _, want := range []string{"FIRST-REVIEW", "FIRST-COMMENT", "DURING-COMMENT", "SECOND-REVIEW"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("round 2's new feedback omits %s, which no round acted on", want)
		}
	}
	if _, ok := section(input, earlierFeedbackHeading); ok {
		t.Errorf("round 2 shows earlier feedback although no round succeeded: %q", input)
	}
	previous, ok := section(input, "Previous round's outcome")
	if !ok || !strings.Contains(previous, "Round 1 (review) failed: runtime_error") ||
		!strings.Contains(previous, "the CLI crashed") {
		t.Errorf("round 2's previous outcome = %q, want round 1's failure", previous)
	}
	if strings.Index(input, "### Previous round's outcome") > strings.Index(input, "### Approver feedback") {
		t.Error("the previous round's outcome follows the round's own feedback")
	}
	if _, err := report.ParsePlanInput([]byte(input)); err != nil {
		t.Errorf("round 2 input refused by agent-runner's parser: %v", err)
	}
}

func (e *env) round(name string, round int32) roundRuns {
	var rs roundRuns
	for _, r := range e.runsOf(name, v1alpha1.IntentStageRevise) {
		if r.Spec.Round == round {
			rs = append(rs, &r)
		}
	}
	return rs
}

// TestEarlierFeedbackIsContext: after a round succeeded, its feedback is
// shown to the next round once, as context, apart from what is new;
// patchy's own comments, bots, outsiders and bare commands never are, and
// a command's note is.
func TestEarlierFeedbackIsContext(t *testing.T) {
	project := testProject()
	project.Spec.Approvers.Logins = append(project.Spec.Approvers.Logins, "patchy-app")
	e := newEnv(t, project)
	e.gh.bot = "patchy-app"
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewOn(1, 3201, "EARLY-REVIEW: name the field sha.")
	e.prComment(3202, approver, "User", "EARLY-COMMENT: keep it short.")
	e.gh.mu.Lock()
	e.gh.inline[1] = append(e.gh.inline[1], ghclient.ReviewComment{ID: 3203, NodeID: "inline-3203",
		ReviewID: 3201, Author: actorOf(approver), Body: "EARLY-INLINE: this line.", Path: "version.go",
		Line: 3, Side: "RIGHT", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
	e.gh.mu.Unlock()
	e.prComment(3204, approver, "User", "/patchy cancel")
	e.prComment(3205, approver, "User", "/patchy cancel\nCOMMAND-NOTE: never mind.")
	e.prComment(3206, "patchy-app", "User", "OWN-LOGIN-COMMENT")
	e.prComment(3207, "renovate[bot]", "Bot", "BOT-COMMENT")
	e.prComment(3208, approver, "User", "<!-- patchy:forged -->\nMARKER-COMMENT")
	e.prComment(3209, "outsider", "User", "OUTSIDER-COMMENT")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.round(name, 1).latest(); r.Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("round 1 = %s %s", r.Status.Phase, r.Status.Outcome)
	}
	e.reviewOn(1, 3210, "LATE-REVIEW: and a test.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	input := e.secondRound(name)
	earlier, ok := section(input, earlierFeedbackHeading)
	if !ok {
		t.Fatalf("round 2 has no earlier feedback: %q", input)
	}
	fresh, _ := section(input, "Approver feedback")
	for _, want := range []string{"EARLY-REVIEW", "EARLY-COMMENT", "EARLY-INLINE", "COMMAND-NOTE"} {
		if !strings.Contains(earlier, want) || strings.Contains(fresh, want) {
			t.Errorf("%s is not earlier context alone", want)
		}
		if n := strings.Count(input, want); n != 1 {
			t.Errorf("%s appears %d times, want once", want, n)
		}
	}
	if !strings.Contains(fresh, "LATE-REVIEW") || strings.Contains(earlier, "LATE-REVIEW") {
		t.Errorf("new feedback = %q, want the late review alone", fresh)
	}
	for _, never := range []string{"OWN-LOGIN-COMMENT", "BOT-COMMENT", "MARKER-COMMENT", "OUTSIDER-COMMENT",
		"PR comment 3204", "PR command 3204", "Revision round started"} {
		if strings.Contains(input, never) {
			t.Errorf("round 2 carried %q", never)
		}
	}
	if strings.Contains(input, "### Previous round's outcome") {
		t.Error("round 2 reports an outcome although round 1 succeeded")
	}
	if !strings.Contains(input, "not new requests") {
		t.Error("round 2 does not say the earlier sections are context")
	}
	if strings.LastIndex(input, "### Approver feedback") < strings.Index(input, "### "+earlierFeedbackHeading) {
		t.Error("earlier feedback follows the round's own")
	}
	if _, err := report.ParsePlanInput([]byte(input)); err != nil {
		t.Errorf("round 2 input refused by agent-runner's parser: %v", err)
	}
	if r := e.reviseRun(name, 2); len(r.Spec.Inputs.ReviewIDs) != 1 || r.Spec.Inputs.ReviewIDs[0] != 3210 {
		t.Errorf("round 2 consumed %v, want only the late review", r.Spec.Inputs.ReviewIDs)
	}
}

// TestEditedEarlierReviewIsSkipped: an older review edited after its round
// is left out of the next round's context; it never fails the round, as one
// of the round's own reviews would.
func TestEditedEarlierReviewIsSkipped(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewOn(1, 3301, "EDITED-LATER: name the field sha.")
	e.prComment(3302, approver, "User", "EDITED-COMMENT")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.mu.Lock()
	e.gh.reviewEdits["review-3301"] = true
	e.gh.edited[3302] = true
	e.gh.mu.Unlock()
	e.reviewOn(1, 3303, "LATE-REVIEW: and a test.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	r := e.round(name, 2).latest()
	if r.Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("round 2 = %s %s %q; an older edited review failed it", r.Status.Phase, r.Status.Outcome,
			r.Status.Detail)
	}
	input := e.secondRound(name)
	if strings.Contains(input, "EDITED-LATER") || strings.Contains(input, "EDITED-COMMENT") ||
		!strings.Contains(input, "LATE-REVIEW") {
		t.Errorf("round 2 input = %q, want the edited items left out", input)
	}
}

// TestCheckFixRoundGetsTheThread: a check-fix round is shown the approvers'
// thread and the failed round before it, as context only, and its failure
// signature is its checks' alone.
func TestCheckFixRoundGetsTheThread(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if strings.Contains(spec.Finding, "-rev1-") {
			return failingBuild(spec)
		}
		return defaultOutput(spec)
	}
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	head := in.Status.PullRequests[0].HeadSHA
	e.reviewOn(1, 3402, "THREAD-REVIEW: name the field sha.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.round(name, 1).latest(); r.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("round 1 = %s, want failed", r.Status.Phase)
	}
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 3403, Name: "test", HeadSHA: head, Status: "completed",
		Conclusion: "failure", Output: ghclient.CheckOutput{Title: "CHECK-TITLE"}}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	run := e.reviseRun(name, 2)
	if run == nil || run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
		t.Fatalf("round 2 = %+v, want a check-fix round", run)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	input := e.secondRound(name)
	earlier, ok := section(input, earlierFeedbackHeading)
	if !ok || !strings.Contains(earlier, "THREAD-REVIEW") {
		t.Errorf("check-fix round's earlier feedback = %q, want the review", earlier)
	}
	if previous, ok := section(input, "Previous round's outcome"); !ok ||
		!strings.Contains(previous, "Round 1 (review) failed") {
		t.Errorf("check-fix round's previous outcome = %q", previous)
	}
	if failures, ok := section(input, "Check failures"); !ok || !strings.Contains(failures, "CHECK-TITLE") ||
		strings.Contains(failures, "THREAD-REVIEW") {
		t.Errorf("check failures = %q, want the checks alone", failures)
	}
	if strings.Contains(input, "### Approver feedback") {
		t.Error("a check-fix round labelled the thread as approver feedback")
	}
	if _, err := report.ParsePlanInput([]byte(input)); err != nil {
		t.Errorf("check-fix input refused by agent-runner's parser: %v", err)
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	var repo v1alpha1.Repository
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Repository.RepositoryRef.Name}, &repo); err != nil {
		t.Fatal(err)
	}
	d, err := (&pass{r: e.intent}).checkDiagnostics(context.Background(), appRepoURL, repo.Status.ResolvedSHA,
		failedChecks{checkIDs: run.Spec.Inputs.CheckRunIDs, statusIDs: run.Spec.Inputs.StatusIDs})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[keyCheckSignature] == "" || cm.Data[keyCheckSignature] != d.signature {
		t.Errorf("check signature = %q, want the checks' own %q", cm.Data[keyCheckSignature], d.signature)
	}
}

// TestCheckFixRoundWithoutTheThread: a check-fix round whose thread GitHub
// will not list still runs on its checks, with the earlier feedback shown as
// unavailable. Regression: the context-only thread read failed the round, or
// requeued it until GitHub answered.
func TestCheckFixRoundWithoutTheThread(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if strings.Contains(spec.Finding, "-rev1-") {
			return failingBuild(spec)
		}
		return defaultOutput(spec)
	}
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	head := in.Status.PullRequests[0].HeadSHA
	e.reviewOn(1, 3502, "THREAD-REVIEW: name the field sha.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.mu.Lock()
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 3503, Name: "test", HeadSHA: head, Status: "completed",
		Conclusion: "failure", Output: ghclient.CheckOutput{Title: "CHECK-TITLE"}}}
	e.gh.repoErrs["ListPullRequestReviewComments acme/app"] = errors.New("graphql: something went wrong")
	e.gh.mu.Unlock()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	if run := e.reviseRun(name, 2); run == nil || run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
		t.Fatalf("round 2 = %+v, want a check-fix round", run)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if r := e.round(name, 2).latest(); r.Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("round 2 = %s %s %q; the thread read failed it", r.Status.Phase, r.Status.Outcome,
			r.Status.Detail)
	}
	input := e.secondRound(name)
	if earlier, ok := section(input, earlierFeedbackHeading); !ok || earlier != earlierUnavailable {
		t.Errorf("earlier feedback = %q, want %q", earlier, earlierUnavailable)
	}
	if failures, ok := section(input, "Check failures"); !ok || !strings.Contains(failures, "CHECK-TITLE") {
		t.Errorf("check failures = %q, want the checks", failures)
	}
}

// testRun is a revise run of round in repoURL at plan revision rev, created
// at, with phase.
func testRun(stage v1alpha1.IntentStage, trigger v1alpha1.IntentRunTrigger, round, rev int32, repoURL string,
	at time.Time, phase v1alpha1.RunPhase) *v1alpha1.IntentRun {
	run := &v1alpha1.IntentRun{}
	run.CreationTimestamp = metav1.NewTime(at)
	run.Spec.Stage, run.Spec.Trigger, run.Spec.Round, run.Spec.Attempt = stage, trigger, round, 1
	run.Spec.Repository.URL, run.Spec.Inputs.PlanRevision = repoURL, rev
	run.Status.Phase = phase
	return run
}

// TestThreadWindow: the thread opens at the first build of the round's plan
// revision, and only a round of the same repository and plan revision that
// completed on approver feedback marks what is new.
func TestThreadWindow(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	const other = "https://github.com/acme/other"
	build := func(attempt int32, m int) *v1alpha1.IntentRun {
		b := testRun(v1alpha1.IntentStageBuild, "", 2, 2, appRepoURL, at(m), v1alpha1.RunComplete)
		b.Spec.Attempt = attempt
		b.Status.FinishedAt = &metav1.Time{Time: at(m + 5)}
		return b
	}
	rev := func(trigger v1alpha1.IntentRunTrigger, round, plan int32, repoURL string, m int,
		phase v1alpha1.RunPhase) *v1alpha1.IntentRun {
		return testRun(v1alpha1.IntentStageRevise, trigger, round, plan, repoURL, at(m), phase)
	}
	current := rev(v1alpha1.IntentRunTriggerReview, 9, 2, appRepoURL, 100, v1alpha1.RunPending)
	for _, tc := range []struct {
		name      string
		runs      []*v1alpha1.IntentRun
		wantAfter time.Time
	}{
		{name: "after the build", runs: []*v1alpha1.IntentRun{build(1, 10), build(2, 20)},
			wantAfter: at(25).Add(-time.Nanosecond)},
		{name: "a failed round moves nothing", runs: []*v1alpha1.IntentRun{build(1, 10),
			rev(v1alpha1.IntentRunTriggerReview, 3, 2, appRepoURL, 40, v1alpha1.RunFailed)},
			wantAfter: at(15).Add(-time.Nanosecond)},
		{name: "a completed round does", runs: []*v1alpha1.IntentRun{build(1, 10),
			rev(v1alpha1.IntentRunTriggerCommand, 3, 2, appRepoURL, 40, v1alpha1.RunComplete),
			rev(v1alpha1.IntentRunTriggerReview, 4, 2, appRepoURL, 50, v1alpha1.RunFailed)},
			wantAfter: at(40)},
		{name: "a check fix, another repository or plan revision does not", runs: []*v1alpha1.IntentRun{
			build(1, 10),
			rev(v1alpha1.IntentRunTriggerChecks, 3, 2, appRepoURL, 40, v1alpha1.RunComplete),
			rev(v1alpha1.IntentRunTriggerReview, 4, 2, other, 50, v1alpha1.RunComplete),
			rev(v1alpha1.IntentRunTriggerReview, 5, 1, appRepoURL, 60, v1alpha1.RunComplete)},
			wantAfter: at(15).Add(-time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &pass{runs: append(tc.runs, current)}
			lower, after := p.threadWindow(current)
			if !lower.Equal(at(10)) || !after.Equal(tc.wantAfter) {
				t.Errorf("window = %s, new after %s; want %s, %s", lower, after, at(10), tc.wantAfter)
			}
		})
	}
}

func TestPreviousOutcome(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	current := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerReview, 5, 2, appRepoURL,
		t0.Add(time.Hour), v1alpha1.RunPending)
	failed := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerChecks, 3, 2, appRepoURL, t0,
		v1alpha1.RunFailed)
	failed.Status.Outcome, failed.Status.Detail = "not_built", strings.Repeat("\u202e`", 8<<10)
	retried := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerChecks, 3, 2, appRepoURL, t0,
		v1alpha1.RunComplete)
	retried.Spec.Attempt = 2
	sibling := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerReview, 4, 2,
		"https://github.com/acme/other", t0, v1alpha1.RunFailed)
	for _, tc := range []struct {
		name string
		runs []*v1alpha1.IntentRun
		want bool
	}{
		{name: "none"},
		{name: "failed", runs: []*v1alpha1.IntentRun{failed, sibling}, want: true},
		{name: "retried to success", runs: []*v1alpha1.IntentRun{failed, retried}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&pass{runs: append(tc.runs, current)}).previousOutcome(current)
			if (got != "") != tc.want {
				t.Fatalf("previous outcome = %q, want shown %t", got, tc.want)
			}
			if got == "" {
				return
			}
			if len(got) > maxPreviousOutcomeBytes || !strings.Contains(got, "Round 3 (checks) failed: not_built") ||
				strings.ContainsRune(got, '\u202e') {
				t.Errorf("previous outcome is not bounded and visible: %d bytes", len(got))
			}
			if _, err := report.ParsePlanInput([]byte(validPlan + got)); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRenderEarlierKeepsTheNewestAndCountsTheRest(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	items := make([]reviseFeedbackItem, 0, 40)
	for i := range 40 {
		items = append(items, reviseFeedbackItem{key: feedbackKey{feedbackComment, int64(i + 1)},
			at: t0.Add(time.Duration(i) * time.Minute), text: fmt.Sprintf("ITEM-%02d %s", i, strings.Repeat("x", 900))})
	}
	skip := func(_ context.Context, it reviseFeedbackItem) (bool, error) { return it.key.id != 40, nil }
	got := renderEarlier(context.Background(), items, skip)
	if len(got) > maxEarlierFeedbackBytes {
		t.Errorf("earlier feedback = %d bytes, over %d", len(got), maxEarlierFeedbackBytes)
	}
	if strings.Contains(got, "ITEM-39") || !strings.Contains(got, "ITEM-38") || strings.Contains(got, "ITEM-00") {
		t.Errorf("earlier feedback did not keep the newest verified items")
	}
	omitted, _, _ := strings.Cut(got, "\n\n")
	shown := strings.Count(got, "ITEM-")
	if want := omittedLine(40 - 1 - shown); omitted != want {
		t.Errorf("first line = %q, want %q", omitted, want)
	}
	if strings.Index(got, "ITEM-37") > strings.Index(got, "ITEM-38") {
		t.Error("earlier feedback is not oldest first")
	}
	if got := renderEarlier(context.Background(), items[:2], skip); strings.Contains(got, "omitted") {
		t.Errorf("nothing was left out, yet %q", got)
	}
}

// TestRenderEarlierSkipsAFailedLookup: an older item GitHub fails to
// verify is left out, never fatal to the round.
func TestRenderEarlierSkipsAFailedLookup(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	items := []reviseFeedbackItem{
		{key: feedbackKey{feedbackComment, 1}, at: t0, text: "OLDER"},
		{key: feedbackKey{feedbackComment, 2}, at: t0.Add(time.Minute), text: "FAILS"},
	}
	verify := func(_ context.Context, it reviseFeedbackItem) (bool, error) {
		if it.key.id == 2 {
			return false, errors.New("graphql: something went wrong")
		}
		return true, nil
	}
	if got := renderEarlier(context.Background(), items, verify); !strings.Contains(got, "OLDER") ||
		strings.Contains(got, "FAILS") {
		t.Errorf("earlier feedback = %q, want the failed lookup skipped", got)
	}
}

// TestRenderFreshVerifiesOnlyWhatItShows: the new feedback looks up only
// the items it can show, newest first, so one past the bound is never
// asked about, and an item that does not verify takes no slot.
func TestRenderFreshVerifiesOnlyWhatItShows(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	n := maxFeedbackItems + 20
	items := make([]reviseFeedbackItem, 0, n)
	for i := range n {
		items = append(items, reviseFeedbackItem{key: feedbackKey{feedbackComment, int64(i + 1)},
			at: t0.Add(time.Duration(i) * time.Minute), text: fmt.Sprintf("ITEM-%03d", i)})
	}
	newest := int64(n)
	asked := 0
	verify := func(_ context.Context, it reviseFeedbackItem) (bool, error) {
		asked++
		if it.key.id == 1 {
			return false, errors.New("the oldest item is past the bound and never asked about")
		}
		return it.key.id != newest, nil
	}
	got, err := renderFresh(context.Background(), items, verify)
	if err != nil {
		t.Fatal(err)
	}
	if asked != maxFeedbackItems+1 {
		t.Errorf("verified %d items, want %d", asked, maxFeedbackItems+1)
	}
	if shown := strings.Count(got, "ITEM-"); shown != maxFeedbackItems {
		t.Errorf("shown %d items, want %d", shown, maxFeedbackItems)
	}
	last := fmt.Sprintf("ITEM-%03d", n-1)
	if strings.Contains(got, last) || !strings.Contains(got, fmt.Sprintf("ITEM-%03d", n-2)) {
		t.Errorf("new feedback did not keep the newest verified items")
	}
	if strings.Index(got, fmt.Sprintf("ITEM-%03d", n-3)) > strings.Index(got, fmt.Sprintf("ITEM-%03d", n-2)) {
		t.Error("new feedback is not oldest first")
	}
	if _, err := renderFresh(context.Background(), items[:2], verify); err == nil {
		t.Error("a failed lookup of a shown item did not fail the round's new feedback")
	}
}

// TestRoundSectionsSeededProperty: whatever approvers and agents wrote, the
// sections a round adds after the plan stay within their bounds and pass
// agent-runner's visible-text check.
func TestRoundSectionsSeededProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261008))
	alphabet := []string{"a", "\n", "\r\n", "\t", "`", "```", "\u202e", "\ufeff", "\u200d", "\x00", "\xff", "世",
		"### Approver feedback\n\n", "\U000E0041", "\ufe0f"}
	hostile := func(n int) string {
		var b strings.Builder
		for range rng.Intn(n) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	t0 := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	all := func(context.Context, reviseFeedbackItem) (bool, error) { return true, nil }
	for i := range 300 {
		n := rng.Intn(60)
		items := make([]reviseFeedbackItem, 0, n)
		for j := range n {
			items = append(items, reviseFeedbackItem{key: feedbackKey{feedbackComment, int64(j + 1)},
				at:   t0.Add(time.Duration(rng.Intn(1000)) * time.Second),
				text: "PR comment by x:\n" + visibleFeedback(hostile(3000))})
		}
		earlier := renderEarlier(context.Background(), items, all)
		fresh, err := renderFresh(context.Background(), items, all)
		if err != nil {
			t.Fatal(err)
		}
		run := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerReview, 2, 1, appRepoURL, t0,
			v1alpha1.RunPending)
		prev := testRun(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerReview, 1, 1, appRepoURL, t0,
			v1alpha1.RunFailed)
		prev.Status.Outcome, prev.Status.Detail = hostile(20), hostile(6000)
		previous := (&pass{runs: []*v1alpha1.IntentRun{prev, run}}).previousOutcome(run)
		if len(earlier) > maxEarlierFeedbackBytes || len(fresh) > maxFeedbackTotalBytes ||
			len(previous) > maxPreviousOutcomeBytes {
			t.Fatalf("seeded case %d: sections of %d, %d, %d bytes", i, len(earlier), len(fresh), len(previous))
		}
		input := validPlan + "\n\n### Previous round's outcome\n\n" + previous + "\n\n### " +
			earlierFeedbackHeading + "\n\n" + earlier + "\n\n### Approver feedback\n\n" + fresh + "\n"
		if _, err := report.ParsePlanInput([]byte(input)); err != nil {
			t.Fatalf("seeded case %d: round input refused: %v", i, err)
		}
	}
}
