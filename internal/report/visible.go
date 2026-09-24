// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// hidden names the class of a rune that does not render as itself — it
// renders as nothing, or it reorders the text around it — or returns ""
// for a rune that does. The classes, in the order they are tested:
//
//   - the control characters (Cc: C0, DEL and C1, NEL among them), tab,
//     line feed and carriage return included; checkVisible admits the first
//     two, and a carriage return that ends a CRLF, itself;
//   - U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR (Zl, Zp), which
//     YAML, JavaScript and many renderers break a line on;
//   - the tag characters, U+E0000-U+E007F, which spell ASCII that renders
//     as nothing but that a model reads;
//   - the variation selectors (U+FE00-U+FE0F, U+E0100-U+E01EF and the
//     Mongolian free variation selectors), which render as nothing and can
//     each carry a byte of smuggled data after any visible character;
//   - the format characters (Cf): zero-width spaces and joiners, the bidi
//     embeddings, overrides, isolates and marks that reorder text (the
//     Trojan Source characters), the soft hyphen, U+FEFF;
//   - the rest of Unicode's default-ignorable code points, which a renderer
//     draws as nothing: the Hangul fillers, the combining grapheme joiner,
//     and the ranges reserved for future invisible characters.
//
// This is stricter than command's note sanitiser, which keeps a ZWJ, a
// ZWNJ or a single variation selector as ordinary text: a note is cleaned
// and quoted to a model, while an intent report is refused, because a
// human approves, byte for byte, exactly what the model wrote.
func hidden(r rune) string {
	switch {
	case unicode.IsControl(r):
		return "a control character"
	case unicode.In(r, unicode.Zl, unicode.Zp):
		return "a line or paragraph separator"
	case r >= 0xe0000 && r <= 0xe007f:
		return "a tag character"
	case unicode.Is(unicode.Variation_Selector, r):
		return "a variation selector"
	case unicode.Is(unicode.Cf, r):
		return "a format character"
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
		return "a default-ignorable character"
	}
	return ""
}

// invisible reports a rune no one-line value may carry: any rune hidden
// names, tab and line feed included.
func invisible(r rune) bool { return hidden(r) != "" }

// hiddenError refuses an intent report for holding a byte its reader cannot
// see: its first rune that renders as nothing or reorders text, or its
// first byte that is not UTF-8, with where it sits.
type hiddenError struct {
	// kind is the report: "plan" or "build".
	kind string
	// line and column place the offending character: 1-based, line counted
	// in line feeds and column in characters.
	line, column int
	// r is the offending character, and class hidden's name for it; unset
	// when invalid.
	r     rune
	class string
	// invalid reports a byte, b, that begins no UTF-8 encoding.
	invalid bool
	b       byte
}

func (e *hiddenError) Error() string {
	if e.invalid {
		return fmt.Sprintf("report: %s: line %d, column %d: byte 0x%02X is not valid UTF-8",
			e.kind, e.line, e.column, e.b)
	}
	return fmt.Sprintf("report: %s: line %d, column %d: U+%04X is %s, which renders invisibly; "+
		"a report may hold only visible characters, tabs and line breaks", e.kind, e.line, e.column, e.r, e.class)
}

// checkVisible refuses a document holding anything its reader cannot see:
// a byte that is not UTF-8, or a rune hidden names. The approver of a plan
// reads it verbatim, and its digest — what the approval binds the build to —
// covers every byte, so every byte must be one the approver saw.
//
// Three control characters are admitted, because each shows as the
// whitespace it is: tab, line feed, and a carriage return immediately
// before a line feed. CRLF is accepted rather than normalised or refused:
// YAML, markdown and every renderer read it as the one line break it is,
// the document's bytes must stay exactly as written, and GitHub hands back
// comment bodies CRLF-terminated, so a revise round's quoted feedback needs
// no rewriting to pass. A lone carriage return is refused: a terminal
// returns to the start of the line on it and prints what follows over what
// came before.
func checkVisible(kind string, data []byte) error {
	line, column := 1, 1
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			return &hiddenError{kind: kind, line: line, column: column, invalid: true, b: data[i]}
		case r == '\n':
			line, column = line+1, 0
		case r == '\t':
		case r == '\r':
			if i+1 == len(data) || data[i+1] != '\n' {
				return &hiddenError{kind: kind, line: line, column: column, r: r,
					class: "a carriage return outside a CRLF line ending"}
			}
		default:
			if class := hidden(r); class != "" {
				return &hiddenError{kind: kind, line: line, column: column, r: r, class: class}
			}
		}
		i += size
		column++
	}
	return nil
}

