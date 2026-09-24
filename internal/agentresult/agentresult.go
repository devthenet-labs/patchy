// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package agentresult converts agent envelope payloads into the CRD status
// shapes — the one place the float-bearing wire format meets the no-float
// structural schemas — and a failed run's result into what its retry is told
// about it. Both job controllers (investigation, remediation) use it so
// cost/confidence formatting and size caps never drift apart.
package agentresult

import (
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// Size caps (bytes) for CRD string fields.
const (
	maxReport = 65536
	maxDetail = 4096
)

// failureOutcomes is every outcome a failed run's stage can record: the
// envelope's, less ok, plus "aborted", the controllers' own verdict on a run
// that ended without one. A retry is told its predecessor's outcome only
// from this list, because the outcome is the pod's to report — on a
// repository-declared image the pod's process is the image's — and the
// prompt states it as fact, outside the fenced detail.
var failureOutcomes = map[envelope.Outcome]bool{
	envelope.OutcomeRuntimeError:      true,
	envelope.OutcomeTimeout:           true,
	envelope.OutcomeBudgetExceeded:    true,
	envelope.OutcomeReportMissing:     true,
	envelope.OutcomeReportInvalid:     true,
	envelope.OutcomeCommitFailed:      true,
	envelope.OutcomeChangesetTooLarge: true,
	envelope.OutcomeImageIncompatible: true,
	envelope.OutcomeChangesetRejected: true,
	"aborted":                         true,
}

// PreviousAttempt is what the retry of a failed run is told about it: the
// run's name and ordinal, its stage outcome — "unknown" unless it is one a
// failed run can record — and its detail capped to the API bound. Nil when
// the run recorded no stage. The detail is untrusted text; the prompt, not
// this, is where it is fenced.
func PreviousAttempt(name string, attempt int32, st *v1alpha1.StageResult) *v1alpha1.PreviousAttempt {
	if st == nil {
		return nil
	}
	outcome := st.Outcome
	if !failureOutcomes[envelope.Outcome(outcome)] {
		outcome = "unknown"
	}
	return &v1alpha1.PreviousAttempt{
		Name:    name,
		Attempt: attempt,
		Outcome: outcome,
		Detail:  TruncateDetail(st.Detail),
	}
}

// EncodePreviousAttempt renders p as the JSON a Job hands the agent-runner
// (PATCHY_PREVIOUS_ATTEMPT, decoded into templates.PreviousAttempt); "" for
// nil, which omits the variable and with it the prompt section.
func EncodePreviousAttempt(p *v1alpha1.PreviousAttempt) string {
	if p == nil {
		return ""
	}
	blob, err := json.Marshal(templates.PreviousAttempt{Attempt: p.Attempt, Outcome: p.Outcome, Detail: p.Detail})
	if err != nil {
		return "" // three plain fields cannot fail to marshal
	}
	return string(blob)
}

// FailedStage builds the stage to record for a run the controller is failing.
//
// When the agent reported a stage, that stage is kept and only the verdict is
// overwritten: a run that failed still burned turns, tokens and money, and
// throwing its accounting away is what blanks a child's usage and leaves the
// harness/model rollup scopes with nothing to key on. reported is nil only
// when no event arrived at all (a vanished or unparseable Job), and then
// there is genuinely nothing but the verdict to record.
func FailedStage(reported *envelope.Stage, outcome, detail string) envelope.Stage {
	st := envelope.Stage{}
	if reported != nil {
		st = *reported
	}
	st.Outcome = envelope.Outcome(outcome)
	st.Detail = detail
	return st
}

// FromStage maps an envelope stage onto the CRD stage result.
func FromStage(st *envelope.Stage) *v1alpha1.StageResult {
	return &v1alpha1.StageResult{
		Outcome:   string(st.Outcome),
		Harness:   st.Harness,
		Model:     st.Model,
		SessionID: st.SessionID,
		NumTurns:  int32(st.NumTurns),
		Usage: v1alpha1.UsageSummary{
			InputTokens:         int64(st.Usage.InputTokens),
			OutputTokens:        int64(st.Usage.OutputTokens),
			CacheReadTokens:     int64(st.Usage.CacheReadTokens),
			CacheCreationTokens: int64(st.Usage.CacheCreationTokens),
			CostUSD:             FormatCost(stageCost(st)),
		},
		ElapsedMilliseconds: int64(st.ElapsedSeconds * 1000),
		Detail:              TruncateDetail(st.Detail),
	}
}

// stageCost is the harness-reported cost, or a fallback priced from the token
// counts at the model's published rates when the harness reports none (codex
// reports tokens but not cost). Zero when neither is available.
func stageCost(st *envelope.Stage) float64 {
	if st.Usage.CostUSD > 0 {
		return st.Usage.CostUSD
	}
	m, ok := model.ModelByID(model.Builtins(), st.Model)
	if !ok {
		return 0
	}
	priced := model.UsageCostUSD(m,
		st.Usage.InputTokens, st.Usage.CacheReadTokens, st.Usage.CacheCreationTokens, st.Usage.OutputTokens)
	if priced == nil {
		return 0
	}
	return *priced
}

// Analysis maps one envelope analysis dimension; nil when unassessed.
func Analysis(a envelope.AnalysisResult) *v1alpha1.Analysis {
	if a.Rating == "" {
		return nil
	}
	return &v1alpha1.Analysis{Rating: v1alpha1.Rating(a.Rating), Summary: TruncateDetail(a.Summary)}
}

// FormatCost renders a float cost as the CRD's decimal string (6 fractional
// digits — micro-USD precision); empty when unreported or not finite (NaN
// slips past a positivity check and +Inf passes it, and neither renders as a
// decimal the CRD pattern admits).
func FormatCost(f float64) string {
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 1) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 6, 64)
}

// FormatConfidence renders confidence as the CRD's decimal string; empty
// when out of range or NaN (which compares false against both bounds).
func FormatConfidence(f float64) string {
	if math.IsNaN(f) || f < 0 || f > 1 {
		return ""
	}
	// Adding zero turns a negative zero, which is in range, into a positive
	// one: rendered as-is it is "-0.0000", which the CRD pattern rejects.
	return strconv.FormatFloat(f+0, 'f', 4, 64)
}

// TruncateReport caps a report markdown for a CRD field.
func TruncateReport(s string) string { return truncate(s, maxReport) }

// TruncateDetail caps a short detail/summary for a CRD field.
func TruncateDetail(s string) string { return truncate(s, maxDetail) }

// truncate caps s at limit bytes on a rune boundary (the API server rejects
// invalid UTF-8).
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
