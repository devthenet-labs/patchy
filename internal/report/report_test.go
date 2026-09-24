// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

const validInvestigation = `---
exploitability:
  rating: high
  summary: reachable from the request path
likelihood:
  rating: medium
  summary: requires an authenticated caller
impact:
  rating: high
  summary: full table read
recommendation: remediate
priority: high
severity: high
confidence: 0.85
breaking_change_available: false
model: claude-sonnet-5
max_turns: 40
token_budget: 200000
---

## Analysis

The finding is real.
`

func TestParseInvestigation(t *testing.T) {
	inv, err := ParseInvestigation([]byte(validInvestigation))
	if err != nil {
		t.Fatalf("ParseInvestigation() error = %v", err)
	}
	if inv.Recommendation != RecommendRemediate || inv.Priority != "high" || inv.Severity != "high" {
		t.Errorf("parsed = %+v", inv)
	}
	if inv.Exploitability.Rating != "high" || inv.Likelihood.Rating != "medium" || inv.Impact.Rating != "high" {
		t.Errorf("dimensions = %s/%s/%s, want high/medium/high",
			inv.Exploitability.Rating, inv.Likelihood.Rating, inv.Impact.Rating)
	}
	if inv.Exploitability.Summary == "" {
		t.Error("exploitability summary lost")
	}
	if inv.Confidence == nil || *inv.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85", inv.Confidence)
	}
	if inv.Model != "claude-sonnet-5" || inv.MaxTurns != 40 || inv.TokenBudget != 200000 {
		t.Errorf("remediation params = %q/%d/%d", inv.Model, inv.MaxTurns, inv.TokenBudget)
	}
	if !strings.Contains(inv.Body, "The finding is real.") {
		t.Errorf("Body = %q", inv.Body)
	}
}

func TestParseInvestigationIgnoreNeedsNoModel(t *testing.T) {
	src := `---
exploitability:
  rating: none
  summary: sink is constant
likelihood:
  rating: none
  summary: not reachable
impact:
  rating: low
  summary: nothing sensitive
recommendation: ignore
priority: low
severity: low
confidence: 0.95
breaking_change_available: false
---
False positive: the sink is constant.
`
	inv, err := ParseInvestigation([]byte(src))
	if err != nil {
		t.Fatalf("ParseInvestigation() error = %v", err)
	}
	if inv.Recommendation != RecommendIgnore {
		t.Errorf("Recommendation = %q", inv.Recommendation)
	}
}

func TestParseInvestigationRepairsUnquotedColonSummary(t *testing.T) {
	// Models write summaries as plain prose; "CWE-614: ..." is invalid as a
	// plain YAML scalar. The parser must quote-and-retry rather than fail
	// the whole run.
	src := strings.Replace(validInvestigation,
		"summary: reachable from the request path",
		"summary: CWE-614: the session cookie is set without the Secure attribute", 1)
	inv, err := ParseInvestigation([]byte(src))
	if err != nil {
		t.Fatalf("ParseInvestigation() error = %v", err)
	}
	if want := "CWE-614: the session cookie is set without the Secure attribute"; inv.Exploitability.Summary != want {
		t.Errorf("Exploitability.Summary = %q, want %q", inv.Exploitability.Summary, want)
	}
	// The untouched siblings survive the repair pass unchanged.
	if inv.Likelihood.Summary != "requires an authenticated caller" {
		t.Errorf("Likelihood.Summary = %q", inv.Likelihood.Summary)
	}
	if inv.Recommendation != RecommendRemediate || *inv.Confidence != 0.85 {
		t.Errorf("parsed = %+v", inv)
	}
}

func TestParseInvestigationRepairEscapesQuotes(t *testing.T) {
	src := strings.Replace(validInvestigation,
		"summary: requires an authenticated caller",
		`summary: attacker needs the "admin: true" claim`, 1)
	inv, err := ParseInvestigation([]byte(src))
	if err != nil {
		t.Fatalf("ParseInvestigation() error = %v", err)
	}
	if want := `attacker needs the "admin: true" claim`; inv.Likelihood.Summary != want {
		t.Errorf("Likelihood.Summary = %q, want %q", inv.Likelihood.Summary, want)
	}
}

func TestParseInvestigationRepairDoesNotMaskOtherErrors(t *testing.T) {
	// A broken summary plus a genuinely bad frontmatter: the repair retry
	// must not swallow the failure, and the original error is reported.
	src := strings.Replace(validInvestigation,
		"summary: full table read",
		"summary: impact: full table read", 1)
	src = strings.Replace(src, "model:", "mdoel:", 1)
	if _, err := ParseInvestigation([]byte(src)); err == nil {
		t.Error("ParseInvestigation() error = nil, want error")
	}
}

func TestParseInvestigationErrors(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(validInvestigation, old, new, 1) }
	tests := []struct {
		name string
		src  string
	}{
		{"no frontmatter", "just markdown"},
		{"unterminated", "---\nrecommendation: ignore\n"},
		{"unknown key", replace("model:", "mdoel:")},
		{"bad recommendation", replace("recommendation: remediate", "recommendation: dismiss")},
		{"bad priority", replace("priority: high", "priority: urgent")},
		{"bad severity", replace("severity: high", "severity: severe")},
		{"bad rating", replace("rating: medium", "rating: sometimes")},
		{"missing confidence", replace("confidence: 0.85\n", "")},
		{"confidence out of range", replace("confidence: 0.85", "confidence: 1.5")},
		{"remediate without model", replace("model: claude-sonnet-5\n", "")},
		{"remediate without max_turns", replace("max_turns: 40\n", "")},
		{"remediate without token_budget", replace("token_budget: 200000\n", "")},
		{"non-numeric confidence", replace("confidence: 0.85", "confidence: high")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseInvestigation([]byte(tt.src)); err == nil {
				t.Error("ParseInvestigation() error = nil, want error")
			}
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"fenced report", "---\nconfidence: 0.9\n---\n\n## Analysis\n\nbody", "## Analysis\n\nbody"},
		{"body only", "## Analysis\n\nbody", "## Analysis\n\nbody"},
		{"unterminated fence", "---\nconfidence: 0.9\nbody", "---\nconfidence: 0.9\nbody"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripFrontmatter(tt.src); got != tt.want {
				t.Errorf("StripFrontmatter() = %q, want %q", got, tt.want)
			}
		})
	}
}

