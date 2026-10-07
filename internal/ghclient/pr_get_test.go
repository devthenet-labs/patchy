// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestGetPullRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want PullRequest
	}{
		{
			name: "merged",
			body: `{"number":11,"state":"closed","merged":true,"merged_at":"2026-07-21T13:00:00Z",` +
				`"merge_commit_sha":"fa82fcdc7efab2777d432ba3385517fa735e0ae0"}`,
			want: PullRequest{
				Number: 11, State: "closed", Merged: true,
				MergedAt:       time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC),
				MergeCommitSHA: "fa82fcdc7efab2777d432ba3385517fa735e0ae0",
			},
		},
		{
			name: "open",
			body: `{"number":11,"state":"open","merged":false,"merged_at":null,` +
				`"merge_commit_sha":"45b1bec5a4d6e0f64d1a4ad7f4d6ac6e1a5b1bec",` +
				`"title":"Add a version endpoint","changed_files":3}`,
			want: PullRequest{
				Number: 11, State: "open",
				MergeCommitSHA: "45b1bec5a4d6e0f64d1a4ad7f4d6ac6e1a5b1bec",
				Title:          "Add a version endpoint", ChangedFiles: 3,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/pulls/11", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tc.body)
			})
			got, err := c.GetPullRequest(context.Background(), testRepo, 11)
			if err != nil {
				t.Fatalf("GetPullRequest() error = %v", err)
			}
			if !got.MergedAt.Equal(tc.want.MergedAt) {
				t.Errorf("MergedAt = %v, want %v", got.MergedAt, tc.want.MergedAt)
			}
			got.MergedAt = tc.want.MergedAt
			if *got != tc.want {
				t.Errorf("GetPullRequest() = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func TestGetPullRequestError(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/pulls/11", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	_, err := c.GetPullRequest(context.Background(), testRepo, 11)
	if !IsNotFound(err) {
		t.Errorf("GetPullRequest() error = %v, want a not-found API error", err)
	}
}

// TestListPullRequestFiles: one page of at most limit files, asked for by
// per_page, each with the path it was renamed from, if any, in GitHub's
// order.
func TestListPullRequestFiles(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		body  string
		want  []PullRequestFile
	}{
		{"paths in order", 50,
			`[{"filename":"src/app.ts","status":"modified"},{"filename":"docs/a.md","status":"added"}]`,
			[]PullRequestFile{{Path: "src/app.ts"}, {Path: "docs/a.md"}}},
		{"a rename", 50,
			`[{"filename":"src/new.ts","previous_filename":"src/old.ts","status":"renamed"}]`,
			[]PullRequestFile{{Path: "src/new.ts", PreviousPath: "src/old.ts"}}},
		{"cut at the limit", 2,
			`[{"filename":"a"},{"filename":""},{"filename":"b"},{"filename":"c"}]`,
			[]PullRequestFile{{Path: "a"}, {Path: "b"}}},
		{"none", 50, `[]`, []PullRequestFile{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/pulls/11/files", func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.URL.Query().Get("per_page"), strconv.Itoa(tc.limit); got != want {
					t.Errorf("per_page = %q, want %q", got, want)
				}
				if page := r.URL.Query().Get("page"); page != "" {
					t.Errorf("page = %q, want the first page only", page)
				}
				writeJSON(t, w, tc.body)
			})
			got, err := c.ListPullRequestFiles(context.Background(), testRepo, 11, tc.limit)
			if err != nil {
				t.Fatalf("ListPullRequestFiles() error = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ListPullRequestFiles() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestListPullRequestFilesLimits(t *testing.T) {
	_, c := newFakeClient(t)
	for _, limit := range []int{0, -1, 101} {
		if _, err := c.ListPullRequestFiles(context.Background(), testRepo, 11, limit); err == nil {
			t.Errorf("ListPullRequestFiles(limit %d) error = nil, want one", limit)
		}
	}
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/pulls/11/files", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	if _, err := c.ListPullRequestFiles(context.Background(), testRepo, 11, 50); !IsNotFound(err) {
		t.Errorf("ListPullRequestFiles() error = %v, want a not-found API error", err)
	}
}
