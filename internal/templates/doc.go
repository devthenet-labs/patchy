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
// comment can show that way (over GitHub's size limit, not UTF-8, or
// holding characters that render as nothing and can carry text, such as tag
// characters) is refused (ErrPlanRefused), and what the block does not show
// at a glance (other such characters, lines past its right edge, long runs
// of blank lines) the comment's header points to. Every other piece of
// agent-authored text that reaches GitHub from the intent flow — a plan's
// summary and dependencies, a pull request's summary — passes through
// Sanitize (or SanitizeInline) first: nothing it holds renders hidden from a
// reader, and no mention, issue reference or closing keyword in it is live. The intent renderers
// take plain values. What GitHub may read as plain text — the commit
// message, the pull request title, and the pull request body, which a
// merge or squash commit can carry — has every reference and mention
// broken apart as well, code spans included, since inline code neutralises
// nothing there.
//
// An intent that changes several repositories renders through the same
// templates: every multi-repository field (the plan prompt's trees, the
// build prompt's own repository and siblings, the repositories a plan
// comment, status comment and pull request body name) is rendered only
// when it names more than one repository, so a one-repository intent's
// prompts and comments are byte-identical to what they were before. The
// pull requests of such an intent are cross-linked by a comment on each
// (IntentSiblingsComment), never by editing a body, and a mixed ending is
// told by its own notice (IntentPartialNotice).
package templates
