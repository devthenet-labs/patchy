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
	"slices"
	"strconv"
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

// PATUser is the account a personal access token acts as. Every request not
// made with an installation token the fake minted — a PAT, or no credential
// at all — is this user's, as GitHub attributes a PAT's writes to the PAT's
// owner; only an installation token's writes are the App's bot's.
var PATUser = Actor{Login: "patchy-e2e", ID: 100000002, Type: "User"}

// accessToken answers POST /app/installations/{id}/access_tokens, minting a
// token valid for an hour and recording the scope requested — the scope the
// token is then held to on every call (see scoped).
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
	token := fmt.Sprintf("ghs_fake%d", len(s.tokens))
	s.minted[token] = TokenRequest{Repositories: body.Repositories, Permissions: body.Permissions}
	s.mu.Unlock()

	perms := body.Permissions
	if len(perms) == 0 {
		perms = map[string]string{"contents": "write", "issues": "write", "pull_requests": "write"}
	}
	perms = maps.Clone(perms)
	perms[permMetadata] = permRead // GitHub adds it to every installation token
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{
		"token":       token,
		"expires_at":  s.now().Add(time.Hour),
		"permissions": perms,
	})
}

// The installation-token permissions a route needs, and their levels.
const (
	permContents       = "contents"
	permIssues         = "issues"
	permPullRequests   = "pull_requests"
	permMetadata       = "metadata"
	permSecurityEvents = "security_events"

	permRead  = "read"
	permWrite = "write"
)

// scoped holds an installation token the fake minted to the scope it was
// minted with, as GitHub does, before h runs: the route's repository must be
// one the token names, and the token must grant perm — at read for GET, at
// write otherwise. A token minted with no repositories covers them all and
// one minted with no permissions holds the installation's full set. Every
// token reads metadata. The issues endpoints also serve a pull request to a
// pull_requests grant (documented; the grant PR comments are posted with).
//
// Anything else — a PAT, the App's JWT, no credential — passes: a PAT's reach
// is its user's, which the fake does not model. A refusal is 403 "Resource
// not accessible by integration", GitHub's answer to a permission the token
// lacks. For a private repository outside the token's list GitHub may answer
// 404 instead (not verified live); the fake answers 403 there too, so a call
// made with the wrong token can never pass as a 404-tolerant success (an
// absent label's removal, say).
func (s *Server) scoped(perm string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		ok := s.permits(r, perm)
		s.mu.Unlock()
		if !ok {
			forbidden(w)
			return
		}
		h(w, r)
	}
}

// permits reports whether r's credential may make a call needing perm.
// Callers hold s.mu.
func (s *Server) permits(r *http.Request, perm string) bool {
	scope, isToken := s.minted[bearer(r)]
	if !isToken {
		return true
	}
	if repo := r.PathValue("repo"); repo != "" && len(scope.Repositories) > 0 &&
		!slices.ContainsFunc(scope.Repositories, func(name string) bool { return strings.EqualFold(name, repo) }) {
		return false
	}
	need := permWrite
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		need = permRead
	}
	switch {
	case perm == permMetadata:
		return need == permRead
	case len(scope.Permissions) == 0:
		return true
	case grants(scope.Permissions[perm], need):
		return true
	}
	return perm == permIssues && s.targetsPull(r) && grants(scope.Permissions[permPullRequests], need)
}

// grants reports whether a granted level covers the needed one.
func grants(granted, need string) bool {
	return granted == permWrite || (granted == permRead && need == permRead)
}

// targetsPull reports whether an issues-endpoint request is about a pull
// request: its {number}, or for issues/comments/{id} the number the comment
// is on, names a pull request and no issue. Callers hold s.mu.
func (s *Server) targetsPull(r *http.Request) bool {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		// issues/comments/{id}; the GET form routes the id as {sub}.
		raw := r.PathValue("id")
		if raw == "" {
			raw = r.PathValue("sub")
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return false
		}
		var found bool
		if n, found = s.commentNumber(id); !found {
			return false
		}
	}
	_, isIssue := s.issues[n]
	_, isPull := s.pulls[n]
	return isPull && !isIssue
}

// commentNumber returns the issue or pull request number a comment is on.
// Callers hold s.mu.
func (s *Server) commentNumber(id int64) (int, bool) {
	for number, cs := range s.comments {
		if slices.ContainsFunc(cs, func(c comment) bool { return c.ID == id }) {
			return number, true
		}
	}
	return 0, false
}

// caller is the account r acts as: the App's bot for an installation token
// the fake minted, PATUser for anything else.
func (s *Server) caller(r *http.Request) Actor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, isToken := s.minted[bearer(r)]; isToken {
		return Bot
	}
	return PATUser
}

// bearer returns the credential in r's Authorization header, under either
// scheme GitHub accepts ("token" from ghinstallation, "Bearer" from
// go-github), or "".
func bearer(r *http.Request) string {
	scheme, cred, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || (!strings.EqualFold(scheme, "token") && !strings.EqualFold(scheme, "bearer")) {
		return ""
	}
	return strings.TrimSpace(cred)
}

// forbidden answers GitHub's 403 for a call the token's scope does not
// cover.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message":           "Resource not accessible by integration",
		"documentation_url": "https://docs.github.com/rest",
		"status":            "403",
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
