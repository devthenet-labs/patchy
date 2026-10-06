// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Report repair bounds. They are constants, not configuration: a new
// PATCHY_* key would change the environment a repository-image Job blanks.
// A round is one resumed run of the agent's own session. It gets at most
// repairMaxTurns turns and repairTimeout of wall clock, and never more than
// the stage has left of its turns, its output tokens and its wall clock; a
// round is not started with less than repairMinWindow of that wall clock
// left, since a resumed run that cannot finish only spends.
const (
	repairRounds    = 2
	repairMaxTurns  = 6
	repairTimeout   = 10 * time.Minute
	repairMinWindow = 2 * time.Minute
)

// stageRun is one stage's agent session: the terms its first run was
// launched on, which every repair continues the session under, and the one
// transcript recorder all of the stage's runs share, so the transcript's
// sequence numbers stay unique and its bounds hold across them.
type stageRun struct {
	h   harness.Harness
	cli string // the CLI run by absolute path on a repository image, "" for PATH
	// req is the first run's request: its prompt, model, turn cap, sandbox
	// posture, directories and session id.
	req     harness.PromptRequest
	timeout time.Duration // the stage's wall clock
	idle    time.Duration // the stage's idle limit, zero for none
	budget  int           // the stage's output-token budget, zero for none
	rec     *transcript.Recorder
	started time.Time
}

// newStage sets up a stage's session. The recorder is built here, once,
// after the stage has read its broker token.
func (a *Agent) newStage(h harness.Harness, cli string, req harness.PromptRequest, timeout, idle time.Duration,
	budget int) *stageRun {
	return &stageRun{h: h, cli: cli, req: req, timeout: timeout, idle: idle, budget: budget, rec: a.recorder(h)}
}

// start runs the stage's first run.
func (a *Agent) start(ctx context.Context, sr *stageRun) (runner.Result, string, error) {
	sr.started = time.Now()
	return a.run(ctx, sr, pinCLI(sr.h.PromptSpec(a.cfg.repoDir(), sr.req), sr.cli), sr.timeout, sr.budget)
}

// writable reports whether the stage may write the working tree, so a
// repair must be held to leaving it as it was.
func (sr *stageRun) writable() bool { return sr.req.Sandbox == harness.SandboxWorkspaceWrite }

// readReportAs reads a stage's report and parses it, with today's outcomes:
// report_missing when it cannot be read, report_invalid when it does not
// parse.
func readReportAs[T any](path string, read func(string) ([]byte, error),
	parse func([]byte) (T, error)) (T, []byte, envelope.Outcome, string) {
	var zero T
	raw, err := read(path)
	if err != nil {
		return zero, nil, envelope.OutcomeReportMissing, err.Error()
	}
	v, err := parse(raw)
	if err != nil {
		return zero, nil, envelope.OutcomeReportInvalid, err.Error()
	}
	return v, raw, envelope.OutcomeOK, ""
}

// settleReport reads and parses a stage's report once its first run ended
// OK, and when the report is missing or invalid asks the agent, in its own
// session, to repair it (repair). It returns the parsed report and its
// exact bytes — the final file, a repaired one included — or the outcome
// and detail the stage ends on. Every repair run's spend is added to st.
//
// A harness that cannot continue a session (codex, copilot) is not asked:
// its stage ends exactly as it did before repair existed.
func settleReport[T any](ctx context.Context, a *Agent, sr *stageRun, st *envelope.Stage, path string,
	read func(string) ([]byte, error), parse func([]byte) (T, error)) (T, []byte, envelope.Outcome, string) {
	v, raw, outcome, detail := readReportAs(path, read, parse)
	if outcome == envelope.OutcomeOK {
		return v, raw, outcome, ""
	}
	resumer, ok := sr.h.(harness.Resumer)
	if !ok {
		return v, nil, outcome, detail
	}

	rounds, last, why := 0, "", ""
	for round := 1; round <= repairRounds; round++ {
		if why = a.repairWindow(ctx, sr, st); why != "" {
			break
		}
		r := a.repairRound(ctx, sr, st, resumer, path, round, outcome, detail)
		if !r.ran {
			why = r.why
			break
		}
		rounds = round
		if r.refused != "" {
			return v, nil, outcome, fmt.Sprintf("%s (repair refused in round %d: %s)", detail, round, r.refused)
		}
		if r.why != "" {
			why, last = r.why, r.failure
			break
		}
		next, nextRaw, nextOutcome, nextDetail := readReportAs(path, read, parse)
		if nextOutcome == envelope.OutcomeOK {
			a.cfg.Log.Info("the agent repaired its report", "phase", a.cfg.Phase, "round", round)
			if sr.rec != nil {
				sr.rec.Notice("patchy accepted the report repaired in round %d", round)
			}
			return next, nextRaw, nextOutcome, ""
		}
		outcome, detail, last = nextOutcome, nextDetail, r.failure
	}
	if rounds == 0 && why == "" {
		return v, nil, outcome, detail // no round is configured
	}
	return v, nil, outcome, detail + " (" + notRepaired(rounds, last, why) + ")"
}

