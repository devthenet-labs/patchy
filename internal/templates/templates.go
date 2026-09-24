// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"
)

//go:embed *.md.tmpl
var files embed.FS

// tmpl parses every embedded template once; a parse failure is a programmer
// error caught by the package's golden tests, so panicking at init is right.
var tmpl = template.Must(template.New("").
	Funcs(template.FuncMap{"join": strings.Join, "code": code, "fence": fence, "chomp": chomp}).
	ParseFS(files, "*.md.tmpl"))

// chomp drops trailing line breaks, so a block ending in one (fence) can sit
// flush against the template text that follows it.
func chomp(s string) string { return strings.TrimRight(s, "\r\n") }

func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return b.String(), nil
}

// PRBody renders the pull-request body for a remediation branch; issue is
// the tracking issue number ("Fixes #issue" auto-links and auto-closes).
func PRBody(issue int, report string) (string, error) {
	return render("pr_body.md.tmpl", struct {
		Issue  int
		Report string
	}{issue, report})
}

// InvestigatePrompt is the data for the analysis-stage prompt (stage 1:
// exploitability/likelihood/impact plus the verdict).
type InvestigatePrompt struct {
	IssuePath     string
	ReportPath    string
	AllowedModels []string
	// AutoMaxTurns/AutoTokenBudget is what a remediation gets unattended, and
	// the line past which an estimate needs human approval — NOT a cap.
	AutoMaxTurns    int
	AutoTokenBudget int
	// ManualMaxTurns/ManualTokenBudget is the most a human-approved run can
	// get. Rendered so the agent knows asking beyond it buys nothing.
	ManualMaxTurns    int
	ManualTokenBudget int
	// Calibration is how earlier estimates compared to reality; nil omits the
	// section (a cold start has nothing honest to say).
	Calibration *Calibration
	// PreviousAttempt is the failed investigation this one retries; nil
	// omits the section.
	PreviousAttempt *PreviousAttempt
}

// Calibration reports how previous remediations compared to what the
// investigation predicted, so the next estimate can correct for the observed
// skew. It is derived from a FindingRollup's remediation stage aggregate and
// carried to the pod as JSON — the runner has no Kubernetes access, so this
// travels as configuration like every other fact it needs.
type Calibration struct {
	// Scope describes where the figures came from, e.g. "owner/repo" or
	// "all repositories" when the per-repository history was too thin.
	Scope string `json:"scope,omitempty"`
	// Runs the averages are drawn from.
	Runs int64 `json:"runs"`
	// AvgPredictedTurns/AvgActualTurns and the token pair are whole-number
	// averages over those runs.
	AvgPredictedTurns        int64 `json:"avgPredictedTurns"`
	AvgActualTurns           int64 `json:"avgActualTurns"`
	AvgPredictedOutputTokens int64 `json:"avgPredictedOutputTokens"`
	AvgActualOutputTokens    int64 `json:"avgActualOutputTokens"`
}

// TurnSkew is how far actual turns ran over (positive) or under (negative)
// the prediction, as a percentage. Zero when there is no prediction to
// compare against.
func (c *Calibration) TurnSkew() int64 {
	return skewPercent(c.AvgPredictedTurns, c.AvgActualTurns)
}

// TokenSkew is the same measure for output tokens.
func (c *Calibration) TokenSkew() int64 {
	return skewPercent(c.AvgPredictedOutputTokens, c.AvgActualOutputTokens)
}

// skewPercent is actual ÷ predicted - 1, as a rounded percentage.
func skewPercent(predicted, actual int64) int64 {
	if predicted <= 0 {
		return 0
	}
	return (actual - predicted) * 100 / predicted
}

// PreviousAttempt is the failed run a retry follows, rendered into the stage
// prompt so the agent is told what went wrong rather than handed the inputs
// that already failed once. It is copied from the retry's own immutable spec
// and carried to the pod as JSON, like Calibration — the runner has no
// Kubernetes access.
//
// Outcome and Detail are UNTRUSTED: a run's detail can quote output from the
// repository or the image it ran on (a git status listing, a stderr tail).
// The prompt renders them bounded (PreviousOutcomeMaxBytes,
// PreviousDetailMaxBytes), stripped of control characters, with the detail in
// a fence no line of it can close, under a statement that it is data, not
// instructions.
type PreviousAttempt struct {
	// Attempt is the failed run's ordinal.
	Attempt int32 `json:"attempt"`
	// Outcome is its stage outcome (commit_failed, timeout, ...), or
	// pull_request_closed when its pull request was closed unmerged.
	Outcome string `json:"outcome"`
	// Detail explains the outcome.
	Detail string `json:"detail,omitempty"`
}

// Bounds on what a prompt quotes from a previous attempt, in bytes. A detail
// cut short is marked with previousDetailCut after the bound.
const (
	PreviousOutcomeMaxBytes = 64
	PreviousDetailMaxBytes  = 4096
	previousDetailCut       = "\n[truncated]"
)

// quotable returns p as a prompt may quote it: the outcome on one line and
// the detail with its line breaks normalized, both free of control and
// format characters, valid UTF-8, and cut to their byte bounds on a rune
// boundary. Nil stays nil.
func (p *PreviousAttempt) quotable() *PreviousAttempt {
	if p == nil {
		return nil
	}
	detail := plainText(p.Detail)
	if len(detail) > PreviousDetailMaxBytes {
		detail = cutRunes(detail, PreviousDetailMaxBytes) + previousDetailCut
	}
	return &PreviousAttempt{
		Attempt: p.Attempt,
		Outcome: cutRunes(strings.Join(strings.Fields(plainText(p.Outcome)), " "), PreviousOutcomeMaxBytes),
		Detail:  strings.TrimRight(detail, "\n"),
	}
}

// plainText makes untrusted text safe to quote in a prompt: invalid UTF-8
// replaced, CRLF and lone CR turned into LF, and every other control or
// format character dropped except tab — a NUL cannot even be passed as a
// command-line argument, and terminal escapes or bidi overrides have no
// business in a prompt.
func plainText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// cutRunes caps valid UTF-8 s at limit bytes without splitting a rune.
func cutRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// RenderInvestigatePrompt renders the investigation prompt.
func RenderInvestigatePrompt(p InvestigatePrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	return render("prompt_investigate.md.tmpl", p)
}

// RemediatePrompt is the data for the stage-2 (remediation) prompt.
type RemediatePrompt struct {
	IssuePath         string
	InvestigationPath string
	ReportPath        string
	CommitScriptPath  string
	// PreviousAttempt is the failed remediation this one retries; nil omits
	// the section.
	PreviousAttempt *PreviousAttempt
}

// RenderRemediatePrompt renders the remediation prompt.
func RenderRemediatePrompt(p RemediatePrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	return render("prompt_remediate.md.tmpl", p)
}
