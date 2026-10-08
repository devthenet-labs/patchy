// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// streamResumed is a resumed claude run as 2.1.291 reports one: the same
// session, its own usage and turns, and a cumulative cost (here above the
// first run's 0.0123), which a stage must never add to the first run's.
const streamResumed = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_r1","usage":{"input_tokens":10,"cache_creation_input_tokens":5,` +
	`"cache_read_input_tokens":1000,"output_tokens":8},"content":[{"type":"text","text":"Report fixed."}]}}` + "\n" +
	`{"type":"result","subtype":"success","is_error":false,"result":"Report fixed.",` +
	`"session_id":"` + testSessionID + `","num_turns":2,"total_cost_usd":0.02,` +
	`"usage":{"input_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":1000,"output_tokens":8}}`

// streamNoSession is what claude prints resuming a session it cannot find.
const streamNoSession = `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,` +
	`"session_id":"` + testSessionID + `","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},` +
	`"errors":["No conversation found with session ID: ` + testSessionID + `"]}`

// The usage streamSuccess and streamResumed report.
var (
	firstUsage   = envelope.Usage{InputTokens: 100, CacheCreationTokens: 20, CacheReadTokens: 50, OutputTokens: 30}
	resumedUsage = envelope.Usage{InputTokens: 10, CacheCreationTokens: 5, CacheReadTokens: 1000, OutputTokens: 8}
)

// The reports a stage's agent gets wrong. Each fails its parser.
var (
	badInvestigation = "---\nrecommendation: nonsense\n---\n"
	badRemediation   = strings.Replace(goodRemediation, "confidence: 0.88", "confidence: .nan", 1)
	badPlan          = strings.Replace(goodPlan, "https://github.com", "http://github.com", 1)
	// scalarNotesBuild is a live slip: notes written as one string.
	scalarNotesBuild = strings.Replace(goodBuild, "notes: []",
		`notes: "What was built: the handler, and a test of its JSON shape"`, 1)
)

// longNoteBuild is overdub-10's build report: one note of 574 characters.
// It once failed its parser; the note is now cut instead.
var longNoteBuild = strings.Replace(goodBuild, "notes: []", `notes:`+"\n"+`  - "`+strings.Repeat("n", 574)+`"`, 1)

// repairCase is one stage as the repair tests drive it: its report, a good
// and a bad one, and what its first run leaves beside the report (a
// writable stage's commit.sh and its change to the tree).
type repairCase struct {
	phase    Phase
	config   func(t *testing.T, out *bytes.Buffer) (Config, string)
	report   string
	good     string
	bad      string
	outputs  map[string]string
	repo     map[string]string
	writable bool
	parse    func([]byte) error
}

func repairCases() []repairCase {
	byPhase := map[Phase]repairCase{
		PhaseInvestigate: {report: "reports/investigation.md", good: goodInvestigation, bad: badInvestigation,
			parse: func(b []byte) error { _, err := report.ParseInvestigation(b); return err }},
		PhaseRemediate: {report: "reports/remediation.md", good: goodRemediation, bad: badRemediation,
			outputs: map[string]string{"commit.sh": commitScript}, repo: map[string]string{"app.js": "escaped();\n"},
			writable: true, parse: func(b []byte) error { _, err := report.ParseRemediation(b); return err }},
		PhasePlan: {report: "reports/plan.md", good: goodPlan, bad: badPlan,
			parse: func(b []byte) error { _, err := report.ParsePlan(b); return err }},
		PhaseBuild: {report: "reports/build.md", good: goodBuild, bad: scalarNotesBuild,
			outputs: map[string]string{"commit.sh": buildCommitScript}, repo: map[string]string{"app.js": "version();\n"},
			writable: true, parse: func(b []byte) error { _, err := report.ParseBuild(b); return err }},
	}
	cases := make([]repairCase, 0, len(everyStage))
	for _, st := range everyStage {
		c := byPhase[st.phase]
		c.phase, c.config = st.phase, st.config
		cases = append(cases, c)
	}
	return cases
}

// setup builds the stage's config on brokered claude with a wall clock a
// repair fits in, and returns it with its workspace.
func (c repairCase) setup(t *testing.T, out *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := c.config(t, out)
	cfg.InvestigateHarness, cfg.RemediateHarness = "claude", "claude"
	cfg.BrokerTokenFile = filepath.Join(ws, "broker-token")
	if err := os.WriteFile(cfg.BrokerTokenFile, []byte("caller-token-first-0001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.InvestigateTimeout, cfg.RemediateTimeout = 30*time.Minute, 30*time.Minute
	return cfg, ws
}

// first is the stage's first run: report as given, the stage's other
// outputs and its change to the tree.
func (c repairCase) first(ws, rep string) step {
	writes := maps.Clone(c.outputs)
	if writes == nil {
		writes = map[string]string{}
	}
	if rep != "" {
		writes[c.report] = rep
	}
	return step{ws: ws, stdout: streamSuccess, writes: writes, repoWrite: c.repo}
}

// detailOf is the parse error a report gets.
func (c repairCase) detailOf(t *testing.T, rep string) string {
	t.Helper()
	err := c.parse([]byte(rep))
	if err == nil {
		t.Fatalf("%s report parses; the test needs one that does not", c.phase)
	}
	return err.Error()
}

// flagValues maps each --flag of a claude argv to its value; the prompt,
// the turn cap and the session flags are left out, since a repair changes
// exactly those.
func flagValues(argv []string) map[string][]string {
	out := map[string][]string{}
	for i := 3; i+1 < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "--") || argv[i] == "--verbose" {
			continue
		}
		switch argv[i] {
		case "--max-turns", "--session-id", "--resume":
		default:
			out[argv[i]] = append(out[argv[i]], argv[i+1])
		}
		i++
	}
	return out
}

