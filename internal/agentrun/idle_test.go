// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// The stream lines the watchdog tests feed: a Bash call that never returns
// (overdub-10's last turn), a model turn, and a line that carries no turn.
const (
	lineHungCall = `{"type":"assistant","message":{"id":"msg_9","content":[{"type":"tool_use","id":"t9",` +
		`"name":"Bash","input":{"command":"npm run test:ci 2>&1 | tail -60"}}]}}`
	lineModelTurn = `{"type":"assistant","message":{"content":[{"type":"text","text":"Still working."}]}}`
	lineNoTurn    = `{"type":"system","subtype":"task_updated"}`
)

// streamExec plays a run the way the runner does: it hands each line to the
// observer as it streams, on its own goroutine, and ends on the run's context
// (returning what streamed with the context's error), on its own timeout
// (TimedOut), or, with finishAfter, by finishing on its own: writing its
// files and the result.
type streamExec struct {
	ws          string
	lines       []string // streamed first
	every       string   // streamed each tick until the run ends, when set
	tick        time.Duration
	finishAfter time.Duration
	writes      map[string]string
	result      string // streamed when the run finishes on its own
	started     func() // called once the first lines have streamed, when set
}

func (e *streamExec) Run(ctx context.Context, _ runner.CommandSpec, timeout time.Duration,
	onLine func([]byte) (bool, string)) (runner.Result, error) {
	start := time.Now()
	var out strings.Builder
	feed := func(line string) {
		out.WriteString(line + "\n")
		if onLine != nil {
			onLine([]byte(line))
		}
	}
	ended := func() runner.Result {
		return runner.Result{Stdout: []byte(out.String()), ExitCode: -1, Elapsed: time.Since(start)}
	}
	for _, line := range e.lines {
		feed(line)
	}
	if e.started != nil {
		e.started()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var finish <-chan time.Time
	if e.finishAfter > 0 {
		finish = time.After(e.finishAfter)
	}
	tick := time.NewTicker(e.tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ended(), ctx.Err()
		case <-deadline.C:
			res := ended()
			res.TimedOut = true
			return res, nil
		case <-finish:
			for path, content := range e.writes {
				full := filepath.Join(e.ws, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					return runner.Result{}, err
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					return runner.Result{}, err
				}
			}
			feed(e.result)
			return runner.Result{Stdout: []byte(out.String()), Elapsed: time.Since(start)}, nil
		case <-tick.C:
			if e.every != "" {
				feed(e.every)
			}
		}
	}
}

// Watchdog test timing: the idle limit, the stage wall clock a run would
// otherwise reach, and how often streamExec ticks.
const (
	testIdle    = 150 * time.Millisecond
	testWall    = 3 * time.Second
	testTick    = 20 * time.Millisecond
	wantCommand = ": npm run test:ci 2>&1 | tail -60"
)

// watchedStages configures each stage with the test idle limit and wall
// clock, over a workspace the stage accepts.
var watchedStages = []struct {
	phase  Phase
	config func(t *testing.T, out *bytes.Buffer) (Config, string)
}{
	{PhaseInvestigate, func(t *testing.T, out *bytes.Buffer) (Config, string) {
		ws := newWorkspace(t)
		return newConfig(t, ws, out), ws
	}},
	{PhaseRemediate, func(t *testing.T, out *bytes.Buffer) (Config, string) {
		cfg, ws := remediateConfig(t, goodInvestigation, out)
		cfg.Phase = PhaseRemediate
		return cfg, ws
	}},
	{PhasePlan, func(t *testing.T, out *bytes.Buffer) (Config, string) {
		return intentConfig(t, PhasePlan, out)
	}},
	{PhaseBuild, func(t *testing.T, out *bytes.Buffer) (Config, string) {
		return intentConfig(t, PhaseBuild, out)
	}},
}

// watched sets every stage's idle limit and wall clock for the tests.
func watched(cfg Config, idle time.Duration) Config {
	cfg.InvestigateIdleTimeout, cfg.RemediateIdleTimeout = idle, idle
	cfg.InvestigateTimeout, cfg.RemediateTimeout = testWall, testWall
	return cfg
}

