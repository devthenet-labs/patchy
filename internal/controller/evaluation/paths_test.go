// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evaluation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/pkg/evaluation"
)

var errBoom = errors.New("boom")

// errRunner is fakeRunner with per-call failures injected.
type errRunner struct {
	*fakeRunner
	createErr, statusErr, linesErr, deleteErr error
}

func (e *errRunner) CreateEval(ctx context.Context, spec jobs.EvalSpec) (string, error) {
	if e.createErr != nil {
		return "", e.createErr
	}
	return e.fakeRunner.CreateEval(ctx, spec)
}

func (e *errRunner) Status(ctx context.Context, name string) (jobs.Status, error) {
	if e.statusErr != nil {
		return jobs.Status{}, e.statusErr
	}
	return e.fakeRunner.Status(ctx, name)
}

func (e *errRunner) ResultLines(ctx context.Context, name string, fn func([]byte) error) error {
	if err := e.fakeRunner.ResultLines(ctx, name, fn); err != nil {
		return err
	}
	return e.linesErr
}

func (e *errRunner) Delete(ctx context.Context, name string) error {
	_ = e.fakeRunner.Delete(ctx, name)
	return e.deleteErr
}

// errWorkspaces fails every Stat.
type errWorkspaces struct{}

func (errWorkspaces) Stat(context.Context, string) (bool, error) { return false, errBoom }

// clientWith is newClient with interceptors.
func clientWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Evaluation{}, &v1alpha1.EvaluationUnit{}).
		WithInterceptorFuncs(funcs).
		Build()
}

func unitReq(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: name}}
}

var present = fakeWorkspaces{present: map[string]bool{digestA: true}}

// failStatusWrites fails every status subresource update.
var failStatusWrites = interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string,
	client.Object, ...client.SubResourceUpdateOption) error {
	return errBoom
}}

// TestGateIgnores: a missing, deleting or already-expanded Evaluation is a
// no-op that creates no children.
func TestGateIgnores(t *testing.T) {
	now := metav1.NewTime(clock)
	tests := []struct {
		name string
		mut  func(*v1alpha1.Evaluation)
	}{
		{"deleting", func(e *v1alpha1.Evaluation) {
			e.DeletionTimestamp = &now
			e.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
		}},
		{"running", func(e *v1alpha1.Evaluation) { e.Status.Phase = v1alpha1.EvaluationRunning }},
		{"complete", func(e *v1alpha1.Evaluation) { e.Status.Phase = v1alpha1.EvaluationComplete }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := testEval(unitPlan("claude"))
			tt.mut(eval)
			c := newClient(t, eval)
			g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude"}}
			if _, err := g.Reconcile(t.Context(), unitReq("eval-1")); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			var units v1alpha1.EvaluationUnitList
			if err := c.List(t.Context(), &units); err != nil {
				t.Fatal(err)
			}
			if len(units.Items) != 0 {
				t.Errorf("units = %d, want none", len(units.Items))
			}
		})
	}
	g := &GateReconciler{Client: newClient(t), Namespace: "patchy"}
	if _, err := g.Reconcile(t.Context(), unitReq("missing")); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// TestGateRealClockStampsStart: with no clock seam the gate still stamps
