// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"strings"
	"testing"

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
