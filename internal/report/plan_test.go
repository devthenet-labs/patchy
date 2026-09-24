// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const validPlan = `---
summary: "Add GET /version returning {sha, built} as JSON"
repositories:
  - "https://github.com/devthenet-labs/patchy-target"
new_dependencies: []
questions:
  - "Should the build time be RFC 3339?"
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## Approach

Add a handler.
`

func TestParsePlan(t *testing.T) {
	p, err := ParsePlan([]byte(validPlan))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if p.Summary != "Add GET /version returning {sha, built} as JSON" {
		t.Errorf("Summary = %q", p.Summary)
	}
	if want := []string{"https://github.com/devthenet-labs/patchy-target"}; !slices.Equal(p.Repositories, want) {
		t.Errorf("Repositories = %q, want %q", p.Repositories, want)
	}
	if len(p.NewDependencies) != 0 {
		t.Errorf("NewDependencies = %q, want none", p.NewDependencies)
	}
	if want := []string{"Should the build time be RFC 3339?"}; !slices.Equal(p.Questions, want) {
		t.Errorf("Questions = %q, want %q", p.Questions, want)
	}
	if p.Confidence == nil || *p.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8", p.Confidence)
	}
	if p.EstimatedMaxTurns != 40 || p.EstimatedTokenBudget != 200000 {
		t.Errorf("estimate = %d/%d, want 40/200000", p.EstimatedMaxTurns, p.EstimatedTokenBudget)
	}
	if p.Body != "## Approach\n\nAdd a handler.\n" {
		t.Errorf("Body = %q", p.Body)
	}
}

// TestParsePlanTrimsFreeText: surrounding whitespace in a value is
// normalised away, so what the controller records and posts is the text.
func TestParsePlanTrimsFreeText(t *testing.T) {
	src := strings.Replace(validPlan, `summary: "Add GET`, `summary: "  Add GET`, 1)
	src = strings.Replace(src, `  - "Should the build time be RFC 3339?"`,
		`  - "  Should the build time be RFC 3339?  "`, 1)
	p, err := ParsePlan([]byte(src))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if strings.HasPrefix(p.Summary, " ") || p.Questions[0] != "Should the build time be RFC 3339?" {
		t.Errorf("free text not trimmed: summary %q, questions %q", p.Summary, p.Questions)
	}
}

// TestParsePlanRepairsUnquotedProse: models write prose unquoted, and prose
// with a colon is not a plain scalar — in a list it even parses as a
// mapping. The parser quotes and retries rather than failing the run.
func TestParsePlanRepairsUnquotedProse(t *testing.T) {
	src := strings.Replace(validPlan,
		`summary: "Add GET /version returning {sha, built} as JSON"`,
		`summary: Version endpoint: GET /version returns "sha" and built`, 1)
	src = strings.Replace(src, `  - "Should the build time be RFC 3339?"`,
		"  - Format: should the build time be RFC 3339?\n  - plain question", 1)
	src = strings.Replace(src, "new_dependencies: []", "new_dependencies:\n  - golang.org/x/mod v0.20.0", 1)
	p, err := ParsePlan([]byte(src))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if want := `Version endpoint: GET /version returns "sha" and built`; p.Summary != want {
		t.Errorf("Summary = %q, want %q", p.Summary, want)
	}
	if want := []string{"Format: should the build time be RFC 3339?", "plain question"}; !slices.Equal(p.Questions, want) {
		t.Errorf("Questions = %q, want %q", p.Questions, want)
	}
	if want := []string{"golang.org/x/mod v0.20.0"}; !slices.Equal(p.NewDependencies, want) {
		t.Errorf("NewDependencies = %q, want %q", p.NewDependencies, want)
	}
	if want := []string{"https://github.com/devthenet-labs/patchy-target"}; !slices.Equal(p.Repositories, want) {
		t.Errorf("Repositories = %q, want the repair to keep the URL", p.Repositories)
	}
}

// TestParsePlanRepairDoesNotMaskOtherErrors: an unknown key beside a
// repairable colon is still an error.
func TestParsePlanRepairDoesNotMaskOtherErrors(t *testing.T) {
	src := strings.Replace(validPlan, `summary: "Add GET`, `summary: Version: Add GET`, 1)
	src = strings.Replace(src, "confidence:", "certainty:", 1)
	if _, err := ParsePlan([]byte(src)); err == nil {
		t.Error("ParsePlan() error = nil, want error")
	}
}