// notRepaired explains, after a report's detail, why the report stands
// refused: how many rounds ran, how the last one's run ended when it did
// not end OK, and why no further round ran when one could have.
func notRepaired(rounds int, last, why string) string {
	if rounds == 0 {
		return "not repaired: " + why
	}
	s := fmt.Sprintf("not repaired in %d round", rounds)
	if rounds > 1 {
		s += "s"
	}
	if last != "" {
		s += "; the last repair run ended " + last
	}
	if why != "" {
		s += "; " + why
	}
	return s
}

// repairWindow says why no repair round may start now, or "" when one may:
// the stage cancelled, or too little of its wall clock, turns or output
// tokens left.
func (a *Agent) repairWindow(ctx context.Context, sr *stageRun, st *envelope.Stage) string {
	if ctx.Err() != nil {
		return "the stage was cancelled"
	}
	if left := sr.timeout - time.Since(sr.started); left < repairMinWindow {
		return fmt.Sprintf("less than %s of the stage's %s wall clock left", shortDuration(repairMinWindow),
			shortDuration(sr.timeout))
	}
	if sr.req.MaxTurns > 0 && sr.req.MaxTurns-st.NumTurns < 1 {
		return fmt.Sprintf("no turns left of the stage's %d", sr.req.MaxTurns)
	}
	if sr.budget > 0 && sr.budget-st.Usage.OutputTokens < 1 {
		return fmt.Sprintf("no output tokens left of the stage's %d", sr.budget)
	}
	return ""
}

// repairResult is what one repair round did: whether its run started
// (ran), why the rounds stop after it (why), a refusal of what it did to
// the working tree (refused), and how its run ended when not OK (failure).
type repairResult struct {
	ran     bool
	why     string
	refused string
	failure string
}

// repairFailureBytes bounds how a repair run's own ending is quoted after
// the report's detail, so the report's error stays the detail's lead.
const repairFailureBytes = 300

