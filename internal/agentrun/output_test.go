// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// syncBuffer is the runner's stdout as a test reads it while the stage's
// followers still write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The claude stream lines a command's run is told by.
func initLine() string {
	return `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}`
}

// startedLine announces a foreground shell command; it names no session,
// so the init event's is used, as on a CLI that leaves it off.
func startedLine(task, toolUse string) string {
	return `{"type":"system","subtype":"task_started","task_id":"` + task + `","tool_use_id":"` + toolUse +
		`","description":"run it","task_type":"local_bash","is_backgrounded":false}`
}

func backgroundedLine(task, toolUse string) string {
	return strings.Replace(startedLine(task, toolUse), `"is_backgrounded":false`, `"is_backgrounded":true`, 1)
}

func notifiedLine(task, toolUse string) string {
	return `{"type":"system","subtype":"task_notification","task_id":"` + task + `","tool_use_id":"` + toolUse +
		`","status":"completed","output_file":""}`
}

func answeredLine(toolUse, text string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + toolUse +
		`","content":"` + text + `"}]}}`
}

const resultLine = `{"type":"result","subtype":"success","is_error":false,"result":"Done.","session_id":"` +
	testSessionID + `","num_turns":3}`

// commandRun is one run as commandExec plays it: the script feeds the
// CLI's stream and writes the commands' files meanwhile, then the run
// writes its files and ends with a result.
type commandRun struct {
	script func(feed func(string))
	writes map[string]string
}

// commandExec plays each run as claude does when its agent runs commands:
// the stream reaches the observer on the runner's goroutine, line by line,
// while the test writes the files the CLI would. slowest is the longest any
// one line kept the observer.
type commandExec struct {
	ws      string
	runs    []commandRun
	slowest time.Duration
}

func (e *commandExec) Run(_ context.Context, _ runner.CommandSpec, _ time.Duration,
	onLine func([]byte) (bool, string)) (runner.Result, error) {
	if len(e.runs) == 0 {
		return runner.Result{}, fmt.Errorf("commandExec: no run scripted")
	}
	r := e.runs[0]
	e.runs = e.runs[1:]
	var out strings.Builder
	feed := func(line string) {
		out.WriteString(line + "\n")
		if onLine == nil {
			return
		}
		start := time.Now()
		onLine([]byte(line + "\n"))
		e.slowest = max(e.slowest, time.Since(start))
	}
	feed(initLine())
	r.script(feed)
	for path, content := range r.writes {
		full := filepath.Join(e.ws, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return runner.Result{}, err
		}
	}
	feed(resultLine)
	return runner.Result{Stdout: []byte(out.String()), Elapsed: time.Second}, nil
}

// reported is the plan stage's good report, for a run to write.
var reported = map[string]string{"reports/plan.md": goodPlan}

// commandFile creates the file claude writes a command's output to, under
// tmp (CLAUDE_CODE_TMPDIR), in a working directory's slug, and returns it
// open for appending, and its path.
func commandFile(t *testing.T, tmp, task string) (*os.File, string) {
	t.Helper()
	dir := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), "-workspace-repo", testSessionID, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, task+".output")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// put appends output to a command's file.
func put(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// outputSetup is a stage whose CLI keeps its temporary files under a test
// directory: an intent's plan stage on the fake harness, which follows
// commands as claude does.
func outputSetup(t *testing.T) (Config, string, string, *syncBuffer) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CODE_TMPDIR", tmp)
	out := &syncBuffer{}
	cfg, ws := intentConfig(t, PhasePlan, nil)
	cfg.Out = out
	return cfg, ws, tmp, out
}

// fastPace makes the tests that do not measure timing quick.
var fastPace = outputPace{poll: 5 * time.Millisecond, flush: 10 * time.Millisecond,
	sample: 40 * time.Millisecond, drain: 2 * time.Second}

// runStage runs the configured stage at the given pace (zero: production's)
// and returns its one event.
func runStage(t *testing.T, cfg Config, exec Executor, pace outputPace, out *syncBuffer) envelope.Event {
	t.Helper()
	a := New(cfg, exec)
	if pace != (outputPace{}) {
		a.pace = pace
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1:\n%s", len(evs), out.String())
	}
	return evs[0]
}

// chunks decodes every output chunk on the runner's stdout, in order.
func chunks(out string) []transcript.Output {
	var got []transcript.Output
	for line := range strings.SplitSeq(out, "\n") {
		if o, ok := transcript.DecodeOutput([]byte(line)); ok {
			got = append(got, o)
		}
	}
	return got
}

