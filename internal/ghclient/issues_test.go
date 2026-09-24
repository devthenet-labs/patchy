// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

var testRepo = Repo{Owner: "o", Name: "r"}

func TestRepoString(t *testing.T) {
	if got := testRepo.String(); got != "o/r" {
		t.Errorf("Repo.String() = %q, want %q", got, "o/r")
	}
}

func TestListOpen(t *testing.T) {
	mux, c := newFakeClient(t)
	page1 := `[
		{"number":1,"title":"t1","body":"b1","state":"open",
		 "labels":[{"name":"security-finding: opened"},{"name":"bug"}],
		 "assignees":[{"login":"u1"}],
		 "created_at":"2026-01-02T03:04:05Z"},
		{"number":2,"title":"a pr","state":"open","pull_request":{"url":"x"}}
	]`
	page2 := `[{"number":3,"title":"t3","state":"open"}]`
	paged := pagedHandler(t, page1, page2)
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		wantHeader(t, r, "Authorization", "Bearer pat-token")
		q := r.URL.Query()
		if q.Get("state") != "open" {
			t.Errorf("state = %q, want %q", q.Get("state"), "open")
		}
		if q.Get("labels") != "security-finding: opened,bug" {
			t.Errorf("labels = %q, want the comma-joined filter", q.Get("labels"))
		}
		paged(w, r)
	})

	got, err := c.ListOpen(context.Background(), testRepo, []string{"security-finding: opened", "bug"})
	if err != nil {
		t.Fatalf("ListOpen() error = %v", err)
	}
	want := []*Issue{
		{
			Repo:   testRepo,
			Number: 1, Title: "t1", Body: "b1", State: "open",
			Labels:    []string{"security-finding: opened", "bug"},
			Assignees: []string{"u1"},
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{Repo: testRepo, Number: 3, Title: "t3", State: "open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListOpen() = %+v, want %+v (PRs skipped, both pages walked)", got, want)
	}
}

func TestCreate(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody[map[string]any](t, r)
		if body["title"] != "T" || body["body"] != "B" {
			t.Errorf("request title/body = %v/%v, want T/B", body["title"], body["body"])
		}
		if !reflect.DeepEqual(body["labels"], []any{"l1", "l2"}) {
			t.Errorf("request labels = %v, want [l1 l2]", body["labels"])
		}
		writeJSON(t, w, `{"number":5,"title":"T","body":"B","state":"open","labels":[{"name":"l1"},{"name":"l2"}]}`)
	})

	got, err := c.Create(context.Background(), testRepo, IssueRequest{Title: "T", Body: "B", Labels: []string{"l1", "l2"}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	want := &Issue{Number: 5, Title: "T", Body: "B", State: "open", Labels: []string{"l1", "l2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Create() = %+v, want %+v", got, want)
	}
}

// TestGetIssueAuthor: GetIssue carries the login that opened the issue — the
// identity the projection recognises its own comments by.
func TestGetIssueAuthor(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues/5", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"number":5,"title":"T","state":"open","user":{"login":"patchy[bot]"}}`)
	})

	got, err := c.GetIssue(context.Background(), testRepo, 5)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Author != "patchy[bot]" {
		t.Errorf("GetIssue().Author = %q, want patchy[bot]", got.Author)
	}
}

func TestListComments(t *testing.T) {
	mux, c := newFakeClient(t)
	page1 := `[{"id":11,"body":"first","user":{"login":"u1"},"author_association":"MEMBER"}]`
	page2 := `[{"id":12,"body":"second","user":{"login":"u2"},"author_association":"NONE"}]`
	mux.HandleFunc("GET /repos/o/r/issues/3/comments", pagedHandler(t, page1, page2))

	got, err := c.ListComments(context.Background(), testRepo, 3)
	if err != nil {
		t.Fatalf("ListComments() error = %v", err)
	}
	want := []*Comment{
		{ID: 11, Body: "first", UserLogin: "u1", AuthorAssociation: "MEMBER"},
		{ID: 12, Body: "second", UserLogin: "u2", AuthorAssociation: "NONE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListComments() = %+v, want %+v", got, want)
	}
}

