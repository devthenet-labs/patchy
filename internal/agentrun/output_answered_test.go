// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"slices"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

// TestOutputToolResultEndsTheCommandItAnswers: a tool result ends the command
// its tool call ran, whether that command is followed or still queued; a
// result answering no known command, or a call with no id, changes nothing.
func TestOutputToolResultEndsTheCommandItAnswers(t *testing.T) {
	cfg, ws, _, _ := outputSetup(t)
	a := New(cfg, &commandExec{ws: ws})
	a.pace = fastPace
	f := a.followOutput(context.Background(), harness.NewFake(), runner.CommandSpec{}, nil)
	defer f.end()
	f.scan([]byte(initLine()))
	for _, id := range []string{"0", "1", "2"} {
		f.scan([]byte(startedLine("bq"+id, "toolu_q"+id)))
	}
	state := func() (string, []string, int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		queue := make([]string, 0, len(f.queue))
		for _, q := range f.queue {
			queue = append(queue, q.id)
		}
		cur := ""
		if f.current != nil {
			cur = f.current.id
		}
		return cur, queue, len(f.known)
	}

	// Unknown and empty tool-use ids answer nothing.
	f.scan([]byte(answeredLine("toolu_other", "x")))
	f.scan([]byte(answeredLine("", "x")))
	if cur, queue, known := state(); cur != "bq0" || !slices.Equal(queue, []string{"bq1", "bq2"}) || known != 3 {
		t.Fatalf("after unrelated results: followed %q, queued %q, known %d", cur, queue, known)
	}

	// A queued command answered is dropped unseen; the others keep their places.
	f.scan([]byte(answeredLine("toolu_q2", "done")))
	if cur, queue, known := state(); cur != "bq0" || !slices.Equal(queue, []string{"bq1"}) || known != 2 {
		t.Errorf("after answering the queued bq2: followed %q, queued %q, known %d; want bq0, [bq1], 2",
			cur, queue, known)
	}

	// The followed command answered is told to end.
	f.mu.Lock()
	current := f.current
	f.mu.Unlock()
	f.scan([]byte(answeredLine("toolu_q0", "done")))
	select {
	case <-current.ended:
	default:
		t.Error("answering the followed command did not end it")
	}
}
