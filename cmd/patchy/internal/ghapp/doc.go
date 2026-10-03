// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package ghapp is the engine behind `patchy setup github-app`: it creates
// the GitHub App patchy authenticates as through GitHub's App manifest flow,
// and turns the credentials GitHub hands back into the Secret manifest the
// Forge and Integration resources read (internal/ghsecret's keys).
//
// The flow has four parts, each its own seam:
//
//   - Build makes the manifest: only the permissions and events the chosen
//     Features use. Intents take theirs from internal/intentperm, the table
//     intent-controller proves a Project's grants against, so the two cannot
//     drift.
//   - Callback is a one-shot loopback server: it serves the page that posts
//     the manifest to GitHub, and takes the code GitHub sends the browser
//     back with, checked against the state it was started with. ParseCode is
//     the same check for a code pasted in by hand.
//   - Convert exchanges the code (POST /app-manifests/{code}/conversions, an
//     unauthenticated endpoint) for the App and its credentials, over plain
//     net/http: the CLI links no GitHub client. The OAuth client ID and
//     secret GitHub also returns are never decoded.
//   - SecretManifest and WriteFile render the Secret and write it with mode
//     0600, refusing to replace a file unless told to.
//
// The private key and webhook secret live only in Credentials, whose
// formatting is redacted, and leave the process only through the Secret
// manifest.
package ghapp
