// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
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

// projectCRD is the Project CRD generated from api/v1alpha1, whose
// repositories[].url pattern repositoryURL copies.
const projectCRD = "../../deploy/kustomize/base/crds/patchy.bitwisemedia.uk_projects.yaml"

// TestRepositoryURLMatchesProjectSchema guards the copy repositoryURL holds
// of the Project schema's repository URL pattern. report does not import
// the API types and they export no constant for it, so nothing else ties
// the two; were they to drift, a plan could name a URL no Project can hold,
// or be refused one it can.
func TestRepositoryURLMatchesProjectSchema(t *testing.T) {
	raw, err := os.ReadFile(projectCRD)
	if err != nil {
		t.Fatalf("read the Project CRD: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decode the Project CRD: %v", err)
	}
	v := any(crd)
	for _, key := range []any{"spec", "versions", 0, "schema", "openAPIV3Schema", "properties", "spec", "properties",
		"repositories", "items", "properties", "url", "pattern"} {
		switch k := key.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("Project CRD: no %q where the repository URL pattern should be", k)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || len(l) <= k {
				t.Fatalf("Project CRD: no item %d where the repository URL pattern should be", k)
			}
			v = l[k]
		}
	}
	if got := repositoryURL.String(); v != got {
		t.Errorf("Project CRD repositories[].url pattern = %v, report's copy = %q; change them together", v, got)
	}
}

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
		{"multi-line summary", planWith(summary, "summary: |\n  one\n  two"),
			"summary holds U+000A, a control character"},
		{"summary with a bidi override", planWith(summary, `summary: "safe \u202e evil"`),
			"summary holds U+202E, a format character"},
		// U+2028 and U+2029 are neither control nor format characters, yet
		// renderers and tokenizers break a line on them; NEL is a control.
		{"summary with a line separator", planWith(summary, `summary: "line one\u2028line two"`),
			"summary holds U+2028, a line or paragraph separator"},
		{"question with a paragraph separator", planWith(questions, `questions:
  - "one\u2029two"`), "questions[0] holds U+2029, a line or paragraph separator"},
		{"dependency with a next line", planWith(deps, `new_dependencies:
  - "example.com/dep\u0085v1"`), "new_dependencies[0] holds U+0085, a control character"},
		{"repository with a line separator", repo(`https://github.com/devthenet-labs/patchy\u2028target`),
			"not an https"},
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
		{"repository listed twice, one with an upper-case .GIT", planWith(repos, "repositories:\n"+
			"  - \"https://github.com/devthenet-labs/patchy-target\"\n"+
			"  - \"https://github.com/devthenet-labs/patchy-target.GIT\""), "listed twice"},
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