// planWith renders validPlan with its frontmatter lines replaced.
func planWith(old, new string) string { return strings.Replace(validPlan, old, new, 1) }

// items renders n quoted list items under key, each built by item(i).
func items(key string, n int, item func(int) string) string {
	var b strings.Builder
	b.WriteString(key + ":\n")
	for i := range n {
		fmt.Fprintf(&b, "  - %q\n", item(i))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func repoURL(i int) string { return fmt.Sprintf("https://github.com/devthenet-labs/app-%d", i) }

func TestParsePlanBounds(t *testing.T) {
	summary := `summary: "Add GET /version returning {sha, built} as JSON"`
	repos := "repositories:\n  - \"https://github.com/devthenet-labs/patchy-target\""
	deps := "new_dependencies: []"
	questions := "questions:\n  - \"Should the build time be RFC 3339?\""
	accepted := []struct {
		name string
		src  string
	}{
		{"summary of exactly 200 multi-byte characters",
			planWith(summary, `summary: "`+strings.Repeat("é", SummaryMaxChars)+`"`)},
		{"eight repositories", planWith(repos, items("repositories", PlanMaxRepositories, repoURL))},
		{"sixteen new dependencies", planWith(deps, items("new_dependencies", PlanMaxNewDependencies,
			func(i int) string { return fmt.Sprintf("example.com/dep%d v1.0.0", i) }))},
		{"ten questions", planWith(questions, items("questions", PlanMaxQuestions,
			func(i int) string { return fmt.Sprintf("question %d?", i) }))},
		{"an item of exactly 500 characters", planWith(questions, items("questions", 1,
			func(int) string { return strings.Repeat("q", ItemMaxChars) }))},
		{"a repository URL of exactly 256 bytes", planWith(repos, items("repositories", 1,
			func(int) string {
				u := "https://github.com/devthenet-labs/"
				return u + strings.Repeat("r", RepositoryURLMaxBytes-len(u))
			}))},
		{"a body of exactly 48 KiB", strings.Replace(validPlan, "## Approach\n\nAdd a handler.\n",
			strings.Repeat("b", BodyMaxBytes), 1)},
		{"null lists", planWith(questions, "questions:")},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePlan([]byte(tt.src)); err != nil {
				t.Errorf("ParsePlan() error = %v, want the bound itself accepted", err)
			}
		})
	}
}

