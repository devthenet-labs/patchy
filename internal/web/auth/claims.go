// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
)

// MapClaims derives the identity from verified ID-token claims using the
// configured claim names — the one claims-to-identity mapping shared by the
// status server's session flow and the evaluation API's bearer flow, so the
// two can never disagree about who a token names.
func MapClaims(claims map[string]any, cfg ClaimsConfig) (*Identity, error) {
	return mapClaims(claims, cfg)
}

// mapClaims derives the identity from verified ID-token claims using the
// configured claim names. The username claim is required — an identity that
// cannot be named cannot be access-reviewed; groups and display name are
// best-effort. With RequireVerifiedEmail a token whose email_verified claim
// is not true names no one. The configured prefixes are applied exactly
// once, here, to the username and to every group: this is the only place an
// identity is made from claims, so no path reaches an access review
// unprefixed. The display name stays the provider's: the UI shows it, RBAC
// never sees it.
func mapClaims(claims map[string]any, cfg ClaimsConfig) (*Identity, error) {
	username, _ := claims[cfg.Username].(string)
	if username == "" {
		return nil, fmt.Errorf("token has no usable %q claim", cfg.Username)
	}
	if cfg.RequireVerifiedEmail && !emailVerified(claims["email_verified"]) {
		return nil, fmt.Errorf("token's email_verified claim is not true")
	}
	id := &Identity{Username: cfg.UsernamePrefix + username, Session: true}
	if name, _ := claims[cfg.DisplayName].(string); name != "" {
		id.DisplayName = name
	}
	switch groups := claims[cfg.Groups].(type) {
	case []any:
		for _, g := range groups {
			if s, ok := g.(string); ok && s != "" {
				id.Groups = append(id.Groups, cfg.GroupsPrefix+s)
			}
		}
	case string:
		if groups != "" {
			id.Groups = []string{cfg.GroupsPrefix + groups}
		}
	}
	return id, nil
}

// emailVerified reads an email_verified claim. OIDC Core makes it a JSON
// boolean; a few providers send the string "true", which is accepted too.
// Anything else, an absent claim included, is unverified.
func emailVerified(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}
