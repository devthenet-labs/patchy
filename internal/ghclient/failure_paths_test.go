// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// newFailingServer answers every request — REST under /api/v3 and GraphQL at
// /api/graphql alike — with a 500, counting the requests it saw.
func newFailingServer(t *testing.T) (base string, hits *atomic.Int64) {
	t.Helper()
	hits = new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"server exploded"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// TestClientSurfacesServerErrors: every Client call that reaches GitHub turns
// a 5xx answer into an error naming the operation, never a zero result read
// as success, and never one of the 404 "already gone" idempotency outs.
func TestClientSurfacesServerErrors(t *testing.T) {
	base, _ := newFailingServer(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	ctx := context.Background()
	yieldAll := func(Repo, *Alert) bool { return true }
	calls := []struct {
		name    string
		wantMsg string
		call    func() error
	}{
		{"GetAlert", "ghclient: get alert", func() error { _, err := c.GetAlert(ctx, testRepo, 1); return err }},
		{"DismissAlert", "ghclient: dismiss alert", func() error {
			return c.DismissAlert(ctx, testRepo, 1, "false positive", "c")
		}},
		{"Tarball", "ghclient: archive link for o/r@main", func() error {
			_, err := c.Tarball(ctx, testRepo, "main")
			return err
		}},
		{"ListCheckRuns", "ghclient: list check runs on o/r@abc", func() error {
			_, err := c.ListCheckRuns(ctx, testRepo, "abc")
			return err
		}},
		{"ListCheckAnnotations", "ghclient: list annotations on o/r check 4", func() error {
			_, err := c.ListCheckAnnotations(ctx, testRepo, 4, 10)
			return err
		}},
		{"ListCommitStatuses", "ghclient: list statuses on o/r@abc", func() error {
			_, err := c.ListCommitStatuses(ctx, testRepo, "abc")
			return err
		}},
		{"ListWorkflowJobs", "ghclient: list jobs for o/r Actions run 9", func() error {
			_, err := c.ListWorkflowJobs(ctx, testRepo, 9)
			return err
		}},
		{"ListWorkflowRunsForCheckSuite", "ghclient: list Actions runs of o/r check suite 6", func() error {
			_, err := c.ListWorkflowRunsForCheckSuite(ctx, testRepo, 6)
			return err
		}},
		{"GetJobLogTail", "ghclient: get job log URL for o/r job 8", func() error {
			_, err := c.GetJobLogTail(ctx, testRepo, 8, 100)
			return err
		}},
		{"ListIssueComments", "ghclient: ", func() error { _, err := c.ListComments(ctx, testRepo, 3); return err }},
		{"GetIssueComment", "ghclient: ", func() error { _, err := c.GetIssueComment(ctx, testRepo, 5); return err }},
		{"CreateIssueComment", "ghclient: ", func() error {
			_, err := c.CreateIssueComment(ctx, testRepo, 3, "b")
			return err
		}},
		{"CommentEdited", "ghclient: ", func() error { _, err := c.CommentEdited(ctx, "IC_1"); return err }},
		{"ReviewEdited", "ghclient: PullRequestReview edited PRR_1", func() error {
			_, err := c.ReviewEdited(ctx, "PRR_1")
			return err
		}},
		{"DeleteIssue", "ghclient: get issue o/r#3", func() error { return c.DeleteIssue(ctx, testRepo, 3) }},
		{"GetIssue", "ghclient: get issue o/r#3", func() error { _, err := c.GetIssue(ctx, testRepo, 3); return err }},
		{"ListOpen", "ghclient: list open issues in o/r", func() error {
			_, err := c.ListOpen(ctx, testRepo, nil)
			return err
		}},
		{"Create", "ghclient: create issue in o/r", func() error {
			_, err := c.Create(ctx, testRepo, IssueRequest{Title: "t"})
			return err
		}},
		{"CreateComment", "ghclient: comment on o/r#3", func() error {
			_, err := c.CreateComment(ctx, testRepo, 3, "b")
			return err
		}},
		{"EditComment", "ghclient: edit comment 11 on o/r", func() error {
			return c.EditComment(ctx, testRepo, 11, "b")
		}},
		{"EditBody", "ghclient: edit body of o/r#3", func() error { return c.EditBody(ctx, testRepo, 3, "b") }},
		{"AddLabels", "ghclient: add labels to o/r#3", func() error {
			return c.AddLabels(ctx, testRepo, 3, []string{"x"})
		}},
		{"RemoveLabel", `ghclient: remove label "x" from o/r#3`, func() error {
			return c.RemoveLabel(ctx, testRepo, 3, "x")
		}},
		{"Assign", "ghclient: assign o/r#3", func() error { return c.Assign(ctx, testRepo, 3, []string{"u"}) }},
		{"CloseIssue", "ghclient: close o/r#3", func() error {
			return c.CloseIssue(ctx, testRepo, 3, CloseNotPlanned)
		}},
		{"ListOrgAlerts", "ghclient: list o org alerts", func() error {
			_, err := c.ListOrgAlerts(ctx, "o", yieldAll)
			return err
		}},
		{"ListRepoAlerts", "ghclient: list o/r alerts", func() error {
			_, err := c.ListRepoAlerts(ctx, testRepo, func(*Alert) bool { return true })
			return err
		}},
		{"InstallationRepos", "ghclient: list installation repositories", func() error {
			_, _, err := c.InstallationRepos(ctx)
			return err
		}},
		{"FindPRByHead", "ghclient: find PR by head b in o/r", func() error {
			_, err := c.FindPRByHead(ctx, testRepo, "b")
			return err
		}},
		{"FindOpenPR", "ghclient: find the open PR from b into main in o/r", func() error {
			_, err := c.FindOpenPR(ctx, testRepo, "b", "main")
			return err
		}},
		{"CreateCommit", "ghclient: create blob a.txt in o/r", func() error {
			req := CommitRequest{BaseSHA: "base", Files: []CommitFile{{Path: "a.txt", Mode: "100644"}}}
			_, err := c.CreateCommit(ctx, testRepo, req)
			return err
		}},
		{"CreateCommit tree", "ghclient: create tree in o/r", func() error {
			_, err := c.CreateCommit(ctx, testRepo, CommitRequest{BaseSHA: "base", Deletes: []string{"gone.txt"}})
			return err
		}},
		{"PushBranch", "ghclient: create tree in o/r", func() error {
			_, err := c.PushBranch(ctx, testRepo, BranchPush{Branch: "b", CommitRequest: CommitRequest{BaseSHA: "base"}})
			return err
		}},
		{"RateRemaining", "ghclient: rate limit", func() error { _, err := c.RateRemaining(ctx); return err }},
		{"DefaultBranch", "ghclient: get o/r", func() error { _, err := c.DefaultBranch(ctx, testRepo); return err }},
		{"HeadSHA", "ghclient: resolve main head of o/r", func() error {
			_, err := c.HeadSHA(ctx, testRepo, "main")
			return err
		}},
		{"CreatePR", "ghclient: create PR in o/r", func() error {
			_, err := c.CreatePR(ctx, testRepo, PRRequest{Title: "t"})
			return err
		}},
		{"SearchIssues", `ghclient: search issues "q"`, func() error {
			_, err := c.SearchIssues(ctx, "q")
			return err
		}},
		{"ListPullRequestReviews", "ghclient: list reviews on o/r#3", func() error {
			_, err := c.ListPullRequestReviews(ctx, testRepo, 3)
			return err
		}},
		{"ListPullRequestReviewComments", "ghclient: list review comments on o/r#3", func() error {
			_, err := c.ListPullRequestReviewComments(ctx, testRepo, 3)
			return err
		}},
		{"ComparePatch", "ghclient: compare patch o/r main...abc", func() error {
			_, err := c.ComparePatch(ctx, testRepo, "main", "abc")
			return err
		}},
		{"RequestReviewers", "ghclient: request reviewers on o/r#3", func() error {
			return c.RequestReviewers(ctx, testRepo, 3, []string{"u"})
		}},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("error = nil, want the 500 surfaced")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
			}
			if IsNotFound(err) || IsRefused(err) {
				t.Errorf("a 500 classified as not-found/refused: %v", err)
			}
		})
	}
}

