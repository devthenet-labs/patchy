// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// A live follow skips a stage result far past maxTailLine (a build's
// changeset) without failing or holding it, and still delivers the turns on
// either side of it, a turn split across the buffer boundary included.
func TestTailSkipsOverLongLines(t *testing.T) {
	const jobName = "demo-1-bld-r1-app-a1"
	huge := "PATCHY-EVENT: " + strings.Repeat("A", 3*maxTailLine)
	body := strings.Join([]string{
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "one"}),
		huge,
		// An over-long line that also happens to carry the turn prefix is
		// still dropped whole: a viewer never sees half a turn.
		"PATCHY-TURN: " + strings.Repeat("B", 2*maxTailLine),
		turnLine(t, transcript.Turn{Seq: 2, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "two"}),
		"no newline at the end, and not a turn",
	}, "\n")

	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = &fakeLogs{body: body}
	var got []string
	err := tl.Tail(t.Context(), jobName, Sink{Turn: func(turn transcript.Turn) error {
		got = append(got, turn.Text)
		return nil
	}})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("turns = %v, want one,two", got)
	}
}

// A final turn line with no trailing newline (the container exited mid
// write of its last newline) is still delivered.
func TestTailDeliversUnterminatedLastTurn(t *testing.T) {
	const jobName = "demo-1-plan-r1-a1"
	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = &fakeLogs{body: turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant,
		Kind: transcript.KindText, Text: "last"})}
	var got []string
	if err := tl.Tail(t.Context(), jobName, Sink{Turn: func(turn transcript.Turn) error {
		got = append(got, turn.Text)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "last" {
		t.Errorf("turns = %v, want [last]", got)
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

// A live follow hands turns and command output to their own handlers, in
// log order, and nothing else: not stage results, not stray lines, not an
// output line that fails its own checks. A line is the kind of the first
// prefix it carries, so a command that prints the turn prefix (a grep
// through patchy's own source) is still output, and a turn that quotes the
// output prefix is still a turn.
func TestTailSplitsTurnsFromOutput(t *testing.T) {
	const jobName = "demo-1-bld-r1-app-a1"
	body := strings.Join([]string{
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindToolUse,
			Tool: "Bash", Text: "grep -rn PATCHY-OUTPUT: internal"}),
		outputLine(t, transcript.Output{Task: "b1", Line: 1, Lines: []string{"ok  internal/web", "ok  internal/jobs"}}),
		"2026-10-06T09:00:01.5Z " + outputLine(t, transcript.Output{Task: "b1", Line: 3,
			Lines: []string{`follow.go:12: if transcript.HasPrefix(line) // "PATCHY-TURN: "`}}),
		eventLine(t, remediationEvent("finding-abc123def0-1")),
		"plain agent chatter on stdout",
		// Output lines that fail their own checks: a future version, no
		// task, no line number, and a body that is not JSON at all.
		`PATCHY-OUTPUT: {"v":2,"task":"b1","line":4,"lines":["future"]}`,
		`PATCHY-OUTPUT: {"v":1,"task":"","line":4,"lines":["no task"]}`,
		`PATCHY-OUTPUT: {"v":1,"task":"b1","line":0,"lines":["no line"]}`,
		`PATCHY-OUTPUT: not json`,
		outputLine(t, transcript.Output{Task: "b1", Line: 4, Done: true}),
		turnLine(t, transcript.Turn{Seq: 2, Role: transcript.RoleUser, Kind: transcript.KindToolResult,
			Text: "PATCHY-OUTPUT: lines are the output format"}),
	}, "\n") + "\n"

	tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
	tl.logs = &fakeLogs{body: body}
	var log []string
	err := tl.Tail(t.Context(), jobName, Sink{
		Turn: func(turn transcript.Turn) error {
			log = append(log, fmt.Sprintf("turn %d", turn.Seq))
			return nil
		},
		Output: func(o transcript.Output) error {
			log = append(log, fmt.Sprintf("output %s@%d+%d done=%v", o.Task, o.Line, len(o.Lines), o.Done))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	want := []string{"turn 1", "output b1@1+2 done=false", "output b1@3+1 done=false", "output b1@4+0 done=true",
		"turn 2"}
	if !slices.Equal(log, want) {
		t.Errorf("delivered %q, want %q", log, want)
	}
}

// A follower that takes only turns (the Findings transcript) skips the
// output lines, and one that takes only output skips the turns; an output
// handler's error ends the follow as a turn handler's does.
func TestTailSinkHandlers(t *testing.T) {
	const jobName = "demo-1-bld-r1-app-a1"
	body := strings.Join([]string{
		outputLine(t, transcript.Output{Task: "b1", Line: 1, Lines: []string{"one"}}),
		turnLine(t, transcript.Turn{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindText, Text: "a"}),
		outputLine(t, transcript.Output{Task: "b1", Line: 2, Lines: []string{"two"}}),
	}, "\n") + "\n"
	follow := func(sink Sink) error {
		tl := NewTailer(fake.NewClientset(runningPod(jobName)), "patchy-agents")
		tl.logs = &fakeLogs{body: body}
		return tl.Tail(t.Context(), jobName, sink)
	}

	turns := 0
	if err := follow(Sink{Turn: func(transcript.Turn) error { turns++; return nil }}); err != nil || turns != 1 {
		t.Errorf("turns only: %d turns, err %v; want 1, nil", turns, err)
	}
	chunks := 0
	if err := follow(Sink{Output: func(transcript.Output) error { chunks++; return nil }}); err != nil || chunks != 2 {
		t.Errorf("output only: %d chunks, err %v; want 2, nil", chunks, err)
	}
	stop := errors.New("viewer gone")
	chunks = 0
	err := follow(Sink{Output: func(transcript.Output) error { chunks++; return stop }})
	if !errors.Is(err, stop) || chunks != 1 {
		t.Errorf("failing output handler: %d chunks, err %v; want 1 and its error", chunks, err)
	}
}

func TestFactsProjectsClockAndIdleLimits(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC))
	started := metav1.NewTime(created.Add(5 * time.Second))
	deadline := int64(5400)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-1-bld-r1-app-a1", Namespace: "patchy-agents",
			CreationTimestamp: created},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "sidecar", Env: []corev1.EnvVar{{Name: envRemediateIdle, Value: "1m"}}},
				{Name: agentContainerName, Env: []corev1.EnvVar{
					{Name: "ANTHROPIC_AUTH_TOKEN", Value: "placeholder"},
					{Name: envInvestigateIdle, Value: "bogus"},
					{Name: envRemediateIdle, Value: "20m"},
				}},
			}}},
		},
		Status: batchv1.JobStatus{StartTime: &started},
	}
	tl := NewTailer(fake.NewClientset(job), "patchy-agents")
	f, err := tl.Facts(t.Context(), job.Name)
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	want := Facts{Created: created.Time, Started: started.Time, DeadlineSeconds: 5400, RemediateIdle: 20 * time.Minute}
	if f != want {
		t.Errorf("Facts = %+v, want %+v (the sidecar's variable and the bogus value ignored)", f, want)
	}
	if _, err := tl.Facts(t.Context(), "absent"); err == nil {
		t.Error("Facts of a missing Job succeeded")
	}
}

func TestIdleLimit(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"20m0s": 20 * time.Minute, "0s": 0, "-5m": 0, "": 0, "x": 0, "49h": 0, "48h": 48 * time.Hour,
	} {
		if got := idleLimit(in); got != want {
			t.Errorf("idleLimit(%q) = %v, want %v", in, got, want)
		}
	}
}
