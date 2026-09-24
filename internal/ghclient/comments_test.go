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

// botComment is a comment the App wrote: its bot user, and — unlike label
// events — a performed_via_github_app slug.
const botComment = `{"id":11,"body":"<!-- patchy:plan -->","html_url":"https://github.com/o/r/issues/3#issuecomment-11",
	"user":{"login":"patchy-devthenet[bot]","id":332617407,"type":"Bot"},"author_association":"NONE",
	"created_at":"2026-09-24T10:00:00Z","updated_at":"2026-09-24T10:05:00Z",
	"performed_via_github_app":{"id":7,"slug":"patchy-devthenet"}}`

// wantBotComment is botComment decoded.
var wantBotComment = &Comment{
	ID: 11, Body: "<!-- patchy:plan -->", UserLogin: "patchy-devthenet[bot]", AuthorAssociation: "NONE",
	UserID: 332617407, UserType: "Bot",
	CreatedAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	UpdatedAt: time.Date(2026, 9, 24, 10, 5, 0, 0, time.UTC),
	ViaApp:    "patchy-devthenet",
	HTMLURL:   "https://github.com/o/r/issues/3#issuecomment-11",
}

func TestListIssueComments(t *testing.T) {
	humanComment := `{"id":12,"body":"/patchy approve","user":{"login":"octocat","id":583231,"type":"User"},
		"author_association":"OWNER","created_at":"2026-09-24T10:06:00Z","updated_at":"2026-09-24T10:06:00Z",
		"performed_via_github_app":null}`
	tests := []struct {
		name      string
		since     time.Time
		wantSince string // "" = no since parameter
	}{
		{name: "every comment"},
		{
			name:      "since a time",
			since:     time.Date(2026, 9, 24, 11, 0, 0, 0, time.FixedZone("BST", 3600)),
			wantSince: "2026-09-24T10:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			paged := pagedHandler(t, "["+botComment+"]", "["+humanComment+"]")
			mux.HandleFunc("GET /repos/o/r/issues/3/comments", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if _, present := q["since"]; present != (tt.wantSince != "") || q.Get("since") != tt.wantSince {
					t.Errorf("since = %q (present=%v), want %q", q.Get("since"), present, tt.wantSince)
				}
				paged(w, r)
			})

			got, err := c.ListIssueComments(context.Background(), testRepo, 3, tt.since)
			if err != nil {
				t.Fatalf("ListIssueComments() error = %v", err)
			}
			want := []*Comment{wantBotComment, {
				ID: 12, Body: "/patchy approve", UserLogin: "octocat", AuthorAssociation: "OWNER",
				UserID: 583231, UserType: "User",
				CreatedAt: time.Date(2026, 9, 24, 10, 6, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 9, 24, 10, 6, 0, 0, time.UTC),
			}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ListIssueComments() =\n%+v\nwant\n%+v", derefAll(got), derefAll(want))
			}
			if a := got[0].Author(); !a.IsBot() || a.Login != "patchy-devthenet[bot]" || a.ID != 332617407 {
				t.Errorf("Author() = %+v, want the App's bot", a)
			}
		})
	}
}

func TestGetIssueComment(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues/comments/11", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, botComment)
	})
	got, err := c.GetIssueComment(context.Background(), testRepo, 11)
	if err != nil {
		t.Fatalf("GetIssueComment() error = %v", err)
	}
	if !reflect.DeepEqual(got, wantBotComment) {
		t.Errorf("GetIssueComment() = %+v, want %+v", *got, *wantBotComment)
	}

	if _, err := c.GetIssueComment(context.Background(), testRepo, 99); !IsNotFound(err) {
		t.Errorf("GetIssueComment(missing) error = %v, want a not-found API error", err)
	}
}

// TestCreateIssueCommentReaction: a new reaction (201) and an existing one
// (200) are both success, so acknowledging twice is harmless; a refusal is
// an error.
func TestCreateIssueCommentReaction(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "new reaction", status: http.StatusCreated},
		{name: "already reacted", status: http.StatusOK},
		{name: "refused", status: http.StatusUnprocessableEntity, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("POST /repos/o/r/issues/comments/12/reactions", func(w http.ResponseWriter, r *http.Request) {
				if body := decodeBody[map[string]any](t, r); !reflect.DeepEqual(body, map[string]any{"content": "eyes"}) {
					t.Errorf("reaction body = %v, want content=eyes", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"id":1,"content":"eyes","user":{"login":"patchy-devthenet[bot]"}}`))
			})
			err := c.CreateIssueCommentReaction(context.Background(), testRepo, 12, ReactionEyes)
			if (err != nil) != tt.wantErr {
				t.Errorf("CreateIssueCommentReaction() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