// the start time and the UnitsCreated condition.
func TestGateRealClockStampsStart(t *testing.T) {
	c := newClient(t, testEval(unitPlan("claude"), unitPlan("fake")))
	g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude", "fake"}}
	before := time.Now().Add(-time.Second)
	if _, err := g.Reconcile(t.Context(), unitReq("eval-1")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	e := getEval(t, c)
	if e.Status.Phase != v1alpha1.EvaluationRunning || e.Status.Units != 2 {
		t.Errorf("eval = %q units %d, want Running/2", e.Status.Phase, e.Status.Units)
	}
	if e.Status.StartedAt == nil || e.Status.StartedAt.Time.Before(before) {
		t.Errorf("startedAt = %v, want now", e.Status.StartedAt)
	}
	var cond *metav1.Condition
	for i := range e.Status.Conditions {
		if e.Status.Conditions[i].Type == v1alpha1.ConditionUnitsCreated {
			cond = &e.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Message != "2 units created" {
		t.Errorf("UnitsCreated = %+v, want 2 units created", cond)
	}
	u := getUnit(t, c, "eval-1-u001")
	if u.Spec.Index != 1 || u.Labels[v1alpha1.LabelUnitIndex] != "1" || u.Spec.EvaluationRef.UID != "eval-uid-1" {
		t.Errorf("unit 1 = %+v %v", u.Spec, u.Labels)
	}
}

// TestGateErrors: a failed child create or parent status write is returned
// so the expansion resumes; the parent stays Pending.
func TestGateErrors(t *testing.T) {
	for name, funcs := range map[string]interceptor.Funcs{
		"create": {Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errBoom
		}},
		"status": failStatusWrites,
	} {
		t.Run(name, func(t *testing.T) {
			c := clientWith(t, funcs, testEval(unitPlan("claude")))
			g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude"}}
			if _, err := g.Reconcile(t.Context(), unitReq("eval-1")); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if e := getEval(t, c); e.Status.Phase != "" {
				t.Errorf("phase = %q, want still Pending", e.Status.Phase)
			}
		})
	}
}

// TestGateSettleUnavailableDetail: the unavailable unit names every
// preference and the fleet, and an already-settled unit is not rewritten.
func TestGateSettleUnavailableDetail(t *testing.T) {
	plan := unitPlan("gemini")
	plan.Harnesses = append(plan.Harnesses, v1alpha1.HarnessOption{Harness: "codex"})
	eval := testEval(plan)
	settled := &v1alpha1.EvaluationUnit{
		ObjectMeta: metav1.ObjectMeta{Name: "eval-1-u000", Namespace: "patchy"},
		Spec:       v1alpha1.EvaluationUnitSpec{EvaluationRef: v1alpha1.ObjectReference{Name: "eval-1"}, Unit: plan},
		Status:     v1alpha1.EvaluationUnitStatus{Phase: v1alpha1.RunComplete},
	}
	t.Run("fresh", func(t *testing.T) {
		c := newClient(t, eval.DeepCopy())
		g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude"},
			Now: func() time.Time { return clock }}
		if _, err := g.Reconcile(t.Context(), unitReq("eval-1")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		u := getUnit(t, c, "eval-1-u000")
		if u.Status.Detail != "no enabled harness among [gemini codex] (fleet: [claude])" {
			t.Errorf("detail = %q", u.Status.Detail)
		}
		if u.Status.FinishedAt == nil || !u.Status.FinishedAt.Time.Equal(clock) {
			t.Errorf("finishedAt = %v, want %v", u.Status.FinishedAt, clock)
		}
	})
	t.Run("already settled", func(t *testing.T) {
		c := newClient(t, eval.DeepCopy(), settled)
		g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude"}}
		if _, err := g.Reconcile(t.Context(), unitReq("eval-1")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if u := getUnit(t, c, "eval-1-u000"); u.Status.Phase != v1alpha1.RunComplete || u.Status.Reason != "" {
			t.Errorf("unit = %q %q, want the settled Complete untouched", u.Status.Phase, u.Status.Reason)
		}
	})
}

// TestResolveHarnessProperties: resolveHarness returns "" exactly when no
// preference is enabled, and otherwise the first preference that is.
func TestResolveHarnessProperties(t *testing.T) {
	universe := []string{"claude", "codex", "copilot", "fake", "gemini"}
	prop := func(prefMask, enabledMask uint8, order []uint8) bool {
		var prefs []v1alpha1.HarnessOption
		for _, o := range order {
			h := universe[int(o)%len(universe)]
			if prefMask&(1<<(o%5)) != 0 {
				prefs = append(prefs, v1alpha1.HarnessOption{Harness: h})
			}
		}
		var enabled []string
		for i, h := range universe {
			if enabledMask&(1<<i) != 0 {
				enabled = append(enabled, h)
			}
		}
		got := resolveHarness(prefs, enabled)
		for _, p := range prefs {
			if slices.Contains(enabled, p.Harness) {
				return got == p.Harness
			}
		}
		return got == ""
	}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20260812))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func ttlEval(phase v1alpha1.EvaluationPhase, completedAgo time.Duration, override *int32) *v1alpha1.Evaluation {
	eval := testEval(unitPlan("claude"))
	eval.Status.Phase = phase
	if completedAgo >= 0 {
		done := metav1.NewTime(clock.Add(-completedAgo))
		eval.Status.CompletedAt = &done
	}
	eval.Spec.TTLSecondsAfterFinished = override
	return eval
}

// TestTTLKeeps: an unfinished, never-completed or keep-forever Evaluation
// is not deleted and not requeued.
func TestTTLKeeps(t *testing.T) {
	zero := int32(0)
	now := metav1.NewTime(clock)
	deleting := ttlEval(v1alpha1.EvaluationComplete, 1000*time.Hour, nil)
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
	tests := []struct {
		name string
		eval *v1alpha1.Evaluation
		ttl  time.Duration
	}{
		{"running", ttlEval(v1alpha1.EvaluationRunning, 1000*time.Hour, nil), -1},
		{"no completion time", ttlEval(v1alpha1.EvaluationFailed, -1, nil), -1},
		{"ttl zero keeps forever", ttlEval(v1alpha1.EvaluationComplete, 1000*time.Hour, nil), 0},
		{"spec override zero keeps forever", ttlEval(v1alpha1.EvaluationFailed, 1000*time.Hour, &zero), -1},
		{"already deleting", deleting, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.eval)
			r := &TTLReconciler{Client: c, Namespace: "patchy", TTL: tt.ttl, Now: func() time.Time { return clock }}
			res, err := r.Reconcile(t.Context(), unitReq("eval-1"))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.RequeueAfter != 0 {
				t.Errorf("requeue = %v, want none", res.RequeueAfter)
			}
			getEval(t, c) // still present
		})
	}
	r := &TTLReconciler{Client: newClient(t), Namespace: "patchy"}
	if _, err := r.Reconcile(t.Context(), unitReq("missing")); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// TestTTLConfiguredRetention: a positive TTL is honoured as given, and the
// real clock is used when no seam is set.
func TestTTLConfiguredRetention(t *testing.T) {
	eval := testEval(unitPlan("claude"))
	eval.Status.Phase = v1alpha1.EvaluationFailed
	done := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	eval.Status.CompletedAt = &done
	c := newClient(t, eval)
	r := &TTLReconciler{Client: c, Namespace: "patchy", TTL: time.Hour, Log: slog.New(slog.DiscardHandler)}
	if _, err := r.Reconcile(t.Context(), unitReq("eval-1")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var e v1alpha1.Evaluation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "eval-1"}, &e); !kerrors.IsNotFound(err) {
		t.Errorf("evaluation lookup error = %v, want deleted after its 1h TTL", err)
	}
}

