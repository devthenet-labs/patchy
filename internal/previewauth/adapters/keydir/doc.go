// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package keydir loads the preview sign-in relay's key material from the
// chart's keys Secret, mounted as a directory, so the relay needs no Secret
// RBAC at all: master and generation (the current key generation), the
// optional previousMaster and previousGeneration (a rotation's overlap),
// signingKey and the optional previousSigningKey (RSA PEM). A previous half
// must be complete or absent. Nothing here is ever logged.
package keydir
