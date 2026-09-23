// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
	"unicode/utf8"
)

func TestPrintable(t *testing.T) {
	cases := map[string]string{
		"git version 2.51.0":                "git version 2.51.0",
		"two\nlines\tand a tab":             "two\nlines\tand a tab",
		"naïve ✓":                           "naïve ✓",
		"\x1b[2A\x1b[2K\rPASS":              `\x1b[2A\x1b[2K\x0dPASS`,
		"\x1b]52;c;cHduZWQ=\x07":            `\x1b]52;c;cHduZWQ=\x07`,
		"\u009b31m C1 CSI, DEL \x7f":        `\x9b31m C1 CSI, DEL \x7f`,
		"lone byte \x9b and \xff not UTF-8": `lone byte \x9b and \xff not UTF-8`,
		"a literal backslash-x \\x1b stays": `a literal backslash-x \x1b stays`,
	}
	for in, want := range cases {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPrintableProperty: for any bytes an image could print, the result is
// valid UTF-8 with no control character but newline and tab, printing it
// again changes nothing, and text that needed no escaping is untouched.
func TestPrintableProperty(t *testing.T) {
	alphabet := []string{"a", " ", "\n", "\t", "\r", "\x1b", "[", "\x07", "\x7f", "\u009b", "\x9b", "\xff", "é", "✓", `\`}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20260923)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			var b strings.Builder
			for range r.Intn(24) {
				b.WriteString(alphabet[r.Intn(len(alphabet))])
			}
			args[0] = reflect.ValueOf(b.String())
		},
	}
	inert := func(s string) bool {
		got := printable(s)
		if !utf8.ValidString(got) || printable(got) != got {
			return false
		}
		for _, r := range got {
			if r != '\n' && r != '\t' && unicode.IsControl(r) {
				return false
			}
		}
		clean := utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
			return r != '\n' && r != '\t' && unicode.IsControl(r)
		})
		return !clean || got == s
	}
	if err := quick.Check(inert, cfg); err != nil {
		t.Error(err)
	}
}