// waitFor polls the runner's stdout until cond holds of its chunks.
func waitFor(t *testing.T, out *syncBuffer, what string, cond func([]transcript.Output) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond(chunks(out.String())) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; chunks: %+v", what, chunks(out.String()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settled waits for the process's goroutines to fall back to n: a command's
// goroutine that outlived its run would hold it above.
func settled(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > n {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines = %d after the stage, want %d:\n%s", runtime.NumGoroutine(), n,
				buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkWholeLines asserts every line of stdout is one whole line of one of
// its three streams: nothing interleaved.
func checkWholeLines(t *testing.T, out string) {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		_, turn := transcript.Decode([]byte(line))
		_, chunk := transcript.DecodeOutput([]byte(line))
		_, event := envelope.Decode([]byte(line))
		if !turn && !chunk && !event {
			t.Errorf("stdout line is none of a turn, a chunk or an event: %.200q", line)
		}
	}
}

// joined is a task's lines over its chunks, checking they are consecutive
// from line 1 and only the last is Done.
func joined(t *testing.T, got []transcript.Output) []string {
	t.Helper()
	var lines []string
	for i, o := range got {
		if o.Line != len(lines)+1 {
			t.Errorf("chunk %d starts at line %d, want %d: lines left out of a command under its budget",
				i, o.Line, len(lines)+1)
		}
		if o.Done != (i == len(got)-1) {
			t.Errorf("chunk %d done = %v; only the last chunk is", i, o.Done)
		}
		if o.Truncated {
			t.Errorf("chunk %d is truncated; the command was under its budget", i)
		}
		if o.At == "" {
			t.Errorf("chunk %d has no timestamp", i)
		}
		lines = append(lines, o.Lines...)
	}
	return lines
}

// TestOutputFollowsACommand runs a command the way claude 2.1.263 does —
// announced, its output appended to a file as it is produced, the file
// deleted as it ends, then the end announced — and checks a viewer sees it
// live, line by line as a terminal shows it, scrubbed and capped, in order,
// with the lines written after the file was deleted, closed by a Done chunk
// before the stage's result; and that following it never held up the stream.
func TestOutputFollowsACommand(t *testing.T) {
	const secret = "s3cr3t-output-value-42"
	t.Setenv("OUTPUT_TEST_API_KEY", secret)
	cfg, ws, tmp, out := outputSetup(t)
	baseline := runtime.NumGoroutine()

	long := strings.Repeat("é", 700) // 1400 bytes
	var live []transcript.Output
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		f, path := commandFile(t, tmp, "b578qoc1g")
		feed(startedLine("b578qoc1g", "toolu_01"))
		put(t, f, "first\n\x1b[31mred\x1b[0m\n10%\r50%\r100%\ncrlf\r\nkey="+secret+"\n"+long+"\n")
		// The production pace: the first lines reach stdout while the
		// command still runs.
		waitFor(t, out, "the first lines, live", func(got []transcript.Output) bool {
			return len(got) > 0 && slices.Contains(got[0].Lines, "first")
		})
		live = chunks(out.String())
		put(t, f, "par")
		time.Sleep(2 * defaultOutputPace.poll) // a read between the halves
		put(t, f, "tial\nbefore unlink\n")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		put(t, f, "after unlink\nunterminated") // the CLI's open file outlives its name
		feed(notifiedLine("b578qoc1g", "toolu_01"))
		feed(answeredLine("toolu_01", "first"))
	}}}}
	ev := runStage(t, cfg, exec, outputPace{}, out)
	if st := stageOf(t, ev); st.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %s (%s)", st.Outcome, st.Detail)
	}

	got := chunks(out.String())
	for _, o := range got {
		if o.Task != "b578qoc1g" {
			t.Errorf("chunk task = %q, want b578qoc1g", o.Task)
		}
	}
	capped := strings.Repeat("é", 510) + "…"
	want := []string{"first", "red", "100%", "crlf", "key=" + transcript.Redacted, capped, "partial",
		"before unlink", "after unlink", "unterminated"}
	if lines := joined(t, got); !slices.Equal(lines, want) {
		t.Errorf("lines = %q,\nwant %q", lines, want)
	}
	if len(capped) > outputLineBytes || !utf8.ValidString(capped) {
		t.Fatalf("the expected cap is wrong: %d bytes", len(capped))
	}
	if len(live) == 0 || live[len(live)-1].Done {
		t.Errorf("chunks while the command ran = %+v, want some, none of them done", live)
	}
	if strings.Contains(out.String(), secret) {
		t.Error("stdout carries the secret the command printed")
	}

	// The Done chunk comes before the stage's result, and nothing after it.
	text := out.String()
	done := strings.LastIndex(text, transcript.OutputPrefix)
	if result := strings.Index(text, envelope.Prefix); done < 0 || result < done {
		t.Errorf("the last output chunk (at %d) is not before the stage's result (at %d)", done, result)
	}
	checkWholeLines(t, text)
	// Output is not the conversation: no turn carries it.
	for _, turn := range turns(t, text) {
		if strings.Contains(turn.Text, "before unlink") {
			t.Errorf("turn %d carries the command's output: %q", turn.Seq, turn.Text)
		}
	}
	if exec.slowest > 100*time.Millisecond {
		t.Errorf("a stream line held the observer %s; following a command must never block it", exec.slowest)
	}
	settled(t, baseline)
	if after := out.String(); after != text {
		t.Errorf("stdout grew after the stage ended:\n%s", strings.TrimPrefix(after, text))
	}
}

// numbered is line n of a numbered command's output.
func numbered(n int) string { return fmt.Sprintf("%05d %s", n, strings.Repeat("x", 100)) }

// sampledFrom is the index of the first chunk that starts past the end of
// the one before it, as a sample does, or len(got).
func sampledFrom(got []transcript.Output) int {
	next := 1
	for i, o := range got {
		if o.Line > next {
			return i
		}
		next = o.Line + len(o.Lines)
	}
	return len(got)
}

// printedBytes is what chunks put on stdout, framing included.
func printedBytes(got []transcript.Output) int {
	n := 0
	for _, o := range got {
		line, _ := transcript.EncodeOutput(o)
		n += len(line) + 1
	}
	return n
}

// textBytes is the text of a chunk's lines.
func textBytes(o transcript.Output) int {
	n := 0
	for _, l := range o.Lines {
		n += len(l)
	}
	return n
}

// TestOutputSamplesALongCommand: a command printing past its budget is
// printed at full fidelity up to it, then sampled: every sample interval its
// newest lines, at most outputSampleLines of them and outputSampleBytes of
// their text, under their true numbers so the gap shows. A sample is not a
// truncation: the command's live output goes on, and its last chunk shows
// its newest lines, Done.
func TestOutputSamplesALongCommand(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	const burst, tail = 3000, 25
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		f, path := commandFile(t, tmp, "blong")
		feed(startedLine("blong", "toolu_long"))
		var b strings.Builder
		for n := 1; n <= burst; n++ {
			b.WriteString(numbered(n) + "\n")
		}
		put(t, f, b.String())
		waitFor(t, out, "a sample", func(got []transcript.Output) bool { return sampledFrom(got) < len(got) })
		// A few lines, fewer than a sample shows: the next sample is those
		// alone, never the lines before them shown again.
		put(t, f, numbered(burst+1)+"\n"+numbered(burst+2)+"\n"+numbered(burst+3)+"\n")
		waitFor(t, out, "a sample of the next lines", func(got []transcript.Output) bool {
			return slices.ContainsFunc(got, func(o transcript.Output) bool {
				return slices.Contains(o.Lines, numbered(burst+3))
			})
		})
		for n := burst + 4; n <= burst+tail; n++ {
			put(t, f, numbered(n)+"\n")
			time.Sleep(2 * time.Millisecond)
		}
		_ = os.Remove(path)
		feed(notifiedLine("blong", "toolu_long"))
	}}}}
	runStage(t, cfg, exec, fastPace, out)

	got := chunks(out.String())
	full := sampledFrom(got)
	for i, o := range got[:full] {
		if o.Done || o.Truncated {
			t.Fatalf("full-fidelity chunk %d = %+v, want neither done nor truncated", i, o)
		}
	}
	if printed := printedBytes(got[:full]); printed < outputTaskBytes || printed > outputTaskBytes+16<<10 {
		t.Errorf("printed %d bytes at full fidelity, want the budget %d and at most one chunk more",
			printed, outputTaskBytes)
	}
	sampled := got[full:]
	if len(sampled) < 2 {
		t.Fatalf("sampled chunks = %+v, want samples and a last chunk", sampled)
	}
	last := got[full-1].Line + len(got[full-1].Lines) - 1
	for i, o := range sampled {
		if o.Truncated || len(o.Lines) > outputSampleLines || textBytes(o) > outputSampleBytes {
			t.Errorf("sample %d = %d lines of %d bytes, truncated %v; want at most %d lines and %d bytes, "+
				"not truncated", i, len(o.Lines), textBytes(o), o.Truncated, outputSampleLines, outputSampleBytes)
		}
		if o.Line <= last {
			t.Errorf("sample %d starts at line %d, not after line %d", i, o.Line, last)
		}
		for k, text := range o.Lines {
			if want := numbered(o.Line + k); text != want {
				t.Errorf("sample %d line %d = %.20q, want %.20q: a line shown under another's number", i,
					o.Line+k, text, want)
			}
		}
		if len(o.Lines) > 0 {
			last = o.Line + len(o.Lines) - 1
		}
		if o.Done != (i == len(sampled)-1) {
			t.Errorf("sample %d done = %v; only the last chunk is", i, o.Done)
		}
	}
	// The newest line is shown by the last chunk, or by a sample before it,
	// when the last chunk then carries none and starts right after it.
	if end := sampled[len(sampled)-1]; last != burst+tail {
		t.Errorf("the last chunk = %+v, want line %d, the command's last, shown by then", end, burst+tail)
	}
}

