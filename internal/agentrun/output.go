// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/ansi"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// Live command output. While the agent's CLI runs a foreground shell
// command in an intent's stage, agent-runner reads the file the CLI writes
// that command's output to and prints it as transcript.Output chunks
// (PATCHY-OUTPUT lines), for a viewer following the pod log to watch a long
// command as it runs. The chunks are live only: the controller persists none
// of them, they never pass through the transcript recorder, and they are not
// progress to the idle watchdog or spend to the token budget, which read the
// CLI's own stream alone.
//
// The bounds are constants, not configuration: a new PATCHY_* key would
// change the environment a repository-image Job blanks. They keep the
// output small beside what else the log must hold (a build's result line
// alone can be several MiB, and the kubelet rotates a container's log at
// about 10 MiB).
const (
	// outputLineBytes caps one line as shown.
	outputLineBytes = 1024
	// outputHeldBytes bounds the raw bytes held of a line not yet ended.
	outputHeldBytes = 8 << 10
	// outputOverlong stands in for a line longer than outputHeldBytes. The
	// bytes held of it were cut before its escapes were stripped and its
	// secrets scrubbed, so a secret straddling the cut would survive in
	// part: none of it is shown.
	outputOverlong = "[a line of more than 8 KiB, not shown]"
	// outputChunkLines and outputChunkBytes (of line text) print a chunk
	// before its flush interval is up.
	outputChunkLines = 32
	outputChunkBytes = 8 << 10
	// outputTaskBytes is what one command may print at full fidelity,
	// counted as the chunk lines it put on stdout. Past it the command is
	// sampled: its newest lines, every sample interval.
	outputTaskBytes = 256 << 10
	// outputSampleLines is how many of a command's newest lines a sample
	// shows.
	outputSampleLines = 10
	// outputProcessBytes bounds every chunk line one agent-runner prints,
	// across all its commands, framing included; outputClosingBytes of it is
	// kept back for the chunk that closes the command the budget ran out on.
	outputProcessBytes = 1 << 20
	outputClosingBytes = 512
	// outputMaxTasks bounds the commands followed at once. The CLI runs its
	// foreground commands one at a time; this only bounds a stream that
	// says otherwise.
	outputMaxTasks = 4
	// outputReadBytes bounds what one read pass takes from a command's file,
	// so a command that floods it costs each pass a bounded amount.
	outputReadBytes = 4 << 20
	// outputEllipsis marks a line cut at outputLineBytes.
	outputEllipsis = "…"
)

// outputPace is the live output's timing.
type outputPace struct {
	poll   time.Duration // how often a command's file is looked for, then read
	flush  time.Duration // the longest a line read waits to be printed
	sample time.Duration // how often a sampled command shows its newest lines
	drain  time.Duration // the longest a command's end waits for its file to be read out
}

// defaultOutputPace is the production timing, which New gives every Agent;
// tests shorten it.
var defaultOutputPace = outputPace{
	poll:   250 * time.Millisecond,
	flush:  500 * time.Millisecond,
	sample: 5 * time.Second,
	drain:  2 * time.Second,
}

// outputFollower follows the commands one run of a stage's CLI runs and
// prints their output live. The runner's line observer hands it every
// stream line (scan), which only starts and signals goroutines and never
// waits; each command followed gets one goroutine, bound to the run's
// context, that reads the command's file. end, once the run is over, ends
// every command still followed and waits for each to print its last chunk,
// so no goroutine outlives its run and no chunk is printed after the
// stage's result.
type outputFollower struct {
	a       *Agent
	tw      harness.TaskWatcher
	ctx     context.Context
	env     []string // the CLI's effective environment
	uid     int
	secrets []string // the run's scrub list, current when the run started
	pace    outputPace
	wg      sync.WaitGroup

	mu      sync.Mutex
	session string // the run's session, off its init event
	tasks   map[string]*followedTask
	over    bool // the run is over: no command is followed from now on
}

// followedTask is one command being followed.
type followedTask struct {
	id, toolUse, pattern string
	ended                chan struct{} // closed when the command or the run ends
	once                 sync.Once
}

// end marks the command ended; a second call is a no-op.
func (t *followedTask) end() { t.once.Do(func() { close(t.ended) }) }