// TestIssueWrites covers the write-only IssueStore methods: each case wires
// one endpoint that asserts the request and answers minimally.
func TestIssueWrites(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		pattern string
		handler func(t *testing.T, w http.ResponseWriter, r *http.Request)
		call    func(c *Client) error
	}{
		{
			name:    "Comment",
			pattern: "POST /repos/o/r/issues/3/comments",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if body := decodeBody[map[string]any](t, r); body["body"] != "hello" {
					t.Errorf("comment body = %v, want hello", body["body"])
				}
				writeJSON(t, w, `{"id":1}`)
			},
			call: func(c *Client) error { return c.Comment(ctx, testRepo, 3, "hello") },
		},
		{
			name:    "CreateComment returns the new id",
			pattern: "POST /repos/o/r/issues/3/comments",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if body := decodeBody[map[string]any](t, r); body["body"] != "hello" {
					t.Errorf("comment body = %v, want hello", body["body"])
				}
				writeJSON(t, w, `{"id":42}`)
			},
			call: func(c *Client) error {
				id, err := c.CreateComment(ctx, testRepo, 3, "hello")
				if err == nil && id != 42 {
					return fmt.Errorf("CreateComment id = %d, want 42", id)
				}
				return err
			},
		},
		{
			name:    "EditBody",
			pattern: "PATCH /repos/o/r/issues/3",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				if !reflect.DeepEqual(body, map[string]any{"body": "new body"}) {
					t.Errorf("edit request = %v, want only body=new body", body)
				}
				writeJSON(t, w, `{"number":3}`)
			},
			call: func(c *Client) error { return c.EditBody(ctx, testRepo, 3, "new body") },
		},
		{
			name:    "AddLabels",
			pattern: "POST /repos/o/r/issues/3/labels",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				if got := decodeBody[[]string](t, r); !reflect.DeepEqual(got, []string{"a", "b"}) {
					t.Errorf("labels body = %v, want [a b]", got)
				}
				writeJSON(t, w, `[{"name":"a"},{"name":"b"}]`)
			},
			call: func(c *Client) error { return c.AddLabels(ctx, testRepo, 3, []string{"a", "b"}) },
		},
		{
			name:    "RemoveLabel",
			pattern: "DELETE /repos/o/r/issues/3/labels/gone",
			handler: func(_ *testing.T, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			call: func(c *Client) error { return c.RemoveLabel(ctx, testRepo, 3, "gone") },
		},
		{
			name:    "RemoveLabel missing is not an error",
			pattern: "DELETE /repos/o/r/issues/3/labels/absent",
			handler: func(_ *testing.T, w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
			},
			call: func(c *Client) error { return c.RemoveLabel(ctx, testRepo, 3, "absent") },
		},
		{
			name:    "Assign",
			pattern: "POST /repos/o/r/issues/3/assignees",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				if !reflect.DeepEqual(body["assignees"], []any{"u1", "u2"}) {
					t.Errorf("assignees = %v, want [u1 u2]", body["assignees"])
				}
				writeJSON(t, w, `{"number":3}`)
			},
			call: func(c *Client) error { return c.Assign(ctx, testRepo, 3, []string{"u1", "u2"}) },
		},
		{
			name:    "Close",
			pattern: "PATCH /repos/o/r/issues/3",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				if !reflect.DeepEqual(body, map[string]any{"state": "closed"}) {
					t.Errorf("close request = %v, want only state=closed", body)
				}
				writeJSON(t, w, `{"number":3,"state":"closed"}`)
			},
			call: func(c *Client) error { return c.Close(ctx, testRepo, 3) },
		},
		{
			name:    "CloseIssue with a reason",
			pattern: "PATCH /repos/o/r/issues/3",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				want := map[string]any{"state": "closed", "state_reason": "completed"}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("close request = %v, want %v", body, want)
				}
				writeJSON(t, w, `{"number":3,"state":"closed","state_reason":"completed"}`)
			},
			call: func(c *Client) error { return c.CloseIssue(ctx, testRepo, 3, CloseCompleted) },
		},
		{
			name:    "CloseIssue without a reason sends none",
			pattern: "PATCH /repos/o/r/issues/3",
			handler: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				body := decodeBody[map[string]any](t, r)
				if !reflect.DeepEqual(body, map[string]any{"state": "closed"}) {
					t.Errorf("close request = %v, want only state=closed", body)
				}
				writeJSON(t, w, `{"number":3,"state":"closed"}`)
			},
			call: func(c *Client) error { return c.CloseIssue(ctx, testRepo, 3, "") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc(tt.pattern, func(w http.ResponseWriter, r *http.Request) { tt.handler(t, w, r) })
			if err := tt.call(c); err != nil {
				t.Errorf("%s error = %v, want nil", tt.name, err)
			}
		})
	}
}
