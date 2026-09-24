// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// checkUsage fails when any of the Intent's usage totals is negative, which
// the API server would refuse.
func checkUsage(t *testing.T, u v1alpha1.IntentUsage) {
	t.Helper()
	if u.CostMicroUSD < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 ||
		u.CacheCreationTokens < 0 {
		t.Errorf("intent usage = %+v, want no negative total", u)
	}
}

// TestPodUsageIsNeverNegative: a build runs in the repository's own image,
// so the usage its envelope reports is untrusted like its outcome. Negative
// counts are recorded as zero, and the Intent's total, which the schema
// holds to minimum 0, is written like any other.
func TestPodUsageIsNeverNegative(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return defaultOutput(spec)
		}
		return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
			Remediation: &envelope.Remediation{Stage: envelope.Stage{Outcome: envelope.OutcomeRuntimeError,
				Detail: "the CLI crashed", Usage: envelope.Usage{
					InputTokens: -math.MaxInt64, OutputTokens: -1, CacheReadTokens: math.MinInt,
					CacheCreationTokens: -7, CostUSD: -3,
				}}}}}}
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentFailed, repoImage)
	for _, r := range e.runsOf(name, v1alpha1.IntentStageBuild) {
		if u := r.Status.Usage; u != (v1alpha1.UsageSummary{}) {
			t.Errorf("build run %s recorded usage %+v, want every negative count as zero", r.Name, u)
		}
	}
	checkUsage(t, in.Status.Usage)
	if in.Status.Usage.InputTokens != 10 || in.Status.Usage.CostMicroUSD != 250000 {
		t.Errorf("intent usage = %+v, want the plan's alone", in.Status.Usage)
	}
}

// recordedRun records a finished run of the intent with usage, as an earlier
// controller (or a pod) left it.
func (e *env) recordedRun(intent string, round int32, usage v1alpha1.UsageSummary) {
	e.t.Helper()
	ctx := context.Background()
	in := e.get(intent)
	name := fmt.Sprintf("%s-plan-r%d-a1", intent, round)
	run := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS,
			Labels: map[string]string{v1alpha1.LabelIntent: intent}},
		Spec: v1alpha1.IntentRunSpec{IntentRef: v1alpha1.ObjectReference{Name: intent, UID: in.UID},
			Stage: v1alpha1.IntentStagePlan, Round: round, Attempt: 1,
			Repository: v1alpha1.IntentRunRepository{URL: appRepoURL,
				RepositoryRef: v1alpha1.LocalObjectReference{Name: name + "-src"}},
			Inputs: v1alpha1.IntentRunInputs{ConfigMap: name + "-input"}},
	}
	if err := e.c.Create(ctx, run); err != nil {
		e.t.Fatal(err)
	}
	run.Status.Phase, run.Status.Outcome, run.Status.Usage = v1alpha1.RunFailed, "runtime_error", usage
	if err := e.c.Status().Update(ctx, run); err != nil {
		e.t.Fatal(err)
	}
}

// TestUsageTallyNeverWedgesTheIntent: whatever the runs recorded, the
// Intent's usage total is one the API server accepts, and a total it refuses
// all the same is no reason to stop the pass: the intent still answers
// /patchy cancel and ends.
func TestUsageTallyNeverWedgesTheIntent(t *testing.T) {
	huge := v1alpha1.UsageSummary{CostUSD: "9223372036854.775807"}
	for _, tt := range []struct {
		name string
		// before sets up the world before the intent is created; after
		// records runs once it awaits approval.
		before func(e *env)
		after  func(e *env, name string)
		// saturated: the cost total, and saturatedTokens the input token
		// total, is expected at its ceiling.
		saturated, saturatedTokens bool
	}{
		{name: "negative counts recorded", after: func(e *env, name string) {
			e.recordedRun(name, 90, v1alpha1.UsageSummary{InputTokens: -math.MaxInt64, OutputTokens: -1})
		}},
		{name: "costs whose sum overflows", saturated: true, after: func(e *env, name string) {
			e.recordedRun(name, 90, huge)
			e.recordedRun(name, 91, huge)
		}},
		{name: "token counts whose sum overflows", saturatedTokens: true, after: func(e *env, name string) {
			e.recordedRun(name, 90, v1alpha1.UsageSummary{InputTokens: math.MaxInt64})
			e.recordedRun(name, 91, v1alpha1.UsageSummary{InputTokens: math.MaxInt64})
		}},
		{name: "every usage write refused", before: func(e *env) { e.refuseUsage = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			if tt.before != nil {
				tt.before(e)
			}
			name := e.awaiting()
			if tt.after != nil {
				tt.after(e, name)
			}
			e.gh.comment(approver, "/patchy cancel")
			in := e.drive(name, v1alpha1.IntentClosed, repoImage)
			if !slices.Equal(e.gh.closes[1], []string{ghclient.CloseNotPlanned}) {
				t.Errorf("issue closes = %v, want one, not planned", e.gh.closes[1])
			}
			checkUsage(t, in.Status.Usage)
			if tt.saturated && in.Status.Usage.CostMicroUSD != math.MaxInt64 {
				t.Errorf("cost total = %d, want it saturated at %d", in.Status.Usage.CostMicroUSD, int64(math.MaxInt64))
			}
			if tt.saturatedTokens && in.Status.Usage.InputTokens != math.MaxInt64 {
				t.Errorf("input token total = %d, want it saturated at %d", in.Status.Usage.InputTokens,
					int64(math.MaxInt64))
			}
		})
	}
}

// TestAddUsage: the tally saturates and never goes negative.
func TestAddUsage(t *testing.T) {
	for _, tt := range []struct{ sum, n, want int64 }{
		{sum: 1, n: 2, want: 3},
		{sum: 5, n: -9, want: 5},
		{sum: 5, n: math.MinInt64, want: 5},
		{sum: math.MaxInt64 - 1, n: 1, want: math.MaxInt64},
		{sum: math.MaxInt64 - 1, n: 2, want: math.MaxInt64},
		{sum: math.MaxInt64, n: math.MaxInt64, want: math.MaxInt64},
	} {
		if got := addUsage(tt.sum, tt.n); got != tt.want {
			t.Errorf("addUsage(%d, %d) = %d, want %d", tt.sum, tt.n, got, tt.want)
		}
	}
}
