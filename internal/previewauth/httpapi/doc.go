// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package httpapi is the preview sign-in relay's HTTP surface: the OpenID
// provider every preview host's ALB signs viewers in through, and the one
// Dex client callback. It is wiring over the pure core (internal/previewauth)
// and its ports; every decision about a token, a redirect URI or a binding
// is the core's.
//
// Endpoints (relay host; GET and POST only, 405 otherwise; bodies capped at
// 8 KiB; repeated parameters refused):
//
//   - GET /.well-known/openid-configuration and GET /jwks: discovery and the
//     ID-token keys, cacheable for five minutes.
//   - GET /authorize: the browser arrives from a preview host's ALB. Every
//     refusal is a page on the relay host, never a redirect, because how the
//     ALB treats an OAuth error redirect is unknown. With no relay session the
//     viewer is sent to Dex (PKCE, nonce, a sealed login state, a login
//     cookie per sign-in); with one, the access review runs and a code goes
//     back to the preview host's /oauth2/idpresponse.
//   - GET /dex/callback: the one fixed Dex redirect URI. Double-submit
//     check, code exchange, a relay session cookie, then the authorize
//     request resumes against the same Preview.
//   - POST /token: the ALB's backchannel only (client_secret_basic or
//     client_secret_post, exactly one). Codes are single-use through the
//     ledger; refresh tokens do not rotate. A transient failure is 503, never
//     invalid_grant, so the ALB keeps its session.
//   - GET|POST /userinfo: {"sub":"<pairwise>"} and nothing else.
//   - POST /logout: ends the relay session (same-origin only).
//
// Every response carries HSTS (without includeSubDomains), nosniff,
// a CSP with frame-ancestors 'none', no-referrer and, but for discovery and
// the JWKS, Cache-Control: no-store. Each source address is rate limited
// (critique F10). One audit line per request names the endpoint, slot, host
// label, Project, result, pairwise subject and a short hash of the token id;
// never a token, code, state, cookie or login, except that a refused access
// review is logged at warn with the viewer's login for operators.
package httpapi
