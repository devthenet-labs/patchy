// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
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
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

var errBoom = errors.New("boom")

const invName = fndName + "-inv-1"

// errRunner is fakeRunner with per-call failures injected.
type errRunner struct {
	*fakeRunner
	createErr, statusErr, resultErr, deleteErr error
}

func (e *errRunner) Create(ctx context.Context, spec jobs.Spec) (string, v1alpha1.RunnerImageRef, error) {
	if e.createErr != nil {
		return "", v1alpha1.RunnerImageRef{}, e.createErr
	}
	return e.fakeRunner.Create(ctx, spec)
}

func (e *errRunner) Status(ctx context.Context, name string) (jobs.Status, error) {
	if e.statusErr != nil {
		return jobs.Status{}, e.statusErr
	}
	return e.fakeRunner.Status(ctx, name)
}

func (e *errRunner) Result(ctx context.Context, name string) (jobs.RunOutput, error) {
	if e.resultErr != nil {
		return jobs.RunOutput{}, e.resultErr
	}
	return e.fakeRunner.Result(ctx, name)
}

func (e *errRunner) Delete(ctx context.Context, name string) error {
	_ = e.fakeRunner.Delete(ctx, name)
	return e.deleteErr
}

// invWith builds an InvestigationReconciler over any Runner, optionally with
// interceptors on the fake client.
func invWith(t *testing.T, runner Runner, funcs *interceptor.Funcs, objs ...client.Object) (
	*InvestigationReconciler, client.Client,
) {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Investigation{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	return &InvestigationReconciler{
		Client:              c,
		Runner:              runner,
		Namespace:           "patchy",
		MaxConcurrent:       2,
		MaxAttempts:         2,
		ConfidenceThreshold: 0.75,
		Now:                 func() time.Time { return clock },
	}, c
}

// gateWith is newGate with interceptors on the fake client.
func gateWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*GateReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Repository{}, &v1alpha1.Investigation{}).
		WithIndex(&v1alpha1.Finding{}, FindingPhaseIndex, FindingPhaseIndexer).
		WithInterceptorFuncs(funcs).
		Build()
	return &GateReconciler{
		Client:    c,
		Forges:    forge.NewStore(c),
		Namespace: "patchy",
		MinAge:    time.Hour,
		Now:       func() time.Time { return clock },
	}, c
}

func gateErr(t *testing.T, r *GateReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), reqFor(fndName))
}

// TestGateIgnores: findings the gate has no edge for are left untouched,
// with no Repository requested.
func TestGateIgnores(t *testing.T) {
	now := metav1.NewTime(clock)
	tests := []struct {
		name string
		mut  func(*v1alpha1.Finding)
	}{
		{"suspended", func(f *v1alpha1.Finding) { f.Spec.Suspend = true }},
		{"deleting", func(f *v1alpha1.Finding) {
			f.DeletionTimestamp = &now
			f.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
		}},
		{"opened", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseOpened }},
		{"queued", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseQueued }},
		{"failed without retry", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseFailed }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fnd := enhancedFinding()
			tt.mut(fnd)
			want := fnd.Status.Phase
			r, c := newGate(t, fnd, testForge())
			if res := gateOnce(t, r); res.RequeueAfter != 0 {
				t.Errorf("requeue = %v, want none", res.RequeueAfter)
			}
			if got := getF(t, c).Status.Phase; got != want {
				t.Errorf("phase = %q, want unchanged %q", got, want)
			}
			var repo v1alpha1.Repository
			err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: fndName + "-src"}, &repo)
			if !kerrors.IsNotFound(err) {
				t.Errorf("Repository lookup error = %v, want not found", err)
			}
		})
	}
	r, _ := newGate(t)
	if _, err := gateErr(t, r); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// TestGateAmbiguousForgeStaysEnhanced: two equally specific Forges covering
// the repository is an operator mistake; the finding parks, recoverably.
func TestGateAmbiguousForgeStaysEnhanced(t *testing.T) {
	other := testForge()
	other.Name = "gh-forge-2"
	r, c := newGate(t, enhancedFinding(), testForge(), other)
	gateOnce(t, r)
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseEnhanced {
		t.Errorf("phase = %q, want Enhanced", f.Status.Phase)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionForgeResolved)
	if cond == nil || cond.Reason != v1alpha1.ReasonAmbiguous || cond.Status != metav1.ConditionFalse ||
		!strings.Contains(cond.Message, "gh-forge, gh-forge-2") {
		t.Errorf("ForgeResolved = %+v, want Ambiguous naming both Forges", cond)
	}
}

