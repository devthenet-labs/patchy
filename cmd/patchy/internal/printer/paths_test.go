// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package printer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// failWriter fails every write, the way a closed pipe does.
type failWriter struct{}

var errClosed = errors.New("closed pipe")

func (failWriter) Write([]byte) (int, error) { return 0, errClosed }

func TestAccessors(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, FormatWide, true)
	if p.Format() != FormatWide {
		t.Errorf("Format() = %q, want wide", p.Format())
	}
	if !p.Color() {
		t.Error("Color() = false for a styled printer")
	}
	if p.Out() != &buf {
		t.Error("Out() did not return the writer the printer was built with")
	}
	// Markdown is never styled, whatever the caller asked for.
	if New(&buf, FormatMarkdown, true).Color() {
		t.Error("markdown printer reports colour")
	}
}

func TestFormatsListsEveryFormat(t *testing.T) {
	got := Formats()
	want := []string{"table", "wide", "json", "yaml", "name", "markdown"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Formats() = %v, want %v", got, want)
	}
	for _, f := range got {
		parsed, err := ParseFormat(" " + strings.ToUpper(f) + " ")
		if err != nil || string(parsed) != f {
			t.Errorf("ParseFormat(%q) = %q, %v", f, parsed, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil || !strings.Contains(err.Error(), "markdown") {
		t.Errorf("ParseFormat(xml) error = %v, want one listing the formats", err)
	}
}

func TestDocFieldfAndUntitledSection(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, FormatMarkdown, false)
	// A field before any Section opens an untitled one rather than panicking.
	err := p.Doc().
		Fieldf("Attempts", "%d investigation, %d remediation", 2, 1).
		Body("   \n\t").
		Body("plain body").
		Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "- **Attempts:** 2 investigation, 1 remediation") {
		t.Errorf("Fieldf not rendered:\n%s", got)
	}
	if strings.Contains(got, "## ") {
		t.Errorf("untitled section got a heading:\n%s", got)
	}
	if !strings.Contains(got, "\nplain body\n") {
		t.Errorf("body not separated from fields:\n%q", got)
	}
}

func TestDocTerminalAlignsKeysPerSection(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, FormatTable, false)
	err := p.Doc().
		Section("A").Field("K", "1").Field("Longer key", "2").
		Section("B").Field("X", "3").Body("report text").
		Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"  K:           1\n",
		"  Longer key:  2\n",
		// Section B's keys are aligned on their own, not stretched by A's.
		"  X:  3\n",
		"\nreport text\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%q", want, got)
		}
	}
}

func TestStyledDocRendersBodyThroughGlamour(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, FormatTable, true)
	if err := p.Doc().Section("Report").Field("Verdict", "remediate").Body("**bold** words").Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "\x1b") {
		t.Errorf("styled doc emitted no escapes:\n%q", got)
	}
	if strings.Contains(got, "**bold**") || !strings.Contains(got, "bold") {
		t.Errorf("body not rendered as markdown:\n%q", got)
	}
}

// TestWriteErrorsSurface: every rendering path reports a failed writer rather
// than swallowing it, so a broken stdout fails the command.
func TestWriteErrorsSurface(t *testing.T) {
	table := testTable()
	cases := []struct {
		name string
		run  func(p *Printer) error
		fmt  Format
		col  bool
	}{
		{"plain table", func(p *Printer) error { return p.Table(table) }, FormatTable, false},
		{"styled table", func(p *Printer) error { return p.Table(table) }, FormatTable, true},
		{"markdown table", func(p *Printer) error { return p.Table(table) }, FormatMarkdown, false},
		{"tables heading", func(p *Printer) error {
			return p.Tables([]Group{{Title: "Findings", Table: table}})
		}, FormatTable, false},
		{"markdown doc", func(p *Printer) error { return p.Doc().Field("k", "v").Render() }, FormatMarkdown, false},
		{"terminal doc", func(p *Printer) error { return p.Doc().Field("k", "v").Render() }, FormatTable, false},
		{"markdown body", func(p *Printer) error { return p.Markdown("# hi") }, FormatTable, false},
		{"names", func(p *Printer) error { return p.Objects([]any{1}, []string{"a/b"}) }, FormatName, false},
		{"yaml", func(p *Printer) error { return p.Objects([]any{map[string]int{"a": 1}}, nil) }, FormatYAML, false},
		{"json", func(p *Printer) error { return p.Objects([]any{map[string]int{"a": 1}}, nil) }, FormatJSON, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(New(failWriter{}, tc.fmt, tc.col))
			if err == nil {
				t.Fatal("write failure was swallowed")
			}
		})
	}
}