// TestOutputStopsSamplingAtItsCap: a command that floods long past its
// budget is sampled until its samples have printed outputSampledBytes, each
// at most outputSampleBytes of text; then one chunk past its last line read
// marks its live output truncated for good, and nothing more is printed for
// it but its last chunk, Done and still Truncated.
func TestOutputStopsSamplingAtItsCap(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	pace := outputPace{poll: 2 * time.Millisecond, flush: 4 * time.Millisecond, sample: 4 * time.Millisecond,
		drain: 2 * time.Second}
	truncated := func(got []transcript.Output) bool {
		return slices.ContainsFunc(got, func(o transcript.Output) bool { return o.Truncated })
	}
	long := strings.Repeat("z", 1500) // shown cut to a KiB: two fill a sample
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		f, path := commandFile(t, tmp, "bflood")
		feed(startedLine("bflood", "toolu_flood"))
		deadline := time.Now().Add(20 * time.Second)
		n := 1
		for ; n%20 != 0 || !truncated(chunks(out.String())); n++ {
			if time.Now().After(deadline) {
				t.Fatalf("no chunk marked the command truncated after %d lines", n)
			}
			put(t, f, fmt.Sprintf("%06d %s\n", n, long))
			time.Sleep(100 * time.Microsecond)
		}
		for range 50 { // after the cut: none of these is shown
			put(t, f, fmt.Sprintf("%06d %s\n", n, long))
			n++
		}
		time.Sleep(10 * pace.sample)
		_ = os.Remove(path)
		feed(notifiedLine("bflood", "toolu_flood"))
	}}}}
	runStage(t, cfg, exec, pace, out)

	got := chunks(out.String())
	full := sampledFrom(got)
	at := slices.IndexFunc(got, func(o transcript.Output) bool { return o.Truncated })
	if full >= at {
		t.Fatalf("the cut is chunk %d and sampling began at chunk %d; want samples before the cut", at, full)
	}
	samples := got[full:at]
	for i, o := range samples {
		if o.Done || len(o.Lines) == 0 || textBytes(o) > outputSampleBytes {
			t.Errorf("sample %d = %d lines of %d bytes, done %v; want lines, at most %d bytes, not done", i,
				len(o.Lines), textBytes(o), o.Done, outputSampleBytes)
		}
	}
	if printed := printedBytes(samples); printed > outputSampledBytes || printed < outputSampledBytes-4<<10 {
		t.Errorf("the samples printed %d bytes, want close to and at most %d", printed, outputSampledBytes)
	}
	cut, shown := got[at], samples[len(samples)-1]
	if cut.Done || len(cut.Lines) != 0 || cut.Line <= shown.Line+len(shown.Lines) {
		t.Errorf("the cut = %+v after a sample ending at line %d; want no lines, not done, a gap before it", cut,
			shown.Line+len(shown.Lines)-1)
	}
	rest := got[at+1:]
	if len(rest) != 1 || !rest[0].Done || !rest[0].Truncated || len(rest[0].Lines) != 0 || rest[0].Line < cut.Line {
		t.Errorf("after the cut = %+v, want only the last chunk: done, truncated, no lines, at or past line %d",
			rest, cut.Line)
	}
}

// TestOutputSamplingKeepsUpWithAFlood: a command flooding its file faster
// than any per-pass byte cap would let a reader follow is still sampled at
// its newest lines: once it pauses, a sample shows its very last line within
// a pass or two, under its exact number, however many lines came before.
func TestOutputSamplingKeepsUpWithAFlood(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	pace := outputPace{poll: 250 * time.Millisecond, flush: 500 * time.Millisecond, sample: 250 * time.Millisecond,
		drain: 2 * time.Second}
	const lines = 6 << 20 // of 9 bytes each: 54 MiB
	last := fmt.Sprintf("%08d", lines)
	var caughtUp time.Duration
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		f, path := commandFile(t, tmp, "bflood")
		feed(startedLine("bflood", "toolu_flood"))
		// Line n is n in eight digits, counted up in place: formatting each
		// would make the writer, not the reader, the test's slow side.
		line, block := []byte("00000001\n"), make([]byte, 0, 1<<20)
		for n := 1; n <= lines; {
			block = block[:0]
			for ; len(block) < cap(block)-len(line) && n <= lines; n++ {
				block = append(block, line...)
				for i := 7; i >= 0; i-- {
					if line[i]++; line[i] <= '9' {
						break
					}
					line[i] = '0'
				}
			}
			if _, err := f.Write(block); err != nil {
				t.Fatal(err)
			}
		}
		wrote := time.Now()
		waitFor(t, out, "a sample of the last line", hasLine("bflood", last))
		caughtUp = time.Since(wrote)
		_ = os.Remove(path)
		feed(notifiedLine("bflood", "toolu_flood"))
	}}}}
	runStage(t, cfg, exec, pace, out)

	t.Logf("the last line was sampled %s after the flood paused", caughtUp)
	if caughtUp > 2*time.Second {
		t.Errorf("the last line was sampled %s after the flood paused; want within a pass or two of %s",
			caughtUp, pace.poll)
	}
	for _, o := range chunks(out.String()) {
		for k, text := range o.Lines {
			if want := fmt.Sprintf("%08d", o.Line+k); text != want {
				t.Fatalf("line %d shows %q: a line under another's number", o.Line+k, text)
			}
		}
	}
}

