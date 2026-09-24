// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// installationID is the fake App's one installation, covering every
// repository.
const installationID = 1

// getApp answers GET /app (authenticated as the App): its slug, from which
// the bot login "<slug>[bot]" follows.
func (s *Server) getApp(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"id": viaApp.ID, "slug": AppSlug, "name": AppSlug})
}

// installation answers GET /repos/{o}/{r}/installation: the one
// installation, whatever the repository.
func (s *Server) installation(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"id":          installationID,
		"account":     map[string]any{"login": r.PathValue("owner")},
		"target_type": "Organization",
	})
}

// TokenRequest is one installation-token request the fake answered: the
// repositories and permissions it was scoped to (both empty for an unscoped
// installation token).
type TokenRequest struct {
	Repositories []string
	Permissions  map[string]string
}

// accessToken answers POST /app/installations/{id}/access_tokens, minting a
// token valid for an hour and recording the scope requested.
func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	// The unscoped installation transport posts no body at all.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.tokens = append(s.tokens, TokenRequest{Repositories: body.Repositories, Permissions: body.Permissions})
	n := len(s.tokens)
	s.mu.Unlock()

	perms := body.Permissions
	if len(perms) == 0 {
		perms = map[string]string{"contents": "write", "issues": "write", "pull_requests": "write"}
	}
	perms = maps.Clone(perms)
	perms["metadata"] = "read" // GitHub adds it to every installation token
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"token":       fmt.Sprintf("ghs_fake%d", n),
		"expires_at":  s.now().Add(time.Hour),
		"permissions": perms,
	})
}

// TokenRequests returns the installation-token requests answered so far.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TokenRequest(nil), s.tokens...)
}

// legacyPermission maps a repository role onto the collaborator-permission
// endpoint's permission field, GitHub's legacy form.
var legacyPermission = map[string]string{
	"admin": "admin", "maintain": "write", "write": "write", "triage": "read", "read": "read",
}

// SetRole gives login a repository role (admin, maintain, write, triage or
// read). Logins with no role read as "read", as every account does on a
// public repository.
func (s *Server) SetRole(login, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[strings.ToLower(login)] = role
}

// MarkUserMissing makes login a nonexistent account: its permission is 404.
func (s *Server) MarkUserMissing(login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missing[strings.ToLower(login)] = true
}

// permission answers GET /repos/{o}/{r}/collaborators/{login}/permission as
// GitHub does for a public repository (verified live): any account "read"
// unless given a role, the App's own bot "none", a nonexistent login 404.
func (s *Server) permission(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	key := strings.ToLower(login)
	s.mu.Lock()
	role, hasRole := s.roles[key]
	missing := s.missing[key]
	s.mu.Unlock()

	switch {
	case missing:
		notFound(w)
	case strings.EqualFold(login, BotLogin):
		writeJSON(w, map[string]any{"permission": "none", "user": Bot})
	case hasRole:
		writeJSON(w, map[string]any{
			"permission": legacyPermission[role], "role_name": role,
			"user": Actor{Login: login, Type: "User"},
		})
	default:
		writeJSON(w, map[string]any{
			"permission": "read", "role_name": "read",
			"user": Actor{Login: login, Type: "User"},
		})
	}
}