// Layout bounds every intent report shares (checkLayout).
const (
	// PadMaxColumns bounds a gap of spaces and tabs between two characters
	// of one line, in columns (TabColumns).
	PadMaxColumns = 16
	// IndentMaxColumns bounds a line's indentation before its first
	// character, in columns.
	IndentMaxColumns = 64
	// TabColumns is the width a tab counts as: GitHub's default tab size,
	// and a terminal's. Any other space but U+0020 counts as two columns,
	// the widest of them (an ideographic space, an em quad).
	TabColumns = 8
	// CombiningMaxMarks bounds the combining marks in a row, nonspacing or
	// enclosing, stacked on one character.
	CombiningMaxMarks = 4
)

// layoutError refuses an intent report for text a reader would not see
// though every character of it is visible: text pushed out of view by
// whitespace, or a stack of combining marks drawn over the text around it.
type layoutError struct {
	// kind is the report: "plan" or "build".
	kind string
	// line and column place the start of the offending run, as hiddenError
	// places a character.
	line, column int
	// width is a gap's width in columns, and indent reports the gap is the
	// line's indentation; marks is a stack's height.
	width  int
	indent bool
	marks  int
}

func (e *layoutError) Error() string {
	at := fmt.Sprintf("report: %s: line %d, column %d: ", e.kind, e.line, e.column)
	switch {
	case e.marks > 0:
		return at + fmt.Sprintf("%d combining marks in a row, over %d: stacked that high they draw over "+
			"the text around them", e.marks, CombiningMaxMarks)
	case e.indent:
		return at + fmt.Sprintf("the line is indented %d columns, over %d (a tab counts as %d): a code "+
			"block does not wrap, so text indented that far sits out of the reader's view",
			e.width, IndentMaxColumns, TabColumns)
	}
	return at + fmt.Sprintf("a gap of %d columns of spaces and tabs before more text, over %d (a tab "+
		"counts as %d): a code block does not wrap, so text past a gap that wide sits out of the reader's view",
		e.width, PadMaxColumns, TabColumns)
}

// checkLayout refuses a document that lays visible text out where its
// reader would not see it. The approver reads a plan verbatim, in a code
// block, and GitHub renders a code block unwrapped, scrolling sideways —
// often with no scroll bar shown — so text after a wide gap of spaces or
// tabs sits past the block's right edge while the line before it looks
// complete; and a tall stack of combining marks draws over the lines around
// it. A build report becomes a pull request's description, whose code
// blocks render the same way. So:
//
//   - a gap of spaces and tabs before more text on its line is at most
//     PadMaxColumns wide, or IndentMaxColumns when it is the line's
//     indentation — a tab counting as TabColumns, any other space but
//     U+0020 as two; whitespace that ends a line hides nothing, and is not
//     bounded;
//   - at most CombiningMaxMarks combining marks (Mn, Me) stand in a row.
//
// checkVisible has run first, so data is valid UTF-8 and every carriage
// return in it ends a CRLF.
func checkLayout(kind string, data []byte) error {
	for i, line := range strings.Split(string(data), "\n") {
		if err := lineLayout(strings.TrimSuffix(line, "\r")); err != nil {
			err.kind, err.line = kind, i+1
			return err
		}
	}
	return nil
}

// lineLayout finds the first run in one line that checkLayout refuses, in
// the order the runs start.
func lineLayout(line string) *layoutError {
	column := 0
	gap, gapStart := 0, 0
	marks, marksStart := 0, 0
	for _, r := range line {
		column++
		switch {
		case r == '\t' || unicode.Is(unicode.Zs, r):
			if gap == 0 {
				gapStart = column
			}
			gap += padColumns(r)
		case gap > 0:
			if gap > padLimit(gapStart) {
				return &layoutError{column: gapStart, width: gap, indent: gapStart == 1}
			}
			gap = 0
		}
		if unicode.In(r, unicode.Mn, unicode.Me) {
			if marks == 0 {
				marksStart = column
			}
			marks++
			continue
		}
		if marks > CombiningMaxMarks {
			return &layoutError{column: marksStart, marks: marks}
		}
		marks = 0
	}
	if marks > CombiningMaxMarks {
		return &layoutError{column: marksStart, marks: marks}
	}
	return nil
}

// padColumns is the width a space or a tab counts as.
func padColumns(r rune) int {
	switch r {
	case ' ':
		return 1
	case '\t':
		return TabColumns
	}
	return 2
}

// padLimit is the widest a gap starting at column may be: the indentation
// bound at the start of a line, the gap bound after its first character.
func padLimit(column int) int {
	if column == 1 {
		return IndentMaxColumns
	}
	return PadMaxColumns
}
