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
// intents may not run on, a plan whose repositories manifest is broken or
// whose trees are missing (repositories), or a build handed no parseable
// approved plan, a request beside it, or a multi-repository plan it cannot
// find its own repository in (buildInput) — which it has already emitted as
// a fatal event. Stage outcomes, failed ones included, are events with a
// nil return.
func (a *Agent) runIntent(ctx context.Context) error {
	fatal := func(err error) error {
		a.emit(envelope.Event{Type: envelope.TypeFatal, Error: err.Error()})
		return err
	}
	if a.cfg.Phase == PhasePlan {
		if err := a.intentHarness(a.cfg.InvestigateHarness); err != nil {
			return fatal(err)
		}
		repos, err := a.repositories()
		if err != nil {
			return fatal(err)
		}
		a.emit(envelope.Event{Type: envelope.TypePlan, Plan: a.plan(ctx, repos)})
		return nil
	}
	if err := a.intentHarness(a.cfg.RemediateHarness); err != nil {
		return fatal(err)
	}
	params, scope, err := a.buildInput()
	if err != nil {
		return fatal(err)
	}
	a.emit(envelope.Event{Type: envelope.TypeRemediation, Remediation: a.build(ctx, params, scope)})
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
// money.
func (a *Agent) planLimits() (maxTurns, budget int) {
	return a.lowered("max_turns", a.cfg.GrantedMaxTurns, a.cfg.InvestigateMaxTurns, a.cfg.InvestigateMaxTurns),
		a.lowered("token_budget", a.cfg.GrantedTokenBudget, a.cfg.InvestigateTokenBudget, a.cfg.InvestigateTokenBudget)
}

// buildLimits resolves the build stage's turns and output tokens with the
// plan stage's semantics, not a remediation's grant(): the remediate
// stage's manual budget is the ceiling, and a per-Job grant may lower it
// but never raise it. A build's grant is its Project's build or revise
// limit, which the operator chose and the controller already clamped — not
// an estimate a low guess could starve, as a Finding's is — so the
// automated budget is no floor under it, and a Project that tightens a
// stage below it is honoured. Only a Job with no grant falls back to the
// automated budget.
func (a *Agent) buildLimits() (maxTurns, budget int) {
	return a.lowered("max_turns", a.cfg.GrantedMaxTurns, a.cfg.RemediateManualMaxTurns,
			min(a.cfg.RemediateAutoMaxTurns, a.cfg.RemediateManualMaxTurns)),
		a.lowered("token_budget", a.cfg.GrantedTokenBudget, a.cfg.RemediateManualTokenBudget,
			min(a.cfg.RemediateAutoTokenBudget, a.cfg.RemediateManualTokenBudget))
}

// lowered resolves one intent-stage limit from its per-Job grant: the grant
// when it is set and within the stage's ceiling, the ceiling when the grant
// is above it, and fallback when there is no grant.
func (a *Agent) lowered(name string, grant, ceiling, fallback int) int {
	switch {
	case grant <= 0:
		return fallback
	case grant > ceiling:
		a.cfg.Log.Warn("granted "+name+" clamped to the stage's ceiling",
			"stage", a.cfg.Phase, "granted", grant, "ceiling", ceiling)
		return ceiling
	}
	return grant
}

// buildInput validates what the controller handed the build and resolves
// what the run may spend (buildLimits).
//
// The approved plan — the Job's analysis handoff, input/investigation.md —
// is checked by report.ParsePlanInput: its frontmatter, and that every byte
// is visible, a revise round's feedback after it included, past the plan's
// own bounds.
//
// The request — the Job's issue handoff, input/issue.md — must be empty.
// The plan is the build's whole contract: the approver read it verbatim,
// byte for byte, but read the request only as GitHub rendered it, which
// hides HTML comments, <details> blocks and characters that render as
// nothing. A request in the pod would sit where the build agent, which may
// read the whole workspace, could act on text no approver saw, so a build
// handed one is refused before any agent runs.
//
// A plan naming several repositories is built once in each, so the build
// resolves which one it is in (scopeBuild) from the controller's
// PATCHY_REPO and the plan's own frontmatter alone: no new input reaches
// it.
func (a *Agent) buildInput() (remediationParams, buildScope, error) {
	request, err := os.ReadFile(a.cfg.issuePath())
	if err != nil {
		return remediationParams{}, buildScope{}, fmt.Errorf("input request: %w", err)
	}
	if len(request) > 0 {
		return remediationParams{}, buildScope{}, fmt.Errorf("input request: %s holds %d bytes; an intent "+
			"build is handed the approved plan alone, and its request file must be empty",
			a.cfg.issuePath(), len(request))
	}
	raw, err := os.ReadFile(a.cfg.inputInvestigation())
	if err != nil {
		return remediationParams{}, buildScope{}, fmt.Errorf("input plan: %w", err)
	}
	plan, err := report.ParsePlanInput(raw)
	if err != nil {
		return remediationParams{}, buildScope{}, fmt.Errorf("input plan: %w", err)
	}
	scope, err := scopeBuild(plan.Repositories, a.cfg.Repo)
	if err != nil {
		return remediationParams{}, buildScope{}, fmt.Errorf("input plan: %w", err)
	}
	maxTurns, budget := a.buildLimits()
	return remediationParams{maxTurns: maxTurns, budget: budget}, scope, nil
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
// and the build follows, and its parsed frontmatter. repos is the
// repositories manifest of a multi-repository plan (nil for one
// repository): the prompt lists each tree, and a plan naming a repository
// outside it is invalid, with the reason its retry is told.
//
// The planner reads the other trees with the read-only posture's own
// tools: they sit under the workspace, which every stage adds as a
// directory the agent may read, and they have no git history for the
// posture's git-only Bash to read, so the posture is not widened for them.
func (a *Agent) plan(ctx context.Context, repos []manifestRepository) *envelope.Plan {
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
		// The most the build can be granted: buildLimits' ceiling, read from
		// this Job's PATCHY_REMEDIATE_MANUAL_*. The intent controller sets it
		// on the plan launch to the grant the Project's build will get, so
		// the plan is sized against the budget its build receives rather
		// than the runner's default ceiling.
		BuildMaxTurns:    a.cfg.RemediateManualMaxTurns,
		BuildTokenBudget: a.cfg.RemediateManualTokenBudget,
		PreviousAttempt:  a.cfg.PreviousAttempt,
		Trees:            planTrees(repos),
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
	// The stage's wall clock and idle limit are the investigate stage's: no
	// per-Job timeout reaches the pod (a new key would change the
	// repository-image Job), so the intent controller sets each stage's on
	// the Env of the jobs Client it launches that stage with.
	res, idle, runErr := a.run(ctx, h, pinCLI(h.PromptSpec(a.cfg.repoDir(), harness.PromptRequest{
		Prompt:    prompt,
		Model:     a.cliModel(a.cfg.InvestigateModel, a.cfg.InvestigateHarness),
		MaxTurns:  maxTurns,
		Sandbox:   harness.SandboxReadOnly,
		SessionID: a.newSessionID(),
		AddDirs:   []string{a.cfg.Workspace},
		Env:       env,
	}), cli), a.cfg.InvestigateTimeout, a.cfg.InvestigateIdleTimeout, budget)
	a.fillStage(&ev.Stage, h, res)

	if idle != "" {
		ev.Outcome, ev.Detail = envelope.OutcomeTimeout, idle
		return ev
	}
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
	if reason := outsideManifest(p.Repositories, repos); reason != "" {
		ev.Outcome = envelope.OutcomeReportInvalid
		ev.Detail = reason
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
// changeset helpers, so the repository, not the agent's claim, decides. In
// one repository of a multi-repository plan, scope names that repository
// and its siblings to the prompt.
func (a *Agent) build(ctx context.Context, params remediationParams, scope buildScope) *envelope.Remediation {
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
		PlanPath:         a.cfg.inputInvestigation(),
		ReportPath:       a.cfg.buildPath(),
		CommitScriptPath: a.cfg.commitScript(),
		PreviousAttempt:  a.cfg.PreviousAttempt,
		ThisRepository:   scope.this,
		Siblings:         scope.siblings,
	})
	if err != nil {
		ev.Outcome = envelope.OutcomeRuntimeError
		ev.Detail = err.Error()
		return ev
	}

	// The remediate timeout and idle limit, for a build and a revise round
	// alike: the intent controller launches each with its stage's in those
	// keys (see the plan stage).
	res, idle, runErr := a.run(ctx, h, pinCLI(h.PromptSpec(a.cfg.repoDir(), harness.PromptRequest{
		Prompt:    prompt,
		Model:     a.cliModel(a.cfg.RemediateModel, a.cfg.RemediateHarness),
		MaxTurns:  params.maxTurns,
		Sandbox:   harness.SandboxWorkspaceWrite,
		SessionID: a.newSessionID(),
		AddDirs:   []string{a.cfg.Workspace},
		Env:       env,
	}), cli), a.cfg.RemediateTimeout, a.cfg.RemediateIdleTimeout, params.budget)
	a.fillStage(&ev.Stage, h, res)

	if idle != "" {
		ev.Outcome, ev.Detail = envelope.OutcomeTimeout, idle
		return ev
	}
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
	// Raw, frontmatter included, as a remediation's. The controller records
	// it on the run; the pull request's body is its own, rendered from the
	// approved plan, never from this report.
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
