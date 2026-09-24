// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// The harnesses an intent stage may run on (see intentHarness).
const (
	claudeHarness = "claude"
	fakeHarness   = "fake"
)

// runIntent runs an intent stage, PhasePlan or PhaseBuild. Like Run it
// returns an error only for a fatal, before-the-stage failure — a harness
// intents may not run on, or a build handed no parseable approved plan —
// which it has already emitted as a fatal event. Stage outcomes, failed ones
// included, are events with a nil return.
func (a *Agent) runIntent(ctx context.Context) error {
	fatal := func(err error) error {
		a.emit(envelope.Event{Type: envelope.TypeFatal, Error: err.Error()})
		return err
	}
	if a.cfg.Phase == PhasePlan {
		if err := a.intentHarness(a.cfg.InvestigateHarness); err != nil {
			return fatal(err)
		}
		a.emit(envelope.Event{Type: envelope.TypePlan, Plan: a.plan(ctx)})
		return nil
	}
	if err := a.intentHarness(a.cfg.RemediateHarness); err != nil {
		return fatal(err)
	}
	params, err := a.buildInput()
	if err != nil {
		return fatal(err)
	}
	a.emit(envelope.Event{Type: envelope.TypeRemediation, Remediation: a.build(ctx, params)})
	return nil
}

// intentHarness refuses every harness an intent stage may not run on. Only
// claude renders the sandbox postures the stages rely on — codex and copilot
// ignore them, so a plan would not stay read-only — and only brokered claude
// keeps the model credential out of the pod, which matters all the more on
// a build that runs in the repository's own image. The fake harness is
// admitted as well: it replays a fixture through cat, runs no agent and
// reaches no model, and it is how tests drive these stages.
func (a *Agent) intentHarness(id string) error {
	switch id {
	case claudeHarness:
		if a.cfg.BrokerTokenFile == "" {
			return fmt.Errorf("intent %s stage: the claude harness must be brokered, and PATCHY_BROKER_TOKEN_FILE is unset",
				a.cfg.Phase)
		}
		return nil
	case fakeHarness:
		return nil
	}
	return fmt.Errorf("intent %s stage: harness %q is refused: intents run on brokered claude only, "+
		"because no other harness honours the sandbox postures", a.cfg.Phase, id)
}

// planLimits resolves the plan stage's turns and output tokens: the
// investigate stage's configured limits, which a per-Job grant may lower but
// never raise. The intent controller grants each run its Project's per-stage
// limits, already clamped to its own ceilings; the runner checks again
// against the ceiling it was configured with, because it is what spends the
// money. (A build takes the remediate stage's grant() instead: the same
// grant channel, clamped between the automated and the manual budgets.)
func (a *Agent) planLimits() (maxTurns, budget int) {
	maxTurns, budget = a.cfg.InvestigateMaxTurns, a.cfg.InvestigateTokenBudget
	if g := a.cfg.GrantedMaxTurns; g > 0 {
		if g > maxTurns {
			a.cfg.Log.Warn("granted max_turns clamped to the plan stage's limit", "granted", g, "limit", maxTurns)
		} else {
			maxTurns = g
		}
	}
	if g := a.cfg.GrantedTokenBudget; g > 0 {
		if g > budget {
			a.cfg.Log.Warn("granted token_budget clamped to the plan stage's limit", "granted", g, "limit", budget)
		} else {
			budget = g
		}
	}
	return maxTurns, budget
}

// buildInput validates the approved plan the controller handed the build —
// the Job's analysis handoff, input/investigation.md — and resolves what
// the run may spend, exactly as remediationInput does for a fix. Only the
// plan's frontmatter is checked (report.ParsePlanInput): a revise round
// follows the plan with that round's feedback, past the plan's own bounds.
func (a *Agent) buildInput() (remediationParams, error) {
	raw, err := os.ReadFile(a.cfg.inputInvestigation())
	if err != nil {
		return remediationParams{}, fmt.Errorf("input plan: %w", err)
	}
	if _, err := report.ParsePlanInput(raw); err != nil {
		return remediationParams{}, fmt.Errorf("input plan: %w", err)
	}
	maxTurns, budget := a.grant()
	return remediationParams{maxTurns: maxTurns, budget: budget}, nil
}