// TestAppSurfacesServerErrors: the App-authenticated calls surface a 5xx the
// same way, and a failed installation lookup is not cached.
func TestAppSurfacesServerErrors(t *testing.T) {
	base, hits := newFailingServer(t)
	app, err := NewApp(AppConfig{AppID: 7, PrivateKey: testPrivateKeyPEM(t), BaseURL: base})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	ctx := context.Background()
	calls := []struct {
		name, wantMsg string
		call          func() error
	}{
		{"Installation", "ghclient: resolve installation for o/r", func() error {
			_, err := app.Installation(ctx, testRepo)
			return err
		}},
		{"Installations", "ghclient: list installations", func() error {
			_, err := app.Installations(ctx)
			return err
		}},
		{"InstallationAccounts", "ghclient: list installations", func() error {
			_, err := app.InstallationAccounts(ctx)
			return err
		}},
		{"ScopedToken", "ghclient: resolve installation for o/r", func() error {
			_, _, err := app.ScopedToken(ctx, testRepo, TokenPerms{Contents: PermRead})
			return err
		}},
		{"Deliveries", "ghclient: list hook deliveries", func() error {
			_, err := app.Deliveries(ctx, func(Delivery) bool { return true })
			return err
		}},
		{"Redeliver", "ghclient: ", func() error { return app.Redeliver(ctx, 99) }},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want containing %q", err, tt.wantMsg)
			}
		})
	}
	before := hits.Load()
	if _, err := app.Installation(ctx, testRepo); err == nil {
		t.Fatal("second Installation error = nil")
	}
	if hits.Load() == before {
		t.Error("a failed installation lookup was served from cache")
	}
}