// priced is a repair run's tokens at the stage model's rates.
func priced(t *testing.T, u envelope.Usage) float64 {
	t.Helper()
	m, ok := model.ModelByID(model.Builtins(), "anthropic/claude-sonnet-5")
	if !ok {
		t.Fatal("the test model has no registry entry")
	}
	c := model.UsageCostUSD(m, u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.OutputTokens)
	if c == nil {
		t.Fatal("the test model has no rates")
	}
	return *c
}

// plus sums usages, cost aside.
func plus(us ...envelope.Usage) envelope.Usage {
	var s envelope.Usage
	for _, u := range us {
		s.InputTokens += u.InputTokens
		s.OutputTokens += u.OutputTokens
		s.CacheReadTokens += u.CacheReadTokens
		s.CacheCreationTokens += u.CacheCreationTokens
	}
	return s
}

// sameTokens compares usages, cost aside.
func sameTokens(a, b envelope.Usage) bool {
	a.CostUSD, b.CostUSD = 0, 0
	return a == b
}

// checkSeqs fails unless the transcript's sequence numbers run 1, 2, 3, …
// with no gap and no repeat: the status page keys turns on them.
func checkSeqs(t *testing.T, got []transcript.Turn) {
	t.Helper()
	for i, turn := range got {
		if turn.Seq != i+1 {
			t.Fatalf("turn %d has seq %d, want %d: one recorder must number every run of the stage", i, turn.Seq, i+1)
		}
	}
}

// TestRepairInvalidReport: a stage whose report patchy refuses asks the
// agent, in its own session, to repair it, and ends ok on the repaired
// report. The repair continues the session the first run left (--resume,
// never --session-id) under the same model, posture and directories, with
// a prompt of patchy's fixed text and the refusal reason alone; its spend is
// added to the stage's without counting the cumulative cost twice; the
// stage's two runs share one transcript; and a writable stage still
// packages its changeset through commit.sh.
func TestRepairInvalidReport(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			firstRun := c.first(ws, c.bad)
			firstRun.budgetLines = conversation
			fx := &fakeExec{steps: []step{firstRun, {
				ws: ws, stdout: streamResumed, writes: map[string]string{c.report: c.good}, budgetLines: conversation,
			}}}
			a := New(cfg, fx)
			a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
			if err := a.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			evs := events(t, out.String())
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			stage := eventStage(t, evs[0])
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (detail %q), want ok after the repair", stage.Outcome, stage.Detail)
			}
			if len(fx.specs) != 2 {
				t.Fatalf("commands = %d, want the first run and one repair", len(fx.specs))
			}
			c.checkRepairCommand(t, ws, fx.specs[0], fx.specs[1])
			if c.phase == PhasePlan || c.phase == PhaseInvestigate {
				checkReadOnlyRepairScope(t, ws, fx.specs[1])
			}
			checkRepairedSpend(t, stage)
			checkRepairTranscript(t, turns(t, out.String()))
			c.checkRepairedReport(t, evs[0])
		})
	}
}

// checkReadOnlyRepairScope pins what a read-only stage's repair may do:
// write its reports directory alone, as the first run (TestPlanRunsReadOnly,
// TestReadOnlyStagesWriteOnlyTheirReports), never the posture's bare Write,
// and with no shell, so fixing a report cannot touch the repository or the
// settings the resumed run reads.
func checkReadOnlyRepairScope(t *testing.T, ws string, repair runner.CommandSpec) {
	t.Helper()
	allow := argvFlag(t, repair.Argv, "--allowedTools")
	tools := strings.Fields(allow)
	if scoped := "Edit(/" + filepath.Join(ws, "reports") + "/**)"; !slices.Contains(tools, scoped) ||
		slices.Contains(tools, "Write") {
		t.Errorf("repair --allowedTools = %q, want %s and no bare Write", allow, scoped)
	}
	checkNoShell(t, repair.Argv)
}