// TestOutputProcessBudget: commands printing more than a process may are
// cut off once its budget is spent: one chunk marks the live output of the
// command it ran out on truncated, past the lines it dropped, one more closes
// that command, Done, unless the chunk it ran out on was the command's last,
// and nothing more is printed for any command.
func TestOutputProcessBudget(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	spent := func(o transcript.Output) bool { return o.Truncated }
	const commands, lines = 12, 150
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		line := strings.Repeat("y", 999) + "\n"
		for i := 1; i <= commands; i++ {
			task := fmt.Sprintf("bcmd%d", i)
			f, _ := commandFile(t, tmp, task)
			feed(startedLine(task, "toolu_"+task))
			put(t, f, strings.Repeat(line, lines))
			feed(notifiedLine(task, "toolu_"+task))
			if slices.ContainsFunc(chunks(out.String()), spent) {
				continue // spent: nothing more is followed
			}
			waitFor(t, out, task+" to end", func(got []transcript.Output) bool {
				return slices.ContainsFunc(got, func(o transcript.Output) bool { return o.Task == task && o.Done })
			})
		}
		time.Sleep(10 * fastPace.poll)
	}}}}
	runStage(t, cfg, exec, fastPace, out)

	got, total := chunks(out.String()), 0
	for line := range strings.SplitSeq(out.String(), "\n") {
		if transcript.HasOutputPrefix([]byte(line)) {
			total += len(line) + 1
		}
	}
	if total > outputProcessBytes {
		t.Errorf("command output = %d bytes, over the process's %d", total, outputProcessBytes)
	}
	at := budgetClose(t, got)
	cut := got[at]
	if cut.Task == "bcmd1" || cut.Task == fmt.Sprintf("bcmd%d", commands) {
		t.Errorf("the budget ran out on %s; the test meant it to run out midway", cut.Task)
	}
	shown := 0
	for _, o := range got[:at] {
		if o.Task == cut.Task && len(o.Lines) > 0 {
			shown = o.Line + len(o.Lines) - 1
		}
	}
	if cut.Line <= shown || cut.Line > lines+1 {
		t.Errorf("the closing chunk is at line %d after line %d was shown; want past it, at most %d", cut.Line,
			shown, lines+1)
	}
}

// budgetClose checks the chunks the process's budget closed the output with
// and returns the index of the first: the cut, truncated with no lines, then
// its command's end, Done at the same number, unless the cut was that end.
// They are the last chunks, and the first truncated.
func budgetClose(t *testing.T, got []transcript.Output) int {
	t.Helper()
	at := slices.IndexFunc(got, func(o transcript.Output) bool { return o.Truncated })
	if at < 0 {
		t.Fatalf("no chunk is truncated; the test meant the budget to run out: %d chunks", len(got))
	}
	cut, closing := got[at], got[at:]
	want := 2
	if cut.Done {
		want = 1
	}
	if len(closing) != want {
		t.Fatalf("the budget's closing chunks = %+v, want %d, last: the cut, then its command's end unless "+
			"the cut was it", closing, want)
	}
	for _, o := range closing {
		if o.Task != cut.Task || o.Line != cut.Line || len(o.Lines) > 0 || !o.Truncated {
			t.Errorf("closing chunk %+v; want %s's, at line %d, truncated, with no lines", o, cut.Task, cut.Line)
		}
	}
	if !closing[len(closing)-1].Done {
		t.Errorf("the last chunk %+v is not done", closing[len(closing)-1])
	}
	return at
}

// TestOutputBudgetClosingChunk: the chunk the process's budget runs out on
// is replaced by one marking its command's live output truncated past every
// line it carried, so the lines it would have shown read as left out rather
// than as never printed. It is not Done, since the command may still run:
// once the command ends, one line-less Done chunk closes it, and nothing else
// is printed after the budget ran out. Both fit in the room kept back for
// them, whatever the task id.
func TestOutputBudgetClosingChunk(t *testing.T) {
	task := strings.Repeat("b", 128) // the longest id the harness follows
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	out := &syncBuffer{}
	a := New(Config{Out: out, Log: slog.New(slog.DiscardHandler)}, nil)
	a.outputUsed = outputProcessBytes - outputClosingBytes
	const last = 1 << 40
	if _, ok := a.emitOutput(transcript.Output{Task: task, Line: last - 3, Lines: []string{"e", "f", "g"}}); ok {
		t.Fatal("a chunk past the budget was printed")
	}
	cut := transcript.Output{V: transcript.OutputVersion, Task: task, Line: last, Truncated: true}
	if got := chunks(out.String()); !reflect.DeepEqual(got, []transcript.Output{cut}) {
		t.Errorf("chunks = %+v, want %+v: the cut past the three lines, not done", got, cut)
	}
	if _, ok := a.emitOutput(transcript.Output{Task: task, Line: last}); ok {
		t.Error("a chunk was printed after the budget ran out")
	}
	a.endOutput("bother", at) // another command's end
	a.endOutput(task, at)
	a.endOutput(task, at) // a second end
	end := transcript.Output{V: transcript.OutputVersion, Task: task, Line: last, Done: true, Truncated: true,
		At: "2026-10-06T12:00:00Z"}
	if got := chunks(out.String()); !reflect.DeepEqual(got, []transcript.Output{cut, end}) {
		t.Errorf("chunks = %+v, want the cut, then once its command's end: %+v", got, end)
	}
	if n := len(out.String()); n > outputClosingBytes {
		t.Errorf("the closing chunks are %d bytes, over the %d kept back for them", n, outputClosingBytes)
	}

	// A chunk the budget runs out on that is its command's last stands for
	// it: the cut is Done, and nothing is owed.
	out = &syncBuffer{}
	a = New(Config{Out: out, Log: slog.New(slog.DiscardHandler)}, nil)
	a.outputUsed = outputProcessBytes - outputClosingBytes
	a.emitOutput(transcript.Output{Task: "b1", Line: 5, Lines: []string{"e"}, Done: true})
	a.endOutput("b1", at)
	want := []transcript.Output{{V: transcript.OutputVersion, Task: "b1", Line: 6, Done: true, Truncated: true}}
	if got := chunks(out.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %+v, want only %+v", got, want)
	}
}

// TestOutputBudgetRunsOutWhileACommandRuns: a command the process's budget
// runs out on while it runs is shown truncated, not finished, until it ends;
// its end is then shown, the one chunk printed past the budget, before the
// stage's result. A command started meanwhile is not followed.
func TestOutputBudgetRunsOutWhileACommandRuns(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	baseline := runtime.NumGoroutine()
	truncated := func(got []transcript.Output) bool {
		return slices.ContainsFunc(got, func(o transcript.Output) bool { return o.Truncated })
	}
	var running []transcript.Output
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		f, _ := commandFile(t, tmp, "bspent")
		feed(startedLine("bspent", "toolu_spent"))
		put(t, f, strings.Repeat(numbered(0)+"\n", 100))
		waitFor(t, out, "the budget to run out", truncated)
		g, _ := commandFile(t, tmp, "blate")
		put(t, g, "never shown\n")
		feed(startedLine("blate", "toolu_late"))
		time.Sleep(10 * fastPace.poll)
		running = chunks(out.String())
		feed(notifiedLine("blate", "toolu_late"))
		feed(notifiedLine("bspent", "toolu_spent"))
		waitFor(t, out, "the command's end", func(got []transcript.Output) bool {
			return slices.ContainsFunc(got, func(o transcript.Output) bool { return o.Done })
		})
	}}}}
	a := New(cfg, exec)
	a.pace = fastPace
	const used = outputProcessBytes - outputClosingBytes - 4000 // a chunk or so left
	a.outputUsed = used
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, o := range running {
		if o.Done {
			t.Errorf("chunk %+v is done while its command still ran", o)
		}
	}
	got := chunks(out.String())
	at := budgetClose(t, got)
	if at == 0 || got[at].Done {
		t.Errorf("the cut %+v is chunk %d; want it not done, after some lines", got[at], at)
	}
	for _, o := range got {
		if o.Task != "bspent" {
			t.Errorf("chunk %+v; nothing is followed once the budget is spent", o)
		}
	}
	for _, o := range got[:at] {
		if o.Done {
			t.Errorf("chunk %+v before the cut is done", o)
		}
	}
	if printed := printedBytes(got); used+printed > outputProcessBytes {
		t.Errorf("command output = %d bytes, over the process's %d", used+printed, outputProcessBytes)
	}
	text := out.String()
	if strings.LastIndex(text, transcript.OutputPrefix) > strings.Index(text, envelope.Prefix) {
		t.Error("a chunk follows the stage's result")
	}
	settled(t, baseline)
}