// TestTTLDeleteErrors: a failed delete is returned for a retry; one already
// gone is not an error.
func TestTTLDeleteErrors(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Resource: "evaluations"}, "eval-1")
	for name, tt := range map[string]struct {
		err     error
		wantErr bool
	}{"transient": {errBoom, true}, "already gone": {gone, false}} {
		t.Run(name, func(t *testing.T) {
			c := clientWith(t, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object,
				...client.DeleteOption) error {
				return tt.err
			}}, ttlEval(v1alpha1.EvaluationComplete, 1000*time.Hour, nil))
			r := &TTLReconciler{Client: c, Namespace: "patchy", TTL: -1, Now: func() time.Time { return clock }}
			if _, err := r.Reconcile(t.Context(), unitReq("eval-1")); (err != nil) != tt.wantErr {
				t.Errorf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// pendingUnit is a Pending unit at index i of eval-1.
func pendingUnit(i int) *v1alpha1.EvaluationUnit {
	return &v1alpha1.EvaluationUnit{
		ObjectMeta: metav1.ObjectMeta{
			Name: UnitName("eval-1", i), Namespace: "patchy",
			Labels:            map[string]string{v1alpha1.LabelEvaluation: "eval-1"},
			CreationTimestamp: metav1.NewTime(clock.Add(time.Duration(i) * time.Second)),
		},
		Spec: v1alpha1.EvaluationUnitSpec{
			EvaluationRef: v1alpha1.ObjectReference{Name: "eval-1", UID: "eval-uid-1"},
			Index:         int32(i), Unit: unitPlan("claude"),
		},
	}
}

// TestSchedulerSlots: running units consume slots, deleting units are not
// granted, and MaxConcurrent <= 0 means four.
func TestSchedulerSlots(t *testing.T) {
	now := metav1.NewTime(clock)
	tests := []struct {
		name        string
		maxConc     int
		mut         func([]*v1alpha1.EvaluationUnit)
		wantRunning []string
	}{
		{"running consumes a slot", 2, func(u []*v1alpha1.EvaluationUnit) {
			u[4].Status.Phase = v1alpha1.RunRunning
		}, []string{"eval-1-u000", "eval-1-u004"}},
		{"zero means four", 0, func([]*v1alpha1.EvaluationUnit) {},
			[]string{"eval-1-u000", "eval-1-u001", "eval-1-u002", "eval-1-u003"}},
		{"deleting skipped", 1, func(u []*v1alpha1.EvaluationUnit) {
			u[0].DeletionTimestamp = &now
			u[0].Finalizers = []string{v1alpha1.FinalizerJobs}
		}, []string{"eval-1-u001"}},
		{"settled units take no slot", 1, func(u []*v1alpha1.EvaluationUnit) {
			u[0].Status.Phase = v1alpha1.RunComplete
			u[1].Status.Phase = v1alpha1.RunFailed
		}, []string{"eval-1-u002"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			units := make([]*v1alpha1.EvaluationUnit, 5)
			objs := make([]client.Object, 5)
			for i := range units {
				units[i] = pendingUnit(i)
				objs[i] = units[i]
			}
			tt.mut(units)
			c := newClient(t, objs...)
			r := newUnitReconciler(c, &fakeRunner{}, present)
			r.MaxConcurrent = tt.maxConc
			res, err := r.Reconcile(t.Context(), unitReq(schedulerRequest))
			if err != nil {
				t.Fatalf("schedule: %v", err)
			}
			if res.RequeueAfter != 5*time.Minute {
				t.Errorf("requeue = %v, want the 5m safety tick", res.RequeueAfter)
			}
			var list v1alpha1.EvaluationUnitList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, u := range list.Items {
				if u.Status.Phase == v1alpha1.RunRunning {
					got = append(got, u.Name)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantRunning) {
				t.Errorf("running = %v, want %v", got, tt.wantRunning)
			}
		})
	}
}

// TestSchedulerErrors: a failed list or grant write is returned.
func TestSchedulerErrors(t *testing.T) {
	for name, funcs := range map[string]interceptor.Funcs{
		"list": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errBoom
		}},
		"grant": failStatusWrites,
	} {
		t.Run(name, func(t *testing.T) {
			r := newUnitReconciler(clientWith(t, funcs, pendingUnit(0)), &fakeRunner{}, present)
			if _, err := r.Reconcile(t.Context(), unitReq(schedulerRequest)); !errors.Is(err, errBoom) {
				t.Errorf("schedule error = %v, want %v", err, errBoom)
			}
		})
	}
}

// TestUnitRunIgnores: a missing unit, and a Pending or settled one, are
// no-ops.
func TestUnitRunIgnores(t *testing.T) {
	for _, phase := range []v1alpha1.RunPhase{v1alpha1.RunPending, v1alpha1.RunComplete, v1alpha1.RunFailed} {
		t.Run(string(phase), func(t *testing.T) {
			u := runningUnit(nil)
			u.Status.Phase = phase
			runner := &fakeRunner{}
			c := newClient(t, testEval(unitPlan("claude")), u)
			reconcileUnit(t, newUnitReconciler(c, runner, present), u.Name)
			if len(runner.created) != 0 {
				t.Errorf("jobs = %d, want none", len(runner.created))
			}
			if got := getUnit(t, c, u.Name).Status.Phase; got != phase {
				t.Errorf("phase = %q, want %q", got, phase)
			}
		})
	}
	reconcileUnit(t, newUnitReconciler(newClient(t), &fakeRunner{}, present), "missing")
}

// TestLaunchHarnessGone: a unit whose harness left the fleet between the
// gate and the launch is settled HarnessUnavailable, with no Job.
func TestLaunchHarnessGone(t *testing.T) {
	u := runningUnit(nil)
	runner := &fakeRunner{}
	c := newClient(t, testEval(unitPlan("claude")), u)
	r := newUnitReconciler(c, runner, present)
	r.EnabledHarnesses = []string{"fake"}
	reconcileUnit(t, r, u.Name)
	if len(runner.created) != 0 {
		t.Errorf("jobs = %d, want none", len(runner.created))
	}
	got := getUnit(t, c, u.Name)
	if got.Status.Phase != v1alpha1.RunFailed || got.Status.Reason != v1alpha1.UnitHarnessUnavailable ||
		got.Status.Detail != "no enabled harness (fleet: [fake])" {
		t.Errorf("unit = %q %q %q, want Failed/HarnessUnavailable", got.Status.Phase, got.Status.Reason, got.Status.Detail)
	}
}

// TestLaunchErrors: a workspace check or Job create that fails is returned
// and the unit stays Running without a Job, for the next pass.
func TestLaunchErrors(t *testing.T) {
	tests := []struct {
		name    string
		runner  Runner
		ws      WorkspaceStat
		wantMsg string
	}{
		{"stat", &fakeRunner{}, errWorkspaces{}, "stat workspace"},
		{"create", &errRunner{fakeRunner: &fakeRunner{}, createErr: errBoom}, present, "launch unit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := runningUnit(nil)
			c := newClient(t, testEval(unitPlan("claude")), u)
			r := newUnitReconciler(c, nil, fakeWorkspaces{})
			r.Runner, r.Workspaces = tt.runner, tt.ws
			_, err := r.Reconcile(t.Context(), unitReq(u.Name))
			if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Reconcile error = %v, want %q wrapping %v", err, tt.wantMsg, errBoom)
			}
			if got := getUnit(t, c, u.Name); got.Status.Phase != v1alpha1.RunRunning || got.Status.JobRef != nil {
				t.Errorf("unit = %q jobRef %+v, want Running without a Job", got.Status.Phase, got.Status.JobRef)
			}
		})
	}
}