// checkRepairCommand checks a repair continues the first run's session under
// its model, posture, directories and working directory, with the repair
// prompt for this report and reason exactly: nothing of the request, the
// plan or the report.
func (c repairCase) checkRepairCommand(t *testing.T, ws string, first, repair runner.CommandSpec) {
	t.Helper()
	if !slices.Contains(first.Argv, "--session-id") || slices.Contains(repair.Argv, "--session-id") {
		t.Errorf("session flags: first %q, repair %q; want --session-id on the first only", first.Argv, repair.Argv)
	}
	if i := slices.Index(repair.Argv, "--resume"); i < 0 || repair.Argv[i+1] != testSessionID {
		t.Errorf("repair argv %q does not resume the session the first run reported (%s)", repair.Argv, testSessionID)
	}
	if got, want := flagValues(repair.Argv), flagValues(first.Argv); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("repair flags = %v, want the first run's %v", got, want)
	}
	if repair.Dir != first.Dir {
		t.Errorf("repair runs in %s, want the first run's %s, where the CLI filed the session", repair.Dir, first.Dir)
	}
	commit := ""
	if c.writable {
		commit = filepath.Join(ws, "commit.sh")
	}
	want, err := templates.RenderRepairPrompt(templates.RepairPrompt{
		ReportPath: filepath.Join(ws, c.report), Error: c.detailOf(t, c.bad), Round: 1, Rounds: repairRounds,
		CommitScriptPath: commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repair.Argv[2] != want {
		t.Errorf("repair prompt =\n%s\nwant\n%s", repair.Argv[2], want)
	}
}

// checkRepairedSpend checks a stage repaired in one round records both
// runs: tokens, turns and time summed, and the cost the first run reported
// plus the repair's tokens priced, never the resumed run's cumulative cost.
func checkRepairedSpend(t *testing.T, stage envelope.Stage) {
	t.Helper()
	if !sameTokens(stage.Usage, plus(firstUsage, resumedUsage)) || stage.NumTurns != 9 ||
		stage.ElapsedSeconds != 6 {
		t.Errorf("accounting = %+v, want both runs' tokens, turns and time summed", stage)
	}
	if want := 0.0123 + priced(t, resumedUsage); math.Abs(stage.Usage.CostUSD-want) > 1e-9 {
		t.Errorf("cost = %v, want the first run's 0.0123 plus the repair priced (%v), never its cumulative 0.02",
			stage.Usage.CostUSD, want)
	}
	if stage.SessionID != testSessionID {
		t.Errorf("session = %q, want the one session", stage.SessionID)
	}
}

// checkRepairTranscript checks the stage's runs share one transcript, with
// the repair announced in it.
func checkRepairTranscript(t *testing.T, got []transcript.Turn) {
	t.Helper()
	checkSeqs(t, got)
	if !slices.ContainsFunc(got, func(turn transcript.Turn) bool {
		return turn.Kind == transcript.KindNotice && strings.Contains(turn.Text, "asking the agent to repair it")
	}) {
		t.Errorf("transcript has no repair notice: %+v", got)
	}
}

// checkRepairedReport checks the event carries the repaired report's exact
// bytes, and a writable stage its packaged changeset.
func (c repairCase) checkRepairedReport(t *testing.T, ev envelope.Event) {
	t.Helper()
	switch {
	case ev.Remediation != nil:
		rem := ev.Remediation
		if rem.ReportMarkdown != c.good || !rem.Success || rem.Changeset == nil {
			t.Errorf("remediation = %+v, want the repaired report and a packaged changeset", rem)
		}
	case ev.Plan != nil:
		if ev.Plan.ReportMarkdown != c.good {
			t.Errorf("plan report = %q, want the repaired file's exact bytes", ev.Plan.ReportMarkdown)
		}
	case ev.Investigation != nil:
		if ev.Investigation.ReportMarkdown != c.good {
			t.Errorf("investigation report = %q, want the repaired one", ev.Investigation.ReportMarkdown)
		}
	}
}

// TestRepairGivesUp: a report still refused after every round ends the
// stage report_invalid, with the last reason and how many rounds ran, and
// every run's spend recorded.
func TestRepairGivesUp(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			again := step{ws: ws, stdout: streamResumed}
			fx := &fakeExec{steps: []step{c.first(ws, c.bad), again, again}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))

			if stage.Outcome != envelope.OutcomeReportInvalid || len(fx.specs) != 1+repairRounds {
				t.Fatalf("outcome %q after %d commands, want report_invalid after %d", stage.Outcome, len(fx.specs),
					1+repairRounds)
			}
			if want := c.detailOf(t, c.bad) + " (not repaired in 2 rounds)"; stage.Detail != want {
				t.Errorf("detail = %q, want %q", stage.Detail, want)
			}
			if !sameTokens(stage.Usage, plus(firstUsage, resumedUsage, resumedUsage)) || stage.NumTurns != 11 {
				t.Errorf("accounting = %+v, want all three runs summed", stage)
			}
		})
	}
}

// TestRepairMissingReport: a run that wrote no report at all is asked to
// write it, in its own session, and the stage ends ok on it.
func TestRepairMissingReport(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			fx := &fakeExec{steps: []step{c.first(ws, ""), {
				ws: ws, stdout: streamResumed, writes: map[string]string{c.report: c.good},
			}}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
			if stage.Outcome != envelope.OutcomeOK || len(fx.specs) != 2 {
				t.Fatalf("outcome %q (detail %q) after %d commands, want ok after one repair",
					stage.Outcome, stage.Detail, len(fx.specs))
			}
			if !strings.Contains(fx.specs[1].Argv[2], "patchy found no report at `"+filepath.Join(ws, c.report)+"`") {
				t.Errorf("repair prompt does not say the report is missing:\n%s", fx.specs[1].Argv[2])
			}
		})
	}
}

