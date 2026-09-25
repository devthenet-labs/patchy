// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"math/rand"
	"slices"
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
	for i := range 250 {
		at := e.clock.Now().Add(time.Duration(i+1) * time.Microsecond)
		e.gh.prComments[prNumber] = append(e.gh.prComments[prNumber], &ghclient.Comment{
			ID: int64(10000 + i), NodeID: "outsider-comment", UserLogin: "outsider",
			UserID: actorOf("outsider").ID, UserType: "User", Body: "OUTSIDER-COMMAND",
			CreatedAt: at, UpdatedAt: at})
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
	assertReviseHandoff(t, cm.Data[keyInvestigation])
}

func assertReviseHandoff(t *testing.T, handoff string) {
	t.Helper()
	if !strings.Contains(handoff, "Please add a regression test.") ||
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
	e.drive(name, v1alpha1.IntentRevising, repoImage)
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
	for i := range 50 {
		at := e.clock.Now().Add(time.Duration(i+1) * time.Microsecond)
		e.gh.prComments[pr.Number] = append(e.gh.prComments[pr.Number], &ghclient.Comment{
			ID: int64(20000 + i), NodeID: "later-approver-comment", UserLogin: approver,
			UserID: actorOf(approver).ID, UserType: "User", Body: "Later review detail.",
			CreatedAt: at, UpdatedAt: at})
	}
	e.clock.Advance(time.Second)
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
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: runs[0].Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data[keyInvestigation], "PR command 940") ||
		!strings.Contains(cm.Data[keyInvestigation], "Please check the response header.") {
		t.Error("a busy PR drowned out the command that authorised the round")
	}
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
	got := fencedBounded(strings.Repeat("`", maxFeedbackItemBytes), maxFeedbackItemBytes)
	if len(got) > maxFeedbackItemBytes ||
		!strings.Contains(got, "<U+0060>") {
		t.Errorf("pathological backtick fence is not bounded and escaped: len=%d", len(got))
	}
}

func TestCheckDiagnosticsBoundsAndFencesMaliciousLog(t *testing.T) {
	e := newEnv(t, testProject())
	p := &pass{r: e.intent}
	e.gh.checks[baseSHA] = []ghclient.CheckRun{{ID: 990, Name: "test", HeadSHA: baseSHA,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		DetailsURL: "https://github.com/acme/app/actions/runs/991/jobs/992"}}
	e.gh.workflowJobs[991] = []ghclient.WorkflowJob{{ID: 992, CheckRunID: 990, HeadSHA: baseSHA,
		Name: "go test", Conclusion: "failure"}}
	e.gh.jobLogs[992] = strings.Repeat("`", 32<<10) + "\u202e"
	visible, _, err := p.checkDiagnostics(context.Background(), appRepoURL, baseSHA,
		failedChecks{checkIDs: []int64{990}})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) > 48<<10 || !strings.Contains(visible, "<U+0060>") ||
		strings.ContainsRune(visible, '\u202e') {
		t.Fatalf("diagnostics were not bounded and visibly fenced: %d bytes", len(visible))
	}
}

func TestReviseComparePatchFenceIsBounded(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 988, NodeID: "review-988", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.gh.patch = strings.Repeat("`", maxVisiblePatchBytes)
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: run.Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	handoff := cm.Data[keyInvestigation]
	marker := "### Compare patch\n\n"
	index := strings.Index(handoff, marker)
	if index < 0 {
		t.Fatalf("revise handoff omitted compare patch: %q", handoff)
	}
	quoted := strings.TrimSpace(handoff[index+len(marker):])
	if len(quoted) > maxVisiblePatchBytes || !strings.Contains(quoted, "<U+0060>") {
		t.Fatalf("compare patch fence = %d bytes, escaped backticks = %t", len(quoted),
			strings.Contains(quoted, "<U+0060>"))
	}
}

func TestDeletedPRBranchDuringFastForwardSettlesHeadMoved(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 981, NodeID: "review-981", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.failNext("FastForwardRef", ghclient.ErrRefNotFound)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) < 1 || runs[0].Status.Outcome != OutcomeHeadMoved {
		t.Fatalf("deleted branch outcome = %+v, want head_moved", runs)
	}
}