// followOutput sets up the live output of one run whose command is spec,
// or returns nil when the stage shows none or the harness cannot follow
// commands. Only an intent's stages show it: an intent run's panel is the
// output's one reader, and a Finding's stages, whose transcript view never
// shows it, would only add it to their pod log. secrets is the run's scrub
// list: the credentials, and the caller token read afresh for this run.
func (a *Agent) followOutput(ctx context.Context, h harness.Harness, spec runner.CommandSpec,
	secrets []string) *outputFollower {
	if a.cfg.Phase != PhasePlan && a.cfg.Phase != PhaseBuild {
		return nil
	}
	tw, ok := h.(harness.TaskWatcher)
	if !ok {
		return nil
	}
	return &outputFollower{
		a: a, tw: tw, ctx: ctx, pace: a.pace, secrets: secrets, uid: os.Getuid(),
		// The runner runs the CLI with its own environment and the spec's
		// after it, so a later assignment wins, as it does here.
		env:   append(os.Environ(), spec.Env...),
		tasks: map[string]*followedTask{},
	}
}

// scan applies one stream line's command events. It runs on the runner's
// reading goroutine for every line of the CLI's stream, so it never waits on
// a command: it starts and signals goroutines, under locks only ever held
// for a map update or one stdout line.
func (f *outputFollower) scan(line []byte) {
	if f == nil {
		return
	}
	for _, ev := range f.tw.ScanTasks(line) {
		switch ev.Kind {
		case harness.TaskSession:
			f.mu.Lock()
			f.session = ev.Session
			f.mu.Unlock()
		case harness.TaskStarted:
			f.start(ev)
		case harness.TaskEnded:
			f.endWhere(func(t *followedTask) bool { return t.id == ev.Task })
		case harness.TaskAnswered:
			f.endWhere(func(t *followedTask) bool { return t.toolUse != "" && t.toolUse == ev.ToolUse })
		}
	}
}

// start follows a command that started, unless the run is over, it is
// already followed, outputMaxTasks are, the process's output budget is
// spent, or there is no file to look for.
func (f *outputFollower) start(ev harness.TaskEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.over || f.tasks[ev.Task] != nil || len(f.tasks) >= outputMaxTasks || f.a.outputSpent() {
		return
	}
	session := ev.Session
	if session == "" {
		session = f.session
	}
	pattern := f.tw.TaskOutputGlob(f.env, f.uid, session, ev.Task)
	if pattern == "" {
		return
	}
	t := &followedTask{id: ev.Task, toolUse: ev.ToolUse, pattern: pattern, ended: make(chan struct{})}
	f.tasks[t.id] = t
	f.wg.Add(1)
	go f.follow(t)
}

// endWhere ends every command followed that match picks.
func (f *outputFollower) endWhere(match func(*followedTask) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		if match(t) {
			t.end()
		}
	}
}