// TestScopedTokenMintFailure: the installation resolves but minting the token
// fails; the error names the repository.
func TestScopedTokenMintFailure(t *testing.T) {
	mux, app := newFakeApp(t)
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"id":42}`)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusUnprocessableEntity)
	})
	tok, _, err := app.ScopedToken(context.Background(), testRepo, TokenPerms{Issues: PermWrite})
	if err == nil || !strings.Contains(err.Error(), "ghclient: scoped token for o/r") || !IsUnprocessable(err) {
		t.Errorf("ScopedToken = (%q, %v), want a 422 scoped-token error", tok, err)
	}
}

// TestEmptyInputsSendNothing: an empty label, assignee or reviewer list is a
// no-op, never a request that GitHub might read as "clear".
func TestEmptyInputsSendNothing(t *testing.T) {
	base, hits := newFailingServer(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"AddLabels":        func() error { return c.AddLabels(ctx, testRepo, 3, nil) },
		"Assign":           func() error { return c.Assign(ctx, testRepo, 3, []string{}) },
		"RequestReviewers": func() error { return c.RequestReviewers(ctx, testRepo, 3, nil) },
	} {
		if err := call(); err != nil {
			t.Errorf("%s(empty) error = %v, want nil", name, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("empty inputs sent %d requests, want 0", n)
	}
}

func TestArgumentValidationSendsNothing(t *testing.T) {
	base, hits := newFailingServer(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, limit := range []int{0, -1, 101} {
		if _, err := c.ListCheckAnnotations(ctx, testRepo, 1, limit); err == nil ||
			!strings.Contains(err.Error(), "annotation limit") {
			t.Errorf("ListCheckAnnotations(limit %d) error = %v, want a limit error", limit, err)
		}
	}
	for _, tail := range []int{0, -5, 32<<10 + 1} {
		if _, err := c.GetJobLogTail(ctx, testRepo, 1, tail); err == nil ||
			!strings.Contains(err.Error(), "must be 1..32768") {
			t.Errorf("GetJobLogTail(tail %d) error = %v, want a bound error", tail, err)
		}
	}
	if _, err := c.ReviewCommentEdited(ctx, "  "); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("ReviewCommentEdited(blank) error = %v, want ErrNodeNotFound", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("invalid arguments sent %d requests, want 0", n)
	}
}

// endlessPages answers every page with one element and a rel="next" link to
// the following page, forever.
func endlessPages(t *testing.T, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Link", `<`+r.URL.Path+`?page=`+strconv.Itoa(max(page, 1)+1)+`>; rel="next"`)
		writeJSON(t, w, body)
	}
}

// TestListingsStopAtThePageCap: a listing that never ends errors out after
// walkPageCap pages rather than walking forever or returning a silently
// truncated result.
func TestListingsStopAtThePageCap(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, pattern, body, wantMsg string
		call                         func(c *Client) error
	}{
		{"ListCheckRuns", "GET /repos/o/r/commits/abc/check-runs", `{"total_count":1,"check_runs":[null,{"id":1}]}`,
			"check runs on o/r@abc run past 50 pages",
			func(c *Client) error { _, err := c.ListCheckRuns(ctx, testRepo, "abc"); return err }},
		{"ListCommitStatuses", "GET /repos/o/r/commits/abc/statuses", `[null,{"id":1}]`,
			"statuses on o/r@abc run past 50 pages",
			func(c *Client) error { _, err := c.ListCommitStatuses(ctx, testRepo, "abc"); return err }},
		{"ListWorkflowJobs", "GET /repos/o/r/actions/runs/7/jobs", `{"total_count":1,"jobs":[null,{"id":1}]}`,
			"jobs for o/r Actions run 7 run past 50 pages",
			func(c *Client) error { _, err := c.ListWorkflowJobs(ctx, testRepo, 7); return err }},
		{"ListWorkflowRunsForCheckSuite", "GET /repos/o/r/actions/runs", `{"total_count":1,"workflow_runs":[null,{"id":1}]}`,
			"Actions runs of o/r check suite 6 run past 50 pages",
			func(c *Client) error { _, err := c.ListWorkflowRunsForCheckSuite(ctx, testRepo, 6); return err }},
		{"ListPullRequestReviews", "GET /repos/o/r/pulls/3/reviews", `[null,{"id":1}]`,
			"reviews on o/r#3 run past 50 pages",
			func(c *Client) error { _, err := c.ListPullRequestReviews(ctx, testRepo, 3); return err }},
		{"ListPullRequestReviewComments", "GET /repos/o/r/pulls/3/comments", `[null,{"id":1}]`,
			"review comments on o/r#3 run past 50 pages",
			func(c *Client) error { _, err := c.ListPullRequestReviewComments(ctx, testRepo, 3); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			var pages atomic.Int64
			h := endlessPages(t, tt.body)
			mux.HandleFunc(tt.pattern, func(w http.ResponseWriter, r *http.Request) { pages.Add(1); h(w, r) })
			err := tt.call(c)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %v, want %q", err, tt.wantMsg)
			}
			if n := pages.Load(); n != walkPageCap {
				t.Errorf("walked %d pages, want %d", n, walkPageCap)
			}
		})
	}
}

// TestInstallationReposPageCap: the inventory walk stops at repoPageCap and
// reports itself incomplete, keeping what it read.
func TestInstallationReposPageCap(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /installation/repositories",
		endlessPages(t, `{"total_count":1,"repositories":[{"name":"r","owner":{"login":"o"}}]}`))
	repos, complete, err := c.InstallationRepos(context.Background())
	if err != nil {
		t.Fatalf("InstallationRepos: %v", err)
	}
	if complete {
		t.Error("complete = true, want false at the page cap")
	}
	if len(repos) != repoPageCap || repos[0] != testRepo {
		t.Errorf("got %d repos (first %v), want %d of o/r", len(repos), repos[0], repoPageCap)
	}
}

func TestTarball(t *testing.T) {
	mux, base := newFake(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("GET /repos/o/r/tarball/abc123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", base+"/api/v3/_archive/abc123.tar.gz")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("GET /_archive/abc123.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		wantAuth(t, r, "Bearer pat-token")
		_, _ = io.WriteString(w, "tarball-bytes")
	})
	mux.HandleFunc("GET /repos/o/r/tarball/gone", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", base+"/api/v3/_archive/gone.tar.gz")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("GET /_archive/gone.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	})

	rc, err := c.Tarball(context.Background(), testRepo, "abc123")
	if err != nil {
		t.Fatalf("Tarball: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "tarball-bytes" {
		t.Errorf("Tarball body = %q, %v", body, err)
	}

	if _, err := c.Tarball(context.Background(), testRepo, "gone"); err == nil ||
		!strings.Contains(err.Error(), "download archive for o/r@gone: status 410") {
		t.Errorf("Tarball(gone) error = %v, want a status 410 error", err)
	}
}

func TestEditComment(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", func(w http.ResponseWriter, r *http.Request) {
		if body := decodeBody[map[string]any](t, r); body["body"] != "edited" {
			t.Errorf("edit body = %v, want edited", body)
		}
		writeJSON(t, w, `{"id":11}`)
	})
	if err := c.EditComment(context.Background(), testRepo, 11, "edited"); err != nil {
		t.Errorf("EditComment: %v", err)
	}
}

func TestGetIssueMapsFields(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues/9", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"number":9,"title":"T","body":"B","state":"open","html_url":"https://x/9",
			"user":{"login":"alice"},"labels":[{"name":"l1"},{"name":"l2"}],"assignees":[{"login":"bob"}]}`)
	})
	is, err := c.GetIssue(context.Background(), testRepo, 9)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if is.Repo != testRepo || is.Number != 9 || is.Title != "T" || is.Body != "B" || is.State != "open" ||
		is.Author != "alice" || is.HTMLURL != "https://x/9" ||
		strings.Join(is.Labels, ",") != "l1,l2" || strings.Join(is.Assignees, ",") != "bob" {
		t.Errorf("GetIssue = %+v", is)
	}
}

