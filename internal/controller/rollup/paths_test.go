// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package rollup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/stats"
)

var errBoom = errors.New("boom")

var (
	remReq = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"}}
	invReq = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-inv-1"}}
	fndReq = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}}
)

// reconcilerWith is newReconciler with interceptors on the fake client.
func reconcilerWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(
			&v1alpha1.Finding{}, &v1alpha1.Investigation{},
			&v1alpha1.Remediation{}, &v1alpha1.FindingRollup{},
		).
		WithInterceptorFuncs(funcs).
		Build()
	return &Reconciler{Client: c, Namespace: "patchy", TTL: DefaultTTL, Now: func() time.Time { return clock }}, c
}

// remediationRun is a settled remediation run with an estimate.
func remediationRun(phase v1alpha1.RunPhase, success bool) *v1alpha1.Remediation {
	return &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finding-aa-1-rem-1", Namespace: "patchy", UID: "rem-uid-1",
			Annotations: map[string]string{v1alpha1.AnnotationRepo: "acme/orders"},
			Finalizers: []string{
				v1alpha1.FinalizerJobs, v1alpha1.FinalizerRollupTotal, v1alpha1.FinalizerRollupRepository,
				v1alpha1.FinalizerRollupHarness, v1alpha1.FinalizerRollupModel,
			},
		},
		Spec: v1alpha1.RemediationSpec{
			FindingRef: v1alpha1.ObjectReference{Name: "finding-aa-1"}, Attempt: 1,
			Parameters: v1alpha1.AgentParameters{Estimate: &v1alpha1.AgentEstimate{MaxTurns: 40, TokenBudget: 2000}},
		},
		Status: v1alpha1.RemediationStatus{
			Phase:   phase,
			Success: success,
			Stage: &v1alpha1.StageResult{
				Outcome: "ok", Harness: "claude", Model: "claude-sonnet-5", NumTurns: 55,
				Usage: v1alpha1.UsageSummary{OutputTokens: 3000, CostUSD: "2.000000"},
			},
		},
	}
}

// TestReconcileRemediation: a settled remediation run contributes one stage
// delta per scope, counted as succeeded only when it completed with a fix,
// and carrying its estimate against what it actually spent.
func TestReconcileRemediation(t *testing.T) {
	tests := []struct {
		name          string
		phase         v1alpha1.RunPhase
		success       bool
		wantSucceeded int64
	}{
		{"complete with fix", v1alpha1.RunComplete, true, 1},
		{"complete handed off", v1alpha1.RunComplete, false, 0},
		{"failed", v1alpha1.RunFailed, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := newReconciler(t, remediationRun(tt.phase, tt.success))
			if _, err := r.ReconcileRemediation(t.Context(), remReq); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			for _, name := range []string{"total", "harness-claude", "model-claude-sonnet-5",
				stats.ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: "acme/orders"})} {
				agg := rollup(t, c, name).Status.Bucket.Stages["remediation"]
				if agg.Runs != 1 || agg.Succeeded != tt.wantSucceeded || agg.CostMicroUSD != 2_000_000 {
					t.Errorf("%s aggregate = runs %d succeeded %d cost %d, want 1/%d/2000000",
						name, agg.Runs, agg.Succeeded, agg.CostMicroUSD, tt.wantSucceeded)
				}
				want := v1alpha1.EstimateAggregate{Runs: 1, PredictedTurns: 40, ActualTurns: 55,
					PredictedOutputTokens: 2000, ActualOutputTokens: 3000}
				if agg.Estimate == nil || *agg.Estimate != want {
					t.Errorf("%s estimate = %+v, want %+v", name, agg.Estimate, want)
				}
			}
			// Idempotent: a second pass changes nothing.
			if _, err := r.ReconcileRemediation(t.Context(), remReq); err != nil {
				t.Fatalf("Reconcile again: %v", err)
			}
			if got := rollup(t, c, "total").Status.Bucket.Stages["remediation"].Runs; got != 1 {
				t.Errorf("runs = %d after re-reconcile, want 1", got)
			}
		})
	}
}

