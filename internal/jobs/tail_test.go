// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

func turnLine(t *testing.T, turn transcript.Turn) string {
	t.Helper()
	line, err := transcript.Encode(turn)
	if err != nil {
		t.Fatalf("encode turn: %v", err)
	}
	return line
}

// runningPod has an agent container that has started but not finished — what
// a live tail joins.
func runningPod(jobName string) *corev1.Pod {
	return jobPodInState(jobName, corev1.PodRunning, corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	})
}

func TestResultSplitsEventsFromTurns(t *testing.T) {
	const jobName = "patchy-abc-inv-a1"
	const finding = "finding-abc123def0-1"
	body := strings.Join([]string{
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant,
			Kind: transcript.KindText, Text: "Reading."}),
		turnLine(t, transcript.Turn{Seq: 2, Role: transcript.RoleAssistant,
			Kind: transcript.KindToolUse, Tool: "Bash", Text: "go test"}),
		eventLine(t, investigationEvent(finding)),
		"agent done",
	}, "\n") + "\n"

	c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
	c.logs = &fakeLogs{body: body}

	out, err := c.Result(context.Background(), jobName)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].Type != envelope.TypeInvestigation {
		t.Errorf("Events = %+v, want one investigation", out.Events)
	}
	if len(out.Turns) != 2 {
		t.Fatalf("Turns = %d, want 2: %+v", len(out.Turns), out.Turns)
	}
	if out.Turns[1].Tool != "Bash" || out.Turns[1].Text != "go test" {
		t.Errorf("Turns[1] = %+v", out.Turns[1])
	}
}

func TestTurnQuotingTheEnvelopePrefixIsNotAnEvent(t *testing.T) {
	// envelope.Decode searches for its prefix anywhere in a line to survive log
	// wrapping, so an agent that quotes the prefix in its own output would
	// otherwise inject a bogus stage result.
	const jobName = "patchy-abc-inv-a1"
	quoted := turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant,
		Kind: transcript.KindText,
		Text: `the controller scans for PATCHY-EVENT: {"v":4,"type":"remediation"} lines`})

	c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
	c.logs = &fakeLogs{body: quoted + "\n"}

	out, err := c.Result(context.Background(), jobName)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(out.Events) != 0 {
		t.Errorf("Events = %+v, want none from a quoted prefix", out.Events)
	}
	if len(out.Turns) != 1 {
		t.Errorf("Turns = %d, want the quoting turn itself", len(out.Turns))
	}
}

func TestTailFollowsRunningAgent(t *testing.T) {
	const jobName = "patchy-abc-rem-a1"
	body := strings.Join([]string{
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleSystem,
			Kind: transcript.KindNotice, Text: "session s1 started"}),
		eventLine(t, remediationEvent("finding-abc123def0-1")),
		turnLine(t, transcript.Turn{Seq: 2, Role: transcript.RoleAssistant,
			Kind: transcript.KindText, Text: "Patching."}),
	}, "\n") + "\n"

	logs := &fakeLogs{body: body}
	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = logs

	var got []transcript.Turn
	err := tl.Tail(context.Background(), jobName, func(turn transcript.Turn) error {
		got = append(got, turn)
		return nil
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(got), got)
	}
	if got[1].Text != "Patching." {
		t.Errorf("turns[1] = %+v", got[1])
	}
	// A live tail must follow, and must not wait for termination — the pod is
	// still running.
	if !logs.follow {
		t.Error("Tail streamed with follow=false, want follow=true")
	}
	if logs.container != agentContainerName {
		t.Errorf("streamed container %q, want %q", logs.container, agentContainerName)
	}
}