// TestGateStalledArtifactHandsOff: an artifact over the size cap needs a
// human; the finding is handed off with the reason.
func TestGateStalledArtifactHandsOff(t *testing.T) {
	r, c := newGate(t, enhancedFinding(), testForge(), srcRepo(nil, stalledCond("ArtifactTooLarge", "big")))
	gateOnce(t, r)
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseHandedOff ||
		f.Status.LastFailureReason != "repository artifact exceeds the size cap" {
		t.Errorf("finding = %q %q, want HandedOff with the size-cap reason", f.Status.Phase, f.Status.LastFailureReason)
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionForgeResolved); cond == nil ||
		cond.Reason != "ArtifactStalled" {
		t.Errorf("ForgeResolved = %+v, want ArtifactStalled", cond)
	}
}

// TestGateWaitsForRepositoryReady: an existing Repository not yet Ready
// opens nothing.
func TestGateWaitsForRepositoryReady(t *testing.T) {
	r, c := newGate(t, enhancedFinding(), testForge(), srcRepo(nil))
	gateOnce(t, r)
	if f := getF(t, c); f.Status.Phase != v1alpha1.PhaseEnhanced || f.Status.Attempts.Investigation != 0 {
		t.Errorf("finding = %q attempts %d, want Enhanced with none", f.Status.Phase, f.Status.Attempts.Investigation)
	}
}

// TestGateOpensWithParameters: the opened Investigation carries the
// configured analysis parameters, and the finding records the Forge it
// resolved to on a True ForgeResolved condition.
func TestGateOpensWithParameters(t *testing.T) {
	r, c := newGate(t, enhancedFinding(), testForge(), readyRepo())
	r.Parameters = v1alpha1.AgentParameters{Model: "anthropic/claude-sonnet-5", MaxTurns: 30, TokenBudget: 1000}
	gateOnce(t, r)
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseInvestigating {
		t.Fatalf("phase = %q, want Investigating", f.Status.Phase)
	}
	if f.Status.Forge == nil || f.Status.Forge.Name != "gh-forge" {
		t.Errorf("forge = %+v, want gh-forge", f.Status.Forge)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionForgeResolved)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != "gh-forge" {
		t.Errorf("ForgeResolved = %+v, want True gh-forge", cond)
	}
	if f.Status.ActiveRun == nil || f.Status.ActiveRun.Name != invName ||
		f.Status.ActiveRun.Kind != v1alpha1.RunKindInvestigation {
		t.Errorf("activeRun = %+v, want %s", f.Status.ActiveRun, invName)
	}
	inv := getInv(t, c)
	if inv.Spec.Parameters != r.Parameters {
		t.Errorf("parameters = %+v, want %+v", inv.Spec.Parameters, r.Parameters)
	}
	if inv.Labels[v1alpha1.LabelSeverity] != "high" || inv.Annotations[v1alpha1.AnnotationRepo] != "acme/orders" {
		t.Errorf("labels/annotations = %v/%v", inv.Labels, inv.Annotations)
	}
	if inv.Spec.RepositoryRef == nil || inv.Spec.RepositoryRef.Name != fndName+"-src" {
		t.Errorf("repositoryRef = %+v", inv.Spec.RepositoryRef)
	}
}

// TestGateTolerantOfExistingLease: an Investigation already created under
// the attempt's name (a racing reconcile) is the lease; the gate still
// advances the finding without error.
func TestGateTolerantOfExistingLease(t *testing.T) {
	existing := investigationFixture()[1].(*v1alpha1.Investigation)
	existing.Status = v1alpha1.InvestigationStatus{}
	r, c := newGate(t, enhancedFinding(), testForge(), readyRepo(), existing)
	gateOnce(t, r)
	if f := getF(t, c); f.Status.Phase != v1alpha1.PhaseInvestigating || f.Status.Attempts.Investigation != 1 {
		t.Errorf("finding = %q attempts %d, want Investigating attempt 1", f.Status.Phase, f.Status.Attempts.Investigation)
	}
}