// TestChildIgnoresInFlight: a running child that is not being deleted
// contributes nothing yet; a missing one is no error.
func TestChildIgnoresInFlight(t *testing.T) {
	r, c := newReconciler(t, remediationRun(v1alpha1.RunRunning, false))
	if _, err := r.ReconcileRemediation(t.Context(), remReq); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var list v1alpha1.FindingRollupList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("rollups = %d, want none for an in-flight run", len(list.Items))
	}
	for name, fn := range map[string]func(context.Context, ctrl.Request) (ctrl.Result, error){
		"remediation": r.ReconcileRemediation, "investigation": r.ReconcileInvestigation, "finding": r.ReconcileFinding,
	} {
		if _, err := fn(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: "patchy", Name: "missing"}}); err != nil {
			t.Errorf("%s Reconcile(missing) error = %v, want nil", name, err)
		}
	}
}

// TestChildDeletedMidFlightCountsAborted: an operator deleting a run that
// never reported still counts it (as aborted) at total and repository, marks
// the keyless harness/model scopes, and releases every rollup finalizer.
func TestChildDeletedMidFlightCountsAborted(t *testing.T) {
	inv := completeInvestigation()
	inv.Status.Phase = v1alpha1.RunRunning
	inv.Status.Stage = nil
	now := metav1.NewTime(clock)
	inv.DeletionTimestamp = &now
	r, c := newReconciler(t, inv)
	if _, err := r.ReconcileInvestigation(t.Context(), invReq); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	agg := rollup(t, c, "total").Status.Bucket.Stages["investigation"]
	if agg.Runs != 1 || agg.Succeeded != 0 || agg.Outcomes["aborted"] != 1 {
		t.Errorf("total aggregate = %+v, want one aborted run", agg)
	}
	var list v1alpha1.FindingRollupList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Errorf("rollups = %d, want total and repository only (no harness/model key)", len(list.Items))
	}
	var cur v1alpha1.Investigation
	if err := c.Get(t.Context(), invReq.NamespacedName, &cur); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !slices.Equal(cur.Finalizers, []string{v1alpha1.FinalizerJobs}) {
		t.Errorf("finalizers = %v, want only the jobs finalizer left", cur.Finalizers)
	}
	for _, cond := range []string{v1alpha1.ConditionRolledUpHarness, v1alpha1.ConditionRolledUpModel} {
		if c := meta.FindStatusCondition(cur.Status.Conditions, cond); c == nil || c.Reason != "NoScopeKey" {
			t.Errorf("%s = %+v, want NoScopeKey", cond, c)
		}
	}
}

// TestChildUnparseableCostCountsZero: a run whose cost does not parse is
// still counted, with zero cost, rather than blocking its deletion.
func TestChildUnparseableCostCountsZero(t *testing.T) {
	inv := completeInvestigation()
	inv.Status.Stage.Usage.CostUSD = "not-a-number"
	inv.Annotations = nil
	r, c := newReconciler(t, inv)
	r.Now = nil
	if _, err := r.ReconcileInvestigation(t.Context(), invReq); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	agg := rollup(t, c, "total").Status.Bucket.Stages["investigation"]
	if agg.Runs != 1 || agg.CostMicroUSD != 0 {
		t.Errorf("total aggregate = runs %d cost %d, want 1/0", agg.Runs, agg.CostMicroUSD)
	}
	var cur v1alpha1.Investigation
	if err := c.Get(t.Context(), invReq.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionRolledUpRepository); cond == nil ||
		cond.Reason != "NoScopeKey" {
		t.Errorf("repository condition = %+v, want NoScopeKey without the repo annotation", cond)
	}
}

// failStatusOf fails status writes for objects of type T with err.
func failStatusOf[T client.Object](err error) interceptor.Funcs {
	return interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string,
		obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if _, ok := obj.(T); ok {
			return err
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}}
}

var conflict = kerrors.NewConflict(schema.GroupResource{Resource: "x"}, "x", errors.New("stale"))

// TestChildMarkerWriteErrors: a conflict writing the markers is left to the
// re-queue (the ledger keeps the rollup exact); any other error is returned.
func TestChildMarkerWriteErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		err     error
		wantErr bool
	}{"conflict": {conflict, false}, "transient": {errBoom, true}} {
		t.Run(name, func(t *testing.T) {
			r, c := reconcilerWith(t, failStatusOf[*v1alpha1.Investigation](tt.err), completeInvestigation())
			if _, err := r.ReconcileInvestigation(t.Context(), invReq); (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			// The rollup itself was written exactly once either way.
			if got := rollup(t, c, "total").Status.Bucket.Stages["investigation"].Runs; got != 1 {
				t.Errorf("total runs = %d, want 1", got)
			}
		})
	}
}

