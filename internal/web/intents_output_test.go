// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

const liveRunStream = "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream"

// fixtureOutput is the running command's output as agent-runner prints it:
// a terminal colour escape to be made visible, a line past the shown cap,
// and the command's last chunk.
func fixtureOutput() []transcript.Output {
	return []transcript.Output{
		{V: 1, Task: "b578qoc1g", Line: 1, Lines: []string{"\x1b[32mPASS\x1b[0m src/router.test.ts",
			"Tests: 12 passed"}},
		{V: 1, Task: "b578qoc1g", Line: 3, Lines: []string{strings.Repeat("x", 3*maxShownOutputLine)}},
		{V: 1, Task: "b578qoc1g", Line: 4, Done: true},
	}
}

// outputEvents decodes a stream body's output events.
func outputEvents(t *testing.T, body string) []RunOutput {
	t.Helper()
	var out []RunOutput
	for _, ev := range sseEvents(body) {
		if ev[0] == eventOutput {
			out = append(out, decode[RunOutput](t, ev[1]))
		}
	}
	return out
}

// One shared follow, a tier 1 and a tier 2 viewer: only the tier 2 stream
// carries the command's output, each line made visible and capped like
// turn text, and the output moves no one's activity.
func TestLiveStreamOutputOnlyForTier2(t *testing.T) {
	for _, order := range [][]*auth.Identity{{viewerAlpha, readerAlpha}, {readerAlpha, viewerAlpha}} {
		hold := make(chan struct{})
		s, _ := intentsServer(t, &fakeTailer{turns: fixtureTurns(), output: fixtureOutput(), hold: hold})
		bodies := map[string]chan string{}
		for _, id := range order {
			ch := make(chan string, 1)
			bodies[id.Username] = ch
			ts := as(t, s, id)
			go func() {
				_, body := get(t, ts, liveRunStream)
				ch <- body
			}()
			waitFor(t, func() bool { return s.tails.activeTails() == 1 })
			time.Sleep(50 * time.Millisecond)
		}
		close(hold)
		tier1, tier2 := <-bodies[viewerAlpha.Username], <-bodies[readerAlpha.Username]

		if got := outputEvents(t, tier1); len(got) != 0 {
			t.Errorf("order %s first: tier 1 received output %+v", order[0].Username, got)
		}
		if strings.Contains(tier1, "Tests: 12") || strings.Contains(tier1, "b578qoc1g") {
			t.Errorf("order %s first: tier 1 stream carries command output: %s", order[0].Username, tier1)
		}

		lines := map[int]string{}
		done := false
		for _, o := range outputEvents(t, tier2) {
			if o.Task != "b578qoc1g" {
				t.Errorf("chunk task = %q", o.Task)
			}
			for i, l := range o.Lines {
				lines[o.Line+i] = l
			}
			done = done || o.Done
		}
		if len(lines) != 3 || !done {
			t.Fatalf("order %s first: tier 2 output lines %v, done %v; want 3 lines and the end",
				order[0].Username, lines, done)
		}
		if strings.ContainsRune(lines[1], 0x1b) || !strings.HasPrefix(lines[1], "[U+001B][32mPASS") {
			t.Errorf("line 1 = %q, want the escape shown as its code point", lines[1])
		}
		if want := strings.Repeat("x", maxShownOutputLine) + "…"; lines[3] != want {
			t.Errorf("line 3 is %d bytes, want it cut to %d and marked", len(lines[3]), maxShownOutputLine)
		}
		for who, body := range map[string]string{"tier 1": tier1, "tier 2": tier2} {
			if a := lastActivity(t, body); a.Turns != 3 || a.LastAt != "2026-07-21T11:07:00Z" {
				t.Errorf("%s activity = %+v, want the 3 turns alone", who, a)
			}
		}
	}
}

// A collected run replays its stored transcript, and command output is
// never stored: its stream carries none, whatever the agent pod still
// holds.
func TestPersistedRunStreamsNoOutput(t *testing.T) {
	s, _ := intentsServer(t, &fakeTailer{turns: fixtureTurns(), output: fixtureOutput()})
	_, body := get(t, as(t, s, readerAlpha), "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream")
	if got := outputEvents(t, body); len(got) != 0 {
		t.Errorf("a persisted run streamed output: %+v", got)
	}
	if n := len(sseEvents(body)); n == 0 {
		t.Fatal("the persisted stream sent nothing at all")
	}
}

// The command's last chunk and the run's end can arrive together; the
// stream still delivers the chunk before it ends, every time.
func TestLiveStreamDeliversTheLastOutputAsTheRunEnds(t *testing.T) {
	for i := range 20 {
		s, _ := intentsServer(t, &fakeTailer{turns: fixtureTurns(), output: fixtureOutput()})
		_, body := get(t, as(t, s, readerAlpha), liveRunStream)
		done := false
		for _, o := range outputEvents(t, body) {
			done = done || o.Done
		}
		if !done {
			t.Fatalf("attempt %d: the stream ended without the command's last chunk: %s", i, body)
		}
	}
}

// floodTailer prints command output without pause until the follow ends.
type floodTailer struct{}

func (floodTailer) Tail(ctx context.Context, _ string, sink jobs.Sink) error {
	for line := 1; ctx.Err() == nil; line++ {
		if err := sink.Output(transcript.Output{V: 1, Task: "b1", Line: line, Lines: []string{"spam"}}); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return ctx.Err()
}

// A stream busy with output still re-checks its grant: revoked, it ends
// with reason revoked rather than following the command forever.
func TestStreamBusyWithOutputIsReauthorised(t *testing.T) {
	s, g := intentsServer(t, floodTailer{})
	s.intents.reauth = 30 * time.Millisecond
	done := make(chan string, 1)
	ts := as(t, s, readerAlpha)
	go func() {
		_, body := get(t, ts, liveRunStream)
		done <- body
	}()
	waitFor(t, func() bool { return s.tails.activeTails() == 1 })
	time.Sleep(60 * time.Millisecond)
	g.set(readerAlpha.Username, "alpha", authz.TierIntents)
	select {
	case body := <-done:
		if len(outputEvents(t, body)) == 0 {
			t.Errorf("the tier 2 stream received no output before its downgrade: %s", body)
		}
		events := sseEvents(body)
		if last := events[len(events)-1]; last[0] != eventEnd || !strings.Contains(last[1], endRevoked) {
			t.Errorf("last event = %v, want end revoked", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stream busy with output outlived its downgraded grant")
	}
	waitFor(t, func() bool { return s.tails.activeTails() == 0 })
}
