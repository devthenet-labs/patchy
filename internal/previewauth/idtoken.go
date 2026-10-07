// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/sha256"
	"encoding/base64"
)

// IDTokenClaims is an ID token's claim set. It names no viewer: its subject is
// the pairwise one, and it carries no name, email or groups. Signing is the
// Signer port's job.
type IDTokenClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	Nonce    string `json:"nonce,omitempty"`
	AtHash   string `json:"at_hash"`
}

// AtHash is the OIDC at_hash of an access token for RS256: base64url of the
// left half of its SHA-256.
func AtHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
}
