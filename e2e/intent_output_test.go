// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
)

// scriptedLines is how many lines the scripted claude's command prints
// before its last, which echoes the caller-token header.
const scriptedLines = 40

// TestIntentBuildLiveOutput: a build whose agent runs a command prints the
// command's output on the pod log as the real agent-runner reads it from the
// file the CLI keeps it in — every line in order under its number, the
// caller token scrubbed, closed by a Done chunk before the stage's result —
// while the run completes as before, and the transcript intent-controller
// persists holds none of it. The fake kubelet serves the log only once the
// agent has exited, so this cannot show the output arriving live; the
// agentrun unit tests do.
func TestIntentBuildLiveOutput(t *testing.T) {
	e := startIntents(t)
	e.kubelet.useAgentRunner(t, "build")
	e.kubelet.setAgentEnv("PATCHY_FAKE_CLAUDE_OUTPUT", fmt.Sprint(scriptedLines))
	// The CLI's temporary directory, kept off the host's /tmp.
	e.kubelet.setAgentEnv("CLAUDE_CODE_TMPDIR", t.TempDir())
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)

	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "ok" {
		t.Fatalf("build run = %s %q, want ok", run.Status.Outcome, run.Status.Detail)
	}

	var chunks []transcript.Output
	var lines []string
	doneAt, resultAt := -1, -1
	for i, line := range bytes.Split(job.Stdout, []byte("\n")) {
		if o, ok := transcript.DecodeOutput(line); ok {
			if o.Task != "bscripted1" || o.Line != len(lines)+1 || o.Truncated {
				t.Errorf("chunk %+v, want task bscripted1 from line %d, whole", o, len(lines)+1)
			}
			if o.Done {
				doneAt = i
			}
			chunks = append(chunks, o)
			lines = append(lines, o.Lines...)
			continue
		}
		if bytes.Contains(line, []byte("PATCHY-EVENT: ")) {
			resultAt = i
		}
	}
	if len(lines) != scriptedLines+1 {
		t.Fatalf("output lines = %d over %d chunks, want %d", len(lines), len(chunks), scriptedLines+1)
	}
	for i, l := range lines[:scriptedLines] {
		if want := fmt.Sprintf("ok  example.com/app/pkg%d", i+1); l != want {
			t.Errorf("line %d = %q, want %q", i+1, l, want)
		}
	}
	if last := lines[scriptedLines]; !strings.Contains(last, transcript.Redacted) {
		t.Errorf("the header line = %q, want the caller token redacted", last)
	}
	if bytes.Contains(job.Stdout, []byte("e2e-projected-caller-token")) {
		t.Error("the agent's log carries the caller token")
	}
	if doneAt < 0 || resultAt < 0 || doneAt > resultAt || !chunks[len(chunks)-1].Done {
		t.Errorf("the Done chunk is line %d and the result line %d; want a last Done chunk before the result",
			doneAt, resultAt)
	}

	if run.Status.Transcript == nil {
		t.Fatal("the build's transcript was not persisted")
	}
	turns, err := transcriptstore.Load(context.Background(), e.cl.client, namespace, run.Status.Transcript.Name)
	if err != nil {
		t.Fatalf("load the build's transcript: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("the build's transcript is empty")
	}
	for _, turn := range turns {
		if strings.Contains(turn.Text, "example.com/app/pkg1") || strings.Contains(turn.Text, transcript.OutputPrefix) {
			t.Errorf("persisted turn %d carries the command's output: %q", turn.Seq, turn.Text)
		}
	}
	checkOneTranscript(t, job)
	e.waitPhase(t, name, v1alpha1.IntentInReview)
}