// TestOutputIsQuietWithoutAFile: a command whose file never appears — a
// fixture's, replayed by the fake harness, or one over before the first
// look — and one the CLI backgrounds print nothing, whether the command's
// end is announced or the run just ends.
func TestOutputIsQuietWithoutAFile(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	baseline := runtime.NumGoroutine()
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		feed(startedLine("bnever", "toolu_1"))
		time.Sleep(5 * fastPace.poll)
		feed(notifiedLine("bnever", "toolu_1"))
		f, _ := commandFile(t, tmp, "bbg")
		put(t, f, "a backgrounded server's log\n")
		feed(backgroundedLine("bbg", "toolu_2"))
		feed(startedLine("bunended", "toolu_3"))
		time.Sleep(5 * fastPace.poll)
	}}}}
	runStage(t, cfg, exec, fastPace, out)
	if got := chunks(out.String()); len(got) != 0 {
		t.Errorf("chunks = %+v, want none", got)
	}
	if exec.slowest > 100*time.Millisecond {
		t.Errorf("a stream line held the observer %s", exec.slowest)
	}
	settled(t, baseline)
}

// TestOutputEnds: a command is closed by its tool result when its end is
// never announced, and by the run's end when neither comes; a file that
// appears after the command started is found.
func TestOutputEnds(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		feed(startedLine("banswered", "toolu_a"))
		time.Sleep(4 * fastPace.poll)
		f, _ := commandFile(t, tmp, "banswered")
		put(t, f, "one\ntwo\n")
		waitFor(t, out, "the late file's lines", func(got []transcript.Output) bool { return len(got) > 0 })
		feed(answeredLine("toolu_a", "one two"))
		waitFor(t, out, "the answered command to end", func(got []transcript.Output) bool {
			return slices.ContainsFunc(got, func(o transcript.Output) bool { return o.Task == "banswered" && o.Done })
		})

		g, _ := commandFile(t, tmp, "bcut")
		feed(startedLine("bcut", "toolu_b"))
		put(t, g, "three\nfour")
	}}}}
	runStage(t, cfg, exec, fastPace, out)

	byTask := map[string][]transcript.Output{}
	for _, o := range chunks(out.String()) {
		byTask[o.Task] = append(byTask[o.Task], o)
	}
	if lines := joined(t, byTask["banswered"]); !slices.Equal(lines, []string{"one", "two"}) {
		t.Errorf("answered command lines = %q", lines)
	}
	if lines := joined(t, byTask["bcut"]); !slices.Equal(lines, []string{"three", "four"}) {
		t.Errorf("the run's last command lines = %q, want its output read out when the run ended", lines)
	}
	text := out.String()
	if strings.LastIndex(text, transcript.OutputPrefix) > strings.Index(text, envelope.Prefix) {
		t.Error("a chunk follows the stage's result")
	}
}

// byTask splits chunks by command, and checks no command's chunks are
// interleaved with another's: each command's are one run of stdout.
func byTask(t *testing.T, got []transcript.Output) map[string][]transcript.Output {
	t.Helper()
	tasks := map[string][]transcript.Output{}
	last := ""
	for _, o := range got {
		if o.Task != last && tasks[o.Task] != nil {
			t.Errorf("a chunk of %s follows one of %s after its own: the commands' chunks interleave", o.Task, last)
		}
		tasks[o.Task] = append(tasks[o.Task], o)
		last = o.Task
	}
	return tasks
}

// hasLine reports a chunk of task showing text.
func hasLine(task, text string) func([]transcript.Output) bool {
	return func(got []transcript.Output) bool {
		return slices.ContainsFunc(got, func(o transcript.Output) bool {
			return o.Task == task && slices.Contains(o.Lines, text)
		})
	}
}

// TestOutputFollowsOneCommandAtATime: a command that starts while another
// is followed waits its turn, so a viewer, who sees one command at a time,
// never sees two interleave. Once the first ends, the second is followed
// from its first line, those printed while it waited included.
func TestOutputFollowsOneCommandAtATime(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		first, firstPath := commandFile(t, tmp, "bfirst")
		feed(startedLine("bfirst", "toolu_a"))
		put(t, first, "a1\na2\n")
		waitFor(t, out, "the first command's lines", hasLine("bfirst", "a2"))
		second, secondPath := commandFile(t, tmp, "bsecond")
		feed(startedLine("bsecond", "toolu_b"))
		put(t, second, "b1\nb2\n")
		put(t, first, "a3\n")
		time.Sleep(10 * fastPace.poll) // were the second followed, its lines would print now
		put(t, second, "b3\n")
		_ = os.Remove(firstPath)
		feed(notifiedLine("bfirst", "toolu_a"))
		feed(answeredLine("toolu_a", "a"))
		waitFor(t, out, "the second command's lines", hasLine("bsecond", "b3"))
		put(t, second, "b4")
		_ = os.Remove(secondPath)
		feed(notifiedLine("bsecond", "toolu_b"))
	}}}}
	runStage(t, cfg, exec, fastPace, out)

	tasks := byTask(t, chunks(out.String()))
	if lines := joined(t, tasks["bfirst"]); !slices.Equal(lines, []string{"a1", "a2", "a3"}) {
		t.Errorf("first command lines = %q", lines)
	}
	if lines := joined(t, tasks["bsecond"]); !slices.Equal(lines, []string{"b1", "b2", "b3", "b4"}) {
		t.Errorf("second command lines = %q, want every line from its first", lines)
	}
}