// TestRepairRunEndings: a repair run that does not end OK — timed out,
// killed by the budget, crashed, or unable to find its session — still has
// its spend recorded, and the report is read again whatever the run's
// ending: a report it fixed before it died is accepted.
func TestRepairRunEndings(t *testing.T) {
	huge := `{"type":"assistant","message":{"usage":{"output_tokens":5000000}}}`
	for _, c := range repairCases() {
		tests := []struct {
			name    string
			repairs []step
			want    envelope.Outcome
			detail  string
			usage   envelope.Usage
		}{
			{
				name: "timed out after fixing the report",
				repairs: []step{{stdout: streamCutOff, writes: map[string]string{c.report: c.good},
					result: runner.Result{TimedOut: true, ExitCode: -1}}},
				want: envelope.OutcomeOK, usage: plus(firstUsage, cutOffUsage),
			},
			{
				name:    "killed by the budget",
				repairs: []step{{stdout: huge, budgetLines: []string{huge}}},
				want:    envelope.OutcomeReportInvalid,
				detail: " (not repaired in 1 round; the last repair run ended budget_exceeded: output token budget " +
					"exceeded (5000000 > ",
				usage: plus(firstUsage, envelope.Usage{OutputTokens: 5000000}),
			},
			{
				name: "crashed",
				repairs: []step{
					{stdout: streamCutOff, result: runner.Result{ExitCode: -1, ExitStatus: "signal: segmentation fault"}},
					{stdout: streamCutOff, result: runner.Result{ExitCode: -1, ExitStatus: "signal: segmentation fault"}},
				},
				want: envelope.OutcomeReportInvalid,
				detail: " (not repaired in 2 rounds; the last repair run ended runtime_error: unparseable CLI output " +
					"(signal: segmentation fault))",
				usage: plus(firstUsage, cutOffUsage, cutOffUsage),
			},
			{
				// The session store is HOME, a writable emptyDir; were it ever
				// lost, the stage ends on its own report's reason.
				name: "the session is gone",
				repairs: []step{
					{stdout: streamNoSession, result: runner.Result{ExitCode: 1}},
					{stdout: streamNoSession, result: runner.Result{ExitCode: 1}},
				},
				want: envelope.OutcomeReportInvalid,
				detail: " (not repaired in 2 rounds; the last repair run ended runtime_error: claude run error " +
					"(error_during_execution): No conversation found with session ID: " + testSessionID,
				usage: firstUsage,
			},
		}
		for _, tt := range tests {
			t.Run(string(c.phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := c.setup(t, &out)
				steps := []step{c.first(ws, c.bad)}
				for _, s := range tt.repairs {
					s.ws = ws
					steps = append(steps, s)
				}
				fx := &fakeExec{steps: steps}
				stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
				if stage.Outcome != tt.want {
					t.Fatalf("outcome = %q (detail %q), want %q", stage.Outcome, stage.Detail, tt.want)
				}
				if tt.detail != "" && !strings.HasPrefix(stage.Detail, c.detailOf(t, c.bad)+tt.detail) {
					t.Errorf("detail = %q, want the report's reason then %q", stage.Detail, tt.detail)
				}
				if !sameTokens(stage.Usage, tt.usage) {
					t.Errorf("usage = %+v, want %+v: every run's spend", stage.Usage, tt.usage)
				}
				if len(fx.steps) != 0 {
					t.Errorf("%d scripted runs never ran", len(fx.steps))
				}
			})
		}
	}
}

// TestRepairRegressions is the build report that threw away a whole build
// live (it fails on a runner without repair): notes written as one string.
// It is repaired in one round, and the build is packaged.
func TestRepairRegressions(t *testing.T) {
	build := repairCases()[3]
	for name, bad := range map[string]string{"scalar notes": scalarNotesBuild} {
		t.Run(name, func(t *testing.T) {
			if _, err := report.ParseBuild([]byte(bad)); err == nil {
				t.Fatal("the report parses; it is not the live failure")
			}
			var out bytes.Buffer
			cfg, ws := build.setup(t, &out)
			fx := &fakeExec{steps: []step{build.first(ws, bad), {
				ws: ws, stdout: streamResumed, writes: map[string]string{build.report: goodBuild},
			}}}
			rem := onlyEvent(t, cfg, fx, &out).Remediation
			if rem == nil || rem.Outcome != envelope.OutcomeOK || !rem.Success || rem.Changeset == nil || len(fx.specs) != 2 {
				t.Fatalf("build = %+v after %d commands, want ok and packaged after one repair", rem, len(fx.specs))
			}
		})
	}
}

