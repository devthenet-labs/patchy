// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// countingTailer records how many follows were opened and streams on demand.
type countingTailer struct {
	follows atomic.Int32

	mu   sync.Mutex
	sink jobs.Sink
	once sync.Once     // live is signalled by the first follow only
	live chan struct{} // closed once a follow is delivering
	stop chan struct{}
}

func newCountingTailer() *countingTailer {
	return &countingTailer{live: make(chan struct{}), stop: make(chan struct{})}
}

func (c *countingTailer) Tail(ctx context.Context, _ string, sink jobs.Sink) error {
	c.follows.Add(1)
	c.mu.Lock()
	c.sink = sink
	c.mu.Unlock()
	c.once.Do(func() { close(c.live) })

	select {
	case <-c.stop:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// send pushes a turn through the active follow.
func (c *countingTailer) send(t transcript.Turn) {
	c.mu.Lock()
	fn := c.sink.Turn
	c.mu.Unlock()
	if fn != nil {
		_ = fn(t)
	}
}

// sendOutput pushes a chunk of command output through the active follow.
func (c *countingTailer) sendOutput(o transcript.Output) {
	c.mu.Lock()
	fn := c.sink.Output
	c.mu.Unlock()
	if fn != nil {
		_ = fn(o)
	}
}

func testHub(t Tailer) *tailHub {
	return newTailHub(t, slog.New(slog.DiscardHandler))
}

func TestHubSharesOneFollowAcrossViewers(t *testing.T) {
	// Ten people opening the same finding must not open ten watches against
	// the API server.
	tl := newCountingTailer()
	h := testHub(tl)

	subs := make([]*subscription, 3)
	for i := range subs {
		sub, err := h.subscribe("job-1", false)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subs[i] = sub
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	<-tl.live
	if got := tl.follows.Load(); got != 1 {
		t.Errorf("opened %d follows for 3 viewers, want 1", got)
	}

	tl.send(transcript.Turn{Seq: 1, Kind: transcript.KindText, Text: "hello"})
	for i, sub := range subs {
		select {
		case turn := <-sub.Turns:
			if turn.Text != "hello" {
				t.Errorf("viewer %d got %+v", i, turn)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("viewer %d received nothing", i)
		}
	}
}

func TestHubReplaysToLateViewer(t *testing.T) {
	// A viewer opening a run already 50 turns in should see all 50, not just
	// what arrives after they connect.
	tl := newCountingTailer()
	h := testHub(tl)

	first, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer first.Close()
	<-tl.live

	tl.send(transcript.Turn{Seq: 1, Kind: transcript.KindText, Text: "one"})
	tl.send(transcript.Turn{Seq: 2, Kind: transcript.KindText, Text: "two"})
	<-first.Turns
	<-first.Turns

	late, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("late subscribe: %v", err)
	}
	defer late.Close()

	if len(late.Replay) != 2 {
		t.Fatalf("replay = %+v, want both earlier turns", late.Replay)
	}
	if late.Replay[0].Text != "one" || late.Replay[1].Text != "two" {
		t.Errorf("replay = %+v", late.Replay)
	}

	// And it still receives what comes next.
	tl.send(transcript.Turn{Seq: 3, Kind: transcript.KindText, Text: "three"})
	select {
	case turn := <-late.Turns:
		if turn.Text != "three" {
			t.Errorf("late viewer got %+v, want three", turn)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late viewer received nothing after replay")
	}
}

// A run that reached the recorder's cap: its closing notice is the turn
// after the cap, and a viewer joining late must still see it, or nothing
// tells them the record stopped while the agent kept working.
func TestHubReplaysTheCapNoticeToLateViewer(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	first, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer first.Close()
	<-tl.live

	// tl.send hands each turn to the follow synchronously, so the buffer
	// holds every one of them when the loop ends.
	for i := range transcript.DefaultMaxTurns {
		tl.send(transcript.Turn{Seq: i + 1, Kind: transcript.KindText, Text: "turn"})
	}
	notice := transcript.Turn{Seq: transcript.DefaultMaxTurns + 1, Role: transcript.RoleSystem,
		Kind: transcript.KindNotice, Text: "transcript truncated: 500 turn cap reached", Truncated: true}
	tl.send(notice)
	// Past the cap nothing more is kept, notices included.
	tl.send(transcript.Turn{Seq: transcript.DefaultMaxTurns + 2, Kind: transcript.KindNotice, Truncated: true})

	late, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("late subscribe: %v", err)
	}
	defer late.Close()
	if n := len(late.Replay); n != transcript.DefaultMaxTurns+1 {
		t.Fatalf("replay holds %d turns, want the %d the recorder admits plus its closing notice",
			n, transcript.DefaultMaxTurns)
	}
	if got := late.Replay[len(late.Replay)-1]; got.Seq != notice.Seq || got.Kind != transcript.KindNotice {
		t.Errorf("last replayed turn = %+v, want the cap notice", got)
	}
}

func TestHubStopsFollowWhenLastViewerLeaves(t *testing.T) {
	// A leaked follow is an open watch against the API server for the rest of
	// the run.
	tl := newCountingTailer()
	h := testHub(tl)

	a, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	b, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	<-tl.live

	a.Close()
	if h.activeTails() != 1 {
		t.Error("follow stopped while a viewer was still watching")
	}
	b.Close()
	waitFor(t, func() bool { return h.activeTails() == 0 })
}

func TestHubCapsConcurrentFollows(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)

	subs := make([]*subscription, 0, maxLiveTails)
	for i := range maxLiveTails {
		sub, err := h.subscribe(jobName(i), false)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subs = append(subs, sub)
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	if _, err := h.subscribe("one-too-many", false); err == nil {
		t.Errorf("subscribe past the cap of %d succeeded, want an error", maxLiveTails)
	}
}

func TestHubClosesChannelWhenRunEnds(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)

	sub, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	<-tl.live

	close(tl.stop) // the agent container exited
	select {
	case _, ok := <-sub.Turns:
		if ok {
			t.Error("received a turn after the run ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed when the run ended")
	}
}

func TestHubWithoutTailerRefuses(t *testing.T) {
	// A deployment with no reach into the agents namespace still serves
	// persisted transcripts; it just cannot follow live ones.
	h := testHub(nil)
	if _, err := h.subscribe("job-1", false); err == nil {
		t.Error("subscribe without a tailer succeeded, want an error")
	}
}

func jobName(i int) string { return "job-" + string(rune('a'+i)) }

// numbered is n output lines, "line <i>" for each i from first.
func numbered(first, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", first+i)
	}
	return lines
}

// outputViewer subscribes to job-1 for output once its follow is live.
func outputViewer(t *testing.T, h *tailHub, tl *countingTailer) *subscription {
	t.Helper()
	sub, err := h.subscribe("job-1", true)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	<-tl.live
	return sub
}

// A late viewer is replayed the latest command's last outputRing lines,
// numbered as they were, never the whole of a long command's output.
func TestHubOutputReplayKeepsTheLatestLines(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	outputViewer(t, h, tl)
	for first := 1; first <= 300; first += 100 {
		tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: first, Lines: numbered(first, 100)})
	}

	late := outputViewer(t, h, tl)
	if len(late.OutputReplay) != 1 {
		t.Fatalf("replay = %d chunks, want one run of consecutive lines", len(late.OutputReplay))
	}
	got := late.OutputReplay[0]
	if got.Task != "b1" || got.Line != 101 || len(got.Lines) != outputRing || got.Done {
		t.Errorf("replay = task %q line %d, %d lines, done %v; want b1 from line 101, %d lines, running",
			got.Task, got.Line, len(got.Lines), got.Done, outputRing)
	}
	if got.Lines[0] != "line 101" || got.Lines[outputRing-1] != "line 300" {
		t.Errorf("replay runs %q to %q, want line 101 to line 300", got.Lines[0], got.Lines[outputRing-1])
	}
}