// repairRound runs one repair: the agent's session resumed with the repair
// prompt, under the stage's terms and what is left of its limits, its spend
// added to st. On a stage that writes the working tree, the tree must be
// exactly as the round found it afterwards, the report and commit.sh aside
// (both sit outside the clone): anything else is refused, since nothing a
// repair changes was built or tested.
func (a *Agent) repairRound(ctx context.Context, sr *stageRun, st *envelope.Stage, resumer harness.Resumer,
	path string, round int, outcome envelope.Outcome, detail string) repairResult {
	var before fingerprint
	if sr.writable() {
		var err error
		if before, err = treeFingerprint(ctx, a.cfg.repoDir()); err != nil {
			return repairResult{why: "the working tree could not be fingerprinted to guard a repair: " + err.Error()}
		}
	}
	env, err := a.brokerEnv()
	if err != nil {
		return repairResult{why: err.Error()}
	}
	missing := outcome == envelope.OutcomeReportMissing
	commitScript := ""
	if sr.writable() {
		commitScript = a.cfg.commitScript()
	}
	prompt, err := templates.RenderRepairPrompt(templates.RepairPrompt{
		ReportPath: path, Missing: missing, Error: detail, Round: round, Rounds: repairRounds,
		CommitScriptPath: commitScript,
	})
	if err != nil {
		return repairResult{why: err.Error()}
	}

	req := sr.req
	req.Prompt, req.Env = prompt, env
	req.MaxTurns = repairMaxTurns
	if sr.req.MaxTurns > 0 {
		req.MaxTurns = min(repairMaxTurns, sr.req.MaxTurns-st.NumTurns)
	}
	budget := 0
	if sr.budget > 0 {
		budget = sr.budget - st.Usage.OutputTokens
	}
	timeout := min(repairTimeout, sr.timeout-time.Since(sr.started))
	session := st.SessionID
	if session == "" {
		session = sr.req.SessionID
	}

	if sr.rec != nil {
		// The caller token was read afresh: the shared recorder must scrub
		// the new value from the turns this run records.
		sr.rec.AddSecrets(a.scrub...)
		sr.rec.Notice("patchy refused the report (%s); asking the agent to repair it, round %d of %d",
			oneLine(detail, repairFailureBytes), round, repairRounds)
	}
	a.cfg.Log.Info("asking the agent to repair its report", "phase", a.cfg.Phase, "round", round,
		"outcome", outcome, "session", session)
	res, idle, runErr := a.run(ctx, sr, pinCLI(resumer.ResumeSpec(a.cfg.repoDir(), session, req), sr.cli),
		timeout, budget)
	var spent envelope.Stage
	a.fillStage(&spent, sr.h, res)
	addRepair(st, spent)

	r := repairResult{ran: true, failure: a.repairFailure(sr.h, res, idle, runErr)}
	if ctx.Err() != nil {
		r.why = fmt.Sprintf("the stage was cancelled during repair round %d", round)
		return r
	}
	if sr.writable() {
		after, err := treeFingerprint(ctx, a.cfg.repoDir())
		switch {
		case err != nil:
			r.refused = "the working tree could not be fingerprinted after it: " + err.Error()
		case after != before:
			r.refused = changed(ctx, a.cfg.repoDir(), before, after)
		}
		if r.refused != "" && sr.rec != nil {
			sr.rec.Notice("patchy refused repair round %d: %s", round, r.refused)
		}
	}
	return r
}

// repairFailure says how a repair run ended when it did not end OK, as
// "<outcome>: <detail>" cut to repairFailureBytes, or "" for a run that
// ended OK.
func (a *Agent) repairFailure(h harness.Harness, res runner.Result, idle string, runErr error) string {
	var outcome envelope.Outcome
	var detail string
	switch {
	case idle != "":
		outcome, detail = envelope.OutcomeTimeout, idle
	case res.Aborted:
		outcome, detail = envelope.OutcomeBudgetExceeded, res.AbortReason
	default:
		outcome, detail = stageOutcome(h, res, runErr, credentialValues(h, a.scrub...))
	}
	if outcome == envelope.OutcomeOK {
		return ""
	}
	return oneLine(string(outcome)+": "+detail, repairFailureBytes)
}

// addRepair adds a repair run's accounting to its stage's. Tokens, turns
// and wall clock are summed, because a resumed claude run reports its own
// (probed on 2.1.291: the resumed result's usage and num_turns count that
// invocation alone). Its total_cost_usd is not, because it is cumulative
// over the session, so summing it would count the first run twice: the
// repair's own tokens are priced at the stage model's rates instead and
// added to the cost the first run reported. A stage whose first run
// reported no cost keeps none, and the controller prices all of its summed
// tokens (agentresult.stageCost); a model with no rates leaves the first
// run's cost as it is.
func addRepair(st *envelope.Stage, run envelope.Stage) {
	st.ElapsedSeconds += run.ElapsedSeconds
	st.NumTurns += run.NumTurns
	st.Usage.InputTokens += run.Usage.InputTokens
	st.Usage.OutputTokens += run.Usage.OutputTokens
	st.Usage.CacheReadTokens += run.Usage.CacheReadTokens
	st.Usage.CacheCreationTokens += run.Usage.CacheCreationTokens
	if st.Usage.CostUSD <= 0 {
		return
	}
	m, ok := model.ModelByID(model.Builtins(), st.Model)
	if !ok {
		return
	}
	if cost := model.UsageCostUSD(m, run.Usage.InputTokens, run.Usage.CacheReadTokens,
		run.Usage.CacheCreationTokens, run.Usage.OutputTokens); cost != nil {
		st.Usage.CostUSD += *cost
	}
}