// TestOutputSkipsAQueuedCommandThatEnded: a command that starts and ends
// while another is followed is never shown; the next one still waiting is
// followed once the first ends.
func TestOutputSkipsAQueuedCommandThatEnded(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	exec := &commandExec{ws: ws, runs: []commandRun{{writes: reported, script: func(feed func(string)) {
		first, firstPath := commandFile(t, tmp, "bfirst")
		feed(startedLine("bfirst", "toolu_a"))
		put(t, first, "a1\n")
		waitFor(t, out, "the first command's line", hasLine("bfirst", "a1"))
		brief, briefPath := commandFile(t, tmp, "bbrief")
		feed(startedLine("bbrief", "toolu_b"))
		put(t, brief, "brief\n")
		time.Sleep(10 * fastPace.poll)
		_ = os.Remove(briefPath)
		feed(notifiedLine("bbrief", "toolu_b"))
		third, thirdPath := commandFile(t, tmp, "bthird")
		feed(startedLine("bthird", "toolu_c"))
		put(t, third, "c1\n")
		_ = os.Remove(firstPath)
		feed(notifiedLine("bfirst", "toolu_a"))
		waitFor(t, out, "the third command's line", hasLine("bthird", "c1"))
		_ = os.Remove(thirdPath)
		feed(notifiedLine("bthird", "toolu_c"))
	}}}}
	runStage(t, cfg, exec, fastPace, out)

	tasks := byTask(t, chunks(out.String()))
	if got := tasks["bbrief"]; got != nil {
		t.Errorf("the command that ended while it waited was shown: %+v", got)
	}
	if lines := joined(t, tasks["bthird"]); !slices.Equal(lines, []string{"c1"}) {
		t.Errorf("third command lines = %q", lines)
	}
}

// TestOutputQueueIsBounded: a stream that starts command after command
// while one is followed queues at most outputQueued of them, in start order;
// the rest are never followed, and the run's end drops the queue.
func TestOutputQueueIsBounded(t *testing.T) {
	cfg, ws, _, _ := outputSetup(t)
	a := New(cfg, &commandExec{ws: ws})
	a.pace = fastPace
	f := a.followOutput(context.Background(), harness.NewFake(), runner.CommandSpec{}, nil)
	f.scan([]byte(initLine()))
	for i := range outputQueued + 3 {
		f.scan([]byte(startedLine(fmt.Sprintf("bq%d", i), fmt.Sprintf("toolu_q%d", i))))
	}
	f.mu.Lock()
	current, queue := f.current.id, make([]string, 0, len(f.queue))
	for _, q := range f.queue {
		queue = append(queue, q.id)
	}
	f.mu.Unlock()
	want := make([]string, 0, outputQueued)
	for i := 1; i <= outputQueued; i++ {
		want = append(want, fmt.Sprintf("bq%d", i))
	}
	if current != "bq0" || !slices.Equal(queue, want) {
		t.Errorf("followed %s, queued %q; want bq0 followed and %q queued", current, queue, want)
	}
	f.end()
	if f.current != nil || len(f.queue) != 0 || len(f.known) != 0 {
		t.Errorf("after the run: followed %v, queued %d, known %d; want none", f.current, len(f.queue), len(f.known))
	}
}

// TestOutputScanIsCheapWhenIdle: the observer hands the follower every line
// of the CLI's stream on the runner's reading goroutine. A tool result can
// only end a command followed or queued, so with none it is passed over
// without a decode, as is every line that carries no command event.
func TestOutputScanIsCheapWhenIdle(t *testing.T) {
	cfg, ws, _, _ := outputSetup(t)
	a := New(cfg, &commandExec{ws: ws})
	f := a.followOutput(context.Background(), harness.NewFake(), runner.CommandSpec{}, nil)
	f.scan([]byte(initLine()))
	for _, line := range []string{
		answeredLine("toolu_1", "ok  ./..."),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading."}]}}`,
		`{"type":"system","subtype":"api_retry","attempt":1}`,
		resultLine,
	} {
		b := []byte(line)
		if allocs := testing.AllocsPerRun(50, func() { f.scan(b) }); allocs != 0 {
			t.Errorf("scan(%.50s) with nothing followed allocates %.0f times a line, want none: no decode",
				line, allocs)
		}
	}
	f.end()
}

// TestOutputEndWaitsForTheLastChunk: once a run's end has returned, every
// command it followed has printed its last chunk and nothing more is
// printed, so the stage's result is the last line.
func TestOutputEndWaitsForTheLastChunk(t *testing.T) {
	cfg, ws, tmp, out := outputSetup(t)
	a := New(cfg, &commandExec{ws: ws})
	a.pace = fastPace
	f := a.followOutput(context.Background(), harness.NewFake(), runner.CommandSpec{}, nil)
	file, _ := commandFile(t, tmp, "bslow")
	f.scan([]byte(initLine()))
	f.scan([]byte(startedLine("bslow", "toolu_slow")))
	// Enough unread output that reading it out takes a while.
	put(t, file, strings.Repeat(strings.Repeat("z", 200)+"\n", 50000))
	f.end()
	ended := out.String()
	got := chunks(ended)
	if len(got) == 0 || !got[len(got)-1].Done {
		t.Fatalf("chunks when end returned = %d, the last done %v; want the command closed by then", len(got),
			len(got) > 0 && got[len(got)-1].Done)
	}
	time.Sleep(10 * fastPace.poll)
	if out.String() != ended {
		t.Error("stdout grew after end returned")
	}
	f.scan([]byte(startedLine("blate", "toolu_late")))
	if f.current != nil || len(f.known) != 0 {
		t.Errorf("tasks followed after the run = %v and %d known, want none", f.current, len(f.known))
	}
}

// TestOutputScrubsTheRepairsToken: a repair run reads the caller token
// afresh, and what its commands print is scrubbed of the token that run
// read, as well as the first run's.
func TestOutputScrubsTheRepairsToken(t *testing.T) {
	const first, rotated = "caller-token-first-0001", "caller-token-rotated-0002"
	cfg, ws, tmp, out := outputSetup(t)
	cfg.InvestigateTimeout = 10 * time.Minute // room for a repair
	cfg.BrokerTokenFile = filepath.Join(ws, "broker-token")
	if err := os.WriteFile(cfg.BrokerTokenFile, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &commandExec{ws: ws, runs: []commandRun{
		{script: func(func(string)) {}, writes: map[string]string{
			"reports/plan.md": strings.Replace(goodPlan, "confidence: 0.8", "confidence: 7", 1),
			"broker-token":    rotated + "\n",
		}},
		{writes: reported, script: func(feed func(string)) {
			f, _ := commandFile(t, tmp, "benv")
			feed(startedLine("benv", "toolu_env"))
			put(t, f, "x-patchy-broker-token: "+rotated+"\nearlier: "+first+"\n")
			feed(notifiedLine("benv", "toolu_env"))
		}},
	}}
	if st := stageOf(t, runStage(t, cfg, exec, fastPace, out)); st.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %s (%s), want the repaired report", st.Outcome, st.Detail)
	}
	lines := joined(t, chunks(out.String()))
	want := []string{"x-patchy-broker-token: " + transcript.Redacted, "earlier: " + transcript.Redacted}
	if !slices.Equal(lines, want) {
		t.Errorf("repair command lines = %q, want %q", lines, want)
	}
	if strings.Contains(out.String(), rotated) || strings.Contains(out.String(), first) {
		t.Error("stdout carries a caller token")
	}
}

