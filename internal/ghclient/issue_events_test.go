// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestListIssueEvents decodes the verified event shape across two pages: a
// human's label, the App's own label removal (performed_via_github_app is
// null on label events, so only the actor identifies it), and a close with
// no label.
func TestListIssueEvents(t *testing.T) {
	mux, c := newFakeClient(t)
	page1 := `[{"id":101,"node_id":"LE_kwDOA","event":"labeled","created_at":"2026-09-24T10:00:00Z",
		"actor":{"login":"octocat","id":583231,"type":"User"},
		"label":{"name":"patchy:approved","color":"0e8a16"},"performed_via_github_app":null}]`
	page2 := `[{"id":102,"node_id":"UNLE_kwDOB","event":"unlabeled","created_at":"2026-09-24T10:01:00Z",
		"actor":{"login":"patchy-devthenet[bot]","id":332617407,"type":"Bot"},
		"label":{"name":"patchy:approved"},"performed_via_github_app":null},
		{"id":103,"node_id":"CE_kwDOC","event":"closed","created_at":"2026-09-24T10:02:00Z",
		"actor":{"login":"patchy-devthenet[bot]","id":332617407,"type":"Bot"},
		"performed_via_github_app":{"slug":"patchy-devthenet"}},
		{"id":104,"node_id":"RE_kwDOD","event":"reopened","created_at":"2026-09-24T10:03:00Z","actor":null}]`
	paged := pagedHandler(t, page1, page2)
	mux.HandleFunc("GET /repos/o/r/issues/7/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		paged(w, r)
	})

	got, err := c.ListIssueEvents(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("ListIssueEvents() error = %v", err)
	}
	bot := Actor{Login: "patchy-devthenet[bot]", ID: 332617407, Type: "Bot"}
	want := []*IssueEvent{
		{
			ID: 101, NodeID: "LE_kwDOA", Event: "labeled", Label: "patchy:approved",
			CreatedAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
			Actor:     Actor{Login: "octocat", ID: 583231, Type: "User"},
		},
		{
			ID: 102, NodeID: "UNLE_kwDOB", Event: "unlabeled", Label: "patchy:approved",
			CreatedAt: time.Date(2026, 9, 24, 10, 1, 0, 0, time.UTC), Actor: bot,
		},
		{
			ID: 103, NodeID: "CE_kwDOC", Event: "closed", ViaApp: "patchy-devthenet",
			CreatedAt: time.Date(2026, 9, 24, 10, 2, 0, 0, time.UTC), Actor: bot,
		},
		{ID: 104, NodeID: "RE_kwDOD", Event: "reopened", CreatedAt: time.Date(2026, 9, 24, 10, 3, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListIssueEvents() =\n%+v\nwant\n%+v", derefAll(got), derefAll(want))
	}
	if !got[1].Actor.IsBot() || got[0].Actor.IsBot() {
		t.Error("IsBot: want the App's event a bot's and the human's not")
	}
}

func TestListIssueEventsError(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues/7/events", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	if _, err := c.ListIssueEvents(context.Background(), testRepo, 7); !IsNotFound(err) {
		t.Errorf("ListIssueEvents() error = %v, want the not-found API error", err)
	}
}

// TestWalkPageCap: a listing that never stops advertising a next page is an
// error, not an unbounded walk or a silently truncated history.
func TestWalkPageCap(t *testing.T) {
	mux, c := newFakeClient(t)
	var pages int
	mux.HandleFunc("GET /repos/o/r/issues/7/events", func(w http.ResponseWriter, r *http.Request) {
		pages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Link", `<`+r.URL.Path+`?page=`+strconv.Itoa(page+1)+`>; rel="next"`)
		writeJSON(t, w, `[{"id":1,"event":"labeled"}]`)
	})
	if _, err := c.ListIssueEvents(context.Background(), testRepo, 7); err == nil {
		t.Fatal("ListIssueEvents() error = nil, want the page-cap error")
	}
	if pages != walkPageCap {
		t.Errorf("fetched %d pages, want exactly the cap %d", pages, walkPageCap)
	}
}

func TestActorIsBot(t *testing.T) {
	tests := []struct {
		actor Actor
		want  bool
	}{
		{Actor{Login: "patchy-devthenet[bot]", ID: 332617407, Type: "Bot"}, true},
		{Actor{Login: "octocat", ID: 583231, Type: "User"}, false},
		// A human login dressed as a bot is still a user.
		{Actor{Login: "mallory[bot]", Type: "User"}, false},
		{Actor{}, false},
	}
	for _, tt := range tests {
		if got := tt.actor.IsBot(); got != tt.want {
			t.Errorf("%+v.IsBot() = %v, want %v", tt.actor, got, tt.want)
		}
	}
}

// derefAll renders pointers' values for a readable failure message.
func derefAll[T any](ps []*T) []T {
	out := make([]T, 0, len(ps))
	for _, p := range ps {
		out = append(out, *p)
	}
	return out
}