// TestLongNoteNeedsNoRepair: overdub-10's build, whose one note ran to 574
// characters, failed as report_invalid. A note over the bound is now cut, so
// the build is packaged on its first run, with no repair round.
func TestLongNoteNeedsNoRepair(t *testing.T) {
	build := repairCases()[3]
	var out bytes.Buffer
	cfg, ws := build.setup(t, &out)
	fx := &fakeExec{steps: []step{build.first(ws, longNoteBuild)}}
	rem := onlyEvent(t, cfg, fx, &out).Remediation
	if rem == nil || rem.Outcome != envelope.OutcomeOK || !rem.Success || rem.Changeset == nil || len(fx.specs) != 1 {
		t.Fatalf("build = %+v after %d commands, want ok and packaged with no repair", rem, len(fx.specs))
	}
}

// TestRepairRefusesTreeChanges: on a stage that writes the working tree, a
// repair that changes anything in it — nothing it changes was built or
// tested — is refused whole, even when the report it wrote is now valid:
// the stage ends on the report's original reason, naming what the repair
// changed, with no changeset and no further round. commit.sh may change.
func TestRepairRefusesTreeChanges(t *testing.T) {
	for _, c := range repairCases() {
		if !c.writable {
			continue
		}
		for _, tt := range []struct {
			name    string
			repair  step
			refused string
		}{
			{"an edited file", step{repoWrite: map[string]string{"app.js": "sneaky();\n"}},
				"it changed the working tree: app.js"},
			{"a new file", step{repoWrite: map[string]string{"extra.txt": "x\n"}},
				"it changed the working tree: extra.txt"},
			{"commit.sh rewritten", step{writes: map[string]string{"commit.sh": c.outputs["commit.sh"] + "\n"}}, ""},
		} {
			t.Run(string(c.phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := c.setup(t, &out)
				repair := tt.repair
				repair.ws, repair.stdout = ws, streamResumed
				if repair.writes == nil {
					repair.writes = map[string]string{}
				}
				repair.writes[c.report] = c.good
				fx := &fakeExec{steps: []step{c.first(ws, c.bad), repair}}
				rem := onlyEvent(t, cfg, fx, &out).Remediation
				if tt.refused == "" {
					if rem.Outcome != envelope.OutcomeOK || !rem.Success {
						t.Fatalf("remediation = %+v, want a repair that rewrote commit.sh accepted", rem)
					}
					return
				}
				want := c.detailOf(t, c.bad) + " (repair refused in round 1: " + tt.refused +
					", and a repair may change nothing but the report and commit.sh)"
				if rem.Outcome != envelope.OutcomeReportInvalid || rem.Detail != want {
					t.Errorf("outcome %q, detail %q; want report_invalid, %q", rem.Outcome, rem.Detail, want)
				}
				if rem.Success || rem.Changeset != nil || len(fx.specs) != 2 {
					t.Errorf("remediation = %+v after %d commands, want nothing packaged and no second round",
						rem, len(fx.specs))
				}
			})
		}
	}
}

// Minimal success streams of the harnesses that cannot resume.
const (
	codexStream = `{"type":"thread.started","thread_id":"t-1"}` + "\n" + `{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"Report written."}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}`
	copilotStream = `{"type":"session.start","sessionId":"s-1","timestamp":"2026-10-05T09:00:00.000Z","data":{}}` +
		"\n" + `{"type":"assistant.message","timestamp":"2026-10-05T09:00:01.000Z",` +
		`"data":{"messageId":"m1","content":"Report written."}}` + "\n" +
		`{"type":"result","timestamp":"2026-10-05T09:00:02.000Z","sessionId":"s-1","exitCode":0,` +
		`"usage":{"premiumRequests":1}}`
)

// TestRepairNeedsAResumer pins the fallback: on codex and copilot, which
// cannot continue a session, a refused report ends the stage exactly as it
// did before repair existed — one run, report_invalid, the parser's reason
// and nothing after it.
func TestRepairNeedsAResumer(t *testing.T) {
	for _, h := range []struct{ id, stream string }{{"codex", codexStream}, {"copilot", copilotStream}} {
		for _, c := range repairCases()[:2] { // the Finding stages; intents refuse these harnesses
			t.Run(h.id+"/"+string(c.phase), func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := c.setup(t, &out)
				cfg.InvestigateHarness, cfg.RemediateHarness, cfg.BrokerTokenFile = h.id, h.id, ""
				first := c.first(ws, c.bad)
				first.stdout = h.stream
				fx := &fakeExec{steps: []step{first}}
				stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
				if stage.Outcome != envelope.OutcomeReportInvalid || stage.Detail != c.detailOf(t, c.bad) ||
					len(fx.specs) != 1 {
					t.Errorf("outcome %q, detail %q after %d commands; want today's report_invalid, %q, after 1",
						stage.Outcome, stage.Detail, len(fx.specs), c.detailOf(t, c.bad))
				}
			})
		}
	}
}

// cancelAfter runs its executor, then cancels the stage's context, as a
// SIGTERM arriving as the first run ends would.
type cancelAfter struct {
	*fakeExec
	cancel context.CancelFunc
}

