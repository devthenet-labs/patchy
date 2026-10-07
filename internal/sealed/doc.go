// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package sealed holds the authenticated-encryption primitives patchy's
// sign-in surfaces share: a purpose key derived from one secret with
// HKDF-SHA256, AES-256-GCM sealing of a JSON value into a URL-safe string,
// and random URL-safe tokens.
//
// Every seal takes additional authenticated data (AAD). The AAD is not in the
// sealed string. Open has to be given the same AAD, so a caller that binds a
// token's kind and key generation into it gets a token of one kind that never
// opens as another, even under the same key. Purpose keys are the first
// separation (one HKDF info string per purpose); the AAD is the second. A nil
// AAD is the same as an empty one.
//
// The sealed form is base64url, unpadded, of nonce || ciphertext || tag, with
// a fresh random 96-bit nonce per seal. Open reports every failure to decode,
// authenticate or decrypt as ErrOpen, and never says which, so a caller can
// treat them all as "this blob is not ours". It does not bound the blob's
// length: a caller reading a blob from a request bounds it first.
//
// The package imports only the standard library (a test pins that), so a
// pure core may depend on it.
package sealed
