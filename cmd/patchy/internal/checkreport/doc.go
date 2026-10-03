// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package checkreport is the report shape the `check` verbs share: a check
// passes, fails or is skipped with a reason, and a report renders as one
// tab-aligned line per check, or, under -o json and -o yaml, as the
// caller's own report data.
//
// Each engine (imagecheck, projectcheck) keeps its own report type, since
// what qualifies a check differs (a platform there, a repository here);
// what they share is the status vocabulary, the escaping that keeps a
// reason inert on a terminal, and the rendering, so the two commands read
// the same and a script parses them the same way.
package checkreport