// getErrOn fails Get for objects of type T only.
func getErrOn[T client.Object]() interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(T); ok {
			return errBoom
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// TestGateErrors: transient API errors at each step are returned and the
// finding stays Enhanced for the retry.
func TestGateErrors(t *testing.T) {
	prevAttempt := enhancedFinding()
	prevAttempt.Status.Attempts.Investigation = 1
	tests := []struct {
		name  string
		funcs interceptor.Funcs
		objs  []client.Object
	}{
		{"forge list", interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
			o ...client.ListOption) error {
			if _, ok := l.(*v1alpha1.ForgeList); ok {
				return errBoom
			}
			return c.List(ctx, l, o...)
		}}, []client.Object{enhancedFinding(), testForge()}},
		{"repository get", getErrOn[*v1alpha1.Repository](), []client.Object{enhancedFinding(), testForge()}},
		{"repository create", interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
			...client.CreateOption) error {
			return errBoom
		}}, []client.Object{enhancedFinding(), testForge()}},
		{"previous attempt get", getErrOn[*v1alpha1.Investigation](),
			[]client.Object{prevAttempt, testForge(), readyRepo()}},
		{"investigation create", interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
			...client.CreateOption) error {
			return errBoom
		}}, []client.Object{enhancedFinding(), testForge(), readyRepo()}},
		{"phase write", interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string,
			client.Object, ...client.SubResourceUpdateOption) error {
			return errBoom
		}}, []client.Object{enhancedFinding(), testForge(), readyRepo()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := gateWith(t, tt.funcs, tt.objs...)
			if _, err := gateErr(t, r); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if got := getF(t, c).Status.Phase; got != v1alpha1.PhaseEnhanced {
				t.Errorf("phase = %q, want Enhanced", got)
			}
		})
	}
}

// TestGateParkAndRetryWriteErrors: a park or retry recovery whose status
// write fails is returned, leaving the phase as it was.
func TestGateParkAndRetryWriteErrors(t *testing.T) {
	failWrites := interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string,
		client.Object, ...client.SubResourceUpdateOption) error {
		return errBoom
	}}
	repoLess := enhancedFinding()
	repoLess.Spec.Repository = nil
	for name, fnd := range map[string]*v1alpha1.Finding{
		"park":  repoLess,
		"retry": failedInvestigationFinding(clock.Add(-time.Minute)),
	} {
		t.Run(name, func(t *testing.T) {
			r, c := gateWith(t, failWrites, fnd, testForge())
			r.Now = nil
			if _, err := gateErr(t, r); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if got := getF(t, c).Status.Phase; got != fnd.Status.Phase {
				t.Errorf("phase = %q, want unchanged %q", got, fnd.Status.Phase)
			}
		})
	}
}

// TestGatePreviousAttemptOfEarlierFinding: a run under the same name that
// belongs to an earlier Finding (other UID) is not this finding's previous
// attempt, and does not block the gate.
func TestGatePreviousAttemptOfEarlierFinding(t *testing.T) {
	fnd := enhancedFinding()
	fnd.Status.Attempts.Investigation = 1
	stale := invChild(1, v1alpha1.RunRunning, nil)
	stale.Spec.FindingRef.UID = "uid-old"
	r, c := newGate(t, fnd, testForge(), readyRepo(), stale)
	gateOnce(t, r)
	var next v1alpha1.Investigation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: fndName + "-inv-2"}, &next); err != nil {
		t.Fatalf("attempt 2 not opened: %v", err)
	}
	if next.Spec.PreviousAttempt != nil {
		t.Errorf("previousAttempt = %+v, want none from another finding's run", next.Spec.PreviousAttempt)
	}
}

// TestCollectFailurePaths: every way a finished (or vanished) Job can fail
// to report a usable verdict fails the run and reverts the finding to
// Enhanced (edge 4) with the reason recorded.
func TestCollectFailurePaths(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job-1")
	tests := []struct {
		name       string
		runner     Runner
		wantReason string
	}{
		{"job vanished", &errRunner{fakeRunner: &fakeRunner{}, statusErr: gone},
			"aborted: agent job vanished before reporting"},
		{"fatal event", &fakeRunner{done: true, events: []envelope.Event{
			{V: envelope.Version, Type: envelope.TypeFatal, Error: "workspace missing"},
		}}, "aborted: workspace missing"},
		{"no investigation event", &fakeRunner{done: true}, "aborted: agent job produced no investigation event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := invWith(t, tt.runner, nil, investigationFixture()...)
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunFailed {
				t.Errorf("run phase = %q, want Failed", inv.Status.Phase)
			}
			f := getF(t, c)
			if f.Status.Phase != v1alpha1.PhaseEnhanced || f.Status.LastFailureReason != tt.wantReason {
				t.Errorf("finding = %q %q, want Enhanced %q", f.Status.Phase, f.Status.LastFailureReason, tt.wantReason)
			}
			if f.Status.ActiveRun != nil {
				t.Errorf("activeRun = %+v, want cleared", f.Status.ActiveRun)
			}
		})
	}
}

