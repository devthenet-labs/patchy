// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package signer is the preview sign-in relay's previewauth.Signer: it signs
// ID tokens with RS256 under the current RSA key and publishes the current
// and, during a rotation, the previous public key as a JWKS. Each key's id is
// its RFC 7638 thumbprint, so the same key always has the same kid and a
// rotated key never reuses one. Security never depends on the ALB validating
// these tokens; they are spec-valid in case it does.
package signer