// TestLaunchArtifactURL: the artifact URL is built from the base without a
// doubled slash.
func TestLaunchArtifactURL(t *testing.T) {
	u := runningUnit(nil)
	runner := &fakeRunner{}
	r := newUnitReconciler(newClient(t, testEval(unitPlan("claude")), u), runner, present)
	r.ArtifactBaseURL = "http://arts.local:9790/"
	reconcileUnit(t, r, u.Name)
	if len(runner.created) != 1 {
		t.Fatalf("jobs = %d, want 1", len(runner.created))
	}
	want := "http://arts.local:9790/artifacts/" + digestA + ".tar.gz"
	if got := runner.created[0].ArtifactURL; got != want {
		t.Errorf("artifact URL = %q, want %q", got, want)
	}
	if got := string(runner.created[0].UnitJSON); got != unitPlan("claude").ExecJSON {
		t.Errorf("unit JSON = %q, want the plan's exec JSON", got)
	}
}

// collectFixture is a running unit with a Job reference and its parent.
func collectFixture(units int) (*v1alpha1.EvaluationUnit, []client.Object, string) {
	plans := make([]v1alpha1.UnitPlan, units)
	for i := range plans {
		plans[i] = unitPlan("claude")
	}
	u := runningUnit(nil)
	job := jobs.EvalNameFor(u.Name)
	u.Status.JobRef = &v1alpha1.JobReference{Name: job}
	return u, []client.Object{testEval(plans...), u}, job
}

