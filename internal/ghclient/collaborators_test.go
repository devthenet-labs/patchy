// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestCollaboratorPermission mirrors the live behaviour on a public
// repository: anyone reads as "read", the App's bot as "none", and a
// nonexistent login is a 404.
func TestCollaboratorPermission(t *testing.T) {
	tests := []struct {
		name       string
		login      string
		status     int
		payload    string
		want       string
		wantErr    bool
		wantNoUser bool
	}{
		{name: "admin", login: "peter", status: http.StatusOK,
			payload: `{"permission":"admin","role_name":"admin"}`, want: PermissionAdmin},
		{name: "maintainer reads as write", login: "maint", status: http.StatusOK,
			payload: `{"permission":"write","role_name":"maintain"}`, want: PermissionWrite},
		{name: "any account on a public repo", login: "octocat", status: http.StatusOK,
			payload: `{"permission":"read","role_name":"read"}`, want: PermissionRead},
		{name: "the App's bot", login: "patchy-devthenet[bot]", status: http.StatusOK,
			payload: `{"permission":"none"}`, want: PermissionNone},
		{name: "nonexistent login", login: "no-such-user-4a1f", status: http.StatusNotFound,
			payload: `{"message":"Not Found"}`, wantErr: true, wantNoUser: true},
		{name: "server error", login: "octocat", status: http.StatusInternalServerError,
			payload: `{"message":"boom"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
				if got := r.PathValue("login"); got != tt.login {
					t.Errorf("login in path = %q, want %q", got, tt.login)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.payload))
			})
			got, err := c.CollaboratorPermission(context.Background(), testRepo, tt.login)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CollaboratorPermission() error = %v, wantErr %v", err, tt.wantErr)
			}
			if errors.Is(err, ErrNoSuchUser) != tt.wantNoUser {
				t.Errorf("errors.Is(%v, ErrNoSuchUser) = %v, want %v", err, !tt.wantNoUser, tt.wantNoUser)
			}
			if got != tt.want {
				t.Errorf("CollaboratorPermission() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCollaboratorPermissionEscapesLogin: a login cannot reshape the
// request path — a slash stays inside the login segment, a dot-dot never
// leaves the client — and an empty login never reaches GitHub.
func TestCollaboratorPermissionEscapesLogin(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("login"); got != "a/b" {
			t.Errorf("login in path = %q, want the escaped login intact", got)
		}
		writeJSON(t, w, `{"permission":"read"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request escaped its path: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	ctx := context.Background()
	if got, err := c.CollaboratorPermission(ctx, testRepo, "a/b"); err != nil || got != PermissionRead {
		t.Errorf("CollaboratorPermission(a/b) = %q, %v, want read", got, err)
	}
	if got, err := c.CollaboratorPermission(ctx, testRepo, "../../x"); err == nil || CanWrite(got) {
		t.Errorf("CollaboratorPermission(../../x) = %q, %v, want an error", got, err)
	}
	if _, err := c.CollaboratorPermission(ctx, testRepo, ""); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("CollaboratorPermission(\"\") error = %v, want ErrNoSuchUser", err)
	}
}

func TestCanWrite(t *testing.T) {
	tests := []struct {
		permission string
		want       bool
	}{
		{PermissionAdmin, true},
		{PermissionMaintain, true},
		{PermissionWrite, true},
		// A public repository grants read to every GitHub account.
		{PermissionRead, false},
		{"triage", false},
		{PermissionNone, false},
		{"", false},
		{"Admin", false},
		{"write ", false},
	}
	for _, tt := range tests {
		if got := CanWrite(tt.permission); got != tt.want {
			t.Errorf("CanWrite(%q) = %v, want %v", tt.permission, got, tt.want)
		}
	}
}
