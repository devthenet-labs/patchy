// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package authz resolves what an identity may do to findings. It asks the
// cluster directly — SubjectAccessReviews with the identity's user and
// groups — so the grant grammar is ordinary Kubernetes RBAC: native get on
// findings gates viewing, and the custom verbs approve, suspend, and resume
// on findings.patchy.bitwisemedia.uk gate the actions. Reviews run as the
// server's own ServiceAccount (which needs only create on
// subjectaccessreviews), never via impersonation.
//
// ProjectReviewer does the same per Project, on virtual subresources of
// projects.patchy.bitwisemedia.uk that no API server serves: projects/intents
// and projects/transcripts are the intents views' read tiers (Tiers), and
// projects/previews is the right to open a Project's live previews
// (Allowed).
package authz
