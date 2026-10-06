// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/intentview"
)

// The status page's intent projection (internal/intentview) copies a few
// facts from this package rather than moving them, so the issue goldens
// never move for the dashboard's sake. These tests pin each copy to its
// original: a change here that the dashboard does not follow fails here.

func TestIntentviewBlockingConditionsPinned(t *testing.T) {
	if !slices.Equal(intentview.BlockingConditions, blockingConditions) {
		t.Errorf("intentview.BlockingConditions = %v, want the controller's %v",
			intentview.BlockingConditions, blockingConditions)
	}
}

func TestIntentviewMaxAttemptsPinned(t *testing.T) {
	if intentview.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("intentview.MaxAttempts = %d, want the controller's %d", intentview.MaxAttempts, DefaultMaxAttempts)
	}
}

// For any cost string, the dashboard's parser and the one that sums a run
// into the Intent's spend agree, so a run row never disagrees with the total
// the cost ceiling is checked against.
func TestIntentviewMicroUSDPinned(t *testing.T) {
	alphabet := []byte("0123456789.-x ")
	cfg := &quick.Config{
		MaxCount: 5000,
		Rand:     rand.New(rand.NewSource(20261006)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			b := make([]byte, r.Intn(24))
			for i := range b {
				b[i] = alphabet[r.Intn(len(alphabet))]
			}
			args[0] = reflect.ValueOf(string(b))
		},
	}
	if err := quick.Check(func(s string) bool { return intentview.MicroUSD(s) == microUSD(s) }, cfg); err != nil {
		t.Error(err)
	}
}

// randomRuns is a random Intent's runs for the pins below: plan and build
// runs beside revise rounds of every trigger, each round's attempts unique
// and in no order, with outcomes the rules single out.
func randomRuns(r *rand.Rand) []*v1alpha1.IntentRun {
	phases := []v1alpha1.RunPhase{"", v1alpha1.RunPending, v1alpha1.RunRunning, v1alpha1.RunComplete,
		v1alpha1.RunFailed, v1alpha1.RunFailed, v1alpha1.RunFailed}
	outcomes := []string{"", "ok", OutcomeNoUsableFeedback, OutcomeHeadMoved, OutcomeImageRequired,
		OutcomeHoldExpired, OutcomeUnschedulable, OutcomeEvicted, string(envelope.OutcomeTimeout),
		string(envelope.OutcomeRuntimeError)}
	triggers := []v1alpha1.IntentRunTrigger{v1alpha1.IntentRunTriggerReview, v1alpha1.IntentRunTriggerCommand,
		v1alpha1.IntentRunTriggerChecks}
	stages := []v1alpha1.IntentStage{v1alpha1.IntentStagePlan, v1alpha1.IntentStageBuild,
		v1alpha1.IntentStageRevise, v1alpha1.IntentStageRevise}
	repos := []string{"https://github.com/acme/app", "https://github.com/Acme/App.git", "https://github.com/acme/api"}
	var runs []*v1alpha1.IntentRun
	rounds := int32(r.Intn(6))
	for round := int32(1); round <= rounds; round++ {
		stage, trigger := stages[r.Intn(len(stages))], v1alpha1.IntentRunTrigger("")
		if stage == v1alpha1.IntentStageRevise {
			trigger = triggers[r.Intn(len(triggers))]
		}
		repo := repos[r.Intn(len(repos))]
		for _, attempt := range r.Perm(1 + r.Intn(4)) {
			run := &v1alpha1.IntentRun{
				Spec: v1alpha1.IntentRunSpec{Stage: stage, Round: round, Attempt: int32(attempt + 1),
					Trigger: trigger, Repository: v1alpha1.IntentRunRepository{URL: repo}},
				Status: v1alpha1.IntentRunStatus{Phase: phases[r.Intn(len(phases))],
					Outcome: outcomes[r.Intn(len(outcomes))]},
			}
			if r.Intn(8) == 0 {
				run.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionSandboxRefused,
					Status: metav1.ConditionTrue, Reason: "SandboxUnenforced"}}
			}
			runs = append(runs, run)
		}
	}
	r.Shuffle(len(runs), func(i, j int) { runs[i], runs[j] = runs[j], runs[i] })
	return runs
}