// end is called once the run is over: it ends every command still
// followed, as the CLI that ran them is gone, and waits for each to print
// its last chunk. That wait is bounded by the drain time.
func (f *outputFollower) end() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.over = true
	for _, t := range f.tasks {
		t.end()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// forget drops a command whose goroutine is done.
func (f *outputFollower) forget(t *followedTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tasks[t.id] == t {
		delete(f.tasks, t.id)
	}
}

// follow is one command's goroutine. It waits for the command's file, then
// reads what is appended to it every poll until the command or the run ends,
// when it reads the rest and prints the last chunk. A command whose file is
// never seen — a fixture's, or one over before the first look — prints
// nothing at all.
func (f *outputFollower) follow(t *followedTask) {
	defer f.wg.Done()
	defer f.forget(t)
	file := f.await(t)
	if file == nil {
		return
	}
	defer func() { _ = file.Close() }()

	s := f.newStream(t.id)
	tick := time.NewTicker(f.pace.poll)
	defer tick.Stop()
	if !s.read(file, time.Now()) {
		return
	}
	for {
		select {
		case <-t.ended:
			s.close(file)
			return
		case <-f.ctx.Done():
			s.close(file)
			return
		case now := <-tick.C:
			if !s.read(file, now) {
				return
			}
		}
	}
}

// await looks for the command's file at once and then every poll, until it
// is there or the command or the run ends; nil when it never was.
func (f *outputFollower) await(t *followedTask) *os.File {
	tick := time.NewTicker(f.pace.poll)
	defer tick.Stop()
	for {
		if file := openOutputFile(t.pattern); file != nil {
			return file
		}
		select {
		case <-t.ended:
			return nil
		case <-f.ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// openOutputFile opens the first regular file pattern matches, or returns
// nil. The CLI's working directory is matched rather than computed, so a
// second match would be some other run's or a decoy; the CLI's is
// indistinguishable from it, and what any of them holds is no more than the
// agent could print itself.
func openOutputFile(pattern string) *os.File {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	for _, m := range matches {
		if file := openRegular(m); file != nil {
			return file
		}
	}
	return nil
}

// clean is one raw line as it is shown: its terminal escapes stripped, made
// valid UTF-8, the run's secrets scrubbed out, and capped at outputLineBytes,
// a cut marked with an ellipsis. A line the splitter cut (over) is shown as
// outputOverlong alone: what was held of it was cut before it was stripped
// or scrubbed.
func (f *outputFollower) clean(raw []byte, over bool) string {
	if over {
		return outputOverlong
	}
	text := strings.ToValidUTF8(string(ansi.Strip(raw)), "�")
	text = transcript.Scrub(text, f.secrets)
	if len(text) > outputLineBytes {
		text, _ = transcript.Truncate(text, outputLineBytes-len(outputEllipsis))
		text += outputEllipsis
	}
	return text
}

// outputStream is one command's output as it is read: split into lines,
// cleaned, batched into chunks, and held to the command's budget, past which
// it is sampled. It belongs to the command's goroutine.
type outputStream struct {
	f     *outputFollower
	task  string
	buf   []byte
	split lineSplitter
	now   time.Time // the current pass's time
	next  int       // the number the next line gets, from 1
	shown int       // the number of the last line printed

	batch      []string // lines read and not yet printed, the first of them batchLine
	batchLine  int
	batchBytes int
	batchSince time.Time

	printed  int  // what the command printed at full fidelity
	sampling bool // past its budget: only samples of the newest lines
	newest   ring // the newest lines, once sampling
	sampled  time.Time
	stopped  bool // the process's budget is spent: nothing more is printed
}

// newStream is command task's output stream, from its first line.
func (f *outputFollower) newStream(task string) *outputStream {
	return &outputStream{f: f, task: task, next: 1, buf: make([]byte, 32<<10)}
}

// read takes what the command appended to its file since the last pass, up
// to outputReadBytes of it, and prints what is due by now. It reports false
// once nothing more may be printed.
func (s *outputStream) read(file *os.File, now time.Time) bool {
	s.now = now
	s.readFile(file, outputReadBytes, time.Time{})
	if s.stopped {
		return false
	}
	switch {
	case s.sampling:
		if s.now.Sub(s.sampled) >= s.f.pace.sample {
			s.sample(false)
		}
	case len(s.batch) > 0 && s.now.Sub(s.batchSince) >= s.f.pace.flush:
		s.flush(false, false)
	}
	return !s.stopped
}

// close ends the command's output: it reads the rest of the file — the CLI
// deletes it when the command ends, and the file it opened still reads to its
// end — within the drain time, then prints the last chunk, Done. An output
// not read to its end in that time is marked truncated.
func (s *outputStream) close(file *os.File) {
	if s.stopped {
		return
	}
	s.now = time.Now()
	whole := s.readFile(file, math.MaxInt, s.now.Add(s.f.pace.drain))
	if s.stopped {
		return
	}
	if raw, over, ok := s.split.rest(); ok && whole {
		s.line(raw, over) // the last line, which the output did not end
	}
	if s.stopped {
		return
	}
	if s.sampling {
		s.sample(true)
		return
	}
	s.flush(true, !whole)
}

// readFile reads up to limit bytes from the file, until deadline when it is
// set, handing each line read to line. It reports whether it reached the
// file's end.
func (s *outputStream) readFile(file *os.File, limit int, deadline time.Time) bool {
	for read := 0; read < limit && !s.stopped; {
		n, err := file.Read(s.buf)
		if n > 0 {
			read += n
			s.split.write(s.buf[:n], s.line)
		}
		if err != nil || n == 0 {
			return true
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false
		}
	}
	return false
}

// line takes one line of the command's output, over when the splitter cut
// it: into the batch at full fidelity, or into the newest lines once
// sampling.
func (s *outputStream) line(raw []byte, over bool) {
	n := s.next
	s.next++
	if s.stopped {
		return
	}
	if s.sampling {
		s.newest.keep(n, raw, over)
		return
	}
	text := s.f.clean(raw, over)
	if len(s.batch) == 0 {
		s.batchLine, s.batchSince = n, s.now
	}
	s.batch = append(s.batch, text)
	s.batchBytes += len(text)
	if len(s.batch) >= outputChunkLines || s.batchBytes >= outputChunkBytes {
		s.flush(false, false)
	}
}

// flush prints the batch as one chunk, done marking the command's last. A
// command whose chunks reach its budget is sampled from then on.
func (s *outputStream) flush(done, truncated bool) {
	if len(s.batch) == 0 && !done {
		return
	}
	o := transcript.Output{Task: s.task, Line: s.batchLine, Lines: s.batch, Done: done, Truncated: truncated}
	if len(s.batch) == 0 {
		o.Line = s.next
	}
	s.printed += s.print(o)
	s.batch, s.batchBytes = nil, 0
	if !s.sampling && s.printed >= outputTaskBytes {
		s.sampling, s.sampled = true, s.now
	}
}

// sample prints the newest lines not yet shown as one chunk, done marking
// the command's last; its line number leaves a gap after the last chunk, so
// a reader sees how much was left out.
func (s *outputStream) sample(done bool) {
	first, lines := s.newest.since(s.shown, s.f.clean)
	if len(lines) == 0 && !done {
		return
	}
	if len(lines) == 0 {
		first = s.next
	}
	s.print(transcript.Output{Task: s.task, Line: first, Lines: lines, Done: done, Truncated: true})
	s.sampled = s.now
}

// print stamps and prints one chunk and returns what it put on stdout,
// stopping the stream once the process's budget is spent.
func (s *outputStream) print(o transcript.Output) int {
	o.At = s.now.UTC().Format(time.RFC3339)
	n, ok := s.f.a.emitOutput(o)
	if !ok {
		s.stopped = true
		return 0
	}
	if len(o.Lines) > 0 {
		s.shown = o.Line + len(o.Lines) - 1
	}
	return n
}

// ring keeps the newest outputSampleLines lines, reusing their buffers.
type ring struct {
	lines [outputSampleLines]rawLine
	head  int // the oldest
	count int
}

// rawLine is one line kept as it was read, its number, and whether the
// splitter cut it.
type rawLine struct {
	n    int
	raw  []byte
	over bool
}

// keep adds line n, dropping the oldest when full.
func (r *ring) keep(n int, raw []byte, over bool) {
	i := (r.head + r.count) % outputSampleLines
	if r.count == outputSampleLines {
		i, r.head = r.head, (r.head+1)%outputSampleLines
	} else {
		r.count++
	}
	r.lines[i].n, r.lines[i].raw, r.lines[i].over = n, append(r.lines[i].raw[:0], raw...), over
}

// since returns the kept lines numbered after shown, cleaned, oldest first,
// and the number of the first of them. The lines kept are consecutive, so
// these are too.
func (r *ring) since(shown int, clean func([]byte, bool) string) (int, []string) {
	first := 0
	var lines []string
	for k := range r.count {
		l := r.lines[(r.head+k)%outputSampleLines]
		if l.n <= shown {
			continue
		}
		if first == 0 {
			first = l.n
		}
		lines = append(lines, clean(l.raw, l.over))
	}
	return first, lines
}

// lineSplitter splits a command's output into lines as it arrives. A line is
// what a terminal shows of it: the text after its last carriage return, as a
// progress bar redraws its line that way, except that a carriage return just
// before the newline ending a line only ends it. At most outputHeldBytes of
// a line are held; the rest of it is dropped, and the line is marked over.
type lineSplitter struct {
	held []byte
	over bool // bytes of the held line past outputHeldBytes were dropped
	cr   bool // the last byte was a carriage return, which the next decides about
}

// write splits p, handing each line it ends to line, with whether it was
// cut; the slice line is handed is valid only during the call.
func (s *lineSplitter) write(p []byte, line func(raw []byte, over bool)) {
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			s.hold(p)
			return
		}
		s.hold(p[:i])
		if p[i] == '\n' {
			line(s.held, s.over)
			s.restart()
		} else {
			s.cr = true
		}
		p = p[i+1:]
	}
}

// hold adds b to the line not yet ended, which a carriage return before it
// restarts.
func (s *lineSplitter) hold(b []byte) {
	if len(b) == 0 {
		return
	}
	if s.cr {
		s.restart()
	}
	if room := outputHeldBytes - len(s.held); len(b) > room {
		b, s.over = b[:room], true
	}
	s.held = append(s.held, b...)
}

// restart begins a line afresh.
func (s *lineSplitter) restart() {
	s.held, s.over, s.cr = s.held[:0], false, false
}

// rest is the line the output stopped in without ending, if there is one,
// and whether it was cut.
func (s *lineSplitter) rest() (raw []byte, over, ok bool) {
	return s.held, s.over, len(s.held) > 0
}
