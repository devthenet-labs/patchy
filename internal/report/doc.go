// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package report defines the agent-written report contracts — the
// investigation and remediation reports of the security Finding flow, and
// the plan and build reports of the intent flow — as YAML frontmatter
// schemas with strict, validated parsing. The prompts promise these exact
// shapes; unknown keys, missing required fields, and out-of-range values are
// errors, because everything downstream (routing, priority, budgets, what a
// human approves) is derived from these files. The intent reports are also
// bounded end to end — every field, the body and the whole document —
// because their text is posted to GitHub and recorded on status, and they
// hold only visible text: a document with a character that renders
// invisibly or reorders text, anywhere, is refused, because a human
// approves a plan by reading every byte of it.
package report
