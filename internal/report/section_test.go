// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"math/rand"
	"strings"
	"testing"
)

func TestSection(t *testing.T) {
	tests := []struct {
		name, body, want string
		ok               bool
	}{
		{name: "absent", body: "## What changed\n\nA file.\n"},
		{name: "empty body"},
		{
			name: "last section",
			body: "## What changed\n\nA file.\n\n## Working notes\n\n- VERSION is at the root.\n- go test works.\n",
			want: "- VERSION is at the root.\n- go test works.", ok: true,
		},
		{
			name: "up to the next level-2 heading",
			body: "## Working notes\n\nNotes.\n\n### A subsection\n\nMore.\n\n## Verification\n\nRan.\n",
			want: "Notes.\n\n### A subsection\n\nMore.", ok: true,
		},
		{
			name: "a level-1 heading ends it too",
			body: "## Working notes\nNotes.\n# Appendix\nOther.\n",
			want: "Notes.", ok: true,
		},
		{
			name: "case, indentation and closing hashes",
			body: "   ## working NOTES ##  \r\nNotes.\r\n",
			want: "Notes.", ok: true,
		},
		{
			name: "the first of two",
			body: "## Working notes\nfirst\n## Working notes\nsecond\n",
			want: "first", ok: true,
		},
		{
			name: "a heading in a fence is text",
			body: "## Working notes\n\n````markdown\n## Verification\n```\nstill quoted\n````\n\nAfter.\n## Next\nx\n",
			want: "````markdown\n## Verification\n```\nstill quoted\n````\n\nAfter.", ok: true,
		},
		{
			name: "a quoted heading is not the section",
			body: "~~~\n## Working notes\n~~~\n## Other\nx\n",
		},
		{
			name: "an unclosed fence runs to the end",
			body: "## Working notes\n```\n## Not a heading\n",
			want: "```\n## Not a heading", ok: true,
		},
		{
			name: "indented four columns is code",
			body: "    ## Working notes\nx\n",
		},
		{
			name: "a level-3 heading of the name is not it",
			body: "### Working notes\nx\n",
		},
		{
			name: "no space after the hashes is not a heading",
			body: "##Working notes\nx\n",
		},
		{
			name: "empty section",
			body: "## Working notes\n\n## Next\n",
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Section(tt.body, WorkingNotesHeading)
			if got != tt.want || ok != tt.ok {
				t.Errorf("Section() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// Whatever the body, a section is a contiguous part of it, and a body with
// no line starting with the heading has none.
func TestSectionSeededProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(propertySeed))
	lines := []string{
		"## Working notes", "## working notes #", "## Other", "# Top", "### Sub", "```", "````", "~~~",
		"```go", "text", "", "    ## Working notes", "- item", "\r",
	}
	for range propertyCount {
		var b strings.Builder
		for range rng.Intn(12) {
			b.WriteString(lines[rng.Intn(len(lines))])
			b.WriteString("\n")
		}
		body := b.String()
		got, ok := Section(body, WorkingNotesHeading)
		if !strings.Contains(body, got) {
			t.Fatalf("Section(%q) = %q, not part of the body", body, got)
		}
		if !ok && got != "" {
			t.Fatalf("Section(%q) = %q without its heading", body, got)
		}
		if !strings.Contains(strings.ToLower(body), "## working notes") && ok {
			t.Fatalf("Section(%q) found a heading the body lacks", body)
		}
	}
}
