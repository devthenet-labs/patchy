// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import "strings"

// The optional body sections an intent report may carry for the agents after
// it. Neither is part of a report's contract: no parser requires, bounds or
// refuses either beyond the body bound every report holds, and a report
// without them is as valid as one with them.
const (
	// WorkingNotesHeading titles a build report's working notes: what the
	// next agent on the intent should know (where the code is, commands
	// that worked in the sandbox, decisions, what was tried and rejected,
	// open questions, the state of the work). patchy carries the latest
	// notes into the intent's next round and into a replan's context file.
	WorkingNotesHeading = "Working notes"
	// BuilderNotesHeading titles a plan's notes for the builder: what the
	// build should know that the plan's steps do not say. They are part of
	// the plan, approved with it and read by the build as part of it.
	BuilderNotesHeading = "Notes for the builder"
)

// Section returns the text of a markdown body's first level-2 section titled
// heading (an ATX "## " heading, matched without regard to case, trailing
// spaces or closing hashes), up to the next level-1 or level-2 heading or the
// end of the body, trimmed of surrounding blank lines; ok reports whether the
// heading was found. Lines inside fenced code blocks are never headings, so a
// section quoting markdown holds its own quoted headings.
func Section(body, heading string) (text string, ok bool) {
	var b strings.Builder
	var fence string
	for line := range strings.Lines(body) {
		bare := strings.TrimRight(line, "\r\n")
		if fence == "" {
			if title, level := atxHeading(bare); level == 1 || level == 2 {
				if ok {
					break
				}
				ok = level == 2 && strings.EqualFold(title, heading)
				continue
			}
		}
		if f, closing := codeFence(bare, fence); f != "" || closing {
			fence = f
		}
		if ok {
			b.WriteString(line)
		}
	}
	return strings.Trim(b.String(), "\r\n"), ok
}

// atxHeading parses an ATX heading line (at most 3 columns of indentation, 1
// to 6 hashes, then a space or nothing) into its title and level; level is 0
// for any other line.
func atxHeading(line string) (title string, level int) {
	rest, ok := cutIndent(line)
	if !ok {
		return "", 0
	}
	level = len(rest) - len(strings.TrimLeft(rest, "#"))
	rest = rest[level:]
	if level == 0 || level > 6 || rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", 0
	}
	title = strings.TrimSpace(rest)
	// A closing sequence of hashes is not part of the title.
	if trimmed := strings.TrimRight(title, "#"); trimmed == "" || strings.HasSuffix(trimmed, " ") ||
		strings.HasSuffix(trimmed, "\t") {
		title = strings.TrimSpace(trimmed)
	}
	return title, level
}

// codeFence tracks fenced code blocks: open is the fence a line inside one
// must close ("" outside any). It returns the fence a line opens (when open
// is ""), or closing true when the line closes open.
func codeFence(line, open string) (opens string, closing bool) {
	rest, ok := cutIndent(line)
	if !ok || rest == "" || rest[0] != '`' && rest[0] != '~' {
		return "", false
	}
	n := len(rest) - len(strings.TrimLeft(rest, rest[:1]))
	if n < 3 {
		return "", false
	}
	if open != "" {
		return "", rest[0] == open[0] && n >= len(open) && strings.TrimSpace(rest[n:]) == ""
	}
	// A backtick fence's info string may not hold a backtick.
	if rest[0] == '`' && strings.Contains(rest[n:], "`") {
		return "", false
	}
	return rest[:n], false
}

// cutIndent strips up to 3 columns of leading spaces; ok is false for a line
// indented further, which markdown reads as code.
func cutIndent(line string) (string, bool) {
	rest := strings.TrimLeft(line, " ")
	return rest, len(line)-len(rest) <= 3
}
