// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import "github.com/bitwise-media-group/patchy/internal/sealed"

// sealKey derives the 32-byte AES key from the OAuth2 client secret via
// HKDF-SHA256. Deriving instead of storing means no key management beyond
// the secret the flow needs anyway; rotating the secret invalidates every
// outstanding state blob and session, which is the desired failure mode.
func sealKey(clientSecret, info string) ([]byte, error) {
	return sealed.PurposeKey([]byte(clientSecret), info)
}

// seal encrypts v's JSON with AES-256-GCM and encodes it URL-safely. The
// random GCM nonce is prepended to the ciphertext. It binds no additional
// data, so blobs sealed before the primitives moved to internal/sealed still
// open.
func seal(key []byte, v any) (string, error) {
	return sealed.Seal(key, nil, v)
}

// unseal decrypts a seal blob into v. Any tampering (or a key rotated since
// sealing) fails authentication and returns an error.
func unseal(key []byte, blob string, v any) error {
	return sealed.Open(key, nil, blob, v)
}

// randomToken returns a URL-safe random string for CSRF tokens, nonces, and
// PKCE verifiers.
func randomToken() (string, error) {
	return sealed.RandomToken()
}