func fatalLine(t *testing.T, msg string) string {
	t.Helper()
	line, err := (evaluation.Event{Type: evaluation.TypeFatal, Error: msg}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// TestCollectOutcomes: the unit's terminal state from what the Job left.
func TestCollectOutcomes(t *testing.T) {
	result := func(passed int) *evaluation.UnitResult {
		return &evaluation.UnitResult{Harness: "claude", Summary: evaluation.ResultSummary{CasesPassed: passed}}
	}
	tests := []struct {
		name       string
		lines      func(t *testing.T) []string
		linesErr   error
		wantPhase  v1alpha1.RunPhase
		wantReason v1alpha1.UnitFailureReason
		wantDetail string
		wantPassed int32
	}{
		{"last result wins", func(t *testing.T) []string {
			return []string{resultLine(t, result(1)), resultLine(t, result(7))}
		}, nil, v1alpha1.RunComplete, "", "", 7},
		{"result beats a fatal", func(t *testing.T) []string {
			return []string{fatalLine(t, "transient"), resultLine(t, result(2))}
		}, nil, v1alpha1.RunComplete, "", "", 2},
		{"first fatal wins", func(t *testing.T) []string {
			return []string{fatalLine(t, "first"), fatalLine(t, "second")}
		}, nil, v1alpha1.RunFailed, v1alpha1.UnitJobFailed, "first", 0},
		{"unreadable log is a failed run", func(*testing.T) []string { return nil }, errBoom,
			v1alpha1.RunFailed, v1alpha1.UnitJobFailed, "agent job finished without a result event", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, objs, job := collectFixture(1)
			runner := &errRunner{fakeRunner: &fakeRunner{
				status: map[string]jobs.Status{job: {Done: true, Succeeded: 1}},
				lines:  map[string][]string{job: tt.lines(t)},
			}, linesErr: tt.linesErr}
			c := newClient(t, objs...)
			r := newUnitReconciler(c, nil, present)
			r.Runner = runner
			if _, err := r.Reconcile(t.Context(), unitReq(u.Name)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := getUnit(t, c, u.Name)
			if got.Status.Phase != tt.wantPhase || got.Status.Reason != tt.wantReason ||
				got.Status.Detail != tt.wantDetail || got.Status.CasesPassed != tt.wantPassed {
				t.Errorf("unit = %q %q %q passed %d, want %q %q %q passed %d", got.Status.Phase, got.Status.Reason,
					got.Status.Detail, got.Status.CasesPassed, tt.wantPhase, tt.wantReason, tt.wantDetail, tt.wantPassed)
			}
			if got.Status.FinishedAt == nil || !got.Status.FinishedAt.Time.Equal(clock) {
				t.Errorf("finishedAt = %v, want %v", got.Status.FinishedAt, clock)
			}
		})
	}
}

// TestCollectJobStates: a vanished Job aborts the unit, an unfinished one
// waits, and a transient status error is returned.
func TestCollectJobStates(t *testing.T) {
	t.Run("vanished", func(t *testing.T) {
		u, objs, job := collectFixture(1)
		c := newClient(t, objs...)
		reconcileUnit(t, newUnitReconciler(c, &fakeRunner{gone: map[string]bool{job: true}}, present), u.Name)
		got := getUnit(t, c, u.Name)
		if got.Status.Phase != v1alpha1.RunFailed || got.Status.Reason != v1alpha1.UnitAborted {
			t.Errorf("unit = %q %q, want Failed/Aborted", got.Status.Phase, got.Status.Reason)
		}
	})
	t.Run("running", func(t *testing.T) {
		u, objs, job := collectFixture(1)
		c := newClient(t, objs...)
		res := reconcileUnit(t, newUnitReconciler(c, &fakeRunner{
			status: map[string]jobs.Status{job: {Active: 1}},
		}, present), u.Name)
		if res.RequeueAfter != 0 || getUnit(t, c, u.Name).Status.Phase != v1alpha1.RunRunning {
			t.Errorf("requeue %v phase %q, want the Job watch to wake a Running unit", res.RequeueAfter,
				getUnit(t, c, u.Name).Status.Phase)
		}
	})
	t.Run("status error", func(t *testing.T) {
		u, objs, _ := collectFixture(1)
		c := newClient(t, objs...)
		r := newUnitReconciler(c, nil, present)
		r.Runner = &errRunner{fakeRunner: &fakeRunner{}, statusErr: errBoom}
		if _, err := r.Reconcile(t.Context(), unitReq(u.Name)); !errors.Is(err, errBoom) {
			t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
		}
		if got := getUnit(t, c, u.Name); got.Status.Phase != v1alpha1.RunRunning {
			t.Errorf("phase = %q, want Running", got.Status.Phase)
		}
	})
}

// TestApplyEmptyEntryWritesNoResults: a result with no entry completes the
// unit with no results ConfigMap.
func TestApplyEmptyEntryWritesNoResults(t *testing.T) {
	u, objs, job := collectFixture(1)
	c := newClient(t, objs...)
	runner := &fakeRunner{
		status: map[string]jobs.Status{job: {Done: true, Succeeded: 1}},
		lines: map[string][]string{job: {resultLine(t, &evaluation.UnitResult{
			Harness: "claude", Summary: evaluation.ResultSummary{CasesPassed: 1},
		})}},
	}
	reconcileUnit(t, newUnitReconciler(c, runner, present), u.Name)
	got := getUnit(t, c, u.Name)
	if got.Status.Phase != v1alpha1.RunComplete || got.Status.ResultsRef != nil {
		t.Errorf("unit = %q resultsRef %+v, want Complete with none", got.Status.Phase, got.Status.ResultsRef)
	}
	var cms corev1.ConfigMapList
	if err := c.List(t.Context(), &cms); err != nil {
		t.Fatal(err)
	}
	if len(cms.Items) != 0 {
		t.Errorf("configmaps = %d, want none", len(cms.Items))
	}
}

// TestApplyPersistFailureKeepsSummary: a results ConfigMap that cannot be
// written degrades the unit to summary-only; it still completes.
func TestApplyPersistFailureKeepsSummary(t *testing.T) {
	u, objs, job := collectFixture(1)
	c := clientWith(t, interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object,
		opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			return errBoom
		}
		return cl.Create(ctx, obj, opts...)
	}}, objs...)
	runner := &fakeRunner{
		status: map[string]jobs.Status{job: {Done: true, Succeeded: 1}},
		lines: map[string][]string{job: {resultLine(t, &evaluation.UnitResult{
			Harness: "claude", Summary: evaluation.ResultSummary{CasesPassed: 4},
			Entry: []byte(`{"schema":5}`),
		})}},
	}
	reconcileUnit(t, newUnitReconciler(c, runner, present), u.Name)
	got := getUnit(t, c, u.Name)
	if got.Status.Phase != v1alpha1.RunComplete || got.Status.ResultsRef != nil || got.Status.CasesPassed != 4 {
		t.Errorf("unit = %q resultsRef %+v passed %d, want Complete summary-only with 4 passed",
			got.Status.Phase, got.Status.ResultsRef, got.Status.CasesPassed)
	}
}