func TestParsePlanErrors(t *testing.T) {
	summary := `summary: "Add GET /version returning {sha, built} as JSON"`
	repos := "repositories:\n  - \"https://github.com/devthenet-labs/patchy-target\""
	deps := "new_dependencies: []"
	questions := "questions:\n  - \"Should the build time be RFC 3339?\""
	repo := func(u string) string { return planWith(repos, "repositories:\n  - \""+u+"\"") }
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no frontmatter", "## Approach", "missing frontmatter"},
		{"unterminated", "---\nsummary: x\n", "unterminated"},
		{"unknown key", planWith("confidence:", "risk: high\nconfidence:"), "field risk not found"},
		{"missing summary", planWith(summary+"\n", ""), "summary is required"},
		{"blank summary", planWith(summary, `summary: "   "`), "summary is required"},
		{"summary over 200 characters", planWith(summary, `summary: "`+strings.Repeat("é", SummaryMaxChars+1)+`"`),
			"over 200"},
		{"multi-line summary", planWith(summary, "summary: |\n  one\n  two"), "line break"},
		{"summary with a bidi override", planWith(summary, `summary: "safe \u202e evil"`), "format character"},
		{"missing repositories", planWith(repos+"\n", ""), "repositories is required"},
		{"empty repositories", planWith(repos, "repositories: []"), "repositories is required"},
		{"nine repositories", planWith(repos, items("repositories", PlanMaxRepositories+1, repoURL)), "over 8"},
		{"http repository", repo("http://github.com/devthenet-labs/patchy-target"), "not an https"},
		{"repository with credentials", repo("https://x:y@github.com/devthenet-labs/patchy-target"), "not an https"},
		{"repository with a query", repo("https://github.com/devthenet-labs/patchy-target?ref=main"), "not an https"},
		{"repository with a deeper path", repo("https://github.com/devthenet-labs/patchy-target/tree/main"),
			"not an https"},
		{"owner only", repo("https://github.com/devthenet-labs"), "not an https"},
		{"repository URL over 256 bytes",
			repo("https://github.com/devthenet-labs/" + strings.Repeat("r", RepositoryURLMaxBytes)), "over 256"},
		{"repository listed twice", planWith(repos, "repositories:\n"+
			"  - \"https://github.com/devthenet-labs/patchy-target\"\n"+
			"  - \"https://GitHub.com/devthenet-labs/Patchy-Target.git\""), "listed twice"},
		{"seventeen new dependencies", planWith(deps, items("new_dependencies", PlanMaxNewDependencies+1,
			func(i int) string { return fmt.Sprintf("example.com/dep%d", i) })), "over 16"},
		{"eleven questions", planWith(questions, items("questions", PlanMaxQuestions+1,
			func(i int) string { return fmt.Sprintf("q%d?", i) })), "over 10"},
		{"an item over 500 characters", planWith(questions, items("questions", 1,
			func(int) string { return strings.Repeat("q", ItemMaxChars+1) })), "over 500"},
		{"an empty item", planWith(questions, "questions:\n  - \"\""), "questions[0] is required"},
		{"a map item", planWith(questions, "questions:\n  - {a: b}"), "cannot unmarshal"},
		{"missing confidence", planWith("confidence: 0.8\n", ""), "confidence is required"},
		{"confidence out of range", planWith("confidence: 0.8", "confidence: 1.2"), "outside [0, 1]"},
		// NaN fails every comparison, so a range check alone passes it, and
		// JSON cannot encode it: the plan event would be dropped in the pod.
		{"confidence NaN", planWith("confidence: 0.8", "confidence: .nan"), "outside [0, 1]"},
		{"confidence NaN, capitalised", planWith("confidence: 0.8", "confidence: .NaN"), "outside [0, 1]"},
		{"confidence infinite", planWith("confidence: 0.8", "confidence: .inf"), "outside [0, 1]"},
		{"confidence negative infinite", planWith("confidence: 0.8", "confidence: -.inf"), "outside [0, 1]"},
		{"missing estimated_max_turns", planWith("estimated_max_turns: 40\n", ""), "estimated_max_turns"},
		{"zero estimated_token_budget", planWith("estimated_token_budget: 200000", "estimated_token_budget: 0"),
			"estimated_token_budget"},
		{"negative estimated_max_turns", planWith("estimated_max_turns: 40", "estimated_max_turns: -1"),
			"estimated_max_turns"},
		{"body over 48 KiB", strings.Replace(validPlan, "## Approach\n\nAdd a handler.\n",
			strings.Repeat("b", BodyMaxBytes+1), 1), "body is"},
		{"document over 64 KiB", validPlan + strings.Repeat("b", ReportMaxBytes), "over the 65536-byte bound"},
		{"invalid UTF-8", strings.Replace(validPlan, "Add a handler.", "Add a \xff handler.", 1), "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePlan([]byte(tt.src))
			if err == nil {
				t.Fatal("ParsePlan() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParsePlan() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestParsePlanInput: the build stage's input is the approved plan, followed
// on a revise round by that round's feedback and compare patch, which can
// take it past the plan's own body and document bounds. The frontmatter is
// held to the plan's contract all the same.
func TestParsePlanInput(t *testing.T) {
	round := "\n\n## Review feedback (round 1)\n\n" + strings.Repeat("feedback\n", ReportMaxBytes/9+1)
	if _, err := ParsePlan([]byte(validPlan + round)); err == nil {
		t.Fatal("ParsePlan() accepted an oversized document; the test no longer exercises the difference")
	}
	p, err := ParsePlanInput([]byte(validPlan + round))
	if err != nil {
		t.Fatalf("ParsePlanInput() error = %v", err)
	}
	if !strings.HasSuffix(p.Body, round) || p.Summary != "Add GET /version returning {sha, built} as JSON" {
		t.Errorf("ParsePlanInput() = summary %q, body %d bytes; want the plan's frontmatter and the whole rest",
			p.Summary, len(p.Body))
	}
	for _, bad := range []string{
		"## just feedback",
		strings.Replace(validPlan, "confidence: 0.8", "confidence: 2", 1) + round,
		"---\n" + strings.Repeat("x", ReportMaxBytes+1) + "\n---\nbody",
	} {
		if _, err := ParsePlanInput([]byte(bad)); err == nil {
			t.Errorf("ParsePlanInput(%.40q...) error = nil, want the frontmatter refused", bad)
		}
	}
}
