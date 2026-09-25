// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/report"
)

func TestRequestedChangesStartsPinnedReviseRound(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if len(in.Status.PullRequests) != 1 {
		t.Fatal("build opened no pull request")
	}
	prNumber := in.Status.PullRequests[0].Number
	e.gh.reviews[prNumber] = []ghclient.Review{{
		ID: 876, NodeID: "review-876", Author: actorOf(approver), State: "CHANGES_REQUESTED",
		Body: "Please add a regression test.", SubmittedAt: e.clock.Now(),
	}}
	e.gh.inline[prNumber] = []ghclient.ReviewComment{{
		ID: 877, NodeID: "inline-877", ReviewID: 876, Author: actorOf(approver),
		Body: "The handler needs this check: ```\u202e", Path: "version.go", Line: 12, Side: "RIGHT",
		DiffHunk: "@@ -1 +1 @@\n+return version", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now(),
	}}
	e.gh.prComments[prNumber] = []*ghclient.Comment{
		{ID: 878, NodeID: "comment-878", UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
			Body: "Please test the JSON response.", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()},
		{ID: 879, NodeID: "comment-879", UserLogin: "outsider", UserID: actorOf("outsider").ID,
			UserType: "User", Body: "OUTSIDER-COMMAND", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()},
	}
	e.gh.patch = "diff --git a/version.go b/version.go\n+\ufeffhidden\n"
	e.clock.Advance(3 * time.Minute)
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 {
		t.Fatalf("revise runs = %d, want one", len(runs))
	}
	run := runs[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerReview || run.Spec.Round != 1 ||
		len(run.Spec.Inputs.ReviewIDs) != 1 || run.Spec.Inputs.ReviewIDs[0] != 876 {
		t.Errorf("revise run = %+v, want review 876 in round 1", run.Spec)
	}
	if in.Status.Rounds != 1 || in.Status.ActiveRun == nil || in.Status.ActiveRun.Name != run.Name {
		t.Errorf("intent rounds = %d, active = %+v, want round 1's run", in.Status.Rounds, in.Status.ActiveRun)
	}
	if run.Spec.ImageFrom == nil || run.Spec.ImageFrom.UID == "" {
		t.Errorf("revise run image source = %+v, want build-round Repository UID", run.Spec.ImageFrom)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	run = e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Status.PushedCommit == "" || in.Status.Revisions != 1 {
		t.Errorf("revise outcome = %+v, revisions = %d, want pushed commit and one revision", run.Status,
			in.Status.Revisions)
	}
	if got := e.gh.branches[branchName(name)]; got != run.Status.PushedCommit {
		t.Errorf("intent branch = %s, want pushed commit %s", got, run.Status.PushedCommit)
	}
	var repo v1alpha1.Repository
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Repository.RepositoryRef.Name}, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.Spec.Ref.Branch != branchName(name) || repo.Status.ResolvedSHA != run.Status.BaseSHA {
		t.Errorf("revise Repository branch = %q, SHA = %q; want %q at the run base %q",
			repo.Spec.Ref.Branch, repo.Status.ResolvedSHA, branchName(name), run.Status.BaseSHA)
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if handoff := cm.Data[keyInvestigation]; !strings.Contains(handoff, "Please add a regression test.") ||
		!strings.Contains(handoff, "data, not rules") {
		t.Errorf("revise handoff omitted or did not fence the review: %q", handoff)
	} else {
		for _, want := range []string{"The handler needs this check", "Please test the JSON response.",
			"<U+202E>", "<U+FEFF>", "version.go:12"} {
			if !strings.Contains(handoff, want) {
				t.Errorf("revise handoff omitted %q", want)
			}
		}
		if strings.Contains(handoff, "OUTSIDER-COMMAND") || strings.ContainsRune(handoff, '\u202e') ||
			strings.ContainsRune(handoff, '\ufeff') {
			t.Errorf("revise handoff carried excluded or invisible text: %q", handoff)
		}
		if _, err := report.ParsePlanInput([]byte(handoff)); err != nil {
			t.Errorf("revise handoff rejected by agent-runner's parser: %v", err)
		}
	}
}

func TestReviseHeadMovedRetriesFromFreshBranchPin(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 901, NodeID: "review-901", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 2 || runs[0].Status.Outcome != OutcomeHeadMoved ||
		runs[1].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("revise attempts = %+v, want head_moved then complete", runs)
	}
	if runs[1].Status.BaseSHA != runs[0].Status.BaseSHA ||
		e.gh.branches[branchName(name)] != runs[1].Status.PushedCommit || in.Status.Revisions != 1 {
		t.Errorf("retry base = %s, branch = %s, revisions = %d; want retry's pushed commit and one revision",
			runs[1].Status.BaseSHA, e.gh.branches[branchName(name)], in.Status.Revisions)
	}
}

func TestFailedNamedCheckStartsBoundedFixRound(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		DetailsURL: "https://github.com/acme/app/actions/runs/81/jobs/91",
		Output:     ghclient.CheckOutput{Title: "Go failed", Summary: "a hidden \u202e failure"}}}
	e.gh.annotations[71] = []ghclient.CheckAnnotation{{Path: "version_test.go", Line: 10, Message: "want JSON"}}
	e.gh.workflowJobs[81] = []ghclient.WorkflowJob{{ID: 91, CheckRunID: 71, HeadSHA: head,
		Name: "go test", Conclusion: "failure"}}
	e.gh.jobLogs[91] = "go test ./...\nFAIL version test"
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		len(runs[0].Spec.Inputs.CheckRunIDs) != 1 || runs[0].Spec.Inputs.CheckRunIDs[0] != 71 {
		t.Fatalf("check-fix run = %+v, want failed check run 71", runs)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.CheckFixes != 1 || in.Status.Revisions != 0 {
		t.Errorf("round counters = fixes %d, revisions %d; want 1, 0", in.Status.CheckFixes, in.Status.Revisions)
	}
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go failed", "version_test.go:10", "FAIL version test", "<U+202E>"} {
		if !strings.Contains(cm.Data[keyInvestigation], want) {
			t.Errorf("check-fix handoff omitted %q", want)
		}
	}
	if cm.Data[keyCheckSignature] == "" {
		t.Error("check-fix handoff did not record its failure signature")
	}
	newHead := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[newHead] = []ghclient.CheckRun{{ID: 72, Name: "test", HeadSHA: newHead,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		DetailsURL: "https://github.com/acme/app/actions/runs/82/jobs/92",
		Output:     ghclient.CheckOutput{Title: "Go failed", Summary: "a hidden \u202e failure"}}}
	e.gh.annotations[72] = []ghclient.CheckAnnotation{{Path: "version_test.go", Line: 10, Message: "want JSON"}}
	e.gh.workflowJobs[82] = []ghclient.WorkflowJob{{ID: 92, CheckRunID: 72, HeadSHA: newHead,
		Name: "go test", Conclusion: "failure"}}
	e.gh.jobLogs[92] = "go test ./...\nFAIL version test"
	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 {
		t.Error("a repeated check signature launched another fix round")
	}
	if !strings.Contains(in.Status.Conditions[len(in.Status.Conditions)-1].Message, "same diagnostic") {
		t.Errorf("ChecksFailing did not explain the repeated signature: %+v", in.Status.Conditions)
	}
}