func (c cancelAfter) Run(ctx context.Context, spec runner.CommandSpec, timeout time.Duration,
	onLine func([]byte) (bool, string)) (runner.Result, error) {
	defer c.cancel()
	return c.fakeExec.Run(ctx, spec, timeout, onLine)
}

// TestRepairLimits: a repair spends only what the stage has left. It gets
// the stage's remaining turns (at most repairMaxTurns), wall clock (at most
// repairTimeout) and output tokens, and none is attempted when the stage
// has too little of any of them left, or was cancelled.
func TestRepairLimits(t *testing.T) {
	investigate := repairCases()[0]
	run := func(t *testing.T, tune func(*Config), exec func(*fakeExec, context.CancelFunc) Executor,
		repairs ...step) (*fakeExec, envelope.Stage) {
		t.Helper()
		var out bytes.Buffer
		cfg, ws := investigate.setup(t, &out)
		tune(&cfg)
		steps := make([]step, 0, 1+len(repairs))
		steps = append(steps, investigate.first(ws, investigate.bad))
		for _, s := range repairs {
			s.ws = ws
			steps = append(steps, s)
		}
		fx := &fakeExec{steps: steps}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var e Executor = fx
		if exec != nil {
			e = exec(fx, cancel)
		}
		if err := New(cfg, e).Run(ctx); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		evs := events(t, out.String())
		if len(evs) != 1 {
			t.Fatalf("events = %d, want 1", len(evs))
		}
		return fx, eventStage(t, evs[0])
	}
	fixed := step{stdout: streamResumed, writes: map[string]string{investigate.report: investigate.good}}
	bad := investigate.detailOf(t, investigate.bad)

	for _, tt := range []struct {
		name string
		tune func(*Config)
		why  string
	}{
		{"no wall clock left", func(c *Config) { c.InvestigateTimeout = time.Minute },
			"less than 2m of the stage's 1m wall clock left"},
		{"no turns left", func(c *Config) { c.InvestigateMaxTurns = 7 }, "no turns left of the stage's 7"},
		{"no output tokens left", func(c *Config) { c.InvestigateTokenBudget = 30 },
			"no output tokens left of the stage's 30"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx, stage := run(t, tt.tune, nil)
			if want := bad + " (not repaired: " + tt.why + ")"; stage.Detail != want || len(fx.specs) != 1 {
				t.Errorf("detail %q after %d commands, want %q after 1", stage.Detail, len(fx.specs), want)
			}
		})
	}

	t.Run("cancelled", func(t *testing.T) {
		fx, stage := run(t, func(*Config) {},
			func(fx *fakeExec, cancel context.CancelFunc) Executor { return cancelAfter{fx, cancel} }, fixed)
		if want := bad + " (not repaired: the stage was cancelled)"; stage.Detail != want || len(fx.specs) != 1 {
			t.Errorf("detail %q after %d commands, want %q after 1", stage.Detail, len(fx.specs), want)
		}
	})

	for _, tt := range []struct {
		turns int
		want  string
	}{{25, "6"}, {10, "3"}, {0, "6"}} {
		t.Run("turns "+tt.want, func(t *testing.T) {
			fx, stage := run(t, func(c *Config) { c.InvestigateMaxTurns = tt.turns }, nil, fixed)
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (%s)", stage.Outcome, stage.Detail)
			}
			if i := slices.Index(fx.specs[1].Argv, "--max-turns"); i < 0 || fx.specs[1].Argv[i+1] != tt.want {
				t.Errorf("stage cap %d after 7 turns: repair argv %q, want --max-turns %s", tt.turns, fx.specs[1].Argv,
					tt.want)
			}
		})
	}

	for _, tt := range []struct {
		wall time.Duration
		low  time.Duration
		high time.Duration
	}{{30 * time.Minute, repairTimeout, repairTimeout}, {5 * time.Minute, 4 * time.Minute, 5 * time.Minute}} {
		t.Run("wall clock "+tt.wall.String(), func(t *testing.T) {
			fx, _ := run(t, func(c *Config) { c.InvestigateTimeout = tt.wall }, nil, fixed)
			if got := fx.timeouts[1]; got < tt.low || got > tt.high {
				t.Errorf("repair timeout = %s, want within [%s, %s]", got, tt.low, tt.high)
			}
		})
	}

	t.Run("output tokens", func(t *testing.T) {
		// 100 for the stage, 30 spent by the first run: the repair's kill
		// switch trips past 70, and with the stage's tokens spent no second
		// round runs.
		trip := `{"type":"assistant","message":{"usage":{"output_tokens":71}}}`
		fx, stage := run(t, func(c *Config) { c.InvestigateTokenBudget = 100 }, nil,
			step{stdout: trip, budgetLines: []string{trip}})
		want := bad + " (not repaired in 1 round; the last repair run ended budget_exceeded: output token budget " +
			"exceeded (71 > 70); no output tokens left of the stage's 100)"
		if stage.Detail != want || len(fx.specs) != 2 {
			t.Errorf("detail %q after %d commands, want %q after 2", stage.Detail, len(fx.specs), want)
		}
	})
}

