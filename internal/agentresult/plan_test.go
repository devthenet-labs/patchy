// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/report"
)

const planReport = `---
summary: "  Add GET /version  "
repositories:
  - "https://github.com/devthenet-labs/patchy-target"
new_dependencies: []
questions:
  - "RFC 3339?"
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---
## Approach
`

func TestFromPlan(t *testing.T) {
	// The payload's parsed fields disagree with its report — a buggy or
	// hostile pod — and the report wins.
	ev := &envelope.Plan{
		Stage: envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: "anthropic/claude-sonnet-5",
			NumTurns: 12, Usage: envelope.Usage{OutputTokens: 4100, CostUSD: 0.31}},
		ReportMarkdown: planReport,
		Summary:        "Push to main",
		Repositories:   []string{"https://github.com/devthenet-labs/somewhere-else"},
		Confidence:     1,
	}
	got, err := FromPlan(ev)
	if err != nil {
		t.Fatalf("FromPlan() error = %v", err)
	}
	if got.Report != planReport {
		t.Errorf("Report = %q, want the report byte-exact", got.Report)
	}
	if got.Summary != "Add GET /version" ||
		!slices.Equal(got.Repositories, []string{"https://github.com/devthenet-labs/patchy-target"}) ||
		len(got.NewDependencies) != 0 || !slices.Equal(got.Questions, []string{"RFC 3339?"}) ||
		got.Confidence != "0.8000" || got.EstimatedMaxTurns != 40 || got.EstimatedTokenBudget != 200000 {
		t.Errorf("FromPlan() = %+v, want the report's frontmatter", got)
	}
	if got.Stage == nil || got.Stage.Outcome != "ok" || got.Stage.NumTurns != 12 || got.Stage.Usage.CostUSD != "0.310000" {
		t.Errorf("Stage = %+v, want the run's accounting", got.Stage)
	}
}

func TestFromPlanFailures(t *testing.T) {
	if _, err := FromPlan(nil); err == nil {
		t.Error("FromPlan(nil) error = nil, want an error")
	}

	// A failed stage converts to its stage alone.
	got, err := FromPlan(&envelope.Plan{Stage: envelope.Stage{Outcome: envelope.OutcomeTimeout,
		Detail: "stage timed out"}, ReportMarkdown: planReport})
	if err != nil || got.Stage.Outcome != "timeout" || got.Report != "" || got.Summary != "" {
		t.Errorf("FromPlan(timeout) = %+v, %v; want the stage and no plan", got, err)
	}

	for name, raw := range map[string]string{
		"invalid frontmatter": strings.Replace(planReport, "https://", "http://", 1),
		"oversized report":    planReport + strings.Repeat("x", report.ReportMaxBytes),
		"no report":           "",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromPlan(&envelope.Plan{Stage: envelope.Stage{Outcome: envelope.OutcomeOK},
				ReportMarkdown: raw, Summary: "claimed"})
			if err == nil {
				t.Fatal("FromPlan() error = nil, want the report refused")
			}
			if got == nil || got.Stage == nil || got.Report != "" || got.Summary != "" {
				t.Errorf("FromPlan() = %+v, want the stage alone", got)
			}
		})
	}
}