// TestCollectFindingGone: results for a finding that no longer exists are
// dropped without error, whether the Job vanished or finished.
func TestCollectFindingGone(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job-1")
	for name, runner := range map[string]Runner{
		"vanished": &errRunner{fakeRunner: &fakeRunner{}, statusErr: gone},
		"finished": &fakeRunner{done: true, events: investigationEvent("remediate", 0.9, false)},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := invWith(t, runner, nil, investigationFixture()[1])
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
				t.Errorf("Reconcile error = %v, want nil", err)
			}
		})
	}
}

// TestCollectTransientErrors: a transient Status or Result error is
// returned for a retry and never fails the run.
func TestCollectTransientErrors(t *testing.T) {
	for name, runner := range map[string]*errRunner{
		"status": {fakeRunner: &fakeRunner{}, statusErr: errBoom},
		"result": {fakeRunner: &fakeRunner{done: true}, resultErr: errBoom},
	} {
		t.Run(name, func(t *testing.T) {
			r, c := invWith(t, runner, nil, investigationFixture()...)
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunRunning {
				t.Errorf("run phase = %q, want Running", inv.Status.Phase)
			}
			if f := getF(t, c); f.Status.Phase != v1alpha1.PhaseInvestigating {
				t.Errorf("finding phase = %q, want Investigating", f.Status.Phase)
			}
		})
	}
}

// TestApplyRouteEdges: a remediate verdict for a repository-less finding,
// and an unknown recommendation, both go to a human; a verdict with no
// estimate and no model records no remediation parameters.
func TestApplyRouteEdges(t *testing.T) {
	tests := []struct {
		name      string
		rec       string
		repoLess  bool
		wantPhase v1alpha1.Phase
	}{
		{"remediate without repository", "remediate", true, v1alpha1.PhaseHandedOff},
		{"unknown recommendation", "escalate", false, v1alpha1.PhaseHandedOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := investigationEvent(tt.rec, 0.99, false)
			events[0].Investigation.RemediationModel = ""
			events[0].Investigation.EstimatedMaxTurns = 0
			events[0].Investigation.EstimatedTokenBudget = 0
			objs := investigationFixture()
			if tt.repoLess {
				objs[0].(*v1alpha1.Finding).Spec.Repository = nil
			}
			r, c := invWith(t, &fakeRunner{done: true, events: events}, nil, objs...)
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			f := getF(t, c)
			if f.Status.Phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tt.wantPhase)
			}
			if f.Status.Investigation == nil || f.Status.Investigation.Estimate != nil ||
				f.Status.Investigation.AwaitApproval {
				t.Errorf("summary = %+v, want no estimate and no hold", f.Status.Investigation)
			}
			if inv := getInv(t, c); inv.Status.RemediationParameters != nil {
				t.Errorf("remediationParameters = %+v, want none", inv.Status.RemediationParameters)
			}
		})
	}
}

// TestApplyLeavesMovedFinding: a verdict for a finding a human moved out of
// Investigating stamps the run and records the summary, but changes no
// phase.
func TestApplyLeavesMovedFinding(t *testing.T) {
	objs := investigationFixture()
	objs[0].(*v1alpha1.Finding).Status.Phase = v1alpha1.PhaseEnhanced
	r, c := invWith(t, &fakeRunner{done: true, events: investigationEvent("ignore", 0.9, false)}, nil, objs...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseEnhanced {
		t.Errorf("phase = %q, want Enhanced untouched", f.Status.Phase)
	}
	if f.Status.Investigation == nil || f.Status.Investigation.Recommendation != v1alpha1.RecommendationIgnore {
		t.Errorf("summary = %+v, want the ignore verdict recorded", f.Status.Investigation)
	}
}

// TestFailLeavesMovedFinding: a failure for a finding no longer
// Investigating stamps the run but does not move the finding.
func TestFailLeavesMovedFinding(t *testing.T) {
	objs := investigationFixture()
	objs[0].(*v1alpha1.Finding).Status.Phase = v1alpha1.PhaseHandedOff
	r, c := invWith(t, &fakeRunner{done: true}, nil, objs...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("run phase = %q, want Failed", inv.Status.Phase)
	}
	if f := getF(t, c); f.Status.Phase != v1alpha1.PhaseHandedOff || f.Status.LastFailureReason != "" {
		t.Errorf("finding = %q %q, want HandedOff untouched", f.Status.Phase, f.Status.LastFailureReason)
	}
}

// TestFailDefaultMaxAttempts: MaxAttempts unset means two, so a second
// failed attempt exhausts the finding (edge 9).
func TestFailDefaultMaxAttempts(t *testing.T) {
	for _, tt := range []struct {
		attempt int32
		want    v1alpha1.Phase
	}{{1, v1alpha1.PhaseEnhanced}, {2, v1alpha1.PhaseFailed}} {
		objs := investigationFixture()
		objs[1].(*v1alpha1.Investigation).Spec.Attempt = tt.attempt
		r, c := invWith(t, &fakeRunner{done: true}, nil, objs...)
		r.MaxAttempts = 0
		if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
			t.Fatalf("attempt %d: Reconcile: %v", tt.attempt, err)
		}
		if got := getF(t, c).Status.Phase; got != tt.want {
			t.Errorf("attempt %d: phase = %q, want %q", tt.attempt, got, tt.want)
		}
	}
}