// TestStampSummaryBounds: case lists are capped at 256 entries with IDs cut
// to 128 bytes, and the harness falls back to the result's when unset.
func TestStampSummaryBounds(t *testing.T) {
	cases := make([]evaluation.CaseStatus, 300)
	for i := range cases {
		cases[i] = evaluation.CaseStatus{ID: fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 200)), Passed: i%2 == 0}
	}
	res := &evaluation.UnitResult{Harness: "fake", Summary: evaluation.ResultSummary{
		CasesPassed: 150, CasesFailed: 140, CasesErrored: 10, Cases: cases, ElapsedMS: 1234,
		TokenUsage: evaluation.TokenUsage{InputTokens: 5, OutputTokens: 6, CacheReadTokens: 7,
			CacheCreationTokens: 8, CostUSD: 0.5},
	}}
	var st v1alpha1.EvaluationUnitStatus
	stampSummary(&st, res)
	if len(st.Cases) != 256 {
		t.Fatalf("cases = %d, want 256", len(st.Cases))
	}
	for i, c := range st.Cases {
		if len(c.ID) != 128 || !strings.HasPrefix(c.ID, fmt.Sprintf("%03d-", i)) || c.Passed != (i%2 == 0) {
			t.Fatalf("case %d = %q (%d bytes) passed %v", i, c.ID, len(c.ID), c.Passed)
		}
	}
	if st.Harness != "fake" || st.CasesErrored != 10 || st.ElapsedMilliseconds != 1234 {
		t.Errorf("status = harness %q errored %d elapsed %d", st.Harness, st.CasesErrored, st.ElapsedMilliseconds)
	}
	want := v1alpha1.UsageSummary{InputTokens: 5, OutputTokens: 6, CacheReadTokens: 7, CacheCreationTokens: 8,
		CostUSD: "0.500000"}
	if st.Usage != want {
		t.Errorf("usage = %+v, want %+v", st.Usage, want)
	}
	st2 := v1alpha1.EvaluationUnitStatus{Harness: "claude"}
	stampSummary(&st2, res)
	if st2.Harness != "claude" {
		t.Errorf("harness = %q, want the launch-resolved claude kept", st2.Harness)
	}
}

