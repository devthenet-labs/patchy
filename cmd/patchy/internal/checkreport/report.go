// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checkreport

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
)

// Status is one check's outcome.
type Status string

// The three outcomes. Only Fail makes the check as a whole fail; Skip means
// the check could not or need not run, and its reason says which.
const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
)

// Line is one check as the table prints it.
type Line struct {
	Status Status
	// Cells are the check's name and then its qualifiers (a platform, a
	// repository), in column order; an empty qualifier is left out.
	Cells []string
	// Reason closes the line, folded onto it.
	Reason string
}

// Render writes a report. Under -o json and -o yaml it encodes data, the
// caller's whole report; under every other format it writes lines, one per
// check: the status, the cells, then the reason, tab-aligned.
func Render(w io.Writer, format printer.Format, data any, lines []Line) error {
	switch format {
	case printer.FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case printer.FormatYAML:
		out, err := yaml.Marshal(data)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, l := range lines {
		cells := []string{string(l.Status)}
		for _, c := range l.Cells {
			if c != "" {
				cells = append(cells, c)
			}
		}
		if _, err := fmt.Fprintln(tw, strings.Join(append(cells, oneLine(l.Reason)), "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// Failed counts the failed lines.
func Failed(lines []Line) int {
	n := 0
	for _, l := range lines {
		if l.Status == Fail {
			n++
		}
	}
	return n
}

// oneLine keeps a reason on its check's line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Printable writes every control character in s but newline and tab, and
// every byte that is not UTF-8, as a visible \xNN escape. A report is read
// on a terminal, and piped from -o json into jq -r, and its reasons carry
// text from outside the CLI (what an image's commands printed, a registry's
// or a cluster's error): an escape sequence in it must show as what it is,
// never move the cursor over the lines above it or reach the clipboard.
// encoding/json alone escapes only C0, not the C1 controls (U+0080 to
// U+009F) a terminal also obeys.
func Printable(s string) string {
	escape := func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }
	if utf8.ValidString(s) && !strings.ContainsFunc(s, escape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escape(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