// TestFailCountError: when refused attempts cannot be counted the run is
// left Running for the next pass.
func TestFailCountError(t *testing.T) {
	funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		o ...client.ListOption) error {
		if _, ok := l.(*v1alpha1.InvestigationList); ok {
			return errBoom
		}
		return c.List(ctx, l, o...)
	}}
	r, c := invWith(t, &fakeRunner{done: true}, funcs, investigationFixture()...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("run phase = %q, want Running", inv.Status.Phase)
	}
}

// TestLaunchPrerequisites: a launch that cannot gather its inputs creates
// no Job; one with no repository reference at all fails the run.
func TestLaunchPrerequisites(t *testing.T) {
	t.Run("no repository reference fails", func(t *testing.T) {
		objs := launchable(readyRepo())
		objs[1].(*v1alpha1.Investigation).Spec.RepositoryRef = nil
		runner := &fakeRunner{}
		r, c := invWith(t, runner, nil, objs...)
		if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(runner.created) != 0 {
			t.Errorf("jobs = %d, want 0", len(runner.created))
		}
		f := getF(t, c)
		if f.Status.Phase != v1alpha1.PhaseEnhanced ||
			f.Status.LastFailureReason != "aborted: investigation has no repository artifact" {
			t.Errorf("finding = %q %q, want Enhanced with the reason", f.Status.Phase, f.Status.LastFailureReason)
		}
	})
	noArtifact := readyRepo()
	noArtifact.Status.Artifact = nil
	tests := []struct {
		name    string
		objs    []client.Object
		wantErr bool
	}{
		{"finding gone", launchable(readyRepo())[1:], false},
		{"repository gone", launchable(readyRepo())[:2], true},
		{"repository without artifact", launchable(noArtifact), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			r, c := invWith(t, runner, nil, tt.objs...)
			_, err := r.Reconcile(t.Context(), reqFor(invName))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(runner.created) != 0 {
				t.Errorf("jobs = %d, want 0", len(runner.created))
			}
			if inv := getInv(t, c); inv.Status.JobRef != nil {
				t.Errorf("jobRef = %+v, want none", inv.Status.JobRef)
			}
		})
	}
}

// TestLaunchCreateError: a Job that cannot be created leaves the run
// Running without a JobRef for the next pass.
func TestLaunchCreateError(t *testing.T) {
	r, c := invWith(t, &errRunner{fakeRunner: &fakeRunner{}, createErr: errBoom}, nil, launchable(readyRepo())...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if inv := getInv(t, c); inv.Status.JobRef != nil {
		t.Errorf("jobRef = %+v, want none", inv.Status.JobRef)
	}
}

// rollup is a FindingRollup for scope with runs estimated remediations.
func rollup(scope v1alpha1.RollupScope, runs int64) *v1alpha1.FindingRollup {
	return &v1alpha1.FindingRollup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: stats.ScopeObjectName(scope)},
		Status: v1alpha1.FindingRollupStatus{Bucket: v1alpha1.RollupBucket{
			Stages: map[string]v1alpha1.StageAggregate{"remediation": {
				Runs: runs,
				Estimate: &v1alpha1.EstimateAggregate{
					Runs: runs, PredictedTurns: 30 * runs, ActualTurns: 45 * runs,
					PredictedOutputTokens: 1000 * runs, ActualOutputTokens: 1500 * runs,
				},
			}},
		}},
	}
}