const validRemediation = `---
success: true
confidence: 0.9
---
Escaped the sink; tests pass.
`

func TestParseRemediation(t *testing.T) {
	r, err := ParseRemediation([]byte(validRemediation))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	if r.Success == nil || !*r.Success {
		t.Errorf("Success = %v, want true", r.Success)
	}
	if r.Confidence == nil || *r.Confidence != 0.9 {
		t.Errorf("Confidence = %v, want 0.9", r.Confidence)
	}
	if !strings.Contains(r.Body, "tests pass") {
		t.Errorf("Body = %q", r.Body)
	}
}

func TestParseRemediationFalseIsNotAbsent(t *testing.T) {
	r, err := ParseRemediation([]byte("---\nsuccess: false\nconfidence: 0.2\n---\ncould not fix\n"))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	if r.Success == nil || *r.Success {
		t.Errorf("Success = %v, want explicit false", r.Success)
	}
}

func TestParseRemediationErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"missing success", "---\nconfidence: 0.5\n---\nbody"},
		{"missing confidence", "---\nsuccess: true\n---\nbody"},
		{"confidence out of range", "---\nsuccess: true\nconfidence: -0.1\n---\nbody"},
		{"unknown key", "---\nsuccess: true\nconfidence: 0.5\nnotes: hi\n---\nbody"},
		{"no frontmatter", "body only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseRemediation([]byte(tt.src)); err == nil {
				t.Error("ParseRemediation() error = nil, want error")
			}
		})
	}
}

// TestParseRefusesNonFiniteConfidence pins YAML's non-finite float spellings
// as invalid reports. NaN compares false against both bounds, so the range
// check alone let it through; in the pod it then reached the envelope
// encoder, which cannot marshal NaN, and the stage's only result event was
// dropped instead of reporting report_invalid.
func TestParseRefusesNonFiniteConfidence(t *testing.T) {
	for _, spelling := range []string{".nan", ".NaN", ".NAN", ".inf", ".Inf", "+.inf", "-.inf", "-.INF"} {
		t.Run(spelling, func(t *testing.T) {
			inv := strings.Replace(validInvestigation, "confidence: 0.85", "confidence: "+spelling, 1)
			if _, err := ParseInvestigation([]byte(inv)); err == nil || !strings.Contains(err.Error(), "confidence") {
				t.Errorf("ParseInvestigation() error = %v, want a confidence error", err)
			}
			rem := "---\nsuccess: true\nconfidence: " + spelling + "\n---\nbody"
			if _, err := ParseRemediation([]byte(rem)); err == nil || !strings.Contains(err.Error(), "confidence") {
				t.Errorf("ParseRemediation() error = %v, want a confidence error", err)
			}
		})
	}
}

// yamlFloat spells c as a YAML float scalar: the core schema's spellings for
// the non-finite values (strconv's "NaN" and "+Inf" read back as strings),
// strconv's shortest round-tripping form for the rest.
func yamlFloat(c float64) string {
	switch {
	case math.IsNaN(c):
		return ".nan"
	case math.IsInf(c, 1):
		return ".inf"
	case math.IsInf(c, -1):
		return "-.inf"
	}
	return strconv.FormatFloat(c, 'g', -1, 64)
}

// TestConfidenceAcceptanceProperty states the confidence rule as an
// invariant over both report kinds: a report is accepted exactly when its
// confidence lies in [0, 1], and an accepted confidence is the value written.
// The generator mixes uniform values straddling the interval with the IEEE
// edge cases — NaN, both infinities, both zeros, each bound's outer
// neighbour — so the non-finite values are drawn on every seed.
func TestConfidenceAcceptanceProperty(t *testing.T) {
	edges := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), 0, math.Copysign(0, -1), 1,
		math.Nextafter(1, 2), math.Nextafter(0, -1), math.SmallestNonzeroFloat64,
		math.MaxFloat64, -math.MaxFloat64,
	}
	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(20260924)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			c := r.Float64()*2 - 0.5 // [-0.5, 1.5)
			if r.Intn(4) == 0 {
				c = edges[r.Intn(len(edges))]
			}
			args[0] = reflect.ValueOf(c)
		},
	}
	acceptedIffInRange := func(c float64) bool {
		want := c >= 0 && c <= 1
		scalar := yamlFloat(c)
		inv, invErr := ParseInvestigation([]byte(
			strings.Replace(validInvestigation, "confidence: 0.85", "confidence: "+scalar, 1)))
		rem, remErr := ParseRemediation([]byte("---\nsuccess: true\nconfidence: " + scalar + "\n---\nbody"))
		if (invErr == nil) != want || (remErr == nil) != want {
			return false
		}
		return !want || (*inv.Confidence == c && *rem.Confidence == c)
	}
	if err := quick.Check(acceptedIffInRange, cfg); err != nil {
		t.Error(err)
	}
}