// runOne runs the agent over exec and returns its single event.
func runOne(t *testing.T, cfg Config, exec Executor, out *bytes.Buffer) envelope.Event {
	t.Helper()
	if err := New(cfg, exec).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1:\n%s", len(evs), out.String())
	}
	return evs[0]
}

// stageOf is the stage an event's payload carries.
func stageOf(t *testing.T, ev envelope.Event) envelope.Stage {
	t.Helper()
	switch {
	case ev.Investigation != nil:
		return ev.Investigation.Stage
	case ev.Remediation != nil:
		return ev.Remediation.Stage
	case ev.Plan != nil:
		return ev.Plan.Stage
	}
	t.Fatalf("event %+v carries no stage", ev)
	return envelope.Stage{}
}

// TestIdleWatchdogEndsAHungRun is the regression guard for overdub-10's
// second build: its agent ran `npm run test:ci 2>&1 | tail -60`, the command
// never returned, and the run sat silent for 50 minutes until the one-hour
// stage timeout, whose detail said only that time ran out. Every stage now
// ends a run that makes no progress for its idle limit, as a timeout (an
// attempt, like the wall clock's) whose detail names the command and how
// long it ran, and leaves that detail as the transcript's last turn.
func TestIdleWatchdogEndsAHungRun(t *testing.T) {
	for _, st := range watchedStages {
		t.Run(string(st.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := st.config(t, &out)
			cfg = watched(cfg, testIdle)
			fx := &streamExec{ws: ws, tick: testTick, lines: []string{
				`{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}`, lineHungCall,
			}}

			stage := stageOf(t, runOne(t, cfg, fx, &out))

			if stage.Outcome != envelope.OutcomeTimeout {
				t.Fatalf("outcome = %q (detail %q), want timeout", stage.Outcome, stage.Detail)
			}
			if !strings.HasPrefix(stage.Detail, "no progress for ") ||
				!strings.Contains(stage.Detail, "while running Bash (") || !strings.HasSuffix(stage.Detail, wantCommand) {
				t.Errorf("detail = %q, want no progress while running Bash, ending with the command", stage.Detail)
			}
			if wall := time.Duration(stage.ElapsedSeconds * float64(time.Second)); wall <= 0 || wall >= testWall {
				t.Errorf("elapsed = %s, want the watchdog to end the run before its %s wall clock", wall, testWall)
			}
			got := turns(t, out.String())
			if last := got[len(got)-1]; last.Kind != transcript.KindNotice || last.Text != stage.Detail {
				t.Errorf("last transcript turn = %+v, want the watchdog's notice %q", last, stage.Detail)
			}
		})
	}
}

// TestIdleWatchdogKeepsAWorkingRun: a run whose turns keep coming is never
// ended, however long it takes past the idle limit.
func TestIdleWatchdogKeepsAWorkingRun(t *testing.T) {
	var out bytes.Buffer
	ws := newWorkspace(t)
	cfg := watched(newConfig(t, ws, &out), testIdle)
	fx := &streamExec{
		ws: ws, tick: testTick, lines: []string{lineHungCall}, every: lineModelTurn,
		finishAfter: 5 * testIdle,
		writes:      map[string]string{"reports/investigation.md": goodInvestigation},
		result:      strings.Split(streamSuccess, "\n")[2],
	}

	inv := runOne(t, cfg, fx, &out).Investigation

	if inv.Outcome != envelope.OutcomeOK {
		t.Errorf("outcome = %q (detail %q), want ok: the run kept making progress", inv.Outcome, inv.Detail)
	}
}