// TestLaunchCalibration: the prompt's calibration comes from the
// repository's own history when it has enough runs, falls back to the
// estate-wide history when that is too thin, and is omitted with neither.
func TestLaunchCalibration(t *testing.T) {
	repoScope := v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: "acme/orders"}
	totalScope := v1alpha1.RollupScope{Type: v1alpha1.ScopeTotal}
	tests := []struct {
		name      string
		rollups   []client.Object
		repoLess  bool
		wantScope string
		wantRuns  int64
	}{
		{"repository history", []client.Object{rollup(repoScope, 4), rollup(totalScope, 9)}, false, "acme/orders", 4},
		{"thin repository falls back", []client.Object{rollup(repoScope, 1), rollup(totalScope, 9)}, false,
			"all repositories", 9},
		{"no repository rollup falls back", []client.Object{rollup(totalScope, 3)}, false, "all repositories", 3},
		{"repository-less finding uses the estate", []client.Object{rollup(repoScope, 4), rollup(totalScope, 5)},
			true, "all repositories", 5},
		{"no history", nil, false, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := launchable(readyRepo())
			if tt.repoLess {
				objs[0].(*v1alpha1.Finding).Spec.Repository = nil
			}
			runner := &fakeRunner{}
			r, _ := invWith(t, runner, nil, append(objs, tt.rollups...)...)
			r.Log = slog.New(slog.DiscardHandler)
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(runner.created) != 1 {
				t.Fatalf("jobs = %d, want 1", len(runner.created))
			}
			got := runner.created[0].Calibration
			if tt.wantScope == "" {
				if got != "" {
					t.Errorf("calibration = %q, want none", got)
				}
				return
			}
			var cal templates.Calibration
			if err := json.Unmarshal([]byte(got), &cal); err != nil {
				t.Fatalf("calibration %q is not JSON: %v", got, err)
			}
			want := templates.Calibration{Scope: tt.wantScope, Runs: tt.wantRuns, AvgPredictedTurns: 30,
				AvgActualTurns: 45, AvgPredictedOutputTokens: 1000, AvgActualOutputTokens: 1500}
			if cal != want {
				t.Errorf("calibration = %+v, want %+v", cal, want)
			}
		})
	}
}

// TestPullFailedEdges: a stuck repository-image Job for a vanished finding
// is dropped; one whose delete fails still fails the run.
func TestPullFailedEdges(t *testing.T) {
	stuck := jobs.Status{
		Active: 1, Created: clock.Add(-5 * time.Minute), Waiting: "ErrImagePull",
		WaitingMessage: "manifest unknown", RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}
	t.Run("delete fails", func(t *testing.T) {
		runner := &errRunner{fakeRunner: &fakeRunner{status: &stuck}, deleteErr: errBoom}
		r, c := invWith(t, runner, nil, stamped(v1alpha1.RunnerImageSourceRepository)...)
		if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(runner.deleted) != 1 {
			t.Errorf("deleted = %v, want one attempt", runner.deleted)
		}
		if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunFailed {
			t.Errorf("run phase = %q, want Failed", inv.Status.Phase)
		}
	})
	t.Run("finding gone", func(t *testing.T) {
		runner := &fakeRunner{status: &stuck}
		objs := stamped(v1alpha1.RunnerImageSourceRepository)
		r, c := invWith(t, runner, nil, objs[1:]...)
		if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(runner.deleted) != 0 {
			t.Errorf("deleted = %v, want none", runner.deleted)
		}
		if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunRunning {
			t.Errorf("run phase = %q, want Running", inv.Status.Phase)
		}
	})
}

// deletingInvestigation is the running Investigation marked for deletion.
func deletingInvestigation(jobRef bool) []client.Object {
	objs := investigationFixture()
	inv := objs[1].(*v1alpha1.Investigation)
	now := metav1.NewTime(clock)
	inv.DeletionTimestamp = &now
	inv.Finalizers = []string{v1alpha1.FinalizerJobs, v1alpha1.FinalizerRollupTotal}
	if !jobRef {
		inv.Status.JobRef = nil
	}
	return objs
}