// TestRollupParentPartial: with one of two units settled, the parent counts
// it but stays Running.
func TestRollupParentPartial(t *testing.T) {
	u, objs, job := collectFixture(2)
	objs[0].(*v1alpha1.Evaluation).Status.Phase = v1alpha1.EvaluationRunning
	other := pendingUnit(1)
	c := newClient(t, append(objs, other)...)
	reconcileUnit(t, newUnitReconciler(c, &fakeRunner{gone: map[string]bool{job: true}}, present), u.Name)
	e := getEval(t, c)
	if e.Status.Phase != v1alpha1.EvaluationRunning || e.Status.UnitsFailed != 1 || e.Status.UnitsComplete != 0 ||
		e.Status.Units != 2 || e.Status.CompletedAt != nil {
		t.Errorf("parent = %q failed %d complete %d units %d completedAt %v, want Running 1/0/2 with no completion",
			e.Status.Phase, e.Status.UnitsFailed, e.Status.UnitsComplete, e.Status.Units, e.Status.CompletedAt)
	}
}

// TestRollupParentTerminalUntouched: a parent already settled is never
// rewritten by a late unit.
func TestRollupParentTerminalUntouched(t *testing.T) {
	u, objs, job := collectFixture(1)
	done := metav1.NewTime(clock.Add(-time.Hour))
	objs[0].(*v1alpha1.Evaluation).Status = v1alpha1.EvaluationStatus{
		Phase: v1alpha1.EvaluationComplete, CompletedAt: &done, UnitsComplete: 1, Units: 1,
	}
	c := newClient(t, objs...)
	reconcileUnit(t, newUnitReconciler(c, &fakeRunner{gone: map[string]bool{job: true}}, present), u.Name)
	e := getEval(t, c)
	if e.Status.Phase != v1alpha1.EvaluationComplete || e.Status.UnitsFailed != 0 || !e.Status.CompletedAt.Equal(&done) {
		t.Errorf("parent = %+v, want the settled Complete untouched", e.Status)
	}
}

