// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
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
	err := tl.Tail(t.Context(), jobName, func(turn transcript.Turn) error {
		got = append(got, turn.Text)
		return nil
	})
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
	if err := tl.Tail(t.Context(), jobName, func(turn transcript.Turn) error {
		got = append(got, turn.Text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "last" {
		t.Errorf("turns = %v, want [last]", got)
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