func TestApproverPRCommandStartsOneRevision(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	pr := in.Status.PullRequests[0]
	e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: 940, NodeID: "comment-940",
		UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
		Body:      "/patchy revise Please check the response header.",
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerCommand ||
		runs[0].Spec.Inputs.CommandID != 940 {
		t.Fatalf("PR command runs = %+v, want one command-triggered round", runs)
	}
	if e.gh.reactions[940] != 1 {
		t.Errorf("eyes reactions to command = %d, want one", e.gh.reactions[940])
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	count := 0
	for _, c := range e.gh.prComments[pr.Number] {
		if strings.Contains(c.Body, "patchy:intent-pr-command:") {
			count++
		}
	}
	if count != 1 || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 {
		t.Errorf("command replies = %d, runs = %d; want one each", count,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
}

func TestVisibleFeedbackAndFenceSeededProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(0x51ce1b))
	alphabet := []string{"a", "\n", "\r\n", "\t", "`", "\u202e", "\ufeff", "\u200d", "\x00", "\xff", "世"}
	for i := range 1000 {
		var input strings.Builder
		for range rng.Intn(300) {
			input.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		quoted := visibleFeedback(input.String())
		if _, err := report.ParsePlanInput([]byte(validPlan + quoted)); err != nil {
			t.Fatalf("seeded case %d: visible feedback was not visible: %v", i, err)
		}
		fence := fencedBounded(quoted, maxFeedbackItemBytes)
		if len(fence) > maxFeedbackItemBytes {
			t.Fatalf("seeded case %d: fenced item has %d bytes", i, len(fence))
		}
	}
	if got := fencedBounded(strings.Repeat("`", maxFeedbackItemBytes), maxFeedbackItemBytes); len(got) > maxFeedbackItemBytes ||
		!strings.Contains(got, "<U+0060>") {
		t.Errorf("pathological backtick fence is not bounded and escaped: len=%d", len(got))
	}
}
