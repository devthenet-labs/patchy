// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
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
	outputTaskBytes = 64 << 10
	// outputSampleLines and outputSampleBytes (of line text) bound one
	// sample: the newest lines that fit both.
	outputSampleLines = 10
	outputSampleBytes = 2 << 10
	// outputSampledBytes bounds what one command's samples print, counted
	// as outputTaskBytes is. A sample past it is not printed: one chunk marks
	// the command's live output truncated instead, and nothing more is
	// printed for it but its last chunk.
	outputSampledBytes = 64 << 10
	// outputProcessBytes bounds every chunk line one agent-runner prints,
	// across all its commands, framing included; outputClosingBytes of it is
	// kept back for the two line-less chunks that close the command the
	// budget ran out on: the one marking its live output truncated, and its
	// last once it ends. Each is under 256 bytes, as no task id over 128 is
	// followed. It keeps the pod log far below the kubelet's rotation beside
	// a build's result line.
	outputProcessBytes = 512 << 10
	outputClosingBytes = 512
	// outputQueued bounds the commands waiting while another is followed.
	// The CLI runs its foreground commands one at a time; this only bounds
	// a stream that says otherwise.
	outputQueued = 8
	// outputReadBuf is one read's buffer. A pass reads until the file's end
	// or a poll interval, whichever is first; once sampling, a read costs
	// little more than counting its newlines (skim).
	outputReadBuf = 64 << 10
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
// prints their output live, one command at a time: a viewer is shown one
// command's output at a time (transcript.Output), so two commands' chunks
// must never interleave. A command that starts while another is followed
// waits in a queue, in start order, and is followed once that one ends,
// from its first line, as long as it has not ended first; one that ends
// while it waits is never shown. The runner's line observer hands the
// follower every stream line (scan), which only starts and signals the
// goroutine and never waits; the command followed has that one goroutine,
// bound to the run's context, reading its file. end, once the run is over,
// ends the command followed, drops the queue and waits for the last chunk,
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
	session string                   // the run's session, off its init event
	current *followedTask            // the command followed, or nil
	queue   []*followedTask          // the commands waiting their turn, oldest first
	known   map[string]*followedTask // current and queue, by task id
	over    bool                     // the run is over: no command is followed from now on
}

// followedTask is one command followed or waiting to be.
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
		known: map[string]*followedTask{},
	}
}

// scan applies one stream line's command events. It runs on the runner's
// reading goroutine for every line of the CLI's stream, so it never waits on
// a command: it starts and signals a goroutine, under locks only ever held
// for a map update or one stdout line. A tool result can only end a command
// followed or queued, so the harness decodes one only while there is such a
// command (watching).
func (f *outputFollower) scan(line []byte) {
	if f == nil {
		return
	}
	for _, ev := range f.tw.ScanTasks(line, f.watching()) {
		switch ev.Kind {
		case harness.TaskSession:
			f.mu.Lock()
			f.session = ev.Session
			f.mu.Unlock()
		case harness.TaskStarted:
			f.start(ev)
		case harness.TaskEnded:
			f.mu.Lock()
			f.endTask(f.known[ev.Task])
			f.mu.Unlock()
		case harness.TaskAnswered:
			f.mu.Lock()
			f.endTask(f.answered(ev.ToolUse))
			f.mu.Unlock()
		}
	}
}

// watching reports whether a command is followed or queued.
func (f *outputFollower) watching() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current != nil || len(f.queue) > 0
}

// start follows a command that started, or queues it while another is
// followed, unless the run is over, it is already known, the queue is full,
// the process's output budget is spent, or there is no file to look for.
func (f *outputFollower) start(ev harness.TaskEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.over || f.known[ev.Task] != nil || len(f.queue) >= outputQueued || f.a.outputSpent() {
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
	f.known[t.id] = t
	if f.current != nil {
		f.queue = append(f.queue, t)
		return
	}
	f.followNow(t)
}

// followNow starts the goroutine following t; the caller holds f.mu.
func (f *outputFollower) followNow(t *followedTask) {
	f.current = t
	f.wg.Add(1)
	go f.follow(t)
}

// answered is the command, followed or queued, that tool call toolUse ran,
// or nil; the caller holds f.mu.
func (f *outputFollower) answered(toolUse string) *followedTask {
	if toolUse == "" {
		return nil
	}
	if f.current != nil && f.current.toolUse == toolUse {
		return f.current
	}
	for _, t := range f.queue {
		if t.toolUse == toolUse {
			return t
		}
	}
	return nil
}

// endTask ends a command: the one followed has its goroutine print its last
// chunk, and a queued one is dropped, never shown. The caller holds f.mu.
func (f *outputFollower) endTask(t *followedTask) {
	switch t {
	case nil:
	case f.current:
		t.end()
	default:
		f.queue = slices.DeleteFunc(f.queue, func(q *followedTask) bool { return q == t })
		delete(f.known, t.id)
	}
}

// end is called once the run is over: it ends the command followed, as the
// CLI that ran it is gone, drops the commands queued, and waits for the last
// chunk. That wait is bounded by the drain time.
func (f *outputFollower) end() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.over = true
	if f.current != nil {
		f.current.end()
	}
	f.dropQueue()
	f.mu.Unlock()
	f.wg.Wait()
}

