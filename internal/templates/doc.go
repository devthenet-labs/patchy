// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package templates renders every piece of markdown patchy writes — issue
// bodies, comments, pull-request bodies, and the agent-facing handoff files —
// from embedded text/templates, so the artifacts stay consistent across the
// estate.
//
// The issue body carries a machine-readable manifest block (an HTML comment
// holding JSON) that is the authoritative record of the alerts accumulated
// into the issue; the human-readable body is re-rendered from the manifest on
// every accumulation, never string-edited. The accumulator (source-controller)
// owns the body; everyone else appends comments.
//
// The plan an approver approves is shown verbatim — the exact bytes the
// build agent reads — in a fenced code block no line of it can close, so
// what the approver sees depends on no list of markdown tricks; a plan no
// comment can show that way (over GitHub's size limit, or not UTF-8) is
// refused (ErrPlanRefused). Every other piece of agent-authored text that
// reaches GitHub from the intent flow — a plan's summary and dependencies, a
// pull request's summary — passes through Sanitize (or SanitizeInline)
// first: nothing it holds renders hidden from a reader, and no mention,
// issue reference or closing keyword in it is live. The intent renderers
// take plain values, and the commit message, which GitHub reads as plain
// text, breaks references apart instead.
package templates