// TestIdleWatchdogIgnoresLinesWithoutTurns: what counts as progress is a
// model turn or a tool result. A CLI's housekeeping lines (a background task
// update, a hook) are not, so a run hung on a command is ended even while the
// CLI keeps printing them.
func TestIdleWatchdogIgnoresLinesWithoutTurns(t *testing.T) {
	var out bytes.Buffer
	ws := newWorkspace(t)
	cfg := watched(newConfig(t, ws, &out), testIdle)
	fx := &streamExec{ws: ws, tick: testTick, lines: []string{lineHungCall}, every: lineNoTurn}

	inv := runOne(t, cfg, fx, &out).Investigation

	if inv.Outcome != envelope.OutcomeTimeout || !strings.HasSuffix(inv.Detail, wantCommand) {
		t.Errorf("outcome = %q (detail %q), want the watchdog's timeout", inv.Outcome, inv.Detail)
	}
}

// TestIdleWatchdogDisabled: an idle limit of zero leaves only the stage's
// wall clock, and its own detail.
func TestIdleWatchdogDisabled(t *testing.T) {
	var out bytes.Buffer
	ws := newWorkspace(t)
	cfg := watched(newConfig(t, ws, &out), 0)
	cfg.InvestigateTimeout = 5 * testIdle
	fx := &streamExec{ws: ws, tick: testTick, lines: []string{lineHungCall}}

	inv := runOne(t, cfg, fx, &out).Investigation

	if inv.Outcome != envelope.OutcomeTimeout || !strings.HasPrefix(inv.Detail, "stage timed out after") {
		t.Errorf("outcome = %q (detail %q), want the wall clock's timeout", inv.Outcome, inv.Detail)
	}
}

// TestIdleWatchdogLeavesACancelledRun: a run cancelled from above (the pod's
// SIGTERM) is the runner's runtime error, not a watchdog verdict, even with
// the watchdog armed.
func TestIdleWatchdogLeavesACancelledRun(t *testing.T) {
	var out bytes.Buffer
	ws := newWorkspace(t)
	cfg := watched(newConfig(t, ws, &out), testWall)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := &streamExec{ws: ws, tick: testTick, lines: []string{lineHungCall}, started: cancel}

	if err := New(cfg, fx).Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if inv := evs[0].Investigation; inv.Outcome != envelope.OutcomeRuntimeError ||
		strings.Contains(inv.Detail, "no progress") {
		t.Errorf("outcome = %q (detail %q), want a runtime error the watchdog does not claim", inv.Outcome, inv.Detail)
	}
}

// TestIdleDetail pins the watchdog's wording: what it was waiting on, for how
// long, on one bounded line with credential values scrubbed.
func TestIdleDetail(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	start := time.Date(2026, 10, 4, 7, 32, 5, 0, time.UTC)
	tests := []struct {
		name    string
		pending []pendingCall
		want    string
	}{
		{
			name: "waiting on no tool",
			want: "no progress for 20m: no model turn or tool result in that time",
		},
		{
			name:    "waiting on a command",
			pending: []pendingCall{{tool: "Bash", input: "npm run test:ci 2>&1 | tail -60", at: start}},
			want:    "no progress for 20m while running Bash (20m without returning): npm run test:ci 2>&1 | tail -60",
		},
		{
			name: "the oldest unanswered call is named, for how long it ran",
			pending: []pendingCall{
				{tool: "Bash", input: "npm ci", at: start.Add(-5 * time.Minute)},
				{tool: "Read", input: "package.json", at: start},
			},
			want: "no progress for 20m while running Bash (25m without returning): npm ci",
		},
		{
			name:    "a multi-line command is one line",
			pending: []pendingCall{{tool: "Bash", input: "cd app &&\n  npm test\t-- --ci", at: start}},
			want:    "no progress for 20m while running Bash (20m without returning): cd app && npm test -- --ci",
		},
		{
			name:    "a credential is scrubbed",
			pending: []pendingCall{{tool: "Bash", input: "curl -H 'x-api-key: " + secret + "' x", at: start}},
			want: "no progress for 20m while running Bash (20m without returning): curl -H 'x-api-key: " +
				transcript.Redacted + "' x",
		},
		{
			name:    "a call with no tool name or input",
			pending: []pendingCall{{at: start}},
			want:    "no progress for 20m while running a tool (20m without returning)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, w := newIdleWatch(context.Background(), 20*time.Minute)
			w.mu.Lock()
			w.pending, w.fired = tt.pending, start.Add(20*time.Minute)
			w.cancel(errIdle)
			w.mu.Unlock()

			if got := w.end(context.Canceled, []string{secret}); got != tt.want {
				t.Errorf("detail = %q\n want %q", got, tt.want)
			}
		})
	}

	t.Run("a long command is cut", func(t *testing.T) {
		_, w := newIdleWatch(context.Background(), time.Minute)
		w.pending = []pendingCall{{tool: "Bash", input: strings.Repeat("é", idleCommandBytes), at: start}}
		w.fired = start
		w.cancel(errIdle)
		got := w.end(context.Canceled, nil)
		_, command, _ := strings.Cut(got, "): ")
		if !strings.HasSuffix(command, "…") || len(command) > idleCommandBytes+len("…") || !utf8.ValidString(got) {
			t.Errorf("command = %q (%d bytes), want at most %d bytes on a rune boundary, marked cut",
				command, len(command), idleCommandBytes)
		}
	})

	t.Run("a run that ended on its own is not claimed", func(t *testing.T) {
		_, w := newIdleWatch(context.Background(), time.Minute)
		w.cancel(errIdle) // the timer fired as the run finished
		if got := w.end(nil, nil); got != "" {
			t.Errorf("detail = %q for a run the runner reports finished, want none", got)
		}
	})
}

