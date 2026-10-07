// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"context"
	"errors"
	"time"
)

// ErrReplayed is a CodeLedger's answer for a code that was already redeemed.
var ErrReplayed = errors.New("previewauth: code already redeemed")

// PreviewLookup finds the Preview at a host label. A label with no Preview is
// a View that is not Live, not an error; an error means the answer is unknown
// (the relay answers 503).
type PreviewLookup interface {
	ByLabel(ctx context.Context, label string) (View, error)
}

// Authorizer runs an AccessReview. An error means the answer is unknown.
type Authorizer interface {
	Allowed(ctx context.Context, review AccessReview) (bool, error)
}

// CodeLedger makes codes single-use across relay replicas. Consume records
// key until exp and returns ErrReplayed when key is already recorded; any
// other error means the answer is unknown.
type CodeLedger interface {
	Consume(ctx context.Context, key [32]byte, exp time.Time) error
}

// Upstream is the identity provider (Dex) the relay signs viewers in with.
type Upstream interface {
	// AuthURL is where to send the browser, with state, nonce and an S256
	// PKCE challenge. An error means the provider cannot be reached (its
	// discovery has not succeeded yet); the relay answers 503.
	AuthURL(ctx context.Context, state, nonce, challenge string) (string, error)
	// Exchange redeems code with the PKCE verifier, verifies the ID token
	// and its nonce, and maps its claims to an Identity.
	Exchange(ctx context.Context, code, verifier, nonce string) (Identity, error)
}

// Signer signs ID tokens and publishes the keys that verify them.
type Signer interface {
	Sign(claims IDTokenClaims) (string, error)
	JWKS() []byte
}
