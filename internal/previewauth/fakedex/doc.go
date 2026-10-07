// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package fakedex is a minimal in-memory Dex for the preview sign-in relay's
// tests: discovery, a JWKS, an authorization endpoint that signs in whoever
// the test says (as if the viewer were already signed in at GitHub) and
// redirects straight back, and a token endpoint that checks the client's
// secret, the redirect URI and the PKCE verifier before minting an RS256 ID
// token with the requested nonce. It is test support only: no binary
// imports it.
package fakedex