func TestFindPRByHead(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "o:patchy/fix" {
			t.Errorf("head filter = %q, want o:patchy/fix", got)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state = %q, want open", got)
		}
		writeJSON(t, w, `[{"number":5,"html_url":"https://x/pull/5","head":{"sha":"h1"},"base":{"ref":"main"}}]`)
	})
	pr, err := c.FindPRByHead(context.Background(), testRepo, "patchy/fix")
	if err != nil || pr == nil || pr.Number != 5 || pr.HeadSHA != "h1" || pr.Base != "main" {
		t.Fatalf("FindPRByHead = (%+v, %v)", pr, err)
	}

	none, c2 := newFakeClient(t)
	none.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, `[]`) })
	if pr, err := c2.FindPRByHead(context.Background(), testRepo, "patchy/fix"); err != nil || pr != nil {
		t.Errorf("FindPRByHead(none) = (%+v, %v), want nil, nil", pr, err)
	}
}

func TestRateRemainingWithoutCore(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"resources":{}}`)
	})
	if n, err := c.RateRemaining(context.Background()); err == nil || n != 0 ||
		!strings.Contains(err.Error(), "no core limit") {
		t.Errorf("RateRemaining = (%d, %v), want a no-core-limit error", n, err)
	}
}

func TestRequestReviewers(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/pulls/3/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody[map[string][]string](t, r)
		if strings.Join(body["reviewers"], ",") != "ann,ben" {
			t.Errorf("reviewers = %v, want [ann ben]", body["reviewers"])
		}
		writeJSON(t, w, `{"number":3}`)
	})
	if err := c.RequestReviewers(context.Background(), testRepo, 3, []string{"ann", "ben"}); err != nil {
		t.Errorf("RequestReviewers: %v", err)
	}
}

// TestJobLogTailFailures covers the download side of a job log: no redirect
// URL, a URL whose scheme is not allowed, a failed download and a log over the
// download cap.
func TestJobLogTailFailures(t *testing.T) {
	tests := []struct {
		name     string
		location string
		status   int
		download http.HandlerFunc
		wantMsg  string
	}{
		{name: "unsafe scheme", location: "ftp://evil/log", status: http.StatusFound, wantMsg: "unsafe job log URL scheme"},
		{name: "plain http to another host", location: "http://elsewhere.invalid/log", status: http.StatusFound,
			wantMsg: "unsafe job log URL scheme"},
		{name: "download refused", location: "/api/v3/_logs/1", status: http.StatusFound,
			download: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			wantMsg:  "download job log for o/r job 1: HTTP 403"},
		{name: "download too large", location: "/api/v3/_logs/1", status: http.StatusFound,
			download: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, strings.Repeat("x", maxJobLogDownload+1))
			},
			wantMsg: "exceeds 8388608 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/actions/jobs/1/logs", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", tt.location)
				w.WriteHeader(tt.status)
			})
			if tt.download != nil {
				mux.HandleFunc("GET /_logs/1", tt.download)
			}
			got, err := c.GetJobLogTail(context.Background(), testRepo, 1, 10)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("GetJobLogTail = (%q, %v), want error containing %q", got, err, tt.wantMsg)
			}
		})
	}
}
