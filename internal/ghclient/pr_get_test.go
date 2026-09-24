// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
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
				`"merge_commit_sha":"45b1bec5a4d6e0f64d1a4ad7f4d6ac6e1a5b1bec"}`,
			want: PullRequest{
				Number: 11, State: "open",
				MergeCommitSHA: "45b1bec5a4d6e0f64d1a4ad7f4d6ac6e1a5b1bec",
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
