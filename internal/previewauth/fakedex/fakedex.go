// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakedex

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const keyID = "fakedex-1"

// Server is a running fake Dex. Its URL is the issuer.
type Server struct {
	*httptest.Server
	ClientID     string
	ClientSecret string

	key *rsa.PrivateKey

	mu             sync.Mutex
	claims         map[string]any
	codes          map[string]pending
	authorizations int
}

// Authorizations counts the /auth requests so far.
func (s *Server) Authorizations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorizations
}

type pending struct {
	redirect, challenge, nonce string
	claims                     map[string]any
}

// Start runs a fake Dex for one client over plain HTTP.
func Start(clientID, clientSecret string) (*Server, error) {
	return start(clientID, clientSecret, false)
}

// StartTLS runs it over HTTPS with httptest's self-signed certificate (its
// Certificate method returns it, to trust).
func StartTLS(clientID, clientSecret string) (*Server, error) {
	return start(clientID, clientSecret, true)
}

func start(clientID, clientSecret string, tls bool) (*Server, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("fakedex: key: %w", err)
	}
	s := &Server{ClientID: clientID, ClientSecret: clientSecret, key: key, codes: map[string]pending{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer": s.URL, "authorization_endpoint": s.URL + "/auth", "token_endpoint": s.URL + "/token",
			"jwks_uri": s.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
		}}})
	})
	mux.HandleFunc("GET /auth", s.authorize)
	mux.HandleFunc("POST /token", s.token)
	if tls {
		s.Server = httptest.NewTLSServer(mux)
	} else {
		s.Server = httptest.NewServer(mux)
	}
	return s, nil
}

// SignIn sets the claims of whoever signs in next (sub, preferred_username,
// groups, ...). With nil claims, /auth answers access_denied.
func (s *Server) SignIn(claims map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = claims
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.authorizations++
	claims := s.claims
	s.mu.Unlock()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != s.ClientID || redirect == "" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back := url.Values{"state": {q.Get("state")}}
	if claims == nil {
		back.Set("error", "access_denied")
	} else {
		code := rand.Text()
		s.mu.Lock()
		s.codes[code] = pending{redirect: redirect, challenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
			claims: claims}
		s.mu.Unlock()
		back.Set("code", code)
	}
	http.Redirect(w, r, redirect+"?"+back.Encode(), http.StatusSeeOther)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	code := r.PostForm.Get("code")
	s.mu.Lock()
	p, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || p.redirect != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	all := map[string]any{
		"iss": s.URL, "aud": s.ClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": p.nonce,
	}
	for k, v := range p.claims {
		all[k] = v
	}
	idToken, err := s.sign(all)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "dex-access", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}

// Mint signs an arbitrary claim set with the issuer's key.
func (s *Server) Mint(claims map[string]any) (string, error) { return s.sign(claims) }

func (s *Server) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), keyID))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