// dropQueue forgets every command waiting its turn; the caller holds f.mu.
func (f *outputFollower) dropQueue() {
	for _, t := range f.queue {
		delete(f.known, t.id)
	}
	f.queue = nil
}

// next lets go of the command whose goroutine is done and follows the
// oldest one queued, from its first line, unless the run is over or the
// process's budget is spent, when the queue is dropped unseen.
func (f *outputFollower) next(done *followedTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.known, done.id)
	f.current = nil
	if f.over || f.a.outputSpent() {
		f.dropQueue()
		return
	}
	if len(f.queue) > 0 {
		t := f.queue[0]
		f.queue = f.queue[1:]
		f.followNow(t)
	}
}

// follow is the goroutine of the command followed. It waits for the
// command's file, then reads what is appended to it every poll until the
// command or the run ends, when it reads the rest and prints the last chunk;
// then it hands over to the next command queued. A command whose file is
// never seen — a fixture's, or one over before the first look — prints
// nothing at all. One the process's budget ran out on is read no further,
// but it stays the command followed until it ends, when its end is printed:
// the one chunk printed past the budget.
func (f *outputFollower) follow(t *followedTask) {
	defer f.wg.Done()
	defer f.next(t)
	file := f.await(t)
	if file == nil {
		return
	}
	stopped := f.readOut(t, file)
	_ = file.Close()
	if !stopped {
		return
	}
	select {
	case <-t.ended:
	case <-f.ctx.Done():
	}
	f.a.endOutput(t.id, time.Now())
}

