// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// TestListIssuesConditional walks the conditional-request path: an
// unconditional first poll returns the ETag, a repeat with that tag is a
// 304 (NotModified, no issues), and a changed listing is a 200 with a new
// tag.
func TestListIssuesConditional(t *testing.T) {
	mux, c := newFakeClient(t)
	current := `W/"v1"`
	payload := `[{"number":1,"title":"Add GET /version","state":"open",
		"html_url":"https://github.com/o/r/issues/1","user":{"login":"octocat"},
		"labels":[{"name":"patchy:target"}],"created_at":"2026-09-24T10:00:00Z"}]`
	var sent []string
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for key, want := range map[string]string{
			"labels": "patchy:target", "state": "open", "sort": "updated", "direction": "desc", "per_page": "100",
		} {
			if got := q.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		inm := r.Header.Get("If-None-Match")
		sent = append(sent, inm)
		w.Header().Set("ETag", current)
		if inm == current {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(t, w, payload)
	})

	ctx := context.Background()
	first, err := c.ListIssues(ctx, testRepo, []string{"patchy:target"}, "open", "")
	if err != nil {
		t.Fatalf("first ListIssues() error = %v", err)
	}
	want := []*Issue{{
		Repo: testRepo, Number: 1, Title: "Add GET /version", State: "open",
		Labels: []string{"patchy:target"}, Author: "octocat",
		CreatedAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		HTMLURL:   "https://github.com/o/r/issues/1",
	}}
	if first.NotModified || first.ETag != `W/"v1"` || !reflect.DeepEqual(first.Issues, want) {
		t.Errorf("first ListIssues() = %+v, want the issues and ETag W/\"v1\"", first)
	}

	again, err := c.ListIssues(ctx, testRepo, []string{"patchy:target"}, "open", first.ETag)
	if err != nil {
		t.Fatalf("conditional ListIssues() error = %v, want the 304 as success", err)
	}
	if !again.NotModified || again.Issues != nil || again.ETag != `W/"v1"` {
		t.Errorf("conditional ListIssues() = %+v, want NotModified with the same tag and no issues", again)
	}

	current = `W/"v2"`
	changed, err := c.ListIssues(ctx, testRepo, []string{"patchy:target"}, "open", again.ETag)
	if err != nil {
		t.Fatalf("changed ListIssues() error = %v", err)
	}
	if changed.NotModified || changed.ETag != `W/"v2"` || len(changed.Issues) != 1 {
		t.Errorf("changed ListIssues() = %+v, want a fresh listing tagged W/\"v2\"", changed)
	}

	wantSent := []string{"", `W/"v1"`, `W/"v1"`}
	if !reflect.DeepEqual(sent, wantSent) {
		t.Errorf("If-None-Match sent = %q, want %q", sent, wantSent)
	}
}

// TestListIssuesPaginates: only the first page is conditional; later pages
// are walked unconditionally, pull requests are skipped, and the ETag is the
// first page's.
func TestListIssuesPaginates(t *testing.T) {
	mux, c := newFakeClient(t)
	page1 := `[{"number":1,"title":"one","state":"open"},
		{"number":2,"title":"a pr","state":"open","pull_request":{"url":"x"}}]`
	page2 := `[{"number":3,"title":"three","state":"closed"}]`
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		inm := r.Header.Get("If-None-Match")
		if r.URL.Query().Get("page") == "2" {
			if inm != "" {
				t.Errorf("page 2 If-None-Match = %q, want none", inm)
			}
			w.Header().Set("ETag", `W/"page2"`)
			writeJSON(t, w, page2)
			return
		}
		if inm != `W/"stale"` {
			t.Errorf("page 1 If-None-Match = %q, want W/\"stale\"", inm)
		}
		w.Header().Set("ETag", `W/"page1"`)
		w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
		writeJSON(t, w, page1)
	})

	got, err := c.ListIssues(context.Background(), testRepo, nil, "all", `W/"stale"`)
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if got.ETag != `W/"page1"` {
		t.Errorf("ETag = %q, want the first page's", got.ETag)
	}
	numbers := make([]int, 0, len(got.Issues))
	for _, is := range got.Issues {
		numbers = append(numbers, is.Number)
		if is.Repo != testRepo {
			t.Errorf("issue %d Repo = %v, want %v", is.Number, is.Repo, testRepo)
		}
	}
	if !reflect.DeepEqual(numbers, []int{1, 3}) {
		t.Errorf("issues = %v, want [1 3] (PR skipped, both pages walked)", numbers)
	}
}

func TestListIssuesError(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	got, err := c.ListIssues(context.Background(), testRepo, []string{"patchy:target"}, "open", `W/"v1"`)
	if !IsNotFound(err) {
		t.Errorf("ListIssues() error = %v, want the not-found API error", err)
	}
	if got != nil {
		t.Errorf("ListIssues() = %+v on error, want nil", got)
	}
}
