// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
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