// TestFinalize: deleting a run deletes its Job, then drops only the jobs
// finalizer; a failing delete keeps it so the Job is never orphaned.
func TestFinalize(t *testing.T) {
	tests := []struct {
		name        string
		withJob     bool
		deleteErr   error
		wantErr     bool
		wantDeleted []string
		wantFins    []string
	}{
		{"with job", true, nil, false, []string{"job-1"}, []string{v1alpha1.FinalizerRollupTotal}},
		{"never launched", false, nil, false, nil, []string{v1alpha1.FinalizerRollupTotal}},
		{"job already gone", true, kerrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, "job-1"), false,
			[]string{"job-1"}, []string{v1alpha1.FinalizerRollupTotal}},
		{"delete fails", true, errBoom, true, []string{"job-1"},
			[]string{v1alpha1.FinalizerJobs, v1alpha1.FinalizerRollupTotal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &errRunner{fakeRunner: &fakeRunner{}, deleteErr: tt.deleteErr}
			r, c := invWith(t, runner, nil, deletingInvestigation(tt.withJob)...)
			r.Now = nil
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(runner.deleted, tt.wantDeleted) {
				t.Errorf("deleted = %v, want %v", runner.deleted, tt.wantDeleted)
			}
			if inv := getInv(t, c); !slices.Equal(inv.Finalizers, tt.wantFins) {
				t.Errorf("finalizers = %v, want %v", inv.Finalizers, tt.wantFins)
			}
		})
	}
}

// TestRunIgnoresSettledAndMissing: pending and settled runs, and missing
// ones, are no-ops.
func TestRunIgnoresSettledAndMissing(t *testing.T) {
	for _, phase := range []v1alpha1.RunPhase{v1alpha1.RunPending, v1alpha1.RunComplete, v1alpha1.RunFailed} {
		t.Run(string(phase), func(t *testing.T) {
			objs := investigationFixture()
			objs[1].(*v1alpha1.Investigation).Status.Phase = phase
			runner := &fakeRunner{done: true, events: investigationEvent("remediate", 0.9, false)}
			r, c := invWith(t, runner, nil, objs...)
			if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if runner.results != 0 || len(runner.created) != 0 {
				t.Errorf("results %d, jobs %d, want none", runner.results, len(runner.created))
			}
			if inv := getInv(t, c); inv.Status.Phase != phase {
				t.Errorf("phase = %q, want %q", inv.Status.Phase, phase)
			}
		})
	}
	r, _ := invWith(t, &fakeRunner{}, nil)
	if _, err := r.Reconcile(t.Context(), reqFor("missing")); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// pendingInv is a Pending Investigation of severity sev.
func pendingInv(name, finding, sev string) *v1alpha1.Investigation {
	return &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "patchy", CreationTimestamp: metav1.NewTime(clock),
			Labels: map[string]string{v1alpha1.LabelSeverity: sev},
		},
		Spec: v1alpha1.InvestigationSpec{FindingRef: v1alpha1.ObjectReference{Name: finding}, Attempt: 1},
	}
}

func scheduleOnce(t *testing.T, r *InvestigationReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), reqFor(schedulerRequest))
}

// TestSchedulerSlots: running runs consume slots, deleting runs are never
// granted, MaxConcurrent <= 0 means three, and an expedited finding's run
// outranks a higher severity.
func TestSchedulerSlots(t *testing.T) {
	running := pendingInv("inv-running", "f0", "low")
	running.Status.Phase = v1alpha1.RunRunning
	deleting := pendingInv("inv-deleting", "f1", "critical")
	now := metav1.NewTime(clock)
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.FinalizerJobs}
	expedited := enhancedFinding()
	expedited.Name = "f-exp"
	expedited.Spec.Expedite = &v1alpha1.ActionRequest{By: "dev", At: now}

	tests := []struct {
		name        string
		maxConc     int
		objs        []client.Object
		wantRunning []string
	}{
		{"running fills a slot", 2,
			[]client.Object{running, pendingInv("inv-a", "f2", "high"), pendingInv("inv-b", "f3", "critical")},
			[]string{"inv-b", "inv-running"}},
		{"zero max means three", 0,
			[]client.Object{pendingInv("inv-a", "f2", "low"), pendingInv("inv-b", "f3", "low"),
				pendingInv("inv-c", "f4", "low"), pendingInv("inv-d", "f5", "low")}, nil},
		{"deleting is skipped", 1,
			[]client.Object{deleting, pendingInv("inv-a", "f2", "low")}, []string{"inv-a"}},
		{"expedited outranks severity", 1,
			[]client.Object{expedited, pendingInv("inv-a", "f-exp", "low"), pendingInv("inv-b", "f3", "critical")},
			[]string{"inv-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, len(tt.objs))
			for i, o := range tt.objs {
				objs[i] = o.DeepCopyObject().(client.Object)
			}
			r, c := invWith(t, &fakeRunner{}, nil, objs...)
			r.MaxConcurrent = tt.maxConc
			res, err := scheduleOnce(t, r)
			if err != nil {
				t.Fatalf("schedule: %v", err)
			}
			if res.RequeueAfter != 5*time.Minute {
				t.Errorf("requeue = %v, want the 5m safety tick", res.RequeueAfter)
			}
			var list v1alpha1.InvestigationList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, inv := range list.Items {
				if inv.Status.Phase == v1alpha1.RunRunning {
					got = append(got, inv.Name)
				}
			}
			slices.Sort(got)
			if tt.wantRunning == nil {
				if len(got) != 3 {
					t.Errorf("running = %v, want three grants", got)
				}
				return
			}
			if !slices.Equal(got, tt.wantRunning) {
				t.Errorf("running = %v, want %v", got, tt.wantRunning)
			}
		})
	}
}

