// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewauth is the pure core of the preview sign-in relay: the
// OpenID provider every preview host's ALB signs viewers in through. It holds
// every decision the relay makes and none of its I/O. There is no HTTP
// server, no Kubernetes client, no Dex client and no signing key here; those
// are adapters over the ports in ports.go.
//
// The pieces:
//
//   - Callbacks is the redirect-URI grammar. The only redirect URI the relay
//     ever sends a code to is https://<label>.<host suffix>/oauth2/idpresponse,
//     spelled exactly that way, so a code can only go back to a preview host.
//   - ClientID, SlotOf and KeyRing.ClientSecret are the per-slot ALB clients.
//     Each slot has its own client, so a token issued to one slot's ALB never
//     redeems at another's.
//   - KeyRing holds the current and, during a rotation, the previous key
//     generation. Every token is sealed (AES-256-GCM through internal/sealed)
//     under a key of its own kind, with the kind and generation in its
//     authenticated data, and reads pa1.<kind>.<generation>.<blob>. A token of
//     one kind never opens as another.
//   - Grant, Access, LoginState and Session are the sealed payloads. Only codes
//     and refresh tokens carry the viewer's identity, and both go only to the
//     ALB's backchannel. An access token, which the ALB forwards to the
//     preview's own code, carries the binding and a pairwise subject and
//     nothing else.
//   - Bound and View are the binding: every token names its client, slot,
//     Preview UID and host label, and is honoured only while a live Preview
//     with exactly that UID and label holds that slot.
//   - KeyRing.Sub is the pairwise subject: stable for one viewer on one
//     Preview, unrelated across Previews, labels and slots, and never the
//     viewer's login.
//   - ParseAuthorize, ParseClientAuth, ParseTokenRequest and ParseBearer
//     validate the three OAuth endpoints' inputs; RedeemCode and CheckRefresh
//     judge a token request against its grant; CodeKey is a code's
//     single-use ledger key (its sealed JTI, not its spelling); TokenError and
//     UserinfoError map every failure to its answer, keeping a transient one
//     a 503 so the ALB does not sign the viewer out.
//   - LoginCookieName and LoginState.CheckCSRF are the sign-in double submit.
//     The login cookie's name carries a hash of the sealed state, so two
//     sign-ins in two tabs do not overwrite each other. LoginState.Resume
//     re-checks the Preview after the sign-in.
//   - ReviewFor is the authorisation decision's input: the viewer and the
//     Preview's Project, refused when the Project is unknown.
//
// Nothing the ALB forwards to a preview identifies the viewer or works
// anywhere else. That is the security boundary: preview code is written by an
// agent and must be treated as hostile to its viewers.
//
// The package imports only the standard library, internal/sealed and the
// api/v1alpha1 value types (a test pins that).
package previewauth