func TestTailStopsOnHandlerError(t *testing.T) {
	const jobName = "patchy-abc-inv-a1"
	body := strings.Join([]string{
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "a"}),
		turnLine(t, transcript.Turn{Seq: 2, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "b"}),
	}, "\n") + "\n"

	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = &fakeLogs{body: body}

	seen := 0
	err := tl.Tail(context.Background(), jobName, func(transcript.Turn) error {
		seen++
		return context.Canceled // the viewer disconnected
	})
	if err == nil {
		t.Fatal("Tail returned nil, want the handler's error")
	}
	if seen != 1 {
		t.Errorf("delivered %d turns after the handler failed, want 1", seen)
	}
}

func TestTailCancelledContextIsNotAnError(t *testing.T) {
	// A viewer closing the page is the normal end of a follow.
	const jobName = "patchy-abc-inv-a1"
	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = &fakeLogs{body: turnLine(t, transcript.Turn{
		Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "a"}) + "\n"}

	ctx, cancel := context.WithCancel(context.Background())
	err := tl.Tail(ctx, jobName, func(transcript.Turn) error {
		cancel()
		return ctx.Err()
	})
	if err != nil {
		t.Errorf("Tail after cancellation = %v, want nil", err)
	}
}

func TestTailNoPod(t *testing.T) {
	tl := NewTailer(fake.NewClientset(), "patchy-agents")
	tl.logs = &fakeLogs{}
	err := tl.Tail(context.Background(), "patchy-none-inv-a1", func(transcript.Turn) error { return nil })
	if err == nil {
		t.Fatal("Tail without a pod succeeded, want error")
	}
}

func outputLine(t *testing.T, o transcript.Output) string {
	t.Helper()
	line, err := transcript.EncodeOutput(o)
	if err != nil {
		t.Fatalf("encode output: %v", err)
	}
	return line
}

// TestResultSkipsLiveOutput: the live command output is neither persisted
// as turns nor read as stage results, even where its lines quote either
// stream, and the result is found after a run's worth of it. A turn or a
// result that quotes an output line is still what it is.
func TestResultSkipsLiveOutput(t *testing.T) {
	const jobName = "patchy-out-bld-a1"
	const finding = "finding-out-1"
	quotedTurn := turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText})
	const chunks = 5000
	body := make([]string, 0, chunks+4)
	body = append(body,
		outputLine(t, transcript.Output{Task: "bq", Line: 1, Lines: []string{
			eventLine(t, investigationEvent(finding)), quotedTurn, "  " + eventLine(t, investigationEvent(finding)),
		}}),
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleUser, Kind: transcript.KindToolResult, Tool: "Bash",
			Text: "the log said " + outputLine(t, transcript.Output{Task: "bx", Line: 1, Lines: []string{"x"}})}),
	)
	lines := make([]string, 32)
	for i := range lines {
		lines[i] = "ok  github.com/acme/shop/pkg" + strings.Repeat("/sub", i)
	}
	for i := range chunks {
		body = append(body, outputLine(t, transcript.Output{Task: "bsuite", Line: 1 + 32*i, Lines: lines,
			At: "2026-10-06T10:00:00Z"}))
	}
	body = append(body, outputLine(t, transcript.Output{Task: "bsuite", Line: 1 + 32*chunks, Done: true}))
	result := remediationEvent(finding)
	result.Remediation.ReportMarkdown = "The runner prints " + outputLine(t,
		transcript.Output{Task: "b1", Line: 1, Lines: []string{"ok"}}) + " lines while a command runs."
	body = append(body, eventLine(t, result))

	c := New(fake.NewClientset(jobPod(jobName)), testConfig(), nil)
	c.logs = &fakeLogs{body: strings.Join(body, "\n") + "\n"}
	out, err := c.Result(context.Background(), jobName)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].Remediation == nil ||
		out.Events[0].Remediation.ReportMarkdown != result.Remediation.ReportMarkdown {
		t.Fatalf("Events = %+v, want only the result, its report quoting an output line intact", out.Events)
	}
	if len(out.Turns) != 1 || !strings.Contains(out.Turns[0].Text, transcript.OutputPrefix) {
		t.Errorf("Turns = %+v, want only the turn quoting an output line", out.Turns)
	}
}