func TestDeletedPRBranchBeforeRevisePinBlocksWithoutSpinning(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	headofBranch := e.gh.branches[branchName(name)]
	e.gh.reviews[number] = []ghclient.Review{{ID: 986, NodeID: "review-986", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	delete(e.gh.branches, branchName(name))
	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 || in.Status.ActiveRun != nil {
		t.Fatalf("missing branch left active run or retries: %+v", in.Status)
	}
	e.gh.branches[branchName(name)] = headofBranch
	e.drive(name, v1alpha1.IntentInReview, repoImage)
}

func TestVanishedReviewDoesNotStrandReviseRun(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 982, NodeID: "review-982", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.reviews[number] = nil
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) == 0 || runs[0].Status.Phase != v1alpha1.RunFailed || in.Status.Revisions != 0 {
		t.Fatalf("vanished review left run %+v, revisions %d", runs, in.Status.Revisions)
	}
}

func TestDeletedPRCommandDoesNotLaunchReviseAgent(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.prComments[number] = []*ghclient.Comment{{ID: 987, NodeID: "comment-987", UserLogin: approver,
		UserID: actorOf(approver).ID, UserType: "User", Body: "/patchy revise Please add a test.",
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.prComments[number] = slices.DeleteFunc(e.gh.prComments[number], func(c *ghclient.Comment) bool {
		return c.ID == 987
	})
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Outcome != OutcomeInputUnavailable || in.Status.Revisions != 0 {
		t.Fatalf("deleted command run = %+v, revisions %d", runs, in.Status.Revisions)
	}
}

func TestReviseImageRefusalStopsAtOneAttempt(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 983, NodeID: "review-983", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.runs.Images.Enabled = false
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Outcome != OutcomeImageRequired || in.Status.Revisions != 0 {
		t.Fatalf("image-refused revise runs = %+v, revisions %d; want one failed round", runs, in.Status.Revisions)
	}
}

func TestRevisionAndCheckFixZeroLimitsLaunchNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks bool
	}{
		{name: "review revision"},
		{name: "named check fix", checks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := testProject()
			if tc.checks {
				project.Spec.Checks.Fix = []string{"test"}
				project.Spec.Limits.MaxCheckFixes = new(int32)
			} else {
				project.Spec.Limits.MaxRevisions = new(int32)
			}
			e := newEnv(t, project)
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)
			head, number := in.Status.PullRequests[0].HeadSHA, in.Status.PullRequests[0].Number
			if tc.checks {
				e.gh.checks[head] = []ghclient.CheckRun{{ID: 74, Name: "test", HeadSHA: head,
					Status: "completed", Conclusion: "failure", Output: ghclient.CheckOutput{Title: "test failed"}}}
			} else {
				e.gh.reviews[number] = []ghclient.Review{{ID: 989, NodeID: "review-989", Author: actorOf(approver),
					State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
				e.clock.Advance(3 * time.Minute)
			}
			in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
			if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 || in.Status.Rounds != 0 {
				t.Fatalf("limit-zero started a revise run: %+v", in.Status)
			}
		})
	}
}

func TestSettledChecksAreNotPolledAgainAtSameHead(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 72, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "success"}}
	e.clock.Advance(2 * time.Minute)
	e.mustIntent(name)
	if got := e.get(name).Status.ChecksObservedHeadSHA; got != head {
		t.Fatalf("settled head = %q, want %q", got, head)
	}
	before := e.gh.calls["ListCheckRuns"]
	e.clock.Advance(2 * time.Minute)
	e.mustIntent(name)
	if got := e.gh.calls["ListCheckRuns"]; got != before {
		t.Errorf("settled check calls = %d, want %d", got, before)
	}
	var updated v1alpha1.Project
	projectKey := types.NamespacedName{Namespace: testNS, Name: "target"}
	if err := e.c.Get(context.Background(), projectKey, &updated); err != nil {
		t.Fatal(err)
	}
	updated.Generation++
	updated.Spec.Checks.Fix = append(updated.Spec.Checks.Fix, "lint")
	if err := e.c.Update(context.Background(), &updated); err != nil {
		t.Fatal(err)
	}
	e.gh.checks[head] = append(e.gh.checks[head], ghclient.CheckRun{ID: 73, Name: "lint", HeadSHA: head,
		Status: "completed", Conclusion: "failure", Output: ghclient.CheckOutput{Title: "lint failed"}})
	e.clock.Advance(2 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	if got := e.gh.calls["ListCheckRuns"]; got <= before {
		t.Errorf("changed Project did not resume check polling: %d <= %d", got, before)
	}
}

func TestReviseRetryDoesNotReadLaterPRFeedback(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 984, NodeID: "review-984", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Original feedback.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)
	for range 20 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) == 1 &&
			runs[0].Status.Outcome == OutcomeHeadMoved {
			break
		}
		e.clock.Advance(time.Minute)
	}
	e.clock.Advance(time.Second)
	e.gh.prComments[number] = append(e.gh.prComments[number], &ghclient.Comment{ID: 985,
		NodeID: "comment-985", UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
		Body: "LATER-FEEDBACK", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 2 {
		t.Fatalf("revise attempts = %d, want two", len(runs))
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS,
		Name: runs[1].Spec.Inputs.ConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cm.Data[keyInvestigation], "LATER-FEEDBACK") {
		t.Error("retry read feedback posted after the round was leased")
	}
}
