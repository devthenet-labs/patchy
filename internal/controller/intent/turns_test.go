// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"math"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// TestPodTurnsClamped: the pod's turn counts are untrusted, and the schema
// holds both to [0, 100000], so a count out of bounds is recorded at the
// nearer bound rather than failing the whole status write.
func TestPodTurnsClamped(t *testing.T) {
	for _, tt := range []struct {
		in   int
		want int32
	}{
		{0, 0}, {1, 1}, {37, 37}, {100000, 100000}, {100001, 100000}, {-1, 0},
		{math.MaxInt32 + 1, 100000}, {math.MaxInt, 100000}, {math.MinInt, 0},
	} {
		if got := podTurns(tt.in); got != tt.want {
			t.Errorf("podTurns(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// withTurns is defaultOutput with the stage reporting turns and a first
// edit turn: plan for the plan, build for every build.
func withTurns(plan, build envelope.Stage) func(jobs.Spec) jobs.RunOutput {
	return func(spec jobs.Spec) jobs.RunOutput {
		out := defaultOutput(spec)
		for i := range out.Events {
			ev := &out.Events[i]
			switch {
			case ev.Plan != nil:
				ev.Plan.NumTurns, ev.Plan.FirstEditTurn = plan.NumTurns, plan.FirstEditTurn
			case ev.Remediation != nil:
				ev.Remediation.NumTurns, ev.Remediation.FirstEditTurn = build.NumTurns, build.FirstEditTurn
			}
		}
		return out
	}
}

// TestRunsRecordTurns: a run records the turns and first edit turn its pod
// reported, whether it is settled (the plan) or pushed (the build, whose
// later settle keeps what the push recorded).
func TestRunsRecordTurns(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = withTurns(envelope.Stage{NumTurns: 12}, envelope.Stage{NumTurns: 30, FirstEditTurn: 6})
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	plan := e.runsOf(name, v1alpha1.IntentStagePlan)[0]
	if plan.Status.NumTurns != 12 || plan.Status.FirstEditTurn != 0 {
		t.Errorf("plan run turns = %d, first edit %d; want 12, 0", plan.Status.NumTurns, plan.Status.FirstEditTurn)
	}
	build := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	if build.Status.PushedCommit == "" || build.Status.NumTurns != 30 || build.Status.FirstEditTurn != 6 {
		t.Errorf("build run commit %q, turns %d, first edit %d; want pushed, 30, 6", build.Status.PushedCommit,
			build.Status.NumTurns, build.Status.FirstEditTurn)
	}
}

// TestFailedRunTurnsClamped: a failed build settles with its untrusted
// counts held into the schema's bounds.
func TestFailedRunTurnsClamped(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		out := failingBuild(spec)
		if rem := out.Events[0].Remediation; rem != nil {
			rem.NumTurns, rem.FirstEditTurn = math.MaxInt, -5
		}
		return out
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) == 0 {
		t.Fatal("no build run")
	}
	for _, r := range runs {
		if r.Status.NumTurns != maxPodTurns || r.Status.FirstEditTurn != 0 {
			t.Errorf("build run %s turns %d, first edit %d; want %d, 0", r.Name, r.Status.NumTurns,
				r.Status.FirstEditTurn, maxPodTurns)
		}
	}
}