// TestRollupParentListError: a failed child list is returned after the unit
// settles.
func TestRollupParentListError(t *testing.T) {
	u, objs, job := collectFixture(1)
	c := clientWith(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return errBoom
	}}, objs...)
	r := newUnitReconciler(c, &fakeRunner{gone: map[string]bool{job: true}}, present)
	if _, err := r.Reconcile(t.Context(), unitReq(u.Name)); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if got := getUnit(t, c, u.Name); got.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("unit phase = %q, want Failed (settled before the rollup)", got.Status.Phase)
	}
}

// TestSettleWriteError: a unit status write that fails is returned and the
// parent is not rolled up.
func TestSettleWriteError(t *testing.T) {
	u, objs, job := collectFixture(1)
	c := clientWith(t, failStatusWrites, objs...)
	r := newUnitReconciler(c, &fakeRunner{gone: map[string]bool{job: true}}, present)
	if _, err := r.Reconcile(t.Context(), unitReq(u.Name)); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if e := getEval(t, c); e.Status.Phase != "" {
		t.Errorf("parent phase = %q, want untouched", e.Status.Phase)
	}
}

// TestFinalizeEdges: a failed Job delete keeps the finalizer, an absent Job
// does not block, and other finalizers are kept.
func TestFinalizeEdges(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "x")
	tests := []struct {
		name     string
		err      error
		fins     []string
		wantErr  bool
		wantFins []string
	}{
		{"delete fails", errBoom, []string{v1alpha1.FinalizerJobs}, true, []string{v1alpha1.FinalizerJobs}},
		{"job already gone", gone, []string{v1alpha1.FinalizerJobs, "patchy.bitwisemedia.uk/other"}, false,
			[]string{"patchy.bitwisemedia.uk/other"}},
		{"no jobs finalizer", nil, []string{"patchy.bitwisemedia.uk/other"}, false,
			[]string{"patchy.bitwisemedia.uk/other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := runningUnit(nil)
			now := metav1.NewTime(clock)
			u.DeletionTimestamp = &now
			u.Finalizers = tt.fins
			c := newClient(t, testEval(unitPlan("claude")), u)
			runner := &errRunner{fakeRunner: &fakeRunner{}, deleteErr: tt.err}
			r := newUnitReconciler(c, nil, present)
			r.Runner = runner
			if _, err := r.Reconcile(t.Context(), unitReq(u.Name)); (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(runner.deleted, []string{jobs.EvalNameFor(u.Name)}) {
				t.Errorf("deleted = %v, want the unit's Job", runner.deleted)
			}
			if got := getUnit(t, c, u.Name); !slices.Equal(got.Finalizers, tt.wantFins) {
				t.Errorf("finalizers = %v, want %v", got.Finalizers, tt.wantFins)
			}
		})
	}
}
