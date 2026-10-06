// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Tailer follows a running agent Job's transcript and command output;
// *jobs.Tailer implements it.
type Tailer interface {
	Tail(ctx context.Context, jobName string, sink jobs.Sink) error
}

// maxLiveTails bounds concurrent upstream log follows. Each one is an open
// watch against the API server, so the cap protects the apiserver rather than
// this process — viewers past it fall back to the persisted transcript.
const maxLiveTails = 8

// tailBuffer bounds the turns a run holds for replay. It matches the
// recorder's own turn cap, so a viewer joining late still sees the whole
// conversation rather than only what arrives after they connect. One more
// slot is kept for the recorder's closing notice (capNotice), the turn after
// its cap that tells a reader the record stopped.
const tailBuffer = transcript.DefaultMaxTurns

// capNotice reports the recorder's closing notice: it stops recording at its
// turn or byte cap and says so in one truncated notice, while the agent may
// go on working.
func capNotice(t transcript.Turn) bool {
	return t.Kind == transcript.KindNotice && t.Truncated
}

// errTooManyTails is returned when the live-follow budget is exhausted.
var errTooManyTails = errors.New("too many live transcripts")

// Command output is live only and kept apart from the turns: its own
// replay, its own channel per viewer, its own bounds.
const (
	// outputRing bounds the command-output lines a run holds for replay: the
	// tail of the latest command, enough to show a late viewer where it is.
	// It also bounds one chunk: a longer one keeps only its last lines, and
	// the line numbers show the rest as left out.
	outputRing = 200
	// maxOutputLine bounds one output line the hub holds, in bytes. The run
	// stream shows at most 1 KiB of a line, so this keeps everything it can
	// show with room to spare.
	maxOutputLine = 4 << 10
	// turnQueue and outputQueue are each viewer's channel buffers. A full
	// one drops: a turn costs that viewer one line of the conversation, an
	// output chunk a gap the line numbers show.
	turnQueue   = 64
	outputQueue = 128
)

// tailHub multiplexes one upstream log follow per running Job across every
// viewer watching it, and replays what the run has already said to each new
// subscriber. Without it, ten people opening the same finding would open ten
// follows and each would see the conversation only from the moment they
// arrived.
type tailHub struct {
	tailer Tailer
	log    *slog.Logger

	mu   sync.Mutex
	runs map[string]*tailRun
}

func newTailHub(t Tailer, log *slog.Logger) *tailHub {
	return &tailHub{tailer: t, log: log, runs: make(map[string]*tailRun)}
}

// tailRun is one followed Job and its watchers.
type tailRun struct {
	cancel context.CancelFunc

	mu       sync.Mutex
	seen     []transcript.Turn
	output   outputTail
	subs     map[*tailSub]struct{}
	finished bool
}

// tailSub is one viewer's channels; output is nil for a viewer that takes
// none.
type tailSub struct {
	turns  chan transcript.Turn
	output chan transcript.Output
}

// close ends both of a viewer's channels.
func (t *tailSub) close() {
	close(t.turns)
	if t.output != nil {
		close(t.output)
	}
}

// subscription is one viewer's view of a followed run.
type subscription struct {
	// Replay is everything the run said before this viewer arrived.
	Replay []transcript.Turn
	// Turns delivers what it says next, and closes when the run ends.
	Turns <-chan transcript.Turn
	// OutputReplay is the latest command's output held so far, for a viewer
	// that asked for output (outputTail.replay); Output delivers the chunks
	// that follow and closes with Turns. Both are empty for a viewer that
	// did not ask.
	OutputReplay []transcript.Output
	Output       <-chan transcript.Output

	hub     *tailHub
	jobName string
	run     *tailRun // the run this viewer joined, whatever the hub follows since
	sub     *tailSub
}

// Close detaches the viewer, stopping the upstream follow when it was the last.
func (s *subscription) Close() { s.hub.unsubscribe(s) }