// TestApplyScopeErrors: a rollup that cannot be read, created or written
// fails the reconcile, and the child's markers are not set.
func TestApplyScopeErrors(t *testing.T) {
	tests := map[string]interceptor.Funcs{
		"get": {Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.FindingRollup); ok {
				return errBoom
			}
			return c.Get(ctx, key, obj, opts...)
		}},
		"create": {Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errBoom
		}},
		"status": failStatusOf[*v1alpha1.FindingRollup](errBoom),
	}
	for name, funcs := range tests {
		t.Run(name, func(t *testing.T) {
			r, c := reconcilerWith(t, funcs, completeInvestigation())
			if _, err := r.ReconcileInvestigation(t.Context(), invReq); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			var cur v1alpha1.Investigation
			if err := c.Get(t.Context(), invReq.NamespacedName, &cur); err != nil {
				t.Fatal(err)
			}
			if meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionRolledUpTotal) {
				t.Error("total marked rolled up although the rollup write failed")
			}
		})
	}
}

// TestFindingNotYetCompleted: a terminal finding whose completedAt is not
// stamped yet, and an in-flight finding never counted, are no-ops.
func TestFindingNotYetCompleted(t *testing.T) {
	unstamped := terminalFinding(v1alpha1.PhaseDismissed)
	unstamped.Status.CompletedAt = nil
	inFlight := terminalFinding(v1alpha1.PhaseInvestigating)
	inFlight.Status.CompletedAt = nil
	for name, fnd := range map[string]*v1alpha1.Finding{"unstamped": unstamped, "in flight": inFlight} {
		t.Run(name, func(t *testing.T) {
			r, c := newReconciler(t, fnd)
			res, err := r.ReconcileFinding(t.Context(), fndReq)
			if err != nil || res != (ctrl.Result{}) {
				t.Fatalf("Reconcile = %+v, %v, want an empty result", res, err)
			}
			var list v1alpha1.FindingRollupList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Errorf("rollups = %d, want none", len(list.Items))
			}
		})
	}
}

// TestFindingMarkerWriteErrors: a conflict writing a finding's markers is
// requeued; any other error is returned — on both the count and the
// revival-reversal paths.
func TestFindingMarkerWriteErrors(t *testing.T) {
	counted := func() *v1alpha1.Finding {
		f := terminalFinding(v1alpha1.PhaseQueued)
		f.Status.CompletedAt = nil
		f.Status.Conditions = []metav1.Condition{{
			Type: v1alpha1.ConditionRolledUpTotal, Status: metav1.ConditionTrue, Reason: "Aggregated",
			Message: countedMarker("handedoff", 1, "remediate"), LastTransitionTime: metav1.NewTime(clock),
		}}
		return f
	}
	tests := []struct {
		name        string
		fnd         func() *v1alpha1.Finding
		err         error
		wantErr     bool
		wantRequeue bool
	}{
		{"count conflict", func() *v1alpha1.Finding { return terminalFinding(v1alpha1.PhaseDismissed) }, conflict,
			false, true},
		{"count error", func() *v1alpha1.Finding { return terminalFinding(v1alpha1.PhaseDismissed) }, errBoom,
			true, false},
		{"reverse conflict", counted, conflict, false, true},
		{"reverse error", counted, errBoom, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := reconcilerWith(t, failStatusOf[*v1alpha1.Finding](tt.err), tt.fnd())
			res, err := r.ReconcileFinding(t.Context(), fndReq)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if res.Requeue != tt.wantRequeue { //nolint:staticcheck // the reconciler under test sets Requeue
				t.Errorf("requeue = %v, want %v", res.Requeue, tt.wantRequeue) //nolint:staticcheck
			}
		})
	}
}

// TestFindingRollupErrors: a failed rollup write while counting or reversing
// is returned and no marker is set.
func TestFindingRollupErrors(t *testing.T) {
	counted := terminalFinding(v1alpha1.PhaseQueued)
	counted.Status.CompletedAt = nil
	counted.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionRolledUpTotal, Status: metav1.ConditionTrue, Reason: "Aggregated",
		Message: countedMarker("handedoff", 1, "remediate"), LastTransitionTime: metav1.NewTime(clock),
	}}
	for name, fnd := range map[string]*v1alpha1.Finding{
		"count":   terminalFinding(v1alpha1.PhaseDismissed),
		"reverse": counted,
	} {
		t.Run(name, func(t *testing.T) {
			r, c := reconcilerWith(t, failStatusOf[*v1alpha1.FindingRollup](errBoom), fnd)
			if _, err := r.ReconcileFinding(t.Context(), fndReq); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			var cur v1alpha1.Finding
			if err := c.Get(t.Context(), fndReq.NamespacedName, &cur); err != nil {
				t.Fatal(err)
			}
			if !slices.EqualFunc(cur.Status.Conditions, fnd.Status.Conditions, func(a, b metav1.Condition) bool {
				return a.Type == b.Type && a.Message == b.Message
			}) {
				t.Errorf("conditions = %+v, want unchanged", cur.Status.Conditions)
			}
		})
	}
}

