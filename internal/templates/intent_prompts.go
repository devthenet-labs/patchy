// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import "strings"

// PlanPrompt is the data for an intent's plan-stage prompt: read the request
// and the repository, write a plan a human approves before anything is
// built.
type PlanPrompt struct {
	// IssuePath is the intent snapshot the controller handed the pod
	// (input/issue.md). It must list the Project's repositories by URL: the
	// prompt tells the planner to name the ones it changes exactly as
	// listed there, and the controller rejects a plan naming any other.
	IssuePath  string
	ReportPath string
	// Intent is the snapshot's text, quoted into the prompt so the planner
	// starts from the request itself. It is UNTRUSTED — an issue anyone may
	// have written and an approver triggered, plus approver comments on a
	// replan — so RenderPlanPrompt bounds it (RequestQuoteMaxBytes), strips
	// its control characters and fences it under a statement that it says
	// what to build, never how the agent works.
	Intent string
	// BuildMaxTurns/BuildTokenBudget are the most a build of this plan can
	// be granted, so the plan is sized to fit. agent-runner fills them from
	// the plan Job's PATCHY_REMEDIATE_MANUAL_*, which is the build stage's
	// own ceiling; the intent controller sets it to the grant the Project's
	// build will receive, so the number stated is the one the build gets.
	BuildMaxTurns    int
	BuildTokenBudget int
	// PreviousAttempt is the failed plan this one retries; nil omits the
	// section.
	PreviousAttempt *PreviousAttempt
}

// RequestQuoteMaxBytes bounds the request a plan prompt quotes; the rest stays
// in the file the prompt names, marked by requestCut.
const (
	RequestQuoteMaxBytes = 32 << 10
	requestCut           = "\n[truncated here: the whole request is in the file]"
)

// quotableRequest makes the request safe to quote, as quotable does a
// previous attempt's detail.
func quotableRequest(s string) string {
	s = plainText(s)
	if len(s) > RequestQuoteMaxBytes {
		s = cutRunes(s, RequestQuoteMaxBytes) + requestCut
	}
	return strings.Trim(s, "\n")
}

// RenderPlanPrompt renders the plan-stage prompt.
func RenderPlanPrompt(p PlanPrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	p.Intent = quotableRequest(p.Intent)
	return render("prompt_plan.md.tmpl", p)
}

// BuildPrompt is the data for an intent's build-stage prompt, which builds
// the approved plan — first as a build, then for each revise round.
//
// It names no request. The approved plan is the build's whole contract:
// the approver read the plan byte for byte, but the request only as GitHub
// rendered it, which hides HTML comments, <details> blocks and characters
// that render as nothing; a request handed to the build could steer it
// past what was approved. agent-runner refuses a build whose request file
// is not empty.
type BuildPrompt struct {
	// PlanPath is the approved plan (input/investigation.md — the Job's
	// analysis handoff, reused). On a revise round the controller follows
	// the plan with that round's feedback and compare patch, which the
	// prompt tells the agent to address as data from the reviewers.
	PlanPath         string
	ReportPath       string
	CommitScriptPath string
	// PreviousAttempt is the failed build this one retries; nil omits the
	// section.
	PreviousAttempt *PreviousAttempt
}

// RenderBuildPrompt renders the build-stage prompt.
func RenderBuildPrompt(p BuildPrompt) (string, error) {
	p.PreviousAttempt = p.PreviousAttempt.quotable()
	return render("prompt_build.md.tmpl", p)
}
