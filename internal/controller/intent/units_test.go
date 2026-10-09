// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// quickConfig is a seeded testing/quick configuration, so a property run is
// the same every time.
func quickConfig(seed int64) *quick.Config {
	return &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(seed))}
}

// TestCutBytesExamples pins cutBytes on the edges: no cut, a cut on an ASCII
// boundary, and a cut inside a multi-byte rune, which backs off to its start.
func TestCutBytesExamples(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 0, ""},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"abcdef", 3, "abc"},
		{"abc", 0, ""},
		{"aé", 2, "a"},    // é is 2 bytes; cutting at 2 splits it
		{"aé", 3, "aé"},   // whole
		{"€uro", 1, ""},   // € is 3 bytes
		{"€uro", 3, "€"},  // exactly the rune
		{"x€uro", 3, "x"}, // inside the rune
		{"日本語", 7, "日本"},
	}
	for _, tc := range cases {
		if got := cutBytes(tc.s, tc.n); got != tc.want {
			t.Errorf("cutBytes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

// TestCutBytesProperties: the cut is a prefix of s, at most n bytes, valid
// UTF-8 whenever s is, and the longest such prefix: the next rune would not
// fit.
func TestCutBytesProperties(t *testing.T) {
	prop := func(s string, n uint16) bool {
		limit := int(n) % (len(s) + 4)
		got := cutBytes(s, limit)
		if !strings.HasPrefix(s, got) || len(got) > limit {
			return false
		}
		if len(s) <= limit {
			return got == s
		}
		if utf8.ValidString(s) {
			if !utf8.ValidString(got) {
				return false
			}
			_, size := utf8.DecodeRuneInString(s[len(got):])
			return len(got)+size > limit
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(1)); err != nil {
		t.Error(err)
	}
}

// pushRun is a completed run of stage, round and attempt, finished at
// finished (none when nil).
func pushRun(name string, stage v1alpha1.IntentStage, round, attempt int32, finished *time.Time) *v1alpha1.IntentRun {
	run := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.IntentRunSpec{Stage: stage, Round: round, Attempt: attempt,
			Repository: v1alpha1.IntentRunRepository{URL: appRepoURL}},
		Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, PushedCommit: "c-" + name},
	}
	if finished != nil {
		at := metav1.NewTime(*finished)
		run.Status.FinishedAt = &at
	}
	return run
}

// TestPushedAfter: a later finish wins; a finished run beats an unfinished
// one; at equal finishes a revise round beats the build, then the later
// round, then the later attempt.
func TestPushedAfter(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	cases := []struct {
		name string
		a, b *v1alpha1.IntentRun
		want bool
	}{
		{"later finish", pushRun("a", v1alpha1.IntentStageBuild, 0, 1, &t1),
			pushRun("b", v1alpha1.IntentStageRevise, 3, 1, &t0), true},
		{"earlier finish", pushRun("a", v1alpha1.IntentStageRevise, 3, 1, &t0),
			pushRun("b", v1alpha1.IntentStageBuild, 0, 1, &t1), false},
		{"finished beats unfinished", pushRun("a", v1alpha1.IntentStageBuild, 0, 1, &t0),
			pushRun("b", v1alpha1.IntentStageRevise, 2, 1, nil), true},
		{"unfinished loses", pushRun("a", v1alpha1.IntentStageRevise, 2, 1, nil),
			pushRun("b", v1alpha1.IntentStageBuild, 0, 1, &t0), false},
		{"revise beats build", pushRun("a", v1alpha1.IntentStageRevise, 1, 1, &t0),
			pushRun("b", v1alpha1.IntentStageBuild, 0, 1, &t0), true},
		{"build loses to revise", pushRun("a", v1alpha1.IntentStageBuild, 0, 1, nil),
			pushRun("b", v1alpha1.IntentStageRevise, 1, 1, nil), false},
		{"later round", pushRun("a", v1alpha1.IntentStageRevise, 2, 1, &t0),
			pushRun("b", v1alpha1.IntentStageRevise, 1, 2, &t0), true},
		{"later attempt", pushRun("a", v1alpha1.IntentStageRevise, 1, 2, nil),
			pushRun("b", v1alpha1.IntentStageRevise, 1, 1, nil), true},
		{"same run", pushRun("a", v1alpha1.IntentStageRevise, 1, 1, &t0),
			pushRun("b", v1alpha1.IntentStageRevise, 1, 1, &t0), false},
	}
	for _, tc := range cases {
		if got := pushedAfter(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: pushedAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPushedAfterIsAStrictOrder: pushedAfter is irreflexive, asymmetric and
// transitive over any runs, so latestPushedRun's answer is the one maximum
// whatever order the runs are listed in.
func TestPushedAfterIsAStrictOrder(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	gen := func(r *rand.Rand, i int) *v1alpha1.IntentRun {
		stage := v1alpha1.IntentStageBuild
		if r.Intn(2) == 0 {
			stage = v1alpha1.IntentStageRevise
		}
		var fin *time.Time
		if r.Intn(3) > 0 {
			f := t0.Add(time.Duration(r.Intn(3)) * time.Minute)
			fin = &f
		}
		return pushRun(fmt.Sprintf("r%d", i), stage, int32(r.Intn(3)), int32(1+r.Intn(2)), fin)
	}
	r := rand.New(rand.NewSource(7))
	for range 300 {
		a, b, c := gen(r, 0), gen(r, 1), gen(r, 2)
		if pushedAfter(a, a) {
			t.Fatalf("pushedAfter(a, a) for %+v", a.Spec)
		}
		if pushedAfter(a, b) && pushedAfter(b, a) {
			t.Fatalf("pushedAfter both ways: %+v %+v", a.Spec, b.Spec)
		}
		if pushedAfter(a, b) && pushedAfter(b, c) && !pushedAfter(a, c) {
			t.Fatalf("pushedAfter not transitive: %+v %+v %+v", a.Spec, b.Spec, c.Spec)
		}
	}

	// latestPushedRun's answer does not depend on the listing order, and
	// skips runs that did not push, did not complete, or are elsewhere.
	for range 100 {
		runs := make([]*v1alpha1.IntentRun, 0, 8)
		for i := range 5 {
			runs = append(runs, gen(r, i))
		}
		noPush := pushRun("nopush", v1alpha1.IntentStageRevise, 9, 9, nil)
		noPush.Status.PushedCommit = ""
		failed := pushRun("failed", v1alpha1.IntentStageRevise, 9, 9, nil)
		failed.Status.Phase = v1alpha1.RunFailed
		elsewhere := pushRun("elsewhere", v1alpha1.IntentStageRevise, 9, 9, nil)
		elsewhere.Spec.Repository.URL = "https://github.com/acme/other"
		runs = append(runs, noPush, failed, elsewhere)
		want := (&pass{runs: runs}).latestPushedRun(appRepoURL)
		reversed := make([]*v1alpha1.IntentRun, len(runs))
		for i, run := range runs {
			reversed[len(runs)-1-i] = run
		}
		got := (&pass{runs: reversed}).latestPushedRun(appRepoURL)
		if got == nil || want == nil {
			t.Fatal("latestPushedRun found nothing among pushed runs")
		}
		if pushedAfter(got, want) || pushedAfter(want, got) {
			t.Fatalf("latestPushedRun depends on order: %s vs %s", got.Name, want.Name)
		}
		switch got.Name {
		case "nopush", "failed", "elsewhere":
			t.Fatalf("latestPushedRun chose %s", got.Name)
		}
	}
	if got := (&pass{}).latestPushedRun(appRepoURL); got != nil {
		t.Errorf("latestPushedRun with no runs = %s, want nil", got.Name)
	}
}

// TestSettingsWithDefaults: every unset or negative setting is filled, and
// a set one is kept.
func TestSettingsWithDefaults(t *testing.T) {
	got := Settings{PollInterval: -1, MaxAttempts: -3}.withDefaults()
	if got.PollInterval != DefaultPollInterval || got.ApprovalPollInterval != DefaultApprovalPollInterval ||
		got.PRPollInterval != DefaultPRPollInterval || got.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("withDefaults of an empty Settings = %+v", got)
	}
	set := Settings{PollInterval: 3 * time.Second, ApprovalPollInterval: 4 * time.Second,
		PRPollInterval: 5 * time.Second, MaxAttempts: 7, Namespace: "ns"}
	if got := set.withDefaults(); !reflect.DeepEqual(got, set) {
		t.Errorf("withDefaults changed set settings: %+v, want %+v", got, set)
	}
}

// TestNudger: a nudge is delivered once however often it is repeated until
// taken; take clears it; restore puts it back without a second event; a nil
// Nudger takes nothing; and a full buffer drops the nudge without blocking
// and without leaving it pending.
func TestNudger(t *testing.T) {
	n := NewNudger()
	n.Nudge(testNS, "a")
	n.Nudge(testNS, "a")
	if got := len(n.events); got != 1 {
		t.Fatalf("%d events for a repeated nudge, want 1", got)
	}
	ev := <-n.events
	if ev.Object.GetName() != "a" || ev.Object.GetNamespace() != testNS {
		t.Errorf("event for %s/%s, want %s/a", ev.Object.GetNamespace(), ev.Object.GetName(), testNS)
	}
	if !n.take("a") {
		t.Error("take of a nudged intent = false")
	}
	if n.take("a") {
		t.Error("take twice = true")
	}
	n.restore("a")
	if len(n.events) != 0 {
		t.Error("restore sent an event")
	}
	if !n.take("a") {
		t.Error("take after restore = false")
	}
	// Once taken, a new nudge is delivered again.
	n.Nudge(testNS, "a")
	if len(n.events) != 1 {
		t.Errorf("%d events for a nudge after take, want 1", len(n.events))
	}
	<-n.events
	n.take("a")

	var none *Nudger
	none.restore("a")
	if none.take("a") {
		t.Error("a nil Nudger took a nudge")
	}

	// Fill the buffer; the next nudge is dropped and not left pending.
	full := NewNudger()
	for i := range nudgeBuffer {
		full.Nudge(testNS, fmt.Sprintf("i%d", i))
	}
	full.Nudge(testNS, "dropped")
	if len(full.events) != nudgeBuffer {
		t.Fatalf("%d events, want the buffer's %d", len(full.events), nudgeBuffer)
	}
	if full.take("dropped") {
		t.Error("a dropped nudge is still pending, so the next one would never be sent")
	}
	if full.source() == nil {
		t.Error("source is nil")
	}
}

// TestErrRoundLeased names the round and the run holding it, and is found by
// adoptLeased's errors.As through wrapping.
func TestErrRoundLeased(t *testing.T) {
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "target-1-rev-r1-a1"},
		Spec: v1alpha1.IntentRunSpec{Round: 3}}
	err := fmt.Errorf("lease: %w", &errRoundLeased{run: run})
	if msg := err.Error(); !strings.Contains(msg, "round 3") || !strings.Contains(msg, run.Name) {
		t.Errorf("message %q does not name the round and run", msg)
	}
	adopted, aerr := (&pass{}).adoptLeased(t.Context(), fmt.Errorf("other"))
	if adopted || aerr != nil {
		t.Errorf("adoptLeased of another error = %v, %v; want false, nil", adopted, aerr)
	}
}

// TestJobRunnerMergesStageEnv: each launch's stage env lands on the Job over
// the base env, and never leaks into the base or the next launch.
func TestJobRunnerMergesStageEnv(t *testing.T) {
	cs := k8sfake.NewClientset()
	base := jobs.Config{
		Namespace: "patchy-agents", ServiceAccount: "patchy-agent", Deadline: time.Hour, TTL: time.Hour,
		Runners: map[string]jobs.Runner{"fake": {Image: "ghcr.io/example/runner:1"}},
		Env:     map[string]string{"PATCHY_A": "base", "PATCHY_B": "base"},
	}
	runner := NewJobRunner(cs, base, nil)
	spec := func(attempt int) jobs.Spec {
		return jobs.Spec{Repo: "acme/app", Attempt: attempt, Phase: "plan", Harness: "fake",
			BaseSHA: baseSHA, IssueMarkdown: "# x\n", Kind: KindIntent, Owner: "run", Finding: "run",
			ArtifactURL: "http://artifacts/x.tar.gz", ArtifactDigest: strings.Repeat("a", 64)}
	}
	ctx := t.Context()
	name, _, err := runner.Create(ctx, spec(1), map[string]string{"PATCHY_B": "launch", "PATCHY_C": "launch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := jobEnv(t, cs, name); got["PATCHY_A"] != "base" || got["PATCHY_B"] != "launch" ||
		got["PATCHY_C"] != "launch" {
		t.Errorf("first Job env = %v; want A=base, B=launch, C=launch", got)
	}
	if !reflect.DeepEqual(base.Env, map[string]string{"PATCHY_A": "base", "PATCHY_B": "base"}) {
		t.Errorf("base env changed: %v", base.Env)
	}
	name2, _, err := runner.Create(ctx, spec(2), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := jobEnv(t, cs, name2); got["PATCHY_B"] != "base" || got["PATCHY_C"] != "" {
		t.Errorf("second Job env = %v; the first launch's env leaked", got)
	}

	// A base with no env still takes the launch's.
	bare := NewJobRunner(k8sfake.NewClientset(), jobs.Config{Namespace: "patchy-agents",
		Runners: map[string]jobs.Runner{"fake": {Image: "ghcr.io/example/runner:1"}}}, nil)
	if _, _, err := bare.Create(ctx, jobs.Spec{}, map[string]string{"PATCHY_C": "x"}); err == nil {
		t.Error("an empty Spec was launched")
	}
}

// jobEnv is the literal env of every container of the named Job.
func jobEnv(t *testing.T, cs *k8sfake.Clientset, name string) map[string]string {
	t.Helper()
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return containerEnv(job)
}

func containerEnv(job *batchv1.Job) map[string]string {
	env := map[string]string{}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}
