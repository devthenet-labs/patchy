// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// DefaultIdleTimeout is how long a run may go without progress (no model
// turn, no tool result) before the watchdog ends it. It sits at twice the
// longest silence a working claude run has: the CLI's Bash tool returns a
// foreground command within its own timeout, two minutes by default and ten at
// most (BASH_MAX_TIMEOUT_MS), and a single model call streams each content
// block as it completes, well inside that. So a long test suite still fits,
// with room to spare, and only a run that is waiting on something that will
// not return (a command the tool failed to kill, a wedged CLI) reaches it. It
// is at or above the investigate stage's 15m and the plan stage's 20m wall
// clocks, so it can only ever end a remediation, build or revise run early.
const DefaultIdleTimeout = 20 * time.Minute

// errIdle is the cause the watchdog cancels a run's context with.
var errIdle = errors.New("no progress within the idle limit")

// idleCommandBytes bounds the command a watchdog detail quotes: the detail
// lands on CR status, in the retry's prompt and in human-facing comments.
const idleCommandBytes = 300

// idleWatch is the no-progress watchdog over one run. The runner reads the
// stream on its own goroutine and reports each line's turns (progress); a
// timer checks, whenever the limit could have passed, how long it has been
// since the last progress, and cancels the run's context with errIdle once it
// is the limit or more. Cancelling is how the runner is stopped from outside:
// it kills the CLI's whole process group, as its own timeout does.
//
// A nil *idleWatch is a disabled watchdog: every method is a no-op.
type idleWatch struct {
	limit  time.Duration
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer

	mu      sync.Mutex
	since   time.Time     // the last progress
	pending []pendingCall // tool calls not yet answered, oldest first
	fired   time.Time     // when the watchdog ended the run
	stopped bool
}

// pendingCall is a tool call the agent made and has had no result for.
type pendingCall struct {
	tool, input string
	at          time.Time
}

// newIdleWatch derives the context a run executes under, one the watchdog
// cancels once limit passes without progress. A limit of zero or less
// disables it: ctx comes back unchanged with a nil watch.
func newIdleWatch(ctx context.Context, limit time.Duration) (context.Context, *idleWatch) {
	if limit <= 0 {
		return ctx, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	w := &idleWatch{limit: limit, ctx: ctx, cancel: cancel, since: time.Now()}
	w.timer = time.AfterFunc(limit, w.check)
	return ctx, w
}

// check runs on the timer: it ends the run when the limit has passed since
// the last progress, and otherwise sleeps until it next could have.
func (w *idleWatch) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if quiet := time.Since(w.since); quiet < w.limit {
		w.timer.Reset(w.limit - quiet)
		return
	}
	w.fired = time.Now()
	w.cancel(errIdle)
}

// progress records the turns one stream line carried. Any turn is progress:
// a model turn, its tool calls, their results. A tool call is remembered
// until a result answers it, so a run that is ended can name what it was
// waiting on; results are matched oldest first, the order the CLI returns
// them in.
func (w *idleWatch) progress(turns []transcript.Turn) {
	if w == nil || len(turns) == 0 {
		return
	}
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since = now
	for _, t := range turns {
		switch t.Kind {
		case transcript.KindToolUse:
			w.pending = append(w.pending, pendingCall{tool: t.Tool, input: t.Text, at: now})
		case transcript.KindToolResult:
			if len(w.pending) > 0 {
				w.pending = w.pending[1:]
			}
		}
	}
}

// alive records progress for a stream the harness cannot project onto turns:
// any line at all counts, so a watchdog that cannot read the stream never
// ends a run that is producing one.
func (w *idleWatch) alive() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since = time.Now()
}

// end stops the watchdog and returns its verdict on the run: the detail of a
// run it ended, or "" for one it did not. runErr is the runner's: a run the
// watchdog ended is one the runner reports cancelled, so a timer that fires
// as a run finishes on its own does not claim it, and neither does a
// cancellation from above (a SIGTERM), whose cause is not errIdle. secrets
// are scrubbed from the command the detail quotes.
func (w *idleWatch) end(runErr error, secrets []string) string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
	defer w.cancel(nil)
	if runErr == nil || !errors.Is(context.Cause(w.ctx), errIdle) {
		return ""
	}
	if len(w.pending) == 0 {
		return fmt.Sprintf("no progress for %s: no model turn or tool result in that time", shortDuration(w.limit))
	}
	c := w.pending[0]
	tool := c.tool
	if tool == "" {
		tool = "a tool"
	}
	detail := fmt.Sprintf("no progress for %s while running %s (%s without returning)",
		shortDuration(w.limit), tool, shortDuration(w.fired.Sub(c.at)))
	if input := oneLine(transcript.Scrub(c.input, secrets), idleCommandBytes); input != "" {
		detail += ": " + input
	}
	return detail
}

// oneLine collapses s's whitespace runs, newlines included, to single spaces
// and cuts it to at most limit bytes on a rune boundary, marking a cut with
// an ellipsis.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// shortDuration renders d to the second without trailing zero units: 20m, not
// 20m0s; 1h, not 1h0m0s.
func shortDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
