// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"errors"
	"fmt"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// Plan is an intent plan stage's result as the controller records it: the
// run's accounting, and — when the stage ended ok — the plan itself.
type Plan struct {
	// Stage is the run's stage result, as every run records it.
	Stage *v1alpha1.StageResult
	// Report is the plan report exactly as the agent wrote it, frontmatter
	// included: the bytes the controller stores, digests and posts for
	// approval, and hands the build. It is at most report.ReportMaxBytes,
	// so it fits a status field whole — it is never truncated, because a
	// cut report would no longer be what its digest says. Empty unless the
	// stage ended ok.
	Report string
	// Summary, Repositories, NewDependencies and Questions are the report's
	// frontmatter, trimmed and bounded by report.ParsePlan.
	Summary         string
	Repositories    []string
	NewDependencies []string
	Questions       []string
	// Confidence is the plan's confidence as the CRD decimal string.
	Confidence string
	// EstimatedMaxTurns/EstimatedTokenBudget are the plan's estimate of its
	// build's cost, verbatim: an estimate, never a grant.
	EstimatedMaxTurns    int
	EstimatedTokenBudget int
}

// FromPlan converts a plan event's payload. The plan is re-derived from the
// report rather than taken from the payload's parsed fields: the report is
// the contract — what the approver reads and the build follows — so what
// the controller records about a plan can never disagree with it, whatever
// the pod put beside it. A stage that did not end ok converts to its Stage
// alone. An ok stage whose report does not parse is an error; the Stage is
// still returned, for the controller to record the run as report_invalid.
func FromPlan(p *envelope.Plan) (*Plan, error) {
	if p == nil {
		return nil, errors.New("agentresult: no plan payload")
	}
	out := &Plan{Stage: FromStage(&p.Stage)}
	if p.Outcome != envelope.OutcomeOK {
		return out, nil
	}
	parsed, err := report.ParsePlan([]byte(p.ReportMarkdown))
	if err != nil {
		return out, fmt.Errorf("agentresult: plan report: %w", err)
	}
	out.Report = p.ReportMarkdown
	out.Summary = parsed.Summary
	out.Repositories = parsed.Repositories
	out.NewDependencies = parsed.NewDependencies
	out.Questions = parsed.Questions
	out.Confidence = FormatConfidence(*parsed.Confidence)
	out.EstimatedMaxTurns = parsed.EstimatedMaxTurns
	out.EstimatedTokenBudget = parsed.EstimatedTokenBudget
	return out, nil
}
