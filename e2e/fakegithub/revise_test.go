// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

func TestReviseAPISurfaceAndScope(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	srv.OpenPull(7, "patchy-intent/target-1")
	reviewID := srv.AddReview(7, human, "CHANGES_REQUESTED", "fix the response", "abc")
	inlineID := srv.AddReviewComment(7, human, reviewID, "main.go", 8, "RIGHT", "bad line", "@@ hunk")
	srv.SetComparePatch("main", "abc", "From abc\n--- a/main.go\n+++ b/main.go\n")
	srv.SetCheckRun("abc", fakegithub.CheckRun{ID: 41, Name: "test", Status: "completed",
		Conclusion: "failure", Summary: "test failed", RunID: 77,
		Annotations: []fakegithub.CheckAnnotation{{Path: "main.go", Line: 8, Message: "bad"}}})
	srv.SetCommitStatus("abc", fakegithub.CommitStatus{ID: 51, Context: "lint", State: "error", Description: "lint failed"})
	srv.SetWorkflowJob(77, fakegithub.WorkflowJob{ID: 88, CheckRunID: 41, HeadSHA: "abc", Name: "test",
		Conclusion: "failure", Log: "failure tail"})

	if got, err := c.ListPullRequestReviews(ctx, target, 7); err != nil || len(got) != 1 || got[0].Body != "fix the response" {
		t.Fatalf("reviews = %+v, %v", got, err)
	}
	if got, err := c.ListPullRequestReviewComments(ctx, target, 7); err != nil || len(got) != 1 || got[0].Path != "main.go" {
		t.Fatalf("inline comments = %+v, %v", got, err)
	}
	reviews, _ := c.ListPullRequestReviews(ctx, target, 7)
	inline, _ := c.ListPullRequestReviewComments(ctx, target, 7)
	if got, err := c.ReviewEdited(ctx, reviews[0].NodeID); err != nil || got {
		t.Fatalf("pristine review edited = %v, %v", got, err)
	}
	if got, err := c.ReviewCommentEdited(ctx, inline[0].NodeID); err != nil || got {
		t.Fatalf("pristine inline comment edited = %v, %v", got, err)
	}
	srv.EditReviewBody(reviewID, "forged")
	srv.EditReviewCommentBody(inlineID, "forged")
	if got, err := c.ReviewEdited(ctx, reviews[0].NodeID); err != nil || !got {
		t.Fatalf("edited review = %v, %v", got, err)
	}
	if got, err := c.ReviewCommentEdited(ctx, inline[0].NodeID); err != nil || !got {
		t.Fatalf("edited inline comment = %v, %v", got, err)
	}
	if got, err := c.ComparePatch(ctx, target, "main", "abc"); err != nil || !strings.Contains(got, "+new") && !strings.Contains(got, "+++ b/main.go") {
		t.Fatalf("patch = %q, %v", got, err)
	}
	if got, err := c.ListCheckRuns(ctx, target, "abc"); err != nil || len(got) != 1 || got[0].ID != 41 {
		t.Fatalf("check runs = %+v, %v", got, err)
	}
	if got, err := c.ListCheckAnnotations(ctx, target, 41, 50); err != nil || len(got) != 1 || got[0].Line != 8 {
		t.Fatalf("annotations = %+v, %v", got, err)
	}
	if got, err := c.ListCommitStatuses(ctx, target, "abc"); err != nil || len(got) != 1 || got[0].ID != 51 {
		t.Fatalf("statuses = %+v, %v", got, err)
	}
	if got, err := c.ListWorkflowJobs(ctx, target, 77); err != nil || len(got) != 1 || got[0].CheckRunID != 41 {
		t.Fatalf("jobs = %+v, %v", got, err)
	}
	if got, err := c.GetJobLogTail(ctx, target, 88, 32); err != nil || got != "failure tail" {
		t.Fatalf("job log = %q, %v", got, err)
	}
	if err := c.RequestReviewers(ctx, target, 7, []string{"peter"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.RequestedReviewers(7); len(got) != 1 || got[0] != "peter" {
		t.Fatalf("requested reviewers = %+v", got)
	}

	app := newApp(t, srv)
	read := scopedClient(t, srv, app, target, ghclient.TokenPerms{Checks: ghclient.PermRead})
	if _, err := read.ListCheckRuns(ctx, target, "abc"); err != nil {
		t.Fatalf("checks read token: %v", err)
	}
	if _, err := read.ListCommitStatuses(ctx, target, "abc"); err == nil {
		t.Error("checks-only token read commit statuses")
	}
	if _, err := read.ListCheckRuns(ctx, intents, "abc"); err == nil {
		t.Error("target-only token read other repository")
	}
}

// TestWorkflowRunsAndRerunScope: a check run names its workflow run's
// check suite, the suite's runs are listed with actions read, and re-running
// a completed run's failed jobs takes actions write, queues the run again
// and is recorded; a run still running is refused, as GitHub refuses it.
func TestWorkflowRunsAndRerunScope(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	srv.SetCheckRun("abc", fakegithub.CheckRun{ID: 41, Name: "test", Status: "completed", Conclusion: "failure",
		RunID: 77})
	srv.SetWorkflowRun(fakegithub.WorkflowRun{ID: 77, HeadSHA: "abc", Status: "completed", Conclusion: "failure"})
	srv.SetWorkflowRun(fakegithub.WorkflowRun{ID: 78, HeadSHA: "abc", Status: "in_progress"})
	runs, err := c.ListCheckRuns(ctx, target, "abc")
	if err != nil || len(runs) != 1 || runs[0].CheckSuiteID != 77 {
		t.Fatalf("check runs = %+v, %v; want run 41 in check suite 77", runs, err)
	}

	app := newApp(t, srv)
	read := scopedClient(t, srv, app, target, ghclient.TokenPerms{Actions: ghclient.PermRead})
	got, err := read.ListWorkflowRunsForCheckSuite(ctx, target, 77)
	if err != nil || len(got) != 1 || got[0] != (ghclient.WorkflowRun{ID: 77, HeadSHA: "abc", Status: "completed",
		Conclusion: "failure"}) {
		t.Fatalf("workflow runs of suite 77 = %+v, %v", got, err)
	}
	if err := read.RerunFailedJobs(ctx, target, 77); !ghclient.IsForbidden(err) {
		t.Errorf("actions read re-ran a run: %v", err)
	}
	write := scopedClient(t, srv, app, target, ghclient.TokenPerms{Actions: ghclient.PermWrite})
	if err := write.RerunFailedJobs(ctx, target, 77); err != nil {
		t.Fatalf("actions write: %v", err)
	}
	if err := write.RerunFailedJobs(ctx, target, 78); !ghclient.IsForbidden(err) {
		t.Errorf("a running run was re-run: %v", err)
	}
	if err := write.RerunFailedJobs(ctx, target, 79); !ghclient.IsNotFound(err) {
		t.Errorf("an unknown run was re-run: %v", err)
	}
	if got := srv.Reruns(); len(got) != 1 || got[0] != 77 {
		t.Errorf("re-runs = %v, want 77", got)
	}
	if again, _ := read.ListWorkflowRunsForCheckSuite(ctx, target, 77); len(again) != 1 ||
		again[0].Status != "queued" || again[0].Conclusion != "" {
		t.Errorf("re-run run = %+v, want queued again", again)
	}
	other := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Actions: ghclient.PermWrite})
	if err := other.RerunFailedJobs(ctx, target, 77); !ghclient.IsForbidden(err) {
		t.Errorf("another repository's token re-ran a run: %v", err)
	}
}