// A chunk for another command starts the replay over: a late viewer sees
// only the newest command, with none of the last one's state.
func TestHubOutputReplayResetsOnANewCommand(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	outputViewer(t, h, tl)
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 1, Lines: numbered(1, 50)})
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 51, Done: true, Truncated: true})
	tl.sendOutput(transcript.Output{V: 1, Task: "b2", Line: 1, Lines: []string{"fresh"}})

	late := outputViewer(t, h, tl)
	want := []transcript.Output{{V: 1, Task: "b2", Line: 1, Lines: []string{"fresh"}}}
	if !reflect.DeepEqual(late.OutputReplay, want) {
		t.Errorf("replay = %+v, want only the new command's line", late.OutputReplay)
	}
}

// The replay keeps every line's number: one chunk per run of consecutive
// lines, then an empty chunk where lines past the last one were left out,
// carrying the command's end and its truncation. Repeats are held once.
func TestHubOutputReplayKeepsGaps(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	outputViewer(t, h, tl)
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 1, Lines: []string{"a", "b"}})
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 2, Lines: []string{"b", "c"}}) // overlaps
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 10, Lines: []string{"j"}})
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 4, Lines: []string{"late"}}) // behind the end
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 20, Done: true, Truncated: true})

	late := outputViewer(t, h, tl)
	want := []transcript.Output{
		{V: 1, Task: "b1", Line: 1, Lines: []string{"a", "b", "c"}},
		{V: 1, Task: "b1", Line: 10, Lines: []string{"j"}},
		{V: 1, Task: "b1", Line: 20, Done: true, Truncated: true},
	}
	if !reflect.DeepEqual(late.OutputReplay, want) {
		t.Errorf("replay = %+v\nwant %+v", late.OutputReplay, want)
	}

	// A command that finished with every line held ends on its last chunk.
	tl.sendOutput(transcript.Output{V: 1, Task: "b2", Line: 1, Lines: []string{"x"}})
	tl.sendOutput(transcript.Output{V: 1, Task: "b2", Line: 2, Done: true})
	again := outputViewer(t, h, tl)
	want = []transcript.Output{{V: 1, Task: "b2", Line: 1, Lines: []string{"x"}, Done: true}}
	if !reflect.DeepEqual(again.OutputReplay, want) {
		t.Errorf("replay of a finished command = %+v, want %+v", again.OutputReplay, want)
	}
}