// readReport reads an intent stage's report, at most one byte past the
// report bound: enough for the parser to refuse an oversized one, without
// reading whatever size the agent wrote into memory.
func readReport(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, report.ReportMaxBytes+1))
}

// plan runs the plan stage read-only and folds the result into the event
// payload: the report exactly as written, which is what the approver reads
// and the build follows, and its parsed frontmatter.
func (a *Agent) plan(ctx context.Context) *envelope.Plan {
	ev := &envelope.Plan{Stage: envelope.Stage{
		Harness: a.cfg.InvestigateHarness,
		Model:   a.cfg.InvestigateModel,
	}}

	h, ok := harness.ByID(a.cfg.InvestigateHarness)
	if !ok {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = fmt.Sprintf("unknown harness %q", a.cfg.InvestigateHarness)
		return ev
	}
	cli, err := a.preflight(ctx, h)
	if err != nil {
		ev.Outcome = envelope.OutcomeImageIncompatible
		ev.Detail = err.Error()
		return ev
	}
	intent, err := os.ReadFile(a.cfg.issuePath())
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}
	prompt, err := templates.RenderPlanPrompt(templates.PlanPrompt{
		IssuePath:  a.cfg.issuePath(),
		ReportPath: a.cfg.planPath(),
		Intent:     string(intent),
		// The most a build can be granted: grant() clamps it to the manual
		// budget.
		BuildMaxTurns:    a.cfg.RemediateManualMaxTurns,
		BuildTokenBudget: a.cfg.RemediateManualTokenBudget,
		PreviousAttempt:  a.cfg.PreviousAttempt,
	})
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}
	env, err := a.brokerEnv()
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}

	maxTurns, budget := a.planLimits()
	onLine, _ := a.observe(h, budget)
	res, runErr := a.exec.Run(ctx, pinCLI(h.PromptSpec(a.cfg.repoDir(), harness.PromptRequest{
		Prompt:    prompt,
		Model:     a.cliModel(a.cfg.InvestigateModel, a.cfg.InvestigateHarness),
		MaxTurns:  maxTurns,
		Sandbox:   harness.SandboxReadOnly,
		SessionID: a.newSessionID(),
		AddDirs:   []string{a.cfg.Workspace},
		Env:       env,
	}), cli), a.cfg.InvestigateTimeout, onLine)
	a.fillStage(&ev.Stage, h, res)

	if res.Aborted {
		ev.Outcome = envelope.OutcomeBudgetExceeded
		ev.Detail = res.AbortReason
		return ev
	}
	if outcome, detail := stageOutcome(h, res, runErr, credentialValues(h, a.scrub...)); outcome != envelope.OutcomeOK {
		ev.Outcome, ev.Detail = outcome, detail
		return ev
	}

	raw, err := readReport(a.cfg.planPath())
	if err != nil {
		ev.Outcome = envelope.OutcomeReportMissing
		ev.Detail = err.Error()
		return ev
	}
	p, err := report.ParsePlan(raw)
	if err != nil {
		ev.Outcome = envelope.OutcomeReportInvalid
		ev.Detail = err.Error()
		return ev
	}
	ev.Outcome = envelope.OutcomeOK
	// Raw, frontmatter included: these bytes are what the controller stores,
	// digests, posts for approval and hands the build.
	ev.ReportMarkdown = string(raw)
	ev.Summary = p.Summary
	ev.Repositories = p.Repositories
	ev.NewDependencies = p.NewDependencies
	ev.Questions = p.Questions
	ev.Confidence = *p.Confidence
	ev.EstimatedMaxTurns = p.EstimatedMaxTurns
	ev.EstimatedTokenBudget = p.EstimatedTokenBudget
	return ev
}