// readOut reads the command's file every poll until the command or the run
// ends, when it reads the rest and prints the last chunk, or until the
// process's budget is spent; it reports whether the budget stopped it.
func (f *outputFollower) readOut(t *followedTask, file *os.File) bool {
	s := f.newStream(t.id)
	tick := time.NewTicker(f.pace.poll)
	defer tick.Stop()
	if !s.read(file, time.Now()) {
		return true
	}
	for {
		select {
		case <-t.ended:
			s.close(file)
			return s.stopped
		case <-f.ctx.Done():
			s.close(file)
			return s.stopped
		case now := <-tick.C:
			if !s.read(file, now) {
				return true
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

// clean is one raw line as it is shown: the run's secrets scrubbed out, its
// terminal escapes stripped, made valid UTF-8, scrubbed again, and capped at
// outputLineBytes, a cut marked with an ellipsis. The scrub before stripping
// catches a secret printed straight after a bare ESC or a malformed escape
// sequence, whose first byte stripping takes as the sequence's final; the
// one after catches a secret split by escape codes. A secret a command
// prints in pieces on purpose, such as split by a carriage return so the
// line shows only its last piece, is not caught: like the transcript
// recorder's, the scrub guards against exposing a secret by accident, not
// against a command hiding one from it. A line the splitter cut (over) is
// shown as outputOverlong alone: what was held of it was cut before it was
// stripped or scrubbed.
func (f *outputFollower) clean(raw []byte, over bool) string {
	if over {
		return outputOverlong
	}
	text := transcript.Scrub(string(raw), f.secrets)
	text = strings.ToValidUTF8(string(ansi.Strip([]byte(text))), "�")
	text = transcript.Scrub(text, f.secrets)
	if len(text) > outputLineBytes {
		text, _ = transcript.Truncate(text, outputLineBytes-len(outputEllipsis))
		text += outputEllipsis
	}
	return text
}

// outputStream is one command's output as it is read: split into lines,
// cleaned, batched into chunks, and held to the command's budget, past which
// it is sampled, and to its samples' budget, past which it is cut. Only a
// limit that stops the command's live output for good marks a chunk
// Truncated: a sample's gap in the line numbers already shows what it left
// out. It belongs to the command's goroutine.
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

	printed      int  // what the command printed at full fidelity
	sampling     bool // past its budget: only samples of the newest lines
	newest       ring // the newest lines, once sampling
	sampled      time.Time
	sampledBytes int  // what the command's samples printed
	cut          bool // past its samples' budget: nothing more is printed but the last chunk
	stopped      bool // the process's budget is spent: nothing more is printed
}

// newStream is command task's output stream, from its first line.
func (f *outputFollower) newStream(task string) *outputStream {
	return &outputStream{f: f, task: task, next: 1, buf: make([]byte, outputReadBuf)}
}

// read takes what the command appended to its file since the last pass, to
// its end or for a poll interval at most, and prints what is due by now. It
// reports false once nothing more may be printed.
func (s *outputStream) read(file *os.File, now time.Time) bool {
	s.now = now
	s.readFile(file, time.Now().Add(s.f.pace.poll))
	if s.stopped {
		return false
	}
	switch {
	case s.cut:
	case s.sampling:
		if s.now.Sub(s.sampled) >= s.f.pace.sample {
			s.sample(false, false)
		}
	case len(s.batch) > 0 && s.now.Sub(s.batchSince) >= s.f.pace.flush:
		s.flush(false, false)
	}
	return !s.stopped
}

// close ends the command's output: it reads the rest of the file — the CLI
// deletes it when the command ends, and the file it opened still reads to its
// end — within the drain time, then prints the last chunk, Done. An output
// not read to its end in that time is marked truncated, as is one already
// cut.
func (s *outputStream) close(file *os.File) {
	if s.stopped {
		return
	}
	s.now = time.Now()
	whole := s.readFile(file, s.now.Add(s.f.pace.drain))
	if s.stopped {
		return
	}
	if raw, over, ok := s.split.rest(); ok && whole {
		s.line(raw, over) // the last line, which the output did not end
	}
	switch {
	case s.stopped:
	case s.cut:
		s.print(s.chunk(s.next, nil, true, true))
	case s.sampling:
		s.sample(true, !whole)
	default:
		s.flush(true, !whole)
	}
}

// readFile reads the file until its end or deadline, handing each line read
// to line: every line at full fidelity, and only those a sample could show
// once sampling (skim). It reports whether it reached the file's end.
func (s *outputStream) readFile(file *os.File, deadline time.Time) bool {
	for !s.stopped {
		n, err := file.Read(s.buf)
		switch {
		case n == 0:
		case s.sampling || s.cut:
			s.skim(s.buf[:n])
		default:
			s.split.write(s.buf[:n], s.line)
		}
		if err != nil || n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
	return false
}

// skim takes one read of a sampled command's output at little more than the
// cost of counting its newlines, so a command flooding its file is sampled
// at its newest lines rather than ever further behind them. Only the last
// outputSampleLines lines the read ends, and the line it leaves unended, go
// through the splitter: those before them are numbered and let go, the line
// held from earlier reads with them, since a sample shows the newest lines
// alone.
func (s *outputStream) skim(p []byte) {
	if k := bytes.Count(p, newline); k > outputSampleLines {
		i := len(p)
		for range outputSampleLines + 1 {
			i = bytes.LastIndexByte(p[:i], '\n')
		}
		// p[i] ends the last line let go: the k-outputSampleLines before it.
		s.next += k - outputSampleLines
		s.split.restart()
		p = p[i+1:]
	}
	s.split.write(p, s.line)
}

// newline is what skim counts.
var newline = []byte{'\n'}

// line takes one line of the command's output, over when the splitter cut
// it: into the batch at full fidelity, or into the newest lines once
// sampling.
func (s *outputStream) line(raw []byte, over bool) {
	n := s.next
	s.next++
	if s.stopped || s.cut {
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
	o := s.chunk(s.batchLine, s.batch, done, truncated)
	if len(s.batch) == 0 {
		o.Line = s.next
	}
	s.printed += s.print(o)
	s.batch, s.batchBytes = nil, 0
	if !s.sampling && s.printed >= outputTaskBytes {
		s.sampling, s.sampled = true, s.now
	}
}

// sample prints the newest lines not yet shown that fit a sample as one
// chunk, done marking the command's last and truncated a last chunk the
// drain time cut short; its line number leaves a gap after the last chunk,
// so a reader sees how much was left out. A sample that would take the
// command's samples past outputSampledBytes cuts the command instead: one
// chunk past its last line read, Truncated, and nothing more until its
// last.
func (s *outputStream) sample(done, truncated bool) {
	first, lines := s.newest.since(s.shown, s.f.clean)
	first, lines = newestWithin(first, lines, outputSampleBytes)
	if len(lines) == 0 && !done {
		return
	}
	if len(lines) == 0 {
		first = s.next
	}
	o := s.chunk(first, lines, done, truncated)
	if line, err := transcript.EncodeOutput(o); err == nil && s.sampledBytes+len(line)+1 > outputSampledBytes {
		s.cut = true
		s.print(s.chunk(s.next, nil, done, true))
		return
	}
	s.sampledBytes += s.print(o)
	s.sampled = s.now
}

// newestWithin keeps the newest of lines, numbered from first, whose text
// fits in limit bytes, the newest line at least, and returns the number of
// the first it keeps.
func newestWithin(first int, lines []string, limit int) (int, []string) {
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if size += len(lines[i]); size > limit && i < len(lines)-1 {
			return first + i + 1, lines[i+1:]
		}
	}
	return first, lines
}

// chunk is a chunk of the command's output, stamped with the pass's time.
func (s *outputStream) chunk(line int, lines []string, done, truncated bool) transcript.Output {
	return transcript.Output{Task: s.task, Line: line, Lines: lines, Done: done, Truncated: truncated,
		At: s.now.UTC().Format(time.RFC3339)}
}

// print prints one chunk and returns what it put on stdout, stopping the
// stream once the process's budget is spent.
func (s *outputStream) print(o transcript.Output) int {
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