// TestOutputOnlyInIntentStages: the live output has one reader, an intent
// run's panel. A Finding's stages, whose transcript view never shows it,
// print none, whatever their commands print, so it adds nothing to their
// pod log.
func TestOutputOnlyInIntentStages(t *testing.T) {
	for _, phase := range []Phase{PhaseInvestigate, PhaseRemediate} {
		t.Run(string(phase), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("CLAUDE_CODE_TMPDIR", tmp)
			out := &syncBuffer{}
			ws := newWorkspace(t)
			cfg := newConfig(t, ws, out)
			writes := map[string]string{"reports/investigation.md": goodInvestigation}
			if phase == PhaseRemediate {
				cfg, ws = remediateConfig(t, goodInvestigation, out)
				writes = map[string]string{"reports/remediation.md": goodRemediation}
			}
			exec := &commandExec{ws: ws, runs: []commandRun{{writes: writes, script: func(feed func(string)) {
				f, _ := commandFile(t, tmp, "bfinding")
				feed(startedLine("bfinding", "toolu_f"))
				put(t, f, "ok  ./...\n")
				time.Sleep(10 * fastPace.poll) // time enough for a read
				feed(notifiedLine("bfinding", "toolu_f"))
			}}}}
			runStage(t, cfg, exec, fastPace, out)
			if strings.Contains(out.String(), transcript.OutputPrefix) {
				t.Errorf("the %s stage printed command output:\n%s", phase, out.String())
			}
		})
	}
}

// streamed reads content as a command's whole file, in one pass and then
// its close, through the stream of a follower that scrubs secrets, and
// returns the chunks it prints.
func streamed(t *testing.T, secrets []string, content string) []transcript.Output {
	t.Helper()
	out := &syncBuffer{}
	a := New(Config{Out: out, Log: slog.New(slog.DiscardHandler)}, nil)
	f := &outputFollower{a: a, secrets: secrets, pace: fastPace}
	path := filepath.Join(t.TempDir(), "bsecret.output")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	s := f.newStream("bsecret")
	if s.read(file, time.Now()) {
		s.close(file)
	}
	return chunks(out.String())
}

// leaked is the first piece of secret, minSecretPiece bytes long, that a
// chunk shows, or "".
func leaked(got []transcript.Output, secret string) string {
	const minSecretPiece = 8
	for _, o := range got {
		for _, l := range o.Lines {
			for i := 0; i+minSecretPiece <= len(secret); i++ {
				if strings.Contains(l, secret[i:i+minSecretPiece]) {
					return secret[i : i+minSecretPiece]
				}
			}
		}
	}
	return ""
}

// secretOf is n bytes of a secret's alphabet, which no filler, placeholder
// or marker a test prints shares.
func secretOf(r *rand.Rand, n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

// TestOutputNeverShowsAnOverlongLine: a line longer than the splitter holds
// is cut before its escapes are stripped and its secrets scrubbed, so a
// secret straddling the cut would survive in part. Such a line is never
// shown from what was held of it: a fixed placeholder stands in for it,
// under its own number, at full fidelity and in a sample alike.
func TestOutputNeverShowsAnOverlongLine(t *testing.T) {
	r := rand.New(rand.NewSource(20261006))
	secret := secretOf(r, 1200)
	repeated := secretOf(r, 1000)
	filler := strings.Repeat(numbered(0)+"\n", 1000) // past a command's full-fidelity budget
	tests := []struct {
		name, secret, line string
		sampled            bool
	}{
		// The reviewer's case: escapes that strip to nothing push the
		// secret across the cut.
		{"escapes before a secret", secret, strings.Repeat("\x1b[0m", 1800) + secret, false},
		{"escapes before a secret, sampled", secret, strings.Repeat("\x1b[0m", 1800) + secret, true},
		// Every whole token is scrubbed; the one the cut splits is not.
		{"a repeated token", repeated, strings.Repeat("T="+repeated+" ", 9), false},
		{"a repeated token, sampled", repeated, strings.Repeat("T="+repeated+" ", 9), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.line) <= outputHeldBytes {
				t.Fatalf("the line is %d bytes, not past the %d held", len(tt.line), outputHeldBytes)
			}
			content := "before\n" + tt.line + "\nafter\n"
			if tt.sampled {
				content = filler + content
			}
			got := streamed(t, []string{tt.secret}, content)
			if piece := leaked(got, tt.secret); piece != "" {
				t.Fatalf("a chunk shows %q of the secret", piece)
			}
			at := strings.Count(content, "\n") - 1 // the overlong line's number
			shown := false
			for _, o := range got {
				for k, l := range o.Lines {
					if o.Line+k == at {
						shown = true
						if l != outputOverlong {
							t.Errorf("line %d = %.40q, want the placeholder %q", at, l, outputOverlong)
						}
					}
				}
			}
			if !shown {
				t.Errorf("line %d is not shown at all; want the placeholder under its number: %+v", at, got)
			}
		})
	}
	if !strings.Contains(outputOverlong, strconv.Itoa(outputHeldBytes>>10)+" KiB") {
		t.Errorf("the placeholder %q does not name the %d KiB a line may hold", outputOverlong, outputHeldBytes>>10)
	}
}

// TestOutputSecretsAroundTheCapProperty: whatever a line around the held
// cap holds before and after a secret, and wherever the secret sits in it,
// no chunk shows any piece of the secret 8 bytes long.
func TestOutputSecretsAroundTheCapProperty(t *testing.T) {
	fillers := []string{"a", "z", " ", "é", "-", "\x1b[0m", "\x1b[1;31m"}
	property := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		secret := secretOf(r, 8+r.Intn(1200))
		escapes := r.Float64() // how much of the filler is escape sequences
		fill := func(n int) string {
			var b strings.Builder
			for b.Len() < n {
				if r.Float64() < escapes {
					b.WriteString(fillers[5+r.Intn(2)])
				} else {
					b.WriteString(fillers[r.Intn(5)])
				}
			}
			return b.String()
		}
		total := outputHeldBytes - 2048 + r.Intn(4096) // the line's length, around the cap
		at := r.Intn(total)
		line := fill(at) + secret + fill(total-at-len(secret))
		if r.Intn(2) == 0 {
			line += "\n" // or the output's last, unended line
		}
		if r.Intn(3) == 0 {
			line = strings.Repeat(numbered(0)+"\n", 1000) + line // sampled
		}
		if piece := leaked(streamed(t, []string{secret}, line), secret); piece != "" {
			t.Logf("seed %d: a chunk shows %q of a %d-byte secret at %d in a %d-byte line", seed, piece,
				len(secret), at, len(line))
			return false
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261007))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}