// One chunk is held to the hub's bounds before anyone sees it: its last
// outputRing lines, renumbered so the rest read as a gap, each line cut to
// maxOutputLine bytes on a rune boundary.
func TestHubOutputBoundsAChunk(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	sub := outputViewer(t, h, tl)
	lines := numbered(1, outputRing+50)
	lines[len(lines)-1] = strings.Repeat("é", maxOutputLine) // two bytes each
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 1, Lines: lines})

	got := <-sub.Output
	if got.Line != 51 || len(got.Lines) != outputRing || got.Lines[0] != "line 51" {
		t.Errorf("chunk = line %d, %d lines from %q; want line 51, %d lines", got.Line, len(got.Lines),
			got.Lines[0], outputRing)
	}
	long := got.Lines[len(got.Lines)-1]
	if len(long) > maxOutputLine || !utf8.ValidString(long) {
		t.Errorf("long line kept %d bytes (valid UTF-8: %v), want at most %d and whole runes",
			len(long), utf8.ValidString(long), maxOutputLine)
	}
}

// Output has its own replay and its own channel: a flood of it neither
// enters the turn replay nor costs a viewer who reads nothing a single turn,
// and only that viewer's own output is dropped, from the newest end.
func TestHubOutputFloodNeverCostsATurn(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	slow := outputViewer(t, h, tl)
	const flood = 1000
	tl.send(transcript.Turn{Seq: 1, Kind: transcript.KindToolUse, Tool: "Bash", Text: "npm test"})
	for i := range flood {
		tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: i + 1, Lines: []string{"x"}})
	}
	tl.send(transcript.Turn{Seq: 2, Kind: transcript.KindToolResult, Text: "done"})

	for want := 1; want <= 2; want++ {
		select {
		case turn := <-slow.Turns:
			if turn.Seq != want {
				t.Errorf("turn %d arrived as seq %d", want, turn.Seq)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("turn %d never arrived behind the output flood", want)
		}
	}
	if n := len(slow.Output); n != outputQueue {
		t.Errorf("the slow viewer holds %d chunks, want its %d-chunk buffer", n, outputQueue)
	}
	for i := range outputQueue {
		if got := <-slow.Output; got.Line != i+1 {
			t.Fatalf("held chunk %d is line %d: a buffered chunk was dropped, not a new one", i, got.Line)
		}
	}

	late := outputViewer(t, h, tl)
	if len(late.Replay) != 2 {
		t.Errorf("turn replay = %d turns, want the 2 turns and no output", len(late.Replay))
	}
	if r := late.OutputReplay; len(r) != 1 || r[0].Line != flood-outputRing+1 || len(r[0].Lines) != outputRing {
		t.Errorf("output replay = %+v, want the last %d lines", r, outputRing)
	}
}

// A viewer who did not ask for output (the Findings transcript, a tier 1
// reader) gets no output channel and no output replay, and its channels
// still close when the run ends; an output viewer's close with them.
func TestHubOutputOnlyForViewersWhoAsk(t *testing.T) {
	tl := newCountingTailer()
	h := testHub(tl)
	viewer := outputViewer(t, h, tl)
	tl.sendOutput(transcript.Output{V: 1, Task: "b1", Line: 1, Lines: []string{"secret-ish"}})

	plain, err := h.subscribe("job-1", false)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer plain.Close()
	if plain.Output != nil || plain.OutputReplay != nil {
		t.Errorf("a viewer without output got channel %v, replay %+v", plain.Output, plain.OutputReplay)
	}
	<-viewer.Output

	close(tl.stop) // the agent container exited
	for name, closed := range map[string]func() bool{
		"turns":  func() bool { _, ok := <-plain.Turns; return !ok },
		"output": func() bool { _, ok := <-viewer.Output; return !ok },
	} {
		done := make(chan bool, 1)
		go func() { done <- closed() }()
		select {
		case ok := <-done:
			if !ok {
				t.Errorf("%s delivered after the run ended", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s channel not closed when the run ended", name)
		}
	}
}
