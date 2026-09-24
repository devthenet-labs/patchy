// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/go-github/v90/github"
)

func TestDefaultBranch(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		wantHeader(t, r, "Authorization", "Bearer pat-token")
		writeJSON(t, w, `{"name":"r","default_branch":"trunk"}`)
	})

	got, err := c.DefaultBranch(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("DefaultBranch() error = %v", err)
	}
	if got != "trunk" {
		t.Errorf("DefaultBranch() = %q, want %q", got, "trunk")
	}
}

func TestCompareStatus(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/compare/{spec}", func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.PathValue("spec"), "fa82fcd...45b1bec"; got != want {
			t.Errorf("compare spec = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("per_page"); got != "1" {
			t.Errorf("per_page = %q, want 1", got)
		}
		writeJSON(t, w, `{"status":"behind","ahead_by":0,"behind_by":1}`)
	})

	got, err := c.CompareStatus(context.Background(), testRepo, "fa82fcd", "45b1bec")
	if err != nil {
		t.Fatalf("CompareStatus() error = %v", err)
	}
	if got != "behind" {
		t.Errorf("CompareStatus() = %q, want behind", got)
	}
}

func TestCompareStatusError(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/compare/{spec}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	if _, err := c.CompareStatus(context.Background(), testRepo, "a", "b"); err == nil {
		t.Error("CompareStatus() error = nil, want the API error")
	}
}

func TestIsForbidden(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "a credential missing the permission", status: http.StatusForbidden, want: true},
		{name: "not found", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/compare/{spec}", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, tt.status)
			})
			_, err := c.CompareStatus(context.Background(), testRepo, "a", "b")
			if got := IsForbidden(err); got != tt.want {
				t.Errorf("IsForbidden(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}
	// go-github reports throttling as its own types, never as a refusal.
	throttled := &github.RateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	if IsForbidden(fmt.Errorf("compare: %w", throttled)) {
		t.Error("IsForbidden(rate limit) = true, want false")
	}
}

func TestCreatePR(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		want := map[string]any{
			"title": "fix: patch it",
			"head":  "patchy/fix-4",
			"base":  "main",
			"body":  "closes #3",
		}
		if body := decodeBody[map[string]any](t, r); !reflect.DeepEqual(body, want) {
			t.Errorf("create PR request = %v, want %v", body, want)
		}
		writeJSON(t, w, `{"number":12,"html_url":"https://gh/o/r/pull/12"}`)
	})

	got, err := c.CreatePR(context.Background(), testRepo, PRRequest{
		Title: "fix: patch it", Head: "patchy/fix-4", Base: "main", Body: "closes #3",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	want := &PR{Number: 12, HTMLURL: "https://gh/o/r/pull/12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CreatePR() = %+v, want %+v", got, want)
	}
}

func TestSearchIssues(t *testing.T) {
	mux, c := newFakeClient(t)
	const query = `label:"security-finding: opened" is:open`
	page1 := `{"total_count":2,"items":[
		{"number":1,"title":"t1","state":"open",
		 "repository_url":"https://api.github.com/repos/o/r"}]}`
	page2 := `{"total_count":2,"items":[
		{"number":2,"title":"t2","state":"open",
		 "repository_url":"https://api.github.com/repos/o2/r2"}]}`
	paged := pagedHandler(t, page1, page2)
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != query {
			t.Errorf("q = %q, want %q", got, query)
		}
		paged(w, r)
	})

	got, err := c.SearchIssues(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchIssues() error = %v", err)
	}
	want := []*Issue{
		{Repo: Repo{Owner: "o", Name: "r"}, Number: 1, Title: "t1", State: "open"},
		{Repo: Repo{Owner: "o2", Name: "r2"}, Number: 2, Title: "t2", State: "open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SearchIssues() = %+v, want %+v", got, want)
	}
}