// hiddenRunes are characters no intent report may hold, one or more per
// class checkVisible refuses, each with the class its refusal names. They
// are built from their code points, never typed, so that no editor can
// render one away.
var hiddenRunes = []struct {
	r     rune
	class string
}{
	{0x00, "a control character"}, // NUL
	{0x07, "a control character"}, // BEL
	{0x0b, "a control character"}, // vertical tab
	{0x0c, "a control character"}, // form feed
	{0x1b, "a control character"}, // ESC, which starts a terminal escape
	{0x7f, "a control character"}, // DEL
	{0x85, "a control character"}, // NEL, a C1 line break
	{0x9b, "a control character"}, // CSI, the C1 terminal escape
	{0x2028, "a line or paragraph separator"},
	{0x2029, "a line or paragraph separator"},
	{0xe0000, "a tag character"},               // unassigned, in the tag block
	{0xe0001, "a tag character"},               // LANGUAGE TAG
	{0xe0041, "a tag character"},               // TAG LATIN CAPITAL LETTER A
	{0xe007f, "a tag character"},               // CANCEL TAG
	{0xfe00, "a variation selector"},           // VS1
	{0xfe0f, "a variation selector"},           // VS16, the emoji presentation selector
	{0xe0100, "a variation selector"},          // VS17
	{0xe01ef, "a variation selector"},          // VS256
	{0x180b, "a variation selector"},           // MONGOLIAN FREE VARIATION SELECTOR ONE
	{0x00ad, "a format character"},             // SOFT HYPHEN
	{0x061c, "a format character"},             // ARABIC LETTER MARK
	{0x180e, "a format character"},             // MONGOLIAN VOWEL SEPARATOR
	{0x200b, "a format character"},             // ZERO WIDTH SPACE
	{0x200c, "a format character"},             // ZERO WIDTH NON-JOINER
	{0x200d, "a format character"},             // ZERO WIDTH JOINER
	{0x200e, "a format character"},             // LEFT-TO-RIGHT MARK
	{0x200f, "a format character"},             // RIGHT-TO-LEFT MARK
	{0x202a, "a format character"},             // LEFT-TO-RIGHT EMBEDDING
	{0x202e, "a format character"},             // RIGHT-TO-LEFT OVERRIDE
	{0x2060, "a format character"},             // WORD JOINER
	{0x2064, "a format character"},             // INVISIBLE PLUS
	{0x2066, "a format character"},             // LEFT-TO-RIGHT ISOLATE
	{0x2069, "a format character"},             // POP DIRECTIONAL ISOLATE
	{0xfeff, "a format character"},             // ZERO WIDTH NO-BREAK SPACE, the BOM
	{0xfff9, "a format character"},             // INTERLINEAR ANNOTATION ANCHOR
	{0x1d173, "a format character"},            // MUSICAL SYMBOL BEGIN BEAM
	{0x034f, "a default-ignorable character"},  // COMBINING GRAPHEME JOINER
	{0x115f, "a default-ignorable character"},  // HANGUL CHOSEONG FILLER
	{0x17b4, "a default-ignorable character"},  // KHMER VOWEL INHERENT AQ
	{0x2065, "a default-ignorable character"},  // reserved for an invisible character
	{0x3164, "a default-ignorable character"},  // HANGUL FILLER
	{0xffa0, "a default-ignorable character"},  // HALFWIDTH HANGUL FILLER
	{0xfff0, "a default-ignorable character"},  // reserved for an invisible character
	{0xe0080, "a default-ignorable character"}, // reserved, past the tag block
	{0xe0fff, "a default-ignorable character"}, // reserved, the last default-ignorable
}

// invalidUTF8 are byte sequences that begin no UTF-8 encoding, each with
// the first byte a refusal names.
var invalidUTF8 = []struct {
	seq   string
	first byte
}{
	{"\xff", 0xff},
	{"\x80", 0x80},             // a continuation byte alone
	{"\xe2\x80", 0xe2},         // a truncated sequence
	{"\xc0\xaf", 0xc0},         // an overlong encoding of '/'
	{"\xed\xa0\x80", 0xed},     // a UTF-16 surrogate
	{"\xf4\x90\x80\x80", 0xf4}, // past U+10FFFF
}