// TestEnforceTTL: TTL 0 keeps forever, a negative TTL means the default, and
// a failed delete is returned while one already gone is not.
func TestEnforceTTL(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Resource: "findings"}, "finding-aa-1")
	tests := []struct {
		name        string
		ttl         time.Duration
		deleteErr   error
		wantErr     bool
		wantRequeue time.Duration
		wantDeleted bool
	}{
		{"zero keeps forever", 0, nil, false, 0, false},
		{"negative means default", -1, nil, false, DefaultTTL - time.Hour, false},
		{"expired", time.Minute, nil, false, 0, true},
		{"delete fails", time.Minute, errBoom, true, 0, false},
		{"already gone", time.Minute, gone, false, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			funcs := interceptor.Funcs{}
			if tt.deleteErr != nil {
				funcs.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return tt.deleteErr
				}
			}
			fnd := terminalFinding(v1alpha1.PhaseRemediated)
			fnd.Finalizers = nil
			r, c := reconcilerWith(t, funcs, fnd)
			r.TTL = tt.ttl
			res, err := r.ReconcileFinding(t.Context(), fndReq)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if res.RequeueAfter != tt.wantRequeue {
				t.Errorf("requeue = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			var cur v1alpha1.Finding
			err = c.Get(t.Context(), fndReq.NamespacedName, &cur)
			if deleted := kerrors.IsNotFound(err); deleted != tt.wantDeleted {
				t.Errorf("deleted = %v (err %v), want %v", deleted, err, tt.wantDeleted)
			}
		})
	}
}

// TestFindingDeletionReleasesOnlyAggregatedScopes: a deleting finding's
// rollup finalizers go only once their scope is marked, and finalizers the
// rollup does not own are kept.
func TestFindingDeletionReleasesOnlyAggregatedScopes(t *testing.T) {
	fnd := terminalFinding(v1alpha1.PhaseDismissed)
	now := metav1.NewTime(clock)
	fnd.DeletionTimestamp = &now
	fnd.Finalizers = append(fnd.Finalizers, "patchy.bitwisemedia.uk/other")
	r, c := newReconciler(t, fnd)
	if _, err := r.ReconcileFinding(t.Context(), fndReq); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var cur v1alpha1.Finding
	if err := c.Get(t.Context(), fndReq.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cur.Finalizers, []string{"patchy.bitwisemedia.uk/other"}) {
		t.Errorf("finalizers = %v, want only the foreign one", cur.Finalizers)
	}
	if got := rollup(t, c, "total").Status.Bucket.Phases["dismissed"]; got != 1 {
		t.Errorf("dismissed = %d, want 1 (counted before release)", got)
	}
}

// TestCountedMarkerRoundTrip: what countedMarker writes, parseCounted reads
// back, and garbage parses as never counted.
func TestCountedMarkerRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		phase, rec string
		seq        int
	}{{"remediated", "remediate", 1}, {"", "", 7}, {"handedoff", "", 3}} {
		phase, seq, rec := parseCounted(countedMarker(tt.phase, tt.seq, tt.rec))
		if phase != tt.phase || seq != tt.seq || rec != tt.rec {
			t.Errorf("round trip of %+v = %q %d %q", tt, phase, seq, rec)
		}
	}
	if phase, seq, rec := parseCounted("garbage;seq=x"); phase != "" || seq != 0 || rec != "" {
		t.Errorf("parseCounted(garbage) = %q %d %q, want zero values", phase, seq, rec)
	}
	// A marker written before rec existed parses with an empty
	// recommendation.
	if phase, seq, rec := parseCounted("phase=failed;seq=2"); phase != "failed" || seq != 2 || rec != "" {
		t.Errorf("parseCounted(legacy) = %q %d %q", phase, seq, rec)
	}
}
