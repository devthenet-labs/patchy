// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// discoveryDocument is the relay's OpenID provider metadata. It advertises
// exactly what the relay serves: the code flow, pairwise subjects, RS256 ID
// tokens and the openid scope.
func discoveryDocument(issuer string) ([]byte, error) {
	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"userinfo_endpoint":                     issuer + "/userinfo",
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"openid"},
		"code_challenge_methods_supported":      []string{"S256"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "nonce", "at_hash"},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	return raw, nil
}

func writeCacheableJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	rec(r).result = "ok"
	writeCacheableJSON(w, s.discovery)
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	rec(r).result = "ok"
	writeCacheableJSON(w, s.cfg.Signer.JWKS())
}