// TestParsePlanRefusesHiddenCharacters: the approver reads a plan verbatim
// and the approval's digest covers every byte of it, so a character that
// renders invisibly or reorders text is refused wherever it is — in the
// frontmatter, where it would first be caught by nothing but YAML, and in
// the body, which no field check reads — naming its code point, line and
// column.
func TestParsePlanRefusesHiddenCharacters(t *testing.T) {
	// The summary's value starts at column 11; the body's "Add a" ends at
	// column 5 of line 15.
	sites := []struct {
		name         string
		insert       func(string) string
		line, column int
	}{
		{"in the summary", func(s string) string {
			return planWith(`summary: "Add GET`, `summary: "Add`+s+` GET`)
		}, 2, 14},
		{"in a list item", func(s string) string {
			return planWith(`  - "Should the build`, `  - "Should`+s+` the build`)
		}, 7, 12},
		{"in a YAML comment", func(s string) string {
			return planWith("confidence: 0.8", "confidence: 0.8 # sure"+s)
		}, 8, 23},
		{"in the body", func(s string) string {
			return strings.Replace(validPlan, "Add a handler.", "Add a"+s+" handler.", 1)
		}, 15, 6},
		{"leading the document", func(s string) string { return s + validPlan }, 1, 1},
		{"ending the document", func(s string) string { return validPlan + s }, 16, 1},
	}
	for _, site := range sites {
		for _, h := range hiddenRunes {
			t.Run(fmt.Sprintf("%s U+%04X", site.name, h.r), func(t *testing.T) {
				_, err := ParsePlan([]byte(site.insert(string(h.r))))
				want := fmt.Sprintf("line %d, column %d: U+%04X is %s", site.line, site.column, h.r, h.class)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePlan() error = %v, want it to name %q", err, want)
				}
			})
		}
		for _, bad := range invalidUTF8 {
			t.Run(fmt.Sprintf("%s %q", site.name, bad.seq), func(t *testing.T) {
				_, err := ParsePlan([]byte(site.insert(bad.seq)))
				want := fmt.Sprintf("line %d, column %d: byte 0x%02X is not valid UTF-8", site.line, site.column,
					bad.first)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePlan() error = %v, want it to name %q", err, want)
				}
			})
		}
	}
}

// TestParsePlanCarriageReturns: a carriage return is admitted only as half
// of a CRLF line ending, which reads as the line break it is; alone, a
// terminal prints what follows it over the line before.
func TestParsePlanCarriageReturns(t *testing.T) {
	crlf := strings.ReplaceAll(validPlan, "\n", "\r\n")
	p, err := ParsePlan([]byte(crlf))
	if err != nil {
		t.Fatalf("ParsePlan(CRLF) error = %v, want a CRLF plan accepted", err)
	}
	if p.Summary != "Add GET /version returning {sha, built} as JSON" ||
		p.Body != "## Approach\r\n\r\nAdd a handler.\r\n" {
		t.Errorf("ParsePlan(CRLF) = summary %q, body %q; want the plan, its body byte-exact", p.Summary, p.Body)
	}
	for _, tt := range []struct {
		name, src    string
		line, column int
	}{
		{"a lone CR in the body", strings.Replace(validPlan, "Add a handler.", "Add a\r handler.", 1), 15, 6},
		{"a CR ending the document", validPlan + "\r", 16, 1},
		{"a CR before a CR", strings.Replace(crlf, "## Approach\r\n", "## Approach\r\r\n", 1), 13, 12},
		{"a lone CR in the summary", planWith(`summary: "Add GET`, "summary: \"Add\r GET"), 2, 14},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePlan([]byte(tt.src))
			want := fmt.Sprintf("line %d, column %d: U+000D is a carriage return outside a CRLF line ending",
				tt.line, tt.column)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ParsePlan() error = %v, want it to name %q", err, want)
			}
		})
	}
}

// TestParsePlanAcceptsVisibleText: the rule refuses what cannot be seen,
// not what is not ASCII — accents, other scripts (right-to-left ones among
// them: their letters are the text, not a control reordering it), emoji
// that need no selector, the space separators, tabs, a combining mark on a
// letter, and a YAML escape written out in the body, where it is six
// visible characters.
func TestParsePlanAcceptsVisibleText(t *testing.T) {
	text := "caf" + string(rune(0x00e9)) + " " + string([]rune{0x6f22, 0x5b57}) + " " +
		string([]rune{0x05e9, 0x05dc, 0x05d5, 0x05dd}) + " " + string(rune(0x1f642)) + string(rune(0x2705)) +
		" a" + string(rune(0x00a0)) + "b" + string(rune(0x3000)) + "c e" + string(rune(0x0301)) +
		string(rune(0xfffd))
	// In the body, where nothing decodes it, an escape is what it shows.
	body := text + "\t" + `\u200b`
	src := planWith(`summary: "Add GET`, `summary: "`+text+` GET`)
	src = strings.Replace(src, "Add a handler.", "Add a handler: "+body, 1)
	p, err := ParsePlan([]byte(src))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v, want visible text accepted", err)
	}
	if !strings.HasPrefix(p.Summary, text) || !strings.Contains(p.Body, body) {
		t.Errorf("ParsePlan() = summary %q, body %q; want both to keep %q", p.Summary, p.Body, text)
	}
}