func TestPendingReviewSubmissionMovesRESTTimestampWithoutEditing(t *testing.T) {
	srv, c, clk := newFake(t)
	ctx := context.Background()
	srv.OpenPull(8, "patchy-intent/target-3")
	reviewID := srv.AddReview(8, human, "PENDING", "", "abc")
	srv.AddReviewComment(8, human, reviewID, "main.go", 8, "RIGHT", "Please fix this line.", "@@ hunk")
	pending, err := c.ListPullRequestReviews(ctx, target, 8)
	if err != nil || len(pending) != 1 || pending[0].State != "PENDING" || !pending[0].SubmittedAt.IsZero() {
		t.Fatalf("pending review = %+v, %v", pending, err)
	}
	before, err := c.ListPullRequestReviewComments(ctx, target, 8)
	if err != nil || len(before) != 1 {
		t.Fatalf("pending inline comments = %+v, %v", before, err)
	}
	clk.advance(6 * time.Second)
	if !srv.SubmitReview(reviewID, "CHANGES_REQUESTED") {
		t.Fatal("pending review not submitted")
	}
	reviews, err := c.ListPullRequestReviews(ctx, target, 8)
	if err != nil || len(reviews) != 1 || reviews[0].State != "CHANGES_REQUESTED" || reviews[0].SubmittedAt.IsZero() {
		t.Fatalf("submitted review = %+v, %v", reviews, err)
	}
	after, err := c.ListPullRequestReviewComments(ctx, target, 8)
	if err != nil || len(after) != 1 || !after[0].UpdatedAt.After(after[0].CreatedAt) {
		t.Fatalf("submitted inline comment = %+v, %v; want updated_at after created_at", after, err)
	}
	if edited, err := c.ReviewCommentEdited(ctx, after[0].NodeID); err != nil || edited {
		t.Fatalf("GraphQL says edited = %t, %v; want unedited", edited, err)
	}
}

func TestReviewEditGraphQLRepositoryScope(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	pr, err := c.CreatePR(ctx, target, ghclient.PRRequest{Title: "test", Head: "patchy-intent/target-2", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	srv.AddReview(pr.Number, human, "CHANGES_REQUESTED", "please fix", "abc")
	reviews, err := c.ListPullRequestReviews(ctx, target, pr.Number)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
	app := newApp(t, srv)
	allowed := scopedClient(t, srv, app, target, ghclient.TokenPerms{PullRequests: ghclient.PermRead})
	if _, err := allowed.ReviewEdited(ctx, reviews[0].NodeID); err != nil {
		t.Fatalf("target token: %v", err)
	}
	other := scopedClient(t, srv, app, intents, ghclient.TokenPerms{PullRequests: ghclient.PermRead})
	if _, err := other.ReviewEdited(ctx, reviews[0].NodeID); err == nil {
		t.Fatal("other repository token read the review")
	}
}