// TestOneLineProperty: oneLine's result is one line of single-spaced words,
// valid UTF-8 when its input is, within its bound (plus the cut marker), and
// the start of the input's words.
func TestOneLineProperty(t *testing.T) {
	alphabet := []string{"a", "Z", " ", "  ", "\n", "\t", "\r\n", "é", "漢", "🙂", "|", "&"}
	property := func(seed int64, n uint8, limit uint8) bool {
		r := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for range int(n) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		in, bound := b.String(), int(limit)+1
		got := oneLine(in, bound)
		words := strings.Join(strings.Fields(in), " ")
		body := strings.TrimSuffix(got, "…")
		return !strings.ContainsAny(got, "\n\r\t") && !strings.Contains(got, "  ") && utf8.ValidString(got) &&
			len(body) <= bound && strings.HasPrefix(words, body) && (body == words) == (body == got)
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261004))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Minute:                      "20m",
		time.Hour:                             "1h",
		time.Hour + 30*time.Second:            "1h0m30s",
		90 * time.Minute:                      "1h30m",
		45 * time.Second:                      "45s",
		20*time.Minute + 400*time.Millisecond: "20m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestFromEnvIdleTimeouts(t *testing.T) {
	env := map[string]string{"PATCHY_REPO": "acme/shop", "PATCHY_FINDING": "f-1"}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InvestigateIdleTimeout != DefaultIdleTimeout || cfg.RemediateIdleTimeout != DefaultIdleTimeout {
		t.Errorf("idle timeouts = %s/%s, want the %s default for both stages",
			cfg.InvestigateIdleTimeout, cfg.RemediateIdleTimeout, DefaultIdleTimeout)
	}

	env["PATCHY_INVESTIGATE_IDLE_TIMEOUT"], env["PATCHY_REMEDIATE_IDLE_TIMEOUT"] = "0s", "35m"
	if cfg, err = FromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if cfg.InvestigateIdleTimeout != 0 || cfg.RemediateIdleTimeout != 35*time.Minute {
		t.Errorf("idle timeouts = %s/%s, want 0s (disabled) and 35m", cfg.InvestigateIdleTimeout,
			cfg.RemediateIdleTimeout)
	}

	env["PATCHY_REMEDIATE_IDLE_TIMEOUT"] = "-1m"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), "PATCHY_REMEDIATE_IDLE_TIMEOUT") {
		t.Errorf("FromEnv error = %v, want one naming PATCHY_REMEDIATE_IDLE_TIMEOUT", err)
	}
}