// TestRepairReadsTheBrokerTokenAfresh: the repair run reads the projected
// caller token again, as every run does (the kubelet rotates it), presents
// the new value, and the stage's one transcript scrubs it.
func TestRepairReadsTheBrokerTokenAfresh(t *testing.T) {
	const first, rotated = "caller-token-first-0001", "caller-token-rotated-0002"
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			firstRun := c.first(ws, c.bad)
			firstRun.writes["broker-token"] = rotated + "\n"
			echo := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` +
				`ANTHROPIC_CUSTOM_HEADERS=x-patchy-broker-token: ` + rotated + " and " + first + `"}]}}`
			fx := &fakeExec{steps: []step{firstRun, {
				ws: ws, stdout: streamResumed, writes: map[string]string{c.report: c.good}, budgetLines: []string{echo},
			}}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (%s)", stage.Outcome, stage.Detail)
			}
			if !slices.ContainsFunc(fx.specs[0].Env, func(kv string) bool { return strings.HasSuffix(kv, ": "+first) }) ||
				!slices.ContainsFunc(fx.specs[1].Env, func(kv string) bool { return strings.HasSuffix(kv, ": "+rotated) }) {
				t.Errorf("env: first %q, repair %q; want each run to present the token it read", fx.specs[0].Env,
					fx.specs[1].Env)
			}
			if strings.Contains(out.String(), rotated) || strings.Contains(out.String(), first) {
				t.Error("stdout carries a caller token; the shared recorder must scrub the rotated one too")
			}
			if !strings.Contains(out.String(), transcript.Redacted) {
				t.Error("the echoed tokens were not redacted in the transcript")
			}
		})
	}
}

// TestRepairInvocation pins the exact command a brokered build's repair
// runs: the first run's flags, --resume, the repair prompt and a fresh
// broker header. The Finding goldens stay what they were.
func TestRepairInvocation(t *testing.T) {
	build := repairCases()[3]
	var out bytes.Buffer
	cfg, ws := build.setup(t, &out)
	fx := &fakeExec{steps: []step{build.first(ws, build.bad), {
		ws: ws, stdout: streamResumed, writes: map[string]string{build.report: build.good},
	}}}
	a := New(cfg, fx)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 2 {
		t.Fatalf("commands = %d, want the first run and one repair", len(fx.specs))
	}
	goldenFile(t, "invocation_build_repair.txt", renderInvocation(fx.specs[1], ws))
}

// TestRepairThroughTheFakeHarness drives a refused plan through the real
// runner and the fake harness, which replays its fixture for every run: the
// first run and both repairs, three replays, with every replay's spend
// recorded — the fixture's tokens three times, its cost once plus the two
// repairs priced.
func TestRepairThroughTheFakeHarness(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamSuccess+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(harness.FakeFixtureEnv, fixture)
	var out bytes.Buffer
	cfg, ws := intentConfig(t, PhasePlan, &out)
	cfg.InvestigateTimeout = 10 * time.Minute
	layDown(t, ws, map[string]string{"reports/plan.md": badPlan})
	counted := &countingExec{Executor: &runner.Exec{}}
	if err := New(cfg, counted).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	p := events(t, out.String())[0].Plan
	if p == nil || p.Outcome != envelope.OutcomeReportInvalid || counted.runs != 3 {
		t.Fatalf("plan = %+v after %d runs, want report_invalid after 3", p, counted.runs)
	}
	if !sameTokens(p.Usage, plus(firstUsage, firstUsage, firstUsage)) || p.NumTurns != 21 {
		t.Errorf("accounting = %+v, want the fixture's three times", p.Stage)
	}
	if want := 0.0123 + 2*priced(t, firstUsage); math.Abs(p.Usage.CostUSD-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", p.Usage.CostUSD, want)
	}
	checkSeqs(t, turns(t, out.String()))
}

// streamEditSecond is streamSuccess with a read on its first API message and
// a file write on its second, each message spread over two events.
const streamEditSecond = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Looking."}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}` +
	"\n" +
	`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"Writing."}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"Write","input":{}}]}}` +
	"\n" + streamSuccess

// TestFirstEditTurnThroughTheFakeHarness: a stage records the turn its main
// run first edited a file on, read off the real stream, and the two repair
// rounds replaying the same fixture never move it, while their turns are
// added to the total.
func TestFirstEditTurnThroughTheFakeHarness(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamEditSecond+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(harness.FakeFixtureEnv, fixture)
	var out bytes.Buffer
	cfg, ws := intentConfig(t, PhasePlan, &out)
	cfg.InvestigateTimeout = 10 * time.Minute
	layDown(t, ws, map[string]string{"reports/plan.md": badPlan})
	counted := &countingExec{Executor: &runner.Exec{}}
	if err := New(cfg, counted).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	p := events(t, out.String())[0].Plan
	if p == nil || counted.runs != 3 {
		t.Fatalf("plan = %+v after %d runs, want a plan event after 3", p, counted.runs)
	}
	if p.FirstEditTurn != 2 || p.NumTurns != 21 {
		t.Errorf("first edit turn / turns = %d/%d, want 2 (the main run's) / 21 (all three runs')",
			p.FirstEditTurn, p.NumTurns)
	}
}

