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

// TestParseBuildRepairKeepsNulls: the repair runs because a colon broke the
// summary, and must not quote the nulls a model leaves beside it — "reason:
// ~" on a success, or "command: null" for tests that never ran, would become
// the strings "~" and "null" and fail a build that is fine. Prose that only
// starts with "null" is prose, and is quoted.
func TestParseBuildRepairKeepsNulls(t *testing.T) {
	colonSummary := buildWith(`summary: "Add GET /version returning the build's SHA and time"`,
		`summary: Version endpoint: returns the SHA and time`)
	notRun := strings.Replace(colonSummary, "tests:\n  ran: true\n  passed: true\n  command: \"go test ./...\"",
		"tests:\n  ran: false\n  passed: false\n  command: null", 1)
	failed := strings.Replace(strings.Replace(notRun, "success: true", "success: false", 1), "notes:",
		"reason: null handler: the router rejects it\nnotes:", 1)
	tests := []struct {
		name        string
		src         string
		wantReason  string
		wantCommand string
	}{
		{"reason ~ on a success", strings.Replace(colonSummary, "notes:", "reason: ~\nnotes:", 1), "", "go test ./..."},
		{"reason null on a success", strings.Replace(colonSummary, "notes:", "reason: null\nnotes:", 1), "",
			"go test ./..."},
		{"reason NULL before a comment", strings.Replace(colonSummary, "notes:", "reason: NULL  # none\nnotes:", 1), "",
			"go test ./..."},
		{"a null command for tests that never ran", notRun, "", ""},
		{"prose starting with null", failed, "null handler: the router rejects it", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := ParseBuild([]byte(tt.src))
			if err != nil {
				t.Fatalf("ParseBuild() error = %v", err)
			}
			if b.Summary != "Version endpoint: returns the SHA and time" {
				t.Errorf("Summary = %q; the repair did not run", b.Summary)
			}
			if b.Reason != tt.wantReason || b.Tests.Command != tt.wantCommand {
				t.Errorf("reason %q, command %q; want %q, %q", b.Reason, b.Tests.Command, tt.wantReason, tt.wantCommand)
			}
		})
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
			"tests.command holds U+000A, a control character"},
		{"a command with a line separator", buildWith(`command: "go test ./..."`,
			`command: "go vet ./...\u2028go test ./..."`),
			"tests.command holds U+2028, a line or paragraph separator"},
		{"a note with a paragraph separator", buildWith(
			`  - "The build time comes from a linker flag; check the Dockerfile sets it."`,
			`  - "Check the flag.\u2029Then the Dockerfile."`),
			"notes[0] holds U+2029, a line or paragraph separator"},
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

// TestParseBuildRefusesHiddenCharacters: a build report becomes the pull
// request's description, and its free text the commit and the notes a
// reviewer reads, so it holds a plan's rule — nothing that renders
// invisibly or reorders text, anywhere — refused with the code point, line
// and column.
func TestParseBuildRefusesHiddenCharacters(t *testing.T) {
	refusesHiddenAt(t, "build", func(doc []byte) error { _, err := ParseBuild(doc); return err }, []hiddenSite{
		{"in the summary", func(s string) string {
			return buildWith(`summary: "Add GET`, `summary: "Add`+s+` GET`)
		}, 3, 14},
		{"in the test command", func(s string) string {
			return buildWith(`command: "go test`, `command: "go`+s+` test`)
		}, 7, 15},
		{"in a note", func(s string) string {
			return buildWith(`  - "The build time`, `  - "The`+s+` build time`)
		}, 9, 9},
		{"in the body", func(s string) string {
			return strings.Replace(validBuild, "A handler", "A"+s+" handler", 1)
		}, 14, 2},
	})
	lone := strings.Replace(validBuild, "A handler", "A\r handler", 1)
	if _, err := ParseBuild([]byte(lone)); err == nil || !strings.Contains(err.Error(),
		"line 14, column 2: U+000D is a carriage return outside a CRLF line ending") {
		t.Errorf("ParseBuild(lone CR) error = %v, want the carriage return named", err)
	}
	b, err := ParseBuild([]byte(strings.ReplaceAll(validBuild, "\n", "\r\n")))
	if err != nil || b.Body != "## What changed\r\n\r\nA handler and its test.\r\n" {
		t.Errorf("ParseBuild(CRLF) = %+v, %v; want a CRLF report accepted byte-exact", b, err)
	}
}

// TestParseBuildRefusesEscapedHiddenCharacters: an escape the frontmatter
// shows as visible text still decodes to a hidden character, which the
// field checks refuse in every free-text value.
func TestParseBuildRefusesEscapedHiddenCharacters(t *testing.T) {
	failed := strings.Replace(buildWith("success: true", "success: false"), "notes:",
		`reason: "The image lacks a dependency"`+"\nnotes:", 1)
	for _, h := range hiddenRunes {
		escape := fmt.Sprintf(`\U%08X`, h.r)
		for field, src := range map[string]string{
			"summary":       buildWith(`summary: "Add GET`, `summary: "Add`+escape+` GET`),
			"tests.command": buildWith(`command: "go test`, `command: "go`+escape+` test`),
			"notes[0]":      buildWith(`  - "The build time`, `  - "The`+escape+` build time`),
			"reason":        strings.Replace(failed, `"The image`, `"The`+escape+` image`, 1),
		} {
			t.Run(fmt.Sprintf("%s U+%04X", field, h.r), func(t *testing.T) {
				_, err := ParseBuild([]byte(src))
				want := fmt.Sprintf("%s holds U+%04X, %s", field, h.r, h.class)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ParseBuild() error = %v, want it to name %q", err, want)
				}
			})
		}
	}
}
