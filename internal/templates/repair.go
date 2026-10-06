// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import "strings"

// RepairPrompt is the data for the one message a stage's agent is sent, in
// its own session, when patchy refused the report it wrote or found none:
// fix the report, change nothing else. agent-runner sends it at most a few
// times before giving the stage up as report_missing or report_invalid.
//
// It carries patchy's fixed text, the report's path and the reason the
// report was refused, and nothing else: not the request, the plan or the
// report itself, which the agent can read. Error is UNTRUSTED — it is
// derived from the file the agent wrote and can quote it (an unknown key,
// a YAML snippet, a repository URL the plan named) — so RenderRepairPrompt
// bounds it (RepairErrorMaxBytes), strips its control and format
// characters and fences it under a statement that it is data, not
// instructions, exactly as a previous attempt's detail is quoted.
type RepairPrompt struct {
	// ReportPath is the report the stage's instructions asked for.
	ReportPath string
	// Missing is set when there was no report to read at all.
	Missing bool
	// Error is why the report was refused (or could not be read).
	Error string
	// Round is this repair's ordinal, of at most Rounds.
	Round, Rounds int
	// CommitScriptPath names commit.sh on a stage that writes the working
	// tree; empty on a read-only stage. A repair there may write it as well
	// as the report, and nothing else.
	CommitScriptPath string
}

// RepairErrorMaxBytes bounds the reason a repair prompt quotes; a longer
// one is cut on a rune boundary and marked as a previous attempt's detail
// is.
const RepairErrorMaxBytes = PreviousDetailMaxBytes

// quotableError makes a refusal reason safe to quote in a prompt: valid
// UTF-8, LF line breaks, no control or format character but tab, bounded.
func quotableError(s string) string {
	s = plainText(s)
	if len(s) > RepairErrorMaxBytes {
		s = cutRunes(s, RepairErrorMaxBytes) + previousDetailCut
	}
	return strings.Trim(s, "\n")
}

// RenderRepairPrompt renders the report-repair prompt.
func RenderRepairPrompt(p RepairPrompt) (string, error) {
	p.Error = quotableError(p.Error)
	return render("prompt_repair.md.tmpl", p)
}
