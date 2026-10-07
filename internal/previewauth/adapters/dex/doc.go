// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package dex is the preview sign-in relay's previewauth.Upstream: an OIDC
// client of the installation's Dex with one fixed redirect URI
// (<relay>/dex/callback), PKCE S256 and a nonce on every sign-in, and the ID
// token verified for the relay's OWN client id. Its claims are mapped with
// web/auth.MapClaims, the one claims-to-identity mapping the dashboard uses,
// under the same posture the dashboard's intents views require: username and
// groups prefixes, and a verified email when the username is the email.
//
// Discovery is lazy and retried: a relay that starts while Dex is down
// still serves token refreshes and userinfo, and only new sign-ins fail
// (503) until Dex answers.
package dex