// splitAll runs a splitter over the pieces and returns every line, the
// unended last one included.
func splitAll(pieces ...string) []string {
	lines, _ := split(pieces...)
	return lines
}

// split is splitAll, and which of the lines the splitter cut.
func split(pieces ...string) (lines []string, cut []bool) {
	var s lineSplitter
	for _, p := range pieces {
		s.write([]byte(p), func(l []byte, over bool) {
			lines, cut = append(lines, string(l)), append(cut, over)
		})
	}
	if rest, over, ok := s.rest(); ok {
		lines, cut = append(lines, string(rest)), append(cut, over)
	}
	return lines, cut
}

func TestLineSplitter(t *testing.T) {
	tests := []struct {
		name   string
		pieces []string
		want   []string
	}{
		{"lines", []string{"a\nb\n"}, []string{"a", "b"}},
		{"blank lines count", []string{"a\n\n\nb\n"}, []string{"a", "", "", "b"}},
		{"an unended last line", []string{"a\nb"}, []string{"a", "b"}},
		{"a progress bar redraws its line", []string{"10%\r50%\r100%\n"}, []string{"100%"}},
		{"crlf ends a line", []string{"one\r\ntwo\r\n"}, []string{"one", "two"}},
		{"crlf split across reads", []string{"one\r", "\ntwo\n"}, []string{"one", "two"}},
		{"a redraw split across reads", []string{"10%\r", "50%\n"}, []string{"50%"}},
		{"a line split across reads", []string{"par", "tial\n"}, []string{"partial"}},
		{"a bar left mid-redraw", []string{"10%\r20%\r"}, []string{"20%"}},
		{"only carriage returns", []string{"\r\r\n"}, []string{""}},
		{"nothing", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitAll(tt.pieces...); !slices.Equal(got, tt.want) {
				t.Errorf("lines = %q, want %q", got, tt.want)
			}
		})
	}

	long := strings.Repeat("a", 3*outputHeldBytes)
	if got, cut := split(long[:outputHeldBytes-1], long, "\nok\n"); len(got) != 2 ||
		len(got[0]) != outputHeldBytes || got[1] != "ok" || !slices.Equal(cut, []bool{true, false}) {
		t.Errorf("an overlong line = %d lines, the first %d bytes, cut %v; want it held to %d and marked cut, "+
			"then the next line", len(got), len(got[0]), cut, outputHeldBytes)
	}
	if got, cut := split(long[:outputHeldBytes], "\n", long[:outputHeldBytes+1]); len(got) != 2 ||
		!slices.Equal(cut, []bool{false, true}) {
		t.Errorf("a line of exactly %d bytes, then an unended one over it: cut %v, want [false true]",
			outputHeldBytes, cut)
	}
	if got, cut := split(long, "\rshort\n"); !slices.Equal(got, []string{"short"}) || cut[0] {
		t.Errorf("an overlong line redrawn = %q, cut %v; want the redraw, whole", got, cut)
	}
}

// TestLineSplitterProperty: however the output is cut into reads, its lines
// are the same, and each is what a terminal shows of its line: the text
// after its last carriage return, the ones ending it aside.
func TestLineSplitterProperty(t *testing.T) {
	alphabet := []string{"a", "é", "漢", " ", "\r", "\n", "\r\n", "\x1b[1m", "%"}
	property := func(seed int64, n uint8) bool {
		r := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for range int(n) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		in := b.String()
		var pieces []string
		for rest := in; rest != ""; {
			k := 1 + r.Intn(len(rest))
			pieces, rest = append(pieces, rest[:k]), rest[k:]
		}
		var want []string
		segments := strings.Split(in, "\n")
		for i, seg := range segments {
			seg = strings.TrimRight(seg, "\r")
			seg = seg[strings.LastIndex(seg, "\r")+1:]
			if i < len(segments)-1 || seg != "" {
				want = append(want, seg)
			}
		}
		whole, cut := splitAll(in), splitAll(pieces...)
		return slices.Equal(whole, want) && slices.Equal(cut, want)
	}
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(20261006))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}

// TestSkimProperty: however a sampled command's output is cut into reads,
// skimming numbers its lines exactly and keeps the newest of them, and the
// line it left unended, as the splitter reading every byte sees them.
func TestSkimProperty(t *testing.T) {
	alphabet := []string{"a", "é", " ", "\r", "\n", "\n", "\r\n", "\x1b[1m"}
	raw := func(b []byte, _ bool) string { return string(b) }
	property := func(seed int64, n uint16) bool {
		r := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for range int(n) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		in := b.String()
		s := (&outputFollower{}).newStream("b")
		s.sampling = true
		for rest := in; rest != ""; {
			k := 1 + r.Intn(len(rest))
			s.skim([]byte(rest[:k]))
			rest = rest[k:]
		}

		all, ended := splitAll(in), strings.Count(in, "\n")
		newest := all[max(0, ended-outputSampleLines):ended]
		first, kept := s.newest.since(0, raw)
		if s.next != ended+1 || !slices.Equal(kept, newest) || (len(kept) > 0 && first != ended-len(kept)+1) {
			t.Logf("seed %d: next %d, kept %q from %d; want next %d, %q", seed, s.next, kept, first, ended+1, newest)
			return false
		}
		held, _, ok := s.split.rest()
		if want := all[ended:]; ok != (len(want) == 1) || (ok && string(held) != want[0]) {
			t.Logf("seed %d: unended line %q (%v), want %q", seed, held, ok, want)
			return false
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(20261008))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}

func TestOutputClean(t *testing.T) {
	f := &outputFollower{secrets: []string{"SECRETVALUE1"}}
	tests := []struct {
		name, raw, want string
		over            bool
	}{
		{"escapes", "\x1b[1;32mok\x1b[0m done", "ok done", false},
		{"invalid utf-8", "bad \xff\xfe bytes", "bad \uFFFD bytes", false},
		{"a secret", "token=SECRETVALUE1;", "token=" + transcript.Redacted + ";", false},
		{"at the cap", strings.Repeat("a", outputLineBytes), strings.Repeat("a", outputLineBytes), false},
		{"over the cap", strings.Repeat("a", 2000), strings.Repeat("a", outputLineBytes-3) + "…", false},
		{"over the cap, mid-rune", strings.Repeat("漢", 400), strings.Repeat("漢", 340) + "…", false},
		{"a line the splitter cut", "token=SECRETVAL", outputOverlong, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.clean([]byte(tt.raw), tt.over)
			if got != tt.want {
				t.Errorf("clean = %.60q (%d bytes), want %.60q", got, len(got), tt.want)
			}
			if len(got) > outputLineBytes || !utf8.ValidString(got) {
				t.Errorf("clean = %d bytes, valid %v; want at most %d, valid", len(got), utf8.ValidString(got),
					outputLineBytes)
			}
		})
	}
}