// countingExec counts the commands it runs.
type countingExec struct {
	Executor
	runs int
}

func (c *countingExec) Run(ctx context.Context, spec runner.CommandSpec, timeout time.Duration,
	onLine func([]byte) (bool, string)) (runner.Result, error) {
	c.runs++
	return c.Executor.Run(ctx, spec, timeout, onLine)
}

// TestAddRepair pins the accounting rule on its own: tokens, turns and
// time sum; the cost the first run reported gains the repair's tokens
// priced, and stays unreported when the first run reported none.
func TestAddRepair(t *testing.T) {
	st := envelope.Stage{Model: "anthropic/claude-sonnet-5", NumTurns: 7, ElapsedSeconds: 3, Usage: firstUsage}
	st.Usage.CostUSD = 0.0123
	addRepair(&st, envelope.Stage{NumTurns: 2, FirstEditTurn: 1, ElapsedSeconds: 1.5, Usage: envelope.Usage{
		InputTokens: 10, CacheCreationTokens: 5, CacheReadTokens: 1000, OutputTokens: 8, CostUSD: 0.02,
	}})
	if !sameTokens(st.Usage, plus(firstUsage, resumedUsage)) || st.NumTurns != 9 || st.ElapsedSeconds != 4.5 {
		t.Errorf("stage = %+v, want tokens, turns and time summed", st)
	}
	if st.FirstEditTurn != 0 {
		t.Errorf("first edit turn = %d, want the first run's 0, never the repair's", st.FirstEditTurn)
	}
	edited := envelope.Stage{NumTurns: 7, FirstEditTurn: 4}
	addRepair(&edited, envelope.Stage{NumTurns: 2, FirstEditTurn: 1})
	if edited.FirstEditTurn != 4 || edited.NumTurns != 9 {
		t.Errorf("stage = %+v, want the first run's first edit turn 4 and 9 turns", edited)
	}
	if want := 0.0123 + priced(t, resumedUsage); math.Abs(st.Usage.CostUSD-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", st.Usage.CostUSD, want)
	}

	unreported := envelope.Stage{Model: "anthropic/claude-sonnet-5", Usage: firstUsage}
	addRepair(&unreported, envelope.Stage{Usage: resumedUsage})
	if unreported.Usage.CostUSD != 0 {
		t.Errorf("cost = %v, want none when the first run reported none", unreported.Usage.CostUSD)
	}
}

// TestTreeFingerprint: the guard sees a commit, a staged change, an edited
// or new file, and nothing else — not an ignored file — and leaves the
// clone's own index exactly as it was.
func TestTreeFingerprint(t *testing.T) {
	ws := newWorkspace(t)
	repo := filepath.Join(ws, "repo")
	ctx := context.Background()
	mustGit := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "build/\n")
	mustGit("add", ".gitignore")
	mustGit("commit", "-qm", "ignore")
	fp := func() fingerprint {
		t.Helper()
		f, err := treeFingerprint(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	base := fp()
	if after, _ := os.ReadFile(index); !bytes.Equal(after, before) {
		t.Error("fingerprinting changed the clone's index")
	}
	if again := fp(); again != base {
		t.Errorf("fingerprint moved with nothing changed: %+v then %+v", base, again)
	}
	if err := os.MkdirAll(filepath.Join(repo, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("build/out.bin", "ignored")
	if got := fp(); got != base {
		t.Errorf("an ignored file moved the fingerprint: %+v", got)
	}

	write("app.js", "edited();\n")
	edited := fp()
	if edited.worktree == base.worktree || edited.head != base.head || edited.staged != base.staged {
		t.Errorf("an edit: %+v from %+v, want only the working tree moved", edited, base)
	}
	write("one.txt", "1")
	write("two.txt", "2")
	write("three.txt", "3")
	if got, want := changed(ctx, repo, base, fp()),
		"it changed the working tree: app.js, one.txt, three.txt and 1 more, and a repair may change nothing "+
			"but the report and commit.sh"; got != want {
		t.Errorf("changed = %q, want %q", got, want)
	}
	mustGit("add", "app.js")
	staged := fp()
	if staged.staged == base.staged {
		t.Errorf("staging did not move the staged tree: %+v", staged)
	}
	mustGit("commit", "-qm", "sneak")
	if got := fp(); got.head == base.head {
		t.Errorf("a commit did not move HEAD: %+v", got)
	}
	if got := changed(ctx, repo, base, fp()); !strings.HasPrefix(got,
		"it moved HEAD (a commit); it staged changes; it changed the working tree: ") {
		t.Errorf("changed after a commit = %q", got)
	}

	if _, err := treeFingerprint(ctx, t.TempDir()); err == nil {
		t.Error("fingerprinting a directory with no clone succeeded")
	}
}