// The dashboard's revision and check-fix counts are the rounds this package
// enforces the limits with, for any runs: a card or a limit-reached reason
// never disagrees with what blocked the intent.
func TestIntentviewRoundCountsPinned(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(20261006)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(randomRuns(r))
		},
	}
	if err := quick.Check(func(runs []*v1alpha1.IntentRun) bool {
		p := &pass{runs: runs}
		return intentview.RevisionRounds(runs) == p.revisionRounds() &&
			intentview.CheckFixRounds(runs) == p.checkFixRounds()
	}, cfg); err != nil {
		t.Error(err)
	}
}

// The two outcomes whose detail names cluster nodes are worded on the issue
// without it; the dashboard says the same words.
func TestIntentviewSharedWordingPinned(t *testing.T) {
	for _, outcome := range []string{OutcomeUnschedulable, OutcomeEvicted} {
		run := &v1alpha1.IntentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-1-bld-r1-app-a1"},
			Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunFailed, Outcome: outcome,
				Detail: "0/3 nodes are available: node ip-10-0-1-23"},
		}
		want := intentview.RunReason(run) + " (run " + run.Name + " records why)"
		if got := failedRunReason(run); got != want {
			t.Errorf("failedRunReason(%s) = %q, want the dashboard's wording %q", outcome, got, want)
		}
	}
}

// Every outcome the controllers record has public wording, so the dashboard
// never falls back to a bare code for one of them.
func TestIntentviewWordsEveryOutcome(t *testing.T) {
	outcomes := []string{
		OutcomeAborted, OutcomeImageRequired, OutcomeNotBuilt, OutcomeBranchExists, OutcomeHoldExpired,
		OutcomePushRefused, OutcomeLaunchRefused, OutcomeHeadMoved, OutcomeInputUnavailable,
		OutcomeNoUsableFeedback, OutcomeUnschedulable, OutcomeEvicted,
		string(envelope.OutcomeOK), string(envelope.OutcomeRuntimeError), string(envelope.OutcomeTimeout),
		string(envelope.OutcomeBudgetExceeded), string(envelope.OutcomeReportMissing),
		string(envelope.OutcomeReportInvalid), string(envelope.OutcomeCommitFailed),
		string(envelope.OutcomeChangesetTooLarge), string(envelope.OutcomeImageIncompatible),
		string(envelope.OutcomeChangesetRejected),
	}
	for _, o := range outcomes {
		if intentview.OutcomeText(o) == "" {
			t.Errorf("outcome %q has no public wording in intentview", o)
		}
	}
}

// Every reason this package blocks an Intent with has its own public wording
// rather than its condition's fallback.
func TestIntentviewWordsEveryBlockReason(t *testing.T) {
	reasons := map[string][]string{
		v1alpha1.ConditionImageRequired: {ReasonNoRepositoryImage, ReasonRepositoryImageRejected,
			ReasonRepositoryImagesOff, ReasonSandboxBreaker, ReasonDefaultImageRan},
		v1alpha1.ConditionBranchConflict: {ReasonBranchExists, ReasonForeignPullRequest, ReasonBranchMissing,
			ReasonBranchChanged, ReasonPullRequestRefused, ReasonStaleRoundBranch},
		v1alpha1.ConditionUnsupportedRepositories: {ReasonMultiRepositoryOff, ReasonRepositoryKeyChanged},
		v1alpha1.ConditionResourcesUnavailable:    {ReasonUnknownResourceClass, ReasonUnschedulable},
		v1alpha1.ConditionChecksFailing:           {"RepeatedFailure", "MaxCheckFixes"},
	}
	reason := func(typ, r string) string {
		in := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Conditions: []metav1.Condition{
			{Type: typ, Status: metav1.ConditionTrue, Reason: r},
		}}}
		got := intentview.BlockedReasons(in, intentview.Limits{}, nil)
		if len(got) != 1 {
			t.Fatalf("BlockedReasons(%s/%s) = %v", typ, r, got)
		}
		return got[0]
	}
	for typ, rs := range reasons {
		fallback := reason(typ, "SomeReasonNobodyUses")
		for _, r := range rs {
			if got := reason(typ, r); got == fallback || strings.TrimSpace(got) == "" {
				t.Errorf("%s/%s has no wording of its own (got %q)", typ, r, got)
			}
		}
	}
}
