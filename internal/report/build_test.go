// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const validBuild = `---
success: true
summary: "Add GET /version returning the build's SHA and time"
tests:
  ran: true
  passed: true
  command: "go test ./..."
notes:
  - "The build time comes from a linker flag; check the Dockerfile sets it."
---

## What changed

A handler and its test.
`

func TestParseBuild(t *testing.T) {
	b, err := ParseBuild([]byte(validBuild))
	if err != nil {
		t.Fatalf("ParseBuild() error = %v", err)
	}
	if b.Success == nil || !*b.Success {
		t.Errorf("Success = %v, want true", b.Success)
	}
	if b.Summary != "Add GET /version returning the build's SHA and time" {
		t.Errorf("Summary = %q", b.Summary)
	}
	if b.Tests == nil || !*b.Tests.Ran || !*b.Tests.Passed || b.Tests.Command != "go test ./..." {
		t.Errorf("Tests = %+v, want ran and passed under go test ./...", b.Tests)
	}
	if want := []string{"The build time comes from a linker flag; check the Dockerfile sets it."}; !slices.Equal(
		b.Notes, want) {
		t.Errorf("Notes = %q, want %q", b.Notes, want)
	}
	if b.Reason != "" {
		t.Errorf("Reason = %q, want none", b.Reason)
	}
	if b.Body != "## What changed\n\nA handler and its test.\n" {
		t.Errorf("Body = %q", b.Body)
	}
}

func buildWith(old, new string) string { return strings.Replace(validBuild, old, new, 1) }

// TestParseBuildFailureReport: a build that could not be done says why, and
// a report of one whose tests never ran is honest, not invalid.
func TestParseBuildFailureReport(t *testing.T) {
	src := buildWith("success: true", "success: false")
	src = strings.Replace(src, "tests:\n  ran: true\n  passed: true\n  command: \"go test ./...\"",
		"tests:\n  ran: false\n  passed: false", 1)
	src = strings.Replace(src, "notes:", "reason: The plan needs golang.org/x/mod, which the image lacks.\nnotes:", 1)
	b, err := ParseBuild([]byte(src))
	if err != nil {
		t.Fatalf("ParseBuild() error = %v", err)
	}
	if *b.Success || *b.Tests.Ran || b.Reason != "The plan needs golang.org/x/mod, which the image lacks." {
		t.Errorf("parsed = success %v, ran %v, reason %q", *b.Success, *b.Tests.Ran, b.Reason)
	}
}

// TestParseBuildRepairsUnquotedProse mirrors the plan's repair for the build
// report's own prose keys, nested ones included.
func TestParseBuildRepairsUnquotedProse(t *testing.T) {
	src := buildWith(`summary: "Add GET /version returning the build's SHA and time"`,
		`summary: Version endpoint: returns the SHA and time`)
	src = strings.Replace(src, `command: "go test ./..."`, `command: go test -run 'Version: ok' ./...`, 1)
	src = strings.Replace(src, `  - "The build time comes from a linker flag; check the Dockerfile sets it."`,
		"  - Check: the linker flag", 1)
	b, err := ParseBuild([]byte(src))
	if err != nil {
		t.Fatalf("ParseBuild() error = %v", err)
	}
	if b.Summary != "Version endpoint: returns the SHA and time" ||
		b.Tests.Command != "go test -run 'Version: ok' ./..." || !slices.Equal(b.Notes, []string{"Check: the linker flag"}) {
		t.Errorf("parsed = summary %q, command %q, notes %q", b.Summary, b.Tests.Command, b.Notes)
	}
}

func TestParseBuildErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no frontmatter", "## What changed", "missing frontmatter"},
		{"unknown key", buildWith("notes:", "confidence: 0.9\nnotes:"), "field confidence not found"},
		{"missing success", buildWith("success: true\n", ""), "success is required"},
		{"missing summary", buildWith(`summary: "Add GET /version returning the build's SHA and time"`+"\n", ""),
			"summary is required"},
		{"summary over 200 characters", buildWith(`summary: "Add GET /version returning the build's SHA and time"`,
			`summary: "`+strings.Repeat("s", SummaryMaxChars+1)+`"`), "over 200"},
		{"missing tests", buildWith("tests:\n  ran: true\n  passed: true\n  command: \"go test ./...\"\n", ""),
			"tests is required"},
		{"missing tests.ran", buildWith("  ran: true\n", ""), "tests.ran is required"},
		{"missing tests.passed", buildWith("  passed: true\n", ""), "tests.passed is required"},
		{"tests ran without a command", buildWith(`  command: "go test ./..."`+"\n", ""),
			"tests.command is required when tests ran"},
		{"tests passed without running", buildWith("  ran: true", "  ran: false"),
			"tests.passed is true but tests.ran is false"},
		{"success with failing tests", buildWith("  passed: true", "  passed: false"),
			"success is true but the tests that ran did not pass"},
		{"a multi-line command", buildWith(`command: "go test ./..."`, `command: "go vet ./...\ngo test ./..."`),
			"line break"},
		{"eleven notes", buildWith(`notes:
  - "The build time comes from a linker flag; check the Dockerfile sets it."`, items("notes", BuildMaxNotes+1,
			func(i int) string { return fmt.Sprintf("note %d", i) })), "over 10"},
		{"a note over 500 characters", buildWith(
			`  - "The build time comes from a linker flag; check the Dockerfile sets it."`,
			`  - "`+strings.Repeat("n", ItemMaxChars+1)+`"`), "over 500"},
		{"failure without a reason", buildWith("success: true", "success: false"),
			"reason is required when success is false"},
		{"success with a reason", buildWith("notes:", "reason: \"none\"\nnotes:"), "reason is set but success is true"},
		{"reason over 1000 characters", strings.Replace(buildWith("success: true", "success: false"), "notes:",
			`reason: "`+strings.Repeat("r", ReasonMaxChars+1)+`"`+"\nnotes:", 1), "over 1000"},
		{"body over 48 KiB", strings.Replace(validBuild, "## What changed\n\nA handler and its test.\n",
			strings.Repeat("b", BodyMaxBytes+1), 1), "body is"},
		{"document over 64 KiB", validBuild + strings.Repeat("b", ReportMaxBytes), "over the 65536-byte bound"},
		{"invalid UTF-8", strings.Replace(validBuild, "A handler", "A \xfe handler", 1), "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseBuild([]byte(tt.src))
			if err == nil {
				t.Fatal("ParseBuild() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseBuild() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
