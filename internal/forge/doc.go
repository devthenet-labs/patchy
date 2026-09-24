// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package forge resolves a finding's repository URL to the Forge custom
// resource whose credentials may act on it, and mints per-operation scoped
// tokens from that Forge's secret.
//
// It is the single shared seam for forge access: source-controller resolves
// read scope (clone/tarball), remediation-controller resolves write scope
// (branch push, pull request). TokenWith mints a token for an explicit
// permission set (contents, issues, pull requests) scoped to one repository,
// cached per repository and permission set until shortly before it expires:
// the per-operation seam for callers that write more than contents.
//
// Matching is host equality, then the org allowlist, then the repository
// regexes; when several Forges match, the more constrained spec wins (orgs
// set beats unset, then repositories set beats unset) and a remaining tie is
// ErrAmbiguous — an operator configuration error, never silently resolved.
package forge