// subscribe attaches a viewer to jobName, starting the follow if this is the
// first. It returns the conversation so far plus the channel carrying the
// rest, and with output the same for the latest command's output, all
// captured atomically so nothing falls between a replay and its channel.
//
// The viewer is registered in the same critical section, under h.mu, that
// found or started its run (lock order: h.mu, then run.mu, everywhere both
// are held). unsubscribe takes h.mu too, so a viewer leaving can never come
// between the two, find the run unwatched and cancel it under the newcomer.
func (h *tailHub) subscribe(jobName string, output bool) (*subscription, error) {
	if h == nil || h.tailer == nil {
		return nil, errTooManyTails
	}
	ts := &tailSub{turns: make(chan transcript.Turn, turnQueue)}
	if output {
		ts.output = make(chan transcript.Output, outputQueue)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	run, ok := h.runs[jobName]
	if !ok {
		if len(h.runs) >= maxLiveTails {
			return nil, errTooManyTails
		}
		run = &tailRun{subs: make(map[*tailSub]struct{})}
		h.runs[jobName] = run
		ctx, cancel := context.WithCancel(context.Background())
		run.cancel = cancel
		go h.follow(ctx, jobName, run)
	}

	sub := &subscription{Turns: ts.turns, Output: ts.output, hub: h, jobName: jobName, run: run, sub: ts}
	run.mu.Lock()
	defer run.mu.Unlock()
	sub.Replay = append([]transcript.Turn(nil), run.seen...)
	if output {
		sub.OutputReplay = run.output.replay()
	}
	if run.finished {
		// The run ended and its follow has not yet taken it out of the hub;
		// the replay is the whole conversation, so hand back closed channels.
		ts.close()
		return sub, nil
	}
	run.subs[ts] = struct{}{}
	return sub, nil
}

// follow drives the upstream log follow, buffering and fanning out each turn
// and each chunk of command output.
func (h *tailHub) follow(ctx context.Context, jobName string, run *tailRun) {
	err := h.tailer.Tail(ctx, jobName, jobs.Sink{
		Turn: func(t transcript.Turn) error {
			run.mu.Lock()
			if len(run.seen) < tailBuffer || (len(run.seen) == tailBuffer && capNotice(t)) {
				run.seen = append(run.seen, t)
			}
			for sub := range run.subs {
				// Never block the follow on a slow viewer: a dropped turn
				// costs that viewer one line, a blocked follow costs everyone
				// the rest.
				select {
				case sub.turns <- t:
				default:
				}
			}
			run.mu.Unlock()
			return ctx.Err()
		},
		Output: func(o transcript.Output) error {
			o = boundOutput(o)
			run.mu.Lock()
			run.output.add(o)
			for sub := range run.subs {
				if sub.output == nil {
					continue
				}
				// As above, on its own channel: a flood of output never
				// costs a viewer a turn.
				select {
				case sub.output <- o:
				default:
				}
			}
			run.mu.Unlock()
			return ctx.Err()
		},
	})
	if err != nil && ctx.Err() == nil {
		h.log.LogAttrs(ctx, slog.LevelWarn, "transcript follow ended",
			slog.String("job", jobName), slog.Any("error", err))
	}

	run.mu.Lock()
	run.finished = true
	for sub := range run.subs {
		sub.close()
		delete(run.subs, sub)
	}
	run.mu.Unlock()

	h.mu.Lock()
	if h.runs[jobName] == run {
		delete(h.runs, jobName)
	}
	h.mu.Unlock()
}

// unsubscribe detaches one viewer from the run it joined, and cancels that
// run's follow when it was the last viewer of a run the hub still follows.
// It acts on the viewer's own run alone: never on a newer run of the same
// Job name the hub has started since that one ended. Lock order: h.mu, then
// run.mu (see subscribe).
func (h *tailHub) unsubscribe(s *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	run := s.run
	run.mu.Lock()
	if _, live := run.subs[s.sub]; live {
		delete(run.subs, s.sub)
		s.sub.close()
	}
	last := len(run.subs) == 0 && !run.finished && h.runs[s.jobName] == run
	run.mu.Unlock()

	if last {
		// Out of the hub at once, not as the follow unwinds: a viewer who
		// comes next starts a follow of its own rather than join this one
		// as it ends.
		delete(h.runs, s.jobName)
		run.cancel()
	}
}

// activeTails reports how many follows are open (tests).
func (h *tailHub) activeTails() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runs)
}

// outputTail is a run's command-output replay: the latest command's last
// outputRing lines by line number, and what is known of that command. A
// chunk for another command starts it over, so it never mixes two.
type outputTail struct {
	task  string
	lines []outputLine // ascending line numbers, at most outputRing
	// end is the number after the last line known to exist, held or not.
	end             int
	done, truncated bool
}

// outputLine is one held line and its number in its command's output.
type outputLine struct {
	n    int
	text string
}

func (o *outputTail) add(c transcript.Output) {
	if c.Task != o.task {
		*o = outputTail{task: c.Task}
	}
	for i, text := range c.Lines {
		// A line before end is a repeat, or one that arrived after lines
		// past it: either way the replay has already moved on.
		if n := c.Line + i; n >= o.end {
			o.lines = append(o.lines, outputLine{n: n, text: text})
		}
	}
	if over := len(o.lines) - outputRing; over > 0 {
		o.lines = append(o.lines[:0], o.lines[over:]...)
	}
	o.end = max(o.end, c.Line+len(c.Lines))
	o.done = o.done || c.Done
	o.truncated = o.truncated || c.Truncated
}

// replay renders the held output as chunks a viewer reads like live ones:
// one per run of consecutive lines, so every line keeps its number, then an
// empty chunk at end when lines past the last held one were left out. The
// last chunk carries the command's done and truncated state. It is nil
// before any output.
func (o *outputTail) replay() []transcript.Output {
	if o.task == "" {
		return nil
	}
	var out []transcript.Output
	for _, l := range o.lines {
		if k := len(out) - 1; k >= 0 && out[k].Line+len(out[k].Lines) == l.n {
			out[k].Lines = append(out[k].Lines, l.text)
			continue
		}
		out = append(out, transcript.Output{V: transcript.OutputVersion, Task: o.task, Line: l.n,
			Lines: []string{l.text}})
	}
	if k := len(out) - 1; k < 0 || out[k].Line+len(out[k].Lines) < o.end {
		out = append(out, transcript.Output{V: transcript.OutputVersion, Task: o.task, Line: max(o.end, 1)})
	}
	last := &out[len(out)-1]
	last.Done, last.Truncated = o.done, o.truncated
	return out
}

// boundOutput holds a chunk to the hub's bounds before it is kept or fanned
// out: at most its last outputRing lines, the first renumbered so the lines
// left out read as a gap, and each line at most maxOutputLine bytes. It
// copies the lines it keeps rather than alias the decoder's.
func boundOutput(o transcript.Output) transcript.Output {
	if over := len(o.Lines) - outputRing; over > 0 {
		o.Line += over
		o.Lines = o.Lines[over:]
	}
	lines := make([]string, len(o.Lines))
	for i, l := range o.Lines {
		lines[i], _ = transcript.Truncate(l, maxOutputLine)
	}
	o.Lines = lines
	return o
}