// build runs the build stage — the approved plan's first build or a revise
// round — with the workspace writable, and packages the changeset exactly as
// a remediation does: the same branch checkout, commit.sh, verification and
// changeset helpers, so the repository, not the agent's claim, decides.
func (a *Agent) build(ctx context.Context, params remediationParams) *envelope.Remediation {
	ev := &envelope.Remediation{Stage: envelope.Stage{
		Harness: a.cfg.RemediateHarness,
		Model:   a.cfg.RemediateModel,
	}}

	h, ok := harness.ByID(a.cfg.RemediateHarness)
	if !ok {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = fmt.Sprintf("unknown harness %q", a.cfg.RemediateHarness)
		return ev
	}
	cli, err := a.preflight(ctx, h)
	if err != nil {
		ev.Outcome = envelope.OutcomeImageIncompatible
		ev.Detail = err.Error()
		return ev
	}
	env, err := a.brokerEnv()
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}
	if err := ensureIdentity(ctx, a.cfg.repoDir()); err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}
	// HEAD at startup is the pinned base the init container fetched — the
	// default branch for a build, the pull request's head for a revise
	// round; the changeset is diffed against it and the pushed commit
	// parents it.
	baseSHA, err := headSHA(ctx, a.cfg.repoDir())
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}
	// The local branch is packageChangeset's (patchy/<run>): it never leaves
	// the pod, and the event does not report it — the intent controller
	// names the pushed branch itself.
	if err := checkoutBranch(ctx, a.cfg.repoDir(), a.cfg.branch()); err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}

	prompt, err := templates.RenderBuildPrompt(templates.BuildPrompt{
		IssuePath:        a.cfg.issuePath(),
		PlanPath:         a.cfg.inputInvestigation(),
		ReportPath:       a.cfg.buildPath(),
		CommitScriptPath: a.cfg.commitScript(),
		PreviousAttempt:  a.cfg.PreviousAttempt,
	})
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}

	onLine, _ := a.observe(h, params.budget)
	res, runErr := a.exec.Run(ctx, pinCLI(h.PromptSpec(a.cfg.repoDir(), harness.PromptRequest{
		Prompt:    prompt,
		Model:     a.cliModel(a.cfg.RemediateModel, a.cfg.RemediateHarness),
		MaxTurns:  params.maxTurns,
		Sandbox:   harness.SandboxWorkspaceWrite,
		SessionID: a.newSessionID(),
		AddDirs:   []string{a.cfg.Workspace},
		Env:       env,
	}), cli), a.cfg.RemediateTimeout, onLine)
	a.fillStage(&ev.Stage, h, res)

	if res.Aborted {
		ev.Outcome = envelope.OutcomeBudgetExceeded
		ev.Detail = res.AbortReason
		return ev
	}
	if outcome, detail := stageOutcome(h, res, runErr, credentialValues(h, a.scrub...)); outcome != envelope.OutcomeOK {
		ev.Outcome, ev.Detail = outcome, detail
		return ev
	}

	raw, err := readReport(a.cfg.buildPath())
	if err != nil {
		ev.Outcome = envelope.OutcomeReportMissing
		ev.Detail = err.Error()
		return ev
	}
	b, err := report.ParseBuild(raw)
	if err != nil {
		ev.Outcome = envelope.OutcomeReportInvalid
		ev.Detail = err.Error()
		return ev
	}
	// Raw, frontmatter included, as a remediation's: the controller strips
	// the fence before rendering the pull-request body.
	ev.ReportMarkdown = string(raw)
	ev.Outcome = envelope.OutcomeOK

	if !*b.Success {
		return ev
	}
	// The agent claims success; the repository decides. commit.sh must run
	// cleanly and leave real commits, else the claim is downgraded.
	if outcome, detail := a.packageChangeset(ctx, baseSHA, ev); outcome != envelope.OutcomeOK {
		ev.Outcome, ev.Detail = outcome, detail
		return ev
	}
	ev.Success = true
	return ev
}
