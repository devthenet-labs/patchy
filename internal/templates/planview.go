// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"unicode"
)

// This file reckons what an approver takes in of a plan shown verbatim in a
// code block (RenderPlanComment). Nothing in the block renders, yet three
// things can still keep part of the plan from the approver: characters that
// render as nothing, lines wider than the block, which GitHub does not
// wrap, and long stretches of blank lines. Characters that render as
// nothing and can carry text a model reads (tag characters, bidi controls,
// stray variation selectors) no comment can show, so the plan is refused.
// The rest the approver can find, and the comment says how much there is.

// planViewColumns is how much of a line GitHub shows of a code block in a
// comment on a common desktop screen before the line runs past the block's
// right edge; text past it is read only by scrolling the block sideways.
const planViewColumns = 100

// planBlankLines is the most blank lines in a row a plan holds without
// comment. A longer run with more of the plan after it is reported: that
// much empty block can look like the plan's end.
const planBlankLines = 3

// planView is what of a plan an approver cannot take in at a glance.
type planView struct {
	// unshowable counts the characters no comment can show (unshowable).
	unshowable int
	// invisible counts the other characters that render as nothing, less
	// the ones an emoji or a script's joining is made of (benignInvisible).
	invisible int
	// wide counts the lines with text past planViewColumns.
	wide int
	// blankRun is the longest run of more than planBlankLines blank lines
	// with more of the plan after it, or 0.
	blankRun int
}

// viewPlan reckons the planView of a UTF-8 report.
func viewPlan(report string) planView {
	var v planView
	runes := []rune(report)
	for i, r := range runes {
		switch {
		case unshowable(runes, i):
			v.unshowable++
		case r != '\r' && invisible(r) && !benignInvisible(runes, i):
			v.invisible++
		}
	}
	blank := 0
	for _, line := range strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(report), "\n") {
		if !strings.ContainsFunc(line, inkRune) {
			blank++
			continue
		}
		if blank > planBlankLines {
			v.blankRun = max(v.blankRun, blank)
		}
		blank = 0
		if textColumns(line) > planViewColumns {
			v.wide++
		}
	}
	return v
}

// unshowable reports a character that no comment can show as the build
// agent reads it, and that no plan written for a person needs: a Unicode tag
// character (U+E0000 to U+E007F), which renders as nothing and which a model
// reads as the ASCII it shadows; a bidi embedding, override or isolate
// control (U+202A to U+202E, U+2066 to U+2069), which reorders the text an
// approver sees around it; and any variation selector but a text or emoji
// presentation selector (U+FE0E, U+FE0F) directly after a visible
// character, since a run of them, or one of the others, can carry bytes a
// model decodes.
func unshowable(runes []rune, i int) bool {
	r := runes[i]
	switch {
	case r >= 0xE0000 && r <= 0xE007F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	case unicode.Is(unicode.Variation_Selector, r):
		return (r != 0xFE0E && r != 0xFE0F) || i == 0 || !visibleRune(runes[i-1])
	}
	return false
}

// benignInvisible reports a character that renders as nothing but is part
// of what the approver does see, so counting it would only teach approvers
// to ignore the count: a presentation selector directly after a visible
// character (U+FE0F in a red heart, U+2764 U+FE0F), and a zero-width joiner
// or non-joiner between two visible characters beyond ASCII (an emoji
// sequence, or joining in Persian and Indic scripts), its left side reached
// through a presentation selector (U+2764 U+FE0F U+200D U+1F525).
func benignInvisible(runes []rune, i int) bool {
	switch runes[i] {
	case 0xFE0E, 0xFE0F:
		return i > 0 && visibleRune(runes[i-1])
	case 0x200C, 0x200D:
		j := i - 1
		if j >= 0 && (runes[j] == 0xFE0E || runes[j] == 0xFE0F) {
			j--
		}
		return j >= 0 && i+1 < len(runes) && visibleBeyondASCII(runes[j]) && visibleBeyondASCII(runes[i+1])
	}
	return false
}

// visibleRune reports a character that shows as something: neither
// whitespace nor a character that renders as nothing.
func visibleRune(r rune) bool {
	return !unicode.IsSpace(r) && !invisible(r)
}

func visibleBeyondASCII(r rune) bool {
	return r > unicode.MaxASCII && visibleRune(r)
}

// blankGlyph reports a character that is neither whitespace nor one that
// renders as nothing, yet draws as an empty cell: U+2800 BRAILLE PATTERN
// BLANK, which GitHub's code fonts take from a braille font, a cell with no
// dots in it. A line of them looks blank, and text after them far off.
func blankGlyph(r rune) bool {
	return r == 0x2800
}

// inkRune reports a character that draws something an approver can see on
// the page: visible (visibleRune), and not a blank glyph. Lines and their
// ends are reckoned by it, so padding made of blank glyphs counts as the
// whitespace it looks like.
func inkRune(r rune) bool {
	return visibleRune(r) && !blankGlyph(r)
}

// textColumns is the column just past a line's last character that draws
// something (inkRune), as a monospace code block lays the line out: a tab
// to the next multiple of eight, a wide character two columns (wideRune), a
// character that renders as nothing or a combining mark none, and any other
// one.
func textColumns(line string) int {
	col, end := 0, 0
	for _, r := range line {
		switch {
		case r == '\t':
			col += 8 - col%8
		case invisible(r) || unicode.In(r, unicode.Mn, unicode.Me):
		case wideRune(r):
			col += 2
		default:
			col++
		}
		if inkRune(r) {
			end = col
		}
	}
	return end
}

// wideRune approximates the East Asian Wide and Fullwidth characters a
// monospace font draws two columns wide: Hangul, the CJK blocks (the
// ideographic space and CJK punctuation among them), fullwidth forms and
// emoji pictographs. It errs wide, so a line is sooner counted as running
// past the block's edge than not.
func wideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo leading consonants
		r >= 0x2E80 && r <= 0xA4CF,   // CJK radicals through Yi
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F,   // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60,   // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,   // fullwidth signs
		r >= 0x1F300 && r <= 0x1FAFF, // pictographs and emoji
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions
		return true
	}
	return false
}