// TestTablesStopAtFirstError covers a failure partway through a listing:
// the separator before the second group is a write too.
func TestTablesStopAtFirstError(t *testing.T) {
	w := &limitWriter{left: 1}
	p := New(w, FormatMarkdown, false)
	err := p.Tables([]Group{{Title: "A", Table: testTable()}, {Title: "B", Table: testTable()}})
	if err == nil {
		t.Fatal("expected the write error to surface")
	}
}

// limitWriter accepts left writes, then fails.
type limitWriter struct{ left int }

func (l *limitWriter) Write(b []byte) (int, error) {
	if l.left <= 0 {
		return 0, errClosed
	}
	l.left--
	return len(b), nil
}

func TestObjectsRefusesHumanFormats(t *testing.T) {
	for _, f := range []Format{FormatTable, FormatWide, FormatMarkdown} {
		var buf bytes.Buffer
		err := New(&buf, f, false).Objects([]any{map[string]string{"a": "b"}}, []string{"x"})
		if err == nil || !strings.Contains(err.Error(), string(f)) {
			t.Errorf("Objects under %s: err = %v, want a refusal naming the format", f, err)
		}
		if buf.Len() != 0 {
			t.Errorf("Objects under %s wrote %q", f, buf.String())
		}
	}
}

func TestObjectsYAMLUnencodable(t *testing.T) {
	var buf bytes.Buffer
	err := New(&buf, FormatYAML, false).Objects([]any{map[string]any{"ch": make(chan int)}}, nil)
	if err == nil || !strings.Contains(err.Error(), "encode yaml") {
		t.Errorf("err = %v, want an encode yaml failure", err)
	}
}

func TestEmptyTablePrintsNothing(t *testing.T) {
	var buf bytes.Buffer
	tbl := &metav1.Table{ColumnDefinitions: testTable().ColumnDefinitions}
	if err := New(&buf, FormatTable, false).Table(tbl); err != nil {
		t.Fatalf("Table: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("an empty table wrote %q; it must stay silent on stdout", buf.String())
	}
}

func TestStyleFor(t *testing.T) {
	cases := []struct {
		column, value string
		want          any
	}{
		{"Severity", "Critical", &styleCritical},
		{"priority", "high", &styleHigh},
		{"SEVERITY", "medium", &styleMedium},
		{"SEVERITY", "low", &styleLow},
		{"PRIORITY", "none", &styleLow},
		{"VERDICT", "remediate", &styleGood},
		{"VERDICT", "manual", &styleWaiting},
		{"VERDICT", "ignore", &styleDim},
		{"PHASE", "Remediated", &styleGood},
		{"PHASE", "Complete", &styleGood},
		{"SUCCESS", "true", &styleGood},
		{"STATE", "merged", &styleGood},
		{"PHASE", "Failed", &styleBad},
		{"SUCCESS", "false", &styleBad},
		{"PHASE", "Dismissed", &styleDim},
		{"PHASE", "AwaitingApproval", &styleWaiting},
		{"PHASE", "HandedOff", &styleWaiting},
		{"PHASE", "InReview", &styleWaiting},
		{"PHASE", "Queued", nil},
		// A meaningful value in a column that carries no status is left alone.
		{"NAME", "critical", nil},
	}
	for _, tc := range cases {
		got := styleFor(tc.column, tc.value)
		if tc.want == nil {
			if got != nil {
				t.Errorf("styleFor(%q, %q) styled a value that should be plain", tc.column, tc.value)
			}
			continue
		}
		if any(got) != tc.want {
			t.Errorf("styleFor(%q, %q) picked the wrong style", tc.column, tc.value)
		}
	}
}

func TestPaint(t *testing.T) {
	if got := paint(false, styleHeader, "x"); got != "x" {
		t.Errorf("paint off = %q", got)
	}
	if got := paint(true, styleHeader, ""); got != "" {
		t.Errorf("paint of empty = %q, want empty", got)
	}
	if got := paint(true, styleHeader, "x"); got == "x" || !strings.Contains(got, "x") {
		t.Errorf("paint on = %q, want styled x", got)
	}
}

func TestWrapWidth(t *testing.T) {
	if got := wrapWidth(&bytes.Buffer{}); got != fallbackWrap {
		t.Errorf("non-file wrap = %d, want %d", got, fallbackWrap)
	}
	// A regular file is not a terminal, so its size query fails.
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got := wrapWidth(f); got != fallbackWrap {
		t.Errorf("regular-file wrap = %d, want %d", got, fallbackWrap)
	}
	if IsTerminal(f) {
		t.Error("a regular file reported as a terminal")
	}
}

func TestColorTermDumb(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if Color(os.Stdout, false) {
		t.Error("styled under TERM=dumb")
	}
}
