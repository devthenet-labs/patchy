// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package changeset validates an agent's changeset (envelope.Changeset)
// before a controller makes any forge call with it: the base it was diffed
// against, the shape of every path, the modes and content of its upserts,
// and — for a run whose process a repository-declared image controlled — an
// entry cap, no control characters in a path and no CI definitions. Rules
// carry an optional deny list of repository-root directories no path may
// touch, which IntentRules fill for intent runs.
//
// It is shared by the controllers that push changesets
// (internal/controller/remediation for a Finding, intent-controller for an
// intent), so neither engine imports the other. It is pure: no I/O, no
// Kubernetes or forge types, nothing from internal/controller.
package changeset
