// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestCommentEdited: whether a comment was ever edited is GraphQL's answer,
// which REST's second-resolution updated_at cannot give for an edit made in
// the second the comment was posted. A comment that is gone, or an id that
// names something else, is ErrNodeNotFound; a refused request is GitHub's
// error, never an answer.
func TestCommentEdited(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		response string
		want     bool
		wantErr  error
		anyErr   bool
	}{
		{name: "never edited",
			response: `{"data":{"node":{"__typename":"IssueComment","lastEditedAt":null,"includesCreatedEdit":false}}}`},
		{name: "edited", want: true,
			response: `{"data":{"node":{"__typename":"IssueComment","lastEditedAt":"2026-09-24T10:00:00Z",` +
				`"includesCreatedEdit":false}}}`},
		{name: "edited, by its creation's edit", want: true,
			response: `{"data":{"node":{"__typename":"IssueComment","lastEditedAt":null,"includesCreatedEdit":true}}}`},
		{name: "gone", wantErr: ErrNodeNotFound,
			response: `{"data":{"node":null},"errors":[{"type":"NOT_FOUND",` +
				`"message":"Could not resolve to a node with the global id of 'IC_x'"}]}`},
		{name: "not a comment", wantErr: ErrNodeNotFound,
			response: `{"data":{"node":{"__typename":"Issue"}}}`},
		{name: "another error", anyErr: true,
			response: `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`},
		{name: "refused", status: http.StatusForbidden, anyErr: true,
			response: `{"message":"Resource not accessible by integration"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, c := newFakeGraphQLClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := decodeBody[struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}](t, r)
				if !strings.Contains(body.Query, "lastEditedAt") || body.Variables["id"] != "IC_x" {
					t.Errorf("request = %+v, want the comment IC_x's edit", body)
				}
				if tt.status != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
				}
				writeJSON(t, w, tt.response)
			})
			got, err := c.CommentEdited(context.Background(), "IC_x")
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CommentEdited() error = %v, want %v", err, tt.wantErr)
				}
			case tt.anyErr:
				if err == nil {
					t.Fatal("CommentEdited() error = nil, want one")
				}
			case err != nil:
				t.Fatalf("CommentEdited() error = %v", err)
			case got != tt.want:
				t.Errorf("CommentEdited() = %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := (&Client{}).CommentEdited(context.Background(), ""); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("CommentEdited(\"\") error = %v, want ErrNodeNotFound", err)
	}
}

func TestPullRequestFeedbackEdited(t *testing.T) {
	for _, tt := range []struct {
		name, kind, nodeID, typename string
		edited                       bool
		wrong                        bool
	}{
		{"review pristine", "review", "PRR_1", "PullRequestReview", false, false},
		{"review edited", "review", "PRR_1", "PullRequestReview", true, false},
		{"review wrong node", "review", "PRR_1", "PullRequestReviewComment", false, true},
		{"inline pristine", "inline", "PRRC_1", "PullRequestReviewComment", false, false},
		{"inline edited", "inline", "PRRC_1", "PullRequestReviewComment", true, false},
		{"inline wrong node", "inline", "PRRC_1", "PullRequestReview", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, c := newFakeGraphQLClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := decodeBody[struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}](t, r)
				wantFragment := "... on PullRequestReview {"
				if tt.kind == "inline" {
					wantFragment = "... on PullRequestReviewComment {"
				}
				if body.Variables["id"] != tt.nodeID || !strings.Contains(body.Query, wantFragment) {
					t.Errorf("query = %+v, want %s %s", body, tt.nodeID, wantFragment)
				}
				last := "null"
				if tt.edited {
					last = `"2026-09-24T10:00:00Z"`
				}
				response := `{"data":{"node":{"__typename":"` + tt.typename + `","lastEditedAt":` +
					last + `,"includesCreatedEdit":false}}}`
				writeJSON(t, w, response)
			})
			var got bool
			var err error
			if tt.kind == "review" {
				got, err = c.ReviewEdited(context.Background(), tt.nodeID)
			} else {
				got, err = c.ReviewCommentEdited(context.Background(), tt.nodeID)
			}
			if tt.wrong {
				if !errors.Is(err, ErrNodeNotFound) {
					t.Fatalf("edited error = %v, want ErrNodeNotFound", err)
				}
				return
			}
			if err != nil || got != tt.edited {
				t.Fatalf("edited = %v, %v; want %v", got, err, tt.edited)
			}
		})
	}
}
