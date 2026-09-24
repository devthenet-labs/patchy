// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// TestScopedTokenPermissionSets: the access-token request carries exactly
// the permissions asked for and no others, and no permissions key at all for
// an empty set (the behaviour contents-only callers always had).
func TestScopedTokenPermissionSets(t *testing.T) {
	tests := []struct {
		name  string
		perms TokenPerms
		want  map[string]any // nil: no "permissions" key at all
	}{
		{
			name:  "contents only, as the Finding flow asks",
			perms: TokenPerms{Contents: PermWrite},
			want:  map[string]any{"contents": "write"},
		},
		{
			name:  "issues only",
			perms: TokenPerms{Issues: PermWrite},
			want:  map[string]any{"issues": "write"},
		},
		{
			name:  "pull requests with contents read",
			perms: TokenPerms{Contents: PermRead, PullRequests: PermWrite},
			want:  map[string]any{"contents": "read", "pull_requests": "write"},
		},
		{
			name:  "all three",
			perms: TokenPerms{Contents: PermWrite, Issues: PermRead, PullRequests: PermRead},
			want:  map[string]any{"contents": "write", "issues": "read", "pull_requests": "read"},
		},
		{name: "none requested", perms: TokenPerms{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, app := newFakeApp(t)
			mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, `{"id": 9}`)
			})
			var body map[string]any
			mux.HandleFunc("POST /app/installations/9/access_tokens", func(w http.ResponseWriter, r *http.Request) {
				body = decodeBody[map[string]any](t, r)
				writeJSON(t, w, `{"token":"scoped-tok","expires_at":"2026-07-13T12:00:00Z"}`)
			})

			if _, _, err := app.ScopedToken(context.Background(), testRepo, tt.perms); err != nil {
				t.Fatalf("ScopedToken() error = %v", err)
			}
			if !reflect.DeepEqual(body["repositories"], []any{"r"}) {
				t.Errorf("repositories = %v, want [r]", body["repositories"])
			}
			perms, present := body["permissions"]
			if tt.want == nil {
				if present {
					t.Errorf("permissions = %v, want the key absent", perms)
				}
				return
			}
			if !reflect.DeepEqual(perms, tt.want) {
				t.Errorf("permissions = %v, want exactly %v", perms, tt.want)
			}
		})
	}
}

func TestTokenPermsValidate(t *testing.T) {
	tests := []struct {
		name    string
		perms   TokenPerms
		wantErr bool
	}{
		{name: "contents write", perms: TokenPerms{Contents: PermWrite}},
		{name: "issues read", perms: TokenPerms{Issues: PermRead}},
		{name: "pull requests write", perms: TokenPerms{PullRequests: PermWrite}},
		{name: "mixed", perms: TokenPerms{Contents: PermRead, Issues: PermWrite, PullRequests: PermRead}},
		// An empty set would mint a token with every permission the
		// installation holds.
		{name: "none requested", perms: TokenPerms{}, wantErr: true},
		{name: "admin level", perms: TokenPerms{Issues: "admin"}, wantErr: true},
		{name: "capitalised level", perms: TokenPerms{PullRequests: "Write"}, wantErr: true},
		{name: "one bad among good", perms: TokenPerms{Contents: PermRead, Issues: "none"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.perms.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
