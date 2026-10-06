// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"os"

	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// FakeFixtureEnv names the environment variable pointing at the stream-json
// fixture file the Fake harness replays.
const FakeFixtureEnv = "PATCHY_FAKE_FIXTURE"

// Fake is a stand-in harness for tests and the agent-runner's --fake mode: it
// replays a captured stream-json fixture through cat instead of invoking an
// agent, and parses it with the same stream-json helpers as Claude, so
// everything downstream of PromptSpec is exercised for real. Unix-only (it
// shells out to /bin/cat's argv convention); the real deployments it fakes
// run in Linux pods anyway.
type Fake struct {
	base
}

// NewFake returns the builtin fake harness.
func NewFake() *Fake {
	return &Fake{base: base{
		id:   "fake",
		name: "Fake",
		clis: []string{"cat"},
		// No credentials: the fixture replay needs none.
		envKeys: nil,
	}}
}

// PromptSpec ignores the request's prompt and flags and replays the fixture
// named by FakeFixtureEnv; ws and Env still land on the spec so runner-side
// behavior (workspace, environment) stays faithful.
func (f *Fake) PromptSpec(ws string, req PromptRequest) runner.CommandSpec {
	return runner.CommandSpec{
		Argv: []string{"cat", os.Getenv(FakeFixtureEnv)},
		Dir:  ws,
		Env:  req.Env,
	}
}

// ResumeSpec replays the fixture exactly as PromptSpec does: a resumed run
// is one more replay, so tests and the dev overlay can drive a stage's
// report repair through the real runner.
func (f *Fake) ResumeSpec(ws, _ string, req PromptRequest) runner.CommandSpec {
	return f.PromptSpec(ws, req)
}

// ParseResult parses the fixture exactly as Claude parses live output.
func (f *Fake) ParseResult(stdout []byte) (AgentResult, bool) {
	return parseStreamResult(stdout)
}

// RuntimeError classifies the fixture exactly as Claude classifies live output.
func (f *Fake) RuntimeError(stdout []byte, exitCode int, timedOut bool) string {
	return streamRuntimeError(stdout, exitCode, timedOut)
}

// ScanUsage scans fixture lines exactly as Claude scans live ones.
func (f *Fake) ScanUsage(line []byte) (int, bool) {
	return scanStreamUsage(line)
}

// StreamUsage tallies a fixture's usage exactly as Claude tallies live output.
func (f *Fake) StreamUsage(stdout []byte) *Usage {
	return streamUsage(stdout)
}

// ScanTurns projects fixture lines exactly as Claude projects live ones, so a
// replayed fixture produces a real transcript end to end.
func (f *Fake) ScanTurns(line []byte) []transcript.Turn {
	return scanStreamTurns(line)
}

// ScanTasks reads fixture lines' command events exactly as Claude reads live
// ones. A fixture's command never writes the file TaskOutputGlob names, so
// following it finds nothing.
func (f *Fake) ScanTasks(line []byte) []TaskEvent {
	return scanStreamTasks(line)
}

// TaskOutputGlob is Claude's: see taskOutputGlob.
func (f *Fake) TaskOutputGlob(env []string, uid int, session, task string) string {
	return taskOutputGlob(env, uid, session, task)
}