// TestSchedulerErrors: a failed list or grant write is returned.
func TestSchedulerErrors(t *testing.T) {
	for name, funcs := range map[string]*interceptor.Funcs{
		"list": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errBoom
		}},
		"grant": {SubResourceUpdate: func(context.Context, client.Client, string, client.Object,
			...client.SubResourceUpdateOption) error {
			return errBoom
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := invWith(t, &fakeRunner{}, funcs, pendingInv("inv-a", "", "high"))
			if _, err := scheduleOnce(t, r); !errors.Is(err, errBoom) {
				t.Errorf("schedule error = %v, want %v", err, errBoom)
			}
		})
	}
}

// TestSandboxRefusedFindingGone: a refused run for a vanished finding still
// trips the breaker and is dropped without error.
func TestSandboxRefusedFindingGone(t *testing.T) {
	exit := int32(jobs.ExitSandboxUnenforced)
	runner := &fakeRunner{status: &jobs.Status{
		Failed: 1, Done: true, Waiting: "PodInitializing", InitExitCode: &exit,
		RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}}
	objs := stamped(v1alpha1.RunnerImageSourceRepository)
	r, c := invWith(t, runner, nil, objs[1:]...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runner.results != 0 {
		t.Errorf("results = %d, want no log read", runner.results)
	}
	if inv := getInv(t, c); inv.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("run phase = %q, want Running (finding gone, nothing to release)", inv.Status.Phase)
	}
}

// TestApplyBareAwaitApprovalHolds: an older runner that sets awaitApproval
// without naming a reason still holds the finding, as a breaking change.
func TestApplyBareAwaitApprovalHolds(t *testing.T) {
	events := investigationEvent("remediate", 0.95, true)
	events[0].Investigation.HoldReasons = nil
	r, c := invWith(t, &fakeRunner{done: true, events: events}, nil, investigationFixture()...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseAwaitingApproval {
		t.Errorf("phase = %q, want AwaitingApproval", f.Status.Phase)
	}
	if f.Status.Investigation == nil ||
		!slices.Equal(f.Status.Investigation.HoldReasons, []v1alpha1.HoldReason{v1alpha1.HoldBreakingChangeAvailable}) {
		t.Errorf("summary = %+v, want the breaking-change hold", f.Status.Investigation)
	}
}

// TestApplyFatalKeepsReportedStage: a fatal event after the verdict fails
// the run but keeps the agent's accounting on the child.
func TestApplyFatalKeepsReportedStage(t *testing.T) {
	events := append(investigationEvent("remediate", 0.9, false), envelope.Event{
		V: envelope.Version, Type: envelope.TypeFatal, Error: "late crash",
	})
	r, c := invWith(t, &fakeRunner{done: true, events: events}, nil, investigationFixture()...)
	if _, err := r.Reconcile(t.Context(), reqFor(invName)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	inv := getInv(t, c)
	if inv.Status.Phase != v1alpha1.RunFailed || inv.Status.Stage == nil ||
		inv.Status.Stage.Usage.CostUSD != "1.250000" || inv.Status.Stage.Outcome != "aborted" {
		t.Errorf("run = %q stage %+v, want Failed aborted with the reported cost kept", inv.Status.Phase, inv.Status.Stage)
	}
	if f := getF(t, c); f.Status.LastFailureReason != "aborted: late crash" {
		t.Errorf("lastFailureReason = %q, want the fatal error", f.Status.LastFailureReason)
	}
}
