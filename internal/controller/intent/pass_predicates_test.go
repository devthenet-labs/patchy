// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// predicatePass is a pass over a Blocked Intent of the test Project, with
// conditions set and runs as given, on e's reconciler.
func predicatePass(e *env, conds []metav1.Condition, runs ...*v1alpha1.IntentRun) *pass {
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS},
		Spec: v1alpha1.IntentSpec{Project: "target",
			Issue: v1alpha1.IntentIssue{Repository: intentRepoURL, Number: 1}},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentBlocked, Conditions: conds},
	}
	proj := testProject()
	proj.Generation = 3
	return &pass{r: e.intent, set: testSettings(), in: in, proj: proj, runs: runs, now: e.clock.Now()}
}

func cond(typ, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: reason, Message: msg}
}

// reviseRun is a revise run of round, started by trigger.
func reviseRun(round int32, trigger v1alpha1.IntentRunTrigger) *v1alpha1.IntentRun {
	return &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-" + string(trigger) + "-" + itoa(int64(round))},
		Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Round: round, Attempt: 1, Trigger: trigger,
			Repository: v1alpha1.IntentRunRepository{URL: appRepoURL}},
		Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete},
	}
}

// TestBlockHolds: each block holds exactly while its cause is still in
// force: the spend at the ceiling, the revisions or check fixes spent, a
// repeated failure under the Project's current generation, and a key change
// this process saw under it.
func TestBlockHolds(t *testing.T) {
	zero, two := int32(0), int32(2)
	cases := []struct {
		name   string
		conds  []metav1.Condition
		runs   []*v1alpha1.IntentRun
		usage  int64
		limits v1alpha1.ProjectLimits
		// blockedAt, when set, is the Project generation this process
		// saw the block made under.
		blockedAt *int64
		want      bool
	}{
		{name: "no block", want: false},
		{name: "budget at the ceiling", conds: []metav1.Condition{cond(v1alpha1.ConditionBudgetExhausted, "x", "")},
			usage: 5, limits: v1alpha1.ProjectLimits{MaxCostMicroUSD: 5}, want: true},
		{name: "budget raised past the spend", conds: []metav1.Condition{cond(v1alpha1.ConditionBudgetExhausted, "x", "")},
			usage: 5, limits: v1alpha1.ProjectLimits{MaxCostMicroUSD: 6}, want: false},
		{name: "revisions spent", conds: []metav1.Condition{cond(v1alpha1.ConditionRevisionLimitReached, "x", "")},
			runs: []*v1alpha1.IntentRun{reviseRun(1, v1alpha1.IntentRunTriggerReview),
				reviseRun(2, v1alpha1.IntentRunTriggerCommand)},
			limits: v1alpha1.ProjectLimits{MaxRevisions: &two}, want: true},
		{name: "revision limit raised", conds: []metav1.Condition{cond(v1alpha1.ConditionRevisionLimitReached, "x", "")},
			runs:   []*v1alpha1.IntentRun{reviseRun(1, v1alpha1.IntentRunTriggerReview)},
			limits: v1alpha1.ProjectLimits{MaxRevisions: &two}, want: false},
		{name: "check fixes spent at a limit of zero",
			conds:  []metav1.Condition{cond(v1alpha1.ConditionChecksFailing, "MaxCheckFixes", "")},
			limits: v1alpha1.ProjectLimits{MaxCheckFixes: &zero}, want: true},
		{name: "check-fix limit raised",
			conds:  []metav1.Condition{cond(v1alpha1.ConditionChecksFailing, "MaxCheckFixes", "")},
			runs:   []*v1alpha1.IntentRun{reviseRun(1, v1alpha1.IntentRunTriggerChecks)},
			limits: v1alpha1.ProjectLimits{MaxCheckFixes: &two}, want: false},
		{name: "check fixes spent at the default limit",
			conds: []metav1.Condition{cond(v1alpha1.ConditionChecksFailing, "MaxCheckFixes", "")},
			runs: func() []*v1alpha1.IntentRun {
				runs := make([]*v1alpha1.IntentRun, 0, v1alpha1.DefaultMaxCheckFixes)
				for i := range v1alpha1.DefaultMaxCheckFixes {
					runs = append(runs, reviseRun(i+1, v1alpha1.IntentRunTriggerChecks))
				}
				return runs
			}(), want: true},
		{name: "repeated failure under this generation",
			conds: []metav1.Condition{cond(v1alpha1.ConditionChecksFailing, "RepeatedFailure", "project-generation=3")},
			want:  true},
		{name: "repeated failure under an older generation",
			conds: []metav1.Condition{cond(v1alpha1.ConditionChecksFailing, "RepeatedFailure", "project-generation=2")},
			want:  false},
		{name: "checks failing resolved",
			conds: []metav1.Condition{{Type: v1alpha1.ConditionChecksFailing, Status: metav1.ConditionFalse,
				Reason: "RepeatedFailure", Message: "project-generation=3"}},
			want: false},
		{name: "key change seen under this generation",
			conds:     []metav1.Condition{cond(v1alpha1.ConditionUnsupportedRepositories, ReasonRepositoryKeyChanged, "")},
			blockedAt: ptrTo(int64(3)), want: true},
		{name: "key change seen under an older generation",
			conds:     []metav1.Condition{cond(v1alpha1.ConditionUnsupportedRepositories, ReasonRepositoryKeyChanged, "")},
			blockedAt: ptrTo(int64(2)), want: false},
		{name: "key change this process never saw",
			conds: []metav1.Condition{cond(v1alpha1.ConditionUnsupportedRepositories, ReasonRepositoryKeyChanged, "")},
			want:  false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			p := predicatePass(e, tc.conds, tc.runs...)
			p.in.Status.Usage.CostMicroUSD = tc.usage
			p.proj.Spec.Limits = tc.limits
			if tc.blockedAt != nil {
				e.intent.memo(func() { e.intent.blockedAt[p.in.Name] = *tc.blockedAt })
			}
			got, err := p.blockHolds(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("blockHolds = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestRoundInFlight: a round is in flight while the Intent revises, or is
// blocked from revising; never otherwise.
func TestRoundInFlight(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		phase v1alpha1.IntentPhase
		from  v1alpha1.IntentPhase
		want  bool
	}{
		{v1alpha1.IntentRevising, "", true},
		{v1alpha1.IntentInReview, "", false},
		{v1alpha1.IntentBuilding, "", false},
		{v1alpha1.IntentBlocked, v1alpha1.IntentRevising, true},
		{v1alpha1.IntentBlocked, v1alpha1.IntentBuilding, false},
	} {
		p := predicatePass(e, nil)
		p.in.Status.Phase = tc.phase
		if tc.from != "" {
			p.in.Status.PhaseTimes = []v1alpha1.IntentPhaseTime{{Phase: tc.from}, {Phase: v1alpha1.IntentBlocked}}
		}
		if got := p.roundInFlight(); got != tc.want {
			t.Errorf("roundInFlight in %s (from %q) = %v, want %v", tc.phase, tc.from, got, tc.want)
		}
	}
}

// TestIsOwnAndEverEdited: with no bot identity only a marker makes a comment
// patchy's, and with one only the bot's authorship does; a comment gone
// since it was listed counts as edited, and any other failure to read its
// edits is returned naming it.
func TestIsOwnAndEverEdited(t *testing.T) {
	e := newEnv(t)
	p := predicatePass(e, nil)
	marked := &ghclient.Comment{ID: 1, Body: "<!-- patchy:status -->\nbody", UserLogin: "mallory"}
	plain := &ghclient.Comment{ID: 2, Body: "hello", UserLogin: testBot}
	p.bot = ""
	if !p.isOwn(marked) || p.isOwn(plain) {
		t.Error("with no bot, a marker alone should make a comment patchy's")
	}
	p.bot = testBot
	if p.isOwn(marked) || !p.isOwn(plain) {
		t.Error("with a bot, only its authorship should make a comment patchy's")
	}

	gone := &ghclient.Comment{ID: 3, NodeID: "IC_gone"}
	if edited, err := p.everEdited(context.Background(), gone); err != nil || !edited {
		t.Errorf("everEdited of a gone comment = %v, %v; want true, nil", edited, err)
	}
	e.gh.failNext("CommentEdited", errTransient)
	edited, err := p.everEdited(context.Background(), gone)
	if !errors.Is(err, errTransient) || edited || !strings.Contains(err.Error(), "comment 3") {
		t.Errorf("everEdited on a failed read = %v, %v; want the error naming comment 3", edited, err)
	}
}

// TestFailedIn names the repository a MaxCheckFixes block's checks failed
// in, and nothing for a one-repository Project.
func TestFailedIn(t *testing.T) {
	if got := failedIn(""); got != "" {
		t.Errorf("failedIn(\"\") = %q, want empty", got)
	}
	if got := failedIn(" in acme/app"); got != " (a named check failed in acme/app)" {
		t.Errorf("failedIn = %q", got)
	}
}

// inputPass is a pass whose reconciler reads through c.
func inputPass(c client.Client) *pass {
	r := &IntentReconciler{Client: c, APIReader: c, Settings: testSettings()}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS}}
	return &pass{r: r, set: testSettings(), in: in, proj: testProject()}
}

// TestRoundChecks: a check-fix round's fixed checks are read from its own
// input alone; an input that is gone, not the run's own, or predates the
// record names none, and a failed read is returned.
func TestRoundChecks(t *testing.T) {
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "rev", Namespace: testNS, UID: "run-uid"},
		Spec: v1alpha1.IntentRunSpec{Inputs: v1alpha1.IntentRunInputs{ConfigMap: "rev-input"}}}
	input := func(owner types.UID, names string) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rev-input", Namespace: testNS},
			Data: map[string]string{keyCheckNames: names}}
		if owner != "" {
			cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "patchy.bitwisemedia.uk/v1alpha1",
				Kind: "IntentRun", Name: "rev", UID: owner, Controller: ptrTo(true)}}
		}
		return cm
	}
	cases := []struct {
		name    string
		cm      *corev1.ConfigMap
		getErr  error
		want    []string
		wantErr bool
	}{
		{name: "gone"},
		{name: "own", cm: input("run-uid", "test\nlint"), want: []string{"test", "lint"}},
		{name: "another run's", cm: input("other-uid", "test")},
		{name: "unowned", cm: input("", "test")},
		{name: "predates the record", cm: input("run-uid", "")},
		{name: "read fails", getErr: errTransient, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(kube.Scheme())
			if tc.cm != nil {
				b = b.WithObjects(tc.cm)
			}
			if tc.getErr != nil {
				b = b.WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch,
					client.ObjectKey, client.Object, ...client.GetOption) error {
					return tc.getErr
				}})
			}
			got, err := inputPass(b.Build()).roundChecks(context.Background(), run)
			if (err != nil) != tc.wantErr || (tc.wantErr && !errors.Is(err, tc.getErr)) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("roundChecks = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreviousPlan: a replan is handed the previous plan fenced and escaped
// only while its ConfigMap still holds exactly the recorded bytes; a plan
// gone or changed is handed as nothing, and a failed read is returned.
func TestPreviousPlan(t *testing.T) {
	const plan = "## Approach\n\nUse ``` fences \u202e here.\n"
	prev := &v1alpha1.IntentPlan{Revision: 1, ConfigMap: "target-1-plan-r1", Digest: digest([]byte(plan))}
	stored := func(body string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "target-1-plan-r1", Namespace: testNS},
			Data: map[string]string{keyPlan: body}}
	}
	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).Build()
	}

	if got, err := inputPass(build()).previousPlan(context.Background(), nil); got != "" || err != nil {
		t.Errorf("no previous plan = %q, %v; want nothing", got, err)
	}
	got, err := inputPass(build(stored(plan))).previousPlan(context.Background(), prev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Use ``` fences") || !strings.Contains(got, "<U+202E>") ||
		strings.ContainsRune(got, '\u202e') || !strings.HasPrefix(got, "````") {
		t.Errorf("previous plan not escaped and fenced:\n%s", got)
	}
	if got, err := inputPass(build()).previousPlan(context.Background(), prev); got != "" || err != nil {
		t.Errorf("a gone plan = %q, %v; want nothing", got, err)
	}
	if got, err := inputPass(build(stored(plan+"tampered"))).previousPlan(context.Background(), prev); got != "" ||
		err != nil {
		t.Errorf("a changed plan = %q, %v; want nothing", got, err)
	}
	failing := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errTransient
		}}).Build()
	if _, err := inputPass(failing).previousPlan(context.Background(), prev); !errors.Is(err, errTransient) {
		t.Errorf("a failed read = %v, want %v", err, errTransient)
	}
}
