// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestPullRequestFeedbackAndPatch(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/pulls/7/reviews", pagedHandler(t,
		`[{"id":11,"node_id":"PRR_11","body":"please fix","state":"CHANGES_REQUESTED","commit_id":"abc",`+
			`"submitted_at":"2026-09-24T10:00:00Z","user":{"login":"alice","id":4,"type":"User"}}]`,
		`[{"id":12,"body":"looks good","state":"APPROVED","user":{"login":"bob","id":5,"type":"User"}}]`))
	mux.HandleFunc("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `[{"id":31,"node_id":"PRRC_31","pull_request_review_id":11,"body":"line issue","path":"main.go",`+
			`"line":8,"side":"RIGHT","diff_hunk":"@@ -1 +1 @@\n-old\n+new",`+
			`"created_at":"2026-09-24T10:01:00Z","user":{"login":"alice","id":4,"type":"User"}}]`)
	})
	mux.HandleFunc("GET /repos/o/r/compare/main...abc", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "patch") {
			t.Errorf("Accept = %q, want patch", r.Header.Get("Accept"))
		}
		_, _ = io.WriteString(w, "From abc\n--- a/main.go\n+++ b/main.go\n")
	})
	mux.HandleFunc("POST /repos/o/r/pulls/7/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody[map[string][]string](t, r)
		if got := body["reviewers"]; len(got) != 1 || got[0] != "alice" {
			t.Errorf("reviewers = %v", got)
		}
		writeJSON(t, w, `{"number":7}`)
	})
	ctx := context.Background()
	reviews, err := c.ListPullRequestReviews(ctx, testRepo, 7)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
	if got, want := []any{reviews[0].ID, reviews[0].NodeID, reviews[0].Author.Login,
		reviews[0].State, reviews[0].CommitSHA, reviews[1].ID},
		[]any{int64(11), "PRR_11", "alice", "CHANGES_REQUESTED", "abc", int64(12)}; !reflect.DeepEqual(got, want) {
		t.Errorf("review fields = %v, want %v", got, want)
	}
	comments, err := c.ListPullRequestReviewComments(ctx, testRepo, 7)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	if got, want := []any{comments[0].NodeID, comments[0].ReviewID, comments[0].Path,
		comments[0].Line, comments[0].Side, comments[0].Author.Login},
		[]any{"PRRC_31", int64(11), "main.go", 8, "RIGHT", "alice"}; !reflect.DeepEqual(got, want) {
		t.Errorf("review comment fields = %v, want %v", got, want)
	}
	patch, err := c.ComparePatch(ctx, testRepo, "main", "abc")
	if err != nil || !strings.Contains(patch, "+++ b/main.go") {
		t.Fatalf("patch = %q, %v", patch, err)
	}
	if err := c.RequestReviewers(ctx, testRepo, 7, []string{"alice"}); err != nil {
		t.Fatalf("RequestReviewers() = %v", err)
	}
}

func TestHeadChecksAndJobLogs(t *testing.T) {
	mux, c := newFakeClient(t)
	if c.logHTTP.Timeout == 0 {
		t.Fatal("job-log download has no deadline")
	}
	mux.HandleFunc("GET /repos/o/r/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"total_count":1,"check_runs":[{"id":41,"name":"test","head_sha":"abc",`+
			`"status":"completed","conclusion":"failure","details_url":"https://github.com/o/r/actions/runs/77/job/88",`+
			`"app":{"slug":"github-actions"},"output":{"title":"test failed","summary":"one failure","text":"details"}}]}`)
	})
	mux.HandleFunc("GET /repos/o/r/check-runs/41/annotations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `[{"path":"main.go","start_line":9,"end_line":9,"message":"bad value"}]`)
	})
	mux.HandleFunc("GET /repos/o/r/commits/abc/statuses", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `[{"id":51,"context":"lint","state":"error","description":"failed"}]`)
	})
	mux.HandleFunc("GET /repos/o/r/actions/runs/77/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"total_count":1,"jobs":[{"id":88,"name":"test","head_sha":"abc",`+
			`"check_run_url":"https://api.github.com/repos/o/r/check-runs/41","conclusion":"failure"}]}`)
	})
	mux.HandleFunc("GET /repos/o/r/actions/jobs/88/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/api/v3/_logs/88")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("GET /_logs/88", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential was forwarded to the log download")
		}
		_, _ = io.WriteString(w, strings.Repeat("a", 40)+"failure tail")
	})
	ctx := context.Background()
	runs, err := c.ListCheckRuns(ctx, testRepo, "abc")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if got, want := []any{runs[0].ID, runs[0].HeadSHA, runs[0].AppSlug, runs[0].Output.Title},
		[]any{int64(41), "abc", "github-actions", "test failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("check run fields = %v, want %v", got, want)
	}
	annotations, err := c.ListCheckAnnotations(ctx, testRepo, 41, 50)
	if err != nil || len(annotations) != 1 {
		t.Fatalf("annotations = %+v, %v", annotations, err)
	}
	if got, want := annotations[0], (CheckAnnotation{Path: "main.go", Line: 9, Message: "bad value"}); got != want {
		t.Errorf("annotation = %+v, want %+v", got, want)
	}
	statuses, err := c.ListCommitStatuses(ctx, testRepo, "abc")
	if err != nil || len(statuses) != 1 {
		t.Fatalf("statuses = %+v, %v", statuses, err)
	}
	wantStatus := CommitStatus{ID: 51, Context: "lint", State: "error", Description: "failed"}
	if got, want := statuses[0], wantStatus; got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	jobs, err := c.ListWorkflowJobs(ctx, testRepo, 77)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	wantJob := WorkflowJob{ID: 88, CheckRunID: 41, HeadSHA: "abc", Name: "test", Conclusion: "failure"}
	if got, want := jobs[0], wantJob; got != want {
		t.Errorf("job = %+v, want %+v", got, want)
	}
	log, err := c.GetJobLogTail(ctx, testRepo, 88, 12)
	if err != nil || log != "failure tail" {
		t.Fatalf("log tail = %q, %v", log, err)
	}
}

func TestComparePatchStatesTruncation(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/compare/main...abc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
	})
	patch, err := c.ComparePatch(context.Background(), testRepo, "main", "abc")
	if err != nil || len(patch) > 48<<10 || !strings.Contains(patch, "truncated") {
		t.Fatalf("ComparePatch() = %d bytes, %v; want bounded patch with truncation notice", len(patch), err)
	}
}
