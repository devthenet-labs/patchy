// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakeoidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// User is someone the authorization-code flow signs in, shaped like an
// identity Dex's GitHub connector issues: a GitHub login as
// preferred_username, org:team groups, and a verified email.
type User struct {
	Login  string
	Name   string
	Groups []string
}

// codeFlow is the authorization-code + PKCE half of the fake issuer: what a
// browser-facing relying party (the status server) signs users in through.
// The authorize endpoint signs in the user named by login_hint at once, or
// offers a page listing the users when there is none, so a person at a
// browser can pick one; nothing asks for a password.
type codeFlow struct {
	mu    sync.Mutex
	users map[string]User
	codes map[string]pendingCode
}

// pendingCode is an issued authorization code and what redeeming it needs.
type pendingCode struct {
	user        User
	clientID    string
	redirectURI string
	nonce       string
	challenge   string
	issued      time.Time
}

// AddUser registers a user the code flow can sign in.
func (s *Server) AddUser(u User) {
	s.flow.mu.Lock()
	defer s.flow.mu.Unlock()
	s.flow.users[u.Login] = u
}

func (s *Server) registerCodeFlow(mux *http.ServeMux) {
	s.flow = &codeFlow{users: map[string]User{}, codes: map[string]pendingCode{}}
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /token", s.handleToken)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "fakeoidc: want response_type=code with an S256 PKCE challenge", http.StatusBadRequest)
		return
	}
	s.flow.mu.Lock()
	user, ok := s.flow.users[q.Get("login_hint")]
	logins := make([]string, 0, len(s.flow.users))
	for l := range s.flow.users {
		logins = append(logins, l)
	}
	s.flow.mu.Unlock()
	if !ok {
		slices.Sort(logins)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<!doctype html><title>fakeoidc sign-in</title><h1>Sign in as</h1><ul>")
		for _, l := range logins {
			pick := r.URL.Query()
			pick.Set("login_hint", l)
			_, _ = fmt.Fprintf(w, `<li><a href="/authorize?%s">%s</a></li>`, html.EscapeString(pick.Encode()),
				html.EscapeString(l))
		}
		_, _ = fmt.Fprint(w, "</ul>")
		return
	}
	code, err := randomHex()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.flow.mu.Lock()
	s.flow.codes[code] = pendingCode{
		user: user, clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), issued: time.Now(),
	}
	s.flow.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "fakeoidc: bad redirect_uri", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, "unsupported_grant_type")
		return
	}
	clientID, _, ok := r.BasicAuth()
	if !ok {
		clientID = r.PostForm.Get("client_id")
	}
	s.flow.mu.Lock()
	pc, found := s.flow.codes[r.PostForm.Get("code")]
	delete(s.flow.codes, r.PostForm.Get("code")) // a code is redeemed once
	s.flow.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !found || time.Since(pc.issued) > time.Minute:
		writeOAuthError(w, "invalid_grant")
		return
	case pc.clientID != clientID || pc.redirectURI != r.PostForm.Get("redirect_uri"):
		writeOAuthError(w, "invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != pc.challenge:
		writeOAuthError(w, "invalid_grant")
		return
	}
	idToken, err := s.userToken(pc.user, clientID, pc.nonce)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"access_token": "fakeoidc-access", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

// userToken signs an ID token for u, as a Dex GitHub connector would issue it.
func (s *Server) userToken(u User, audience, nonce string) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), keyID),
	)
	if err != nil {
		return "", fmt.Errorf("fakeoidc: new signer: %w", err)
	}
	now := time.Now()
	claims := struct {
		jwt.Claims
		Nonce             string   `json:"nonce,omitempty"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name,omitempty"`
		Email             string   `json:"email"`
		Verified          bool     `json:"email_verified"`
		Groups            []string `json:"groups,omitempty"`
	}{
		Claims: jwt.Claims{
			Issuer: s.URL, Subject: u.Login, Audience: jwt.Audience{audience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Nonce: nonce, PreferredUsername: u.Login, Name: u.Name,
		Email: u.Login + "@users.example.com", Verified: true, Groups: u.Groups,
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("fakeoidc: sign: %w", err)
	}
	return raw, nil
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

func randomHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