// TestParsePlanRefusesEscapedHiddenCharacters: a YAML escape is visible in
// the document, but the value it decodes to is not — and a summary becomes
// a commit subject and a status field. The field checks refuse every class
// the document check does, spelled as an escape.
func TestParsePlanRefusesEscapedHiddenCharacters(t *testing.T) {
	for _, h := range hiddenRunes {
		escape := fmt.Sprintf(`\U%08X`, h.r)
		for field, src := range map[string]string{
			"summary":      planWith(`summary: "Add GET`, `summary: "Add`+escape+` GET`),
			"questions[0]": planWith(`  - "Should the build`, `  - "Should`+escape+` the build`),
		} {
			t.Run(fmt.Sprintf("%s U+%04X", field, h.r), func(t *testing.T) {
				_, err := ParsePlan([]byte(src))
				want := fmt.Sprintf("%s holds U+%04X, %s", field, h.r, h.class)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePlan() error = %v, want it to name %q", err, want)
				}
			})
		}
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

// TestParsePlanInputRefusesHiddenCharacters: the build agent reads the
// whole input — the approved plan and whatever a revise round appends — so
// every byte of it is held to the plan's rule, past the plan's own bounds:
// a hidden character in the plan or in the round's feedback is refused,
// with where it sits in the input. GitHub's CRLF comment bodies pass as
// they are.
func TestParsePlanInputRefusesHiddenCharacters(t *testing.T) {
	// validPlan is 15 lines and a final line feed; the round's heading is on
	// line 18 and its first feedback line on line 20.
	round := "\n\n## Review feedback (round 1)\n\nRename the handler.\n" + strings.Repeat("feedback\n", ReportMaxBytes/9+1)
	for _, h := range hiddenRunes {
		for _, tt := range []struct {
			name         string
			src          string
			line, column int
		}{
			{"in the plan's frontmatter", planWith(`summary: "Add GET`, `summary: "Add`+string(h.r)+` GET`) + round,
				2, 14},
			{"in the plan's body", strings.Replace(validPlan, "Add a handler.", "Add a"+string(h.r)+" handler.", 1) +
				round, 15, 6},
			{"in the round's feedback", validPlan + strings.Replace(round, "Rename the", "Rename"+string(h.r)+" the", 1),
				20, 7},
			{"past the plan's document bound", validPlan + round + string(h.r), 20 + ReportMaxBytes/9 + 2, 1},
		} {
			t.Run(fmt.Sprintf("%s U+%04X", tt.name, h.r), func(t *testing.T) {
				_, err := ParsePlanInput([]byte(tt.src))
				want := fmt.Sprintf("line %d, column %d: U+%04X is %s", tt.line, tt.column, h.r, h.class)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePlanInput() error = %v, want it to name %q", err, want)
				}
			})
		}
	}
	for _, bad := range invalidUTF8 {
		_, err := ParsePlanInput([]byte(validPlan + round + bad.seq))
		want := fmt.Sprintf("byte 0x%02X is not valid UTF-8", bad.first)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParsePlanInput(round + %q) error = %v, want it to name %q", bad.seq, err, want)
		}
	}
	crlf := validPlan + strings.ReplaceAll(round, "\n", "\r\n")
	if p, err := ParsePlanInput([]byte(crlf)); err != nil || !strings.HasSuffix(p.Body, "feedback\r\n") {
		t.Errorf("ParsePlanInput(CRLF round) error = %v, want the round accepted as written", err)
	}
}
