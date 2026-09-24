// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestEnsureLabel: an existing label is left alone, a missing one is created
// with the color and description asked for, and a create that races another
// (422) counts as the label existing.
func TestEnsureLabel(t *testing.T) {
	tests := []struct {
		name        string
		getStatus   int
		postStatus  int
		wantCreated bool
		wantErr     bool
		wantPost    bool
	}{
		{name: "exists", getStatus: http.StatusOK},
		{name: "missing is created", getStatus: http.StatusNotFound, postStatus: http.StatusCreated,
			wantCreated: true, wantPost: true},
		{name: "created meanwhile", getStatus: http.StatusNotFound, postStatus: http.StatusUnprocessableEntity,
			wantPost: true},
		{name: "lookup fails", getStatus: http.StatusInternalServerError, wantErr: true},
		{name: "create forbidden", getStatus: http.StatusNotFound, postStatus: http.StatusForbidden,
			wantErr: true, wantPost: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			posted := false
			mux.HandleFunc("GET /repos/o/r/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
				if got := r.PathValue("name"); got != "patchy:target" {
					t.Errorf("label looked up = %q, want patchy:target", got)
				}
				w.WriteHeader(tt.getStatus)
				writeJSON(t, w, `{"name":"patchy:target"}`)
			})
			mux.HandleFunc("POST /repos/o/r/labels", func(w http.ResponseWriter, r *http.Request) {
				posted = true
				body := decodeBody[map[string]string](t, r)
				if body["name"] != "patchy:target" || body["color"] != "5319e7" || body["description"] != "d" {
					t.Errorf("create label request = %v", body)
				}
				w.WriteHeader(tt.postStatus)
				writeJSON(t, w, `{"message":"x"}`)
			})
			created, err := c.EnsureLabel(context.Background(), testRepo, "patchy:target", "5319e7", "d")
			if (err != nil) != tt.wantErr {
				t.Fatalf("EnsureLabel() error = %v, wantErr %v", err, tt.wantErr)
			}
			if created != tt.wantCreated || posted != tt.wantPost {
				t.Errorf("created = %v posted = %v, want %v %v", created, posted, tt.wantCreated, tt.wantPost)
			}
		})
	}
}

func TestRateRemaining(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"resources":{"core":{"limit":5000,"remaining":4321,"reset":1}}}`)
	})
	got, err := c.RateRemaining(context.Background())
	if err != nil || got != 4321 {
		t.Errorf("RateRemaining() = %d, %v; want 4321", got, err)
	}
}

// TestCreateIssueComment: the comment comes back as GitHub stored it, body
// and created_at included, which is what an approval later binds to.
func TestCreateIssueComment(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/issues/3/comments", func(w http.ResponseWriter, r *http.Request) {
		if body := decodeBody[map[string]string](t, r); body["body"] != "hello" {
			t.Errorf("comment request = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, `{"id":77,"body":"hello\r\n","created_at":"2026-09-24T10:00:00Z",`+
			`"updated_at":"2026-09-24T10:00:00Z","html_url":"https://gh/o/r/issues/3#issuecomment-77",`+
			`"user":{"login":"patchy[bot]","id":9,"type":"Bot"},"performed_via_github_app":{"slug":"patchy"}}`)
	})
	got, err := c.CreateIssueComment(context.Background(), testRepo, 3, "hello")
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if got.ID != 77 || got.Body != "hello\r\n" || !got.CreatedAt.Equal(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)) ||
		got.UserLogin != "patchy[bot]" || got.ViaApp != "patchy" || got.HTMLURL == "" {
		t.Errorf("CreateIssueComment() = %+v", got)
	}
}

// TestPullRequestIdentity: the node id and head commit ride on every read
// and create of a pull request, so a caller can correlate by node id.
func TestPullRequestIdentity(t *testing.T) {
	const pr = `{"number":12,"html_url":"https://gh/o/r/pull/12","node_id":"PR_kw1","state":"open",` +
		`"head":{"sha":"1111111111111111111111111111111111111111"}}`
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, pr)
	})
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, "["+pr+"]")
	})
	mux.HandleFunc("GET /repos/o/r/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, pr)
	})
	ctx := context.Background()
	const head = "1111111111111111111111111111111111111111"
	created, err := c.CreatePR(ctx, testRepo, PRRequest{Title: "t", Head: "h", Base: "main"})
	if err != nil || created.NodeID != "PR_kw1" || created.HeadSHA != head {
		t.Errorf("CreatePR() = %+v, %v", created, err)
	}
	found, err := c.FindPRByHead(ctx, testRepo, "h")
	if err != nil || found == nil || found.NodeID != "PR_kw1" || found.HeadSHA != head {
		t.Errorf("FindPRByHead() = %+v, %v", found, err)
	}
	got, err := c.GetPullRequest(ctx, testRepo, 12)
	if err != nil || got.NodeID != "PR_kw1" || got.HeadSHA != head {
		t.Errorf("GetPullRequest() = %+v, %v", got, err)
	}
}
