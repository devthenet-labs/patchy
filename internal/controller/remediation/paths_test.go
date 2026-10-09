// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"context"
	"errors"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// errRunner is fakeCRRunner with per-call failures injected.
type errRunner struct {
	*fakeCRRunner
	createErr, statusErr, resultErr, deleteErr error
}

func (e *errRunner) Create(ctx context.Context, spec jobs.Spec) (string, v1alpha1.RunnerImageRef, error) {
	if e.createErr != nil {
		return "", v1alpha1.RunnerImageRef{}, e.createErr
	}
	return e.fakeCRRunner.Create(ctx, spec)
}

func (e *errRunner) Status(ctx context.Context, name string) (jobs.Status, error) {
	if e.statusErr != nil {
		return jobs.Status{}, e.statusErr
	}
	return e.fakeCRRunner.Status(ctx, name)
}

func (e *errRunner) Result(ctx context.Context, name string) (jobs.RunOutput, error) {
	if e.resultErr != nil {
		return jobs.RunOutput{}, e.resultErr
	}
	return e.fakeCRRunner.Result(ctx, name)
}

func (e *errRunner) Delete(ctx context.Context, name string) error {
	_ = e.fakeCRRunner.Delete(ctx, name)
	return e.deleteErr
}

// errForge is a ForgeWriter whose push or PR fails.
type errForge struct {
	fakeForge
	pushErr, prErr error
}

func (e *errForge) Push(ctx context.Context, ns, url, branch string, cs *envelope.Changeset) (string, error) {
	if e.pushErr != nil {
		return "", e.pushErr
	}
	return e.fakeForge.Push(ctx, ns, url, branch, cs)
}

func (e *errForge) EnsurePR(ctx context.Context, ns, url, branch, title, body string) (int64, string, error) {
	if e.prErr != nil {
		return 0, "", e.prErr
	}
	return e.fakeForge.EnsurePR(ctx, ns, url, branch, title, body)
}

var errBoom = errors.New("boom")

// remWith is newRemediation over any CRRunner/ForgeWriter, optionally with
// interceptors on the fake client.
func remWith(
	t *testing.T, runner CRRunner, fw ForgeWriter, funcs *interceptor.Funcs, objs ...client.Object,
) (*RemediationReconciler, client.Client) {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Remediation{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	return &RemediationReconciler{
		Client:        c,
		Runner:        runner,
		Forge:         fw,
		Namespace:     "patchy",
		MaxConcurrent: 1,
		MaxAttempts:   2,
		Enabled:       []string{"claude", "codex"},
		Now:           func() time.Time { return crdClock },
	}, c
}

func remReconcileErr(t *testing.T, r *RemediationReconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: name}})
}

// TestRemediationCollectFailurePaths: every way a finished (or vanished)
// Job can fail to report a usable result fails the run and re-queues the
// finding (edge 12) with the reason recorded.
func TestRemediationCollectFailurePaths(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job-rem-1")
	tests := []struct {
		name       string
		runner     *errRunner
		wantReason string
	}{
		{"job vanished", &errRunner{fakeCRRunner: &fakeCRRunner{}, statusErr: gone},
			"aborted: agent job vanished before reporting"},
		{"fatal event", &errRunner{fakeCRRunner: &fakeCRRunner{done: true, events: []envelope.Event{
			{V: envelope.Version, Type: envelope.TypeFatal, Error: "workspace missing"},
		}}}, "aborted: workspace missing"},
		{"no remediation event", &errRunner{fakeCRRunner: &fakeCRRunner{done: true}},
			"aborted: agent job produced no remediation event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &fakeForge{}
			r, c := remWith(t, tt.runner, fw, nil, runningRemediation()...)
			if _, err := remReconcileErr(t, r, remName); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(fw.pushed) != 0 || fw.prCalls != 0 {
				t.Errorf("forge calls %v/%d, want none", fw.pushed, fw.prCalls)
			}
			rem := getRem(t, c)
			if rem.Status.Phase != v1alpha1.RunFailed {
				t.Errorf("run phase = %q, want Failed", rem.Status.Phase)
			}
			f := findingNow(t, c)
			if f.Status.Phase != v1alpha1.PhaseQueued || f.Status.LastFailureReason != tt.wantReason {
				t.Errorf("finding = %q %q, want Queued %q", f.Status.Phase, f.Status.LastFailureReason, tt.wantReason)
			}
			if f.Status.ActiveRun != nil {
				t.Errorf("activeRun = %+v, want cleared", f.Status.ActiveRun)
			}
			if f.Status.Remediation == nil || f.Status.Remediation.Success || f.Status.Remediation.Name != remName {
				t.Errorf("remediation summary = %+v, want an unsuccessful %s", f.Status.Remediation, remName)
			}
		})
	}
}

// TestRemediationFatalKeepsReportedStage: a fatal event after the
// remediation event fails the run but keeps the agent's accounting.
func TestRemediationFatalKeepsReportedStage(t *testing.T) {
	events := append(crdRemediationEvent(true), envelope.Event{
		V: envelope.Version, Type: envelope.TypeFatal, Error: "late crash",
	})
	r, c := remWith(t, &fakeCRRunner{done: true, events: events}, &fakeForge{}, nil, runningRemediation()...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	rem := getRem(t, c)
	if rem.Status.Phase != v1alpha1.RunFailed || rem.Status.Stage == nil ||
		rem.Status.Stage.Usage.CostUSD != "3.500000" || rem.Status.Stage.Outcome != "aborted" {
		t.Errorf("run = %q stage %+v, want Failed aborted with the reported cost kept", rem.Status.Phase, rem.Status.Stage)
	}
}

// TestRemediationCollectTransientErrors: a transient Status or Result error
// is returned for a retry and never fails the run.
func TestRemediationCollectTransientErrors(t *testing.T) {
	for name, runner := range map[string]*errRunner{
		"status": {fakeCRRunner: &fakeCRRunner{}, statusErr: errBoom},
		"result": {fakeCRRunner: &fakeCRRunner{done: true}, resultErr: errBoom},
	} {
		t.Run(name, func(t *testing.T) {
			r, c := remWith(t, runner, &fakeForge{}, nil, runningRemediation()...)
			if _, err := remReconcileErr(t, r, remName); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunRunning {
				t.Errorf("run phase = %q, want still Running", rem.Status.Phase)
			}
			if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseRemediating {
				t.Errorf("finding phase = %q, want still Remediating", f.Status.Phase)
			}
		})
	}
}

// TestRemediationCollectWaitsForDefaultJob: an unfinished default-image Job
// is left to the Job watch, with no requeue and no state change.
func TestRemediationCollectWaitsForDefaultJob(t *testing.T) {
	runner := &fakeCRRunner{status: &jobs.Status{Active: 1, Created: crdClock.Add(-time.Minute)}}
	r, c := remWith(t, runner, &fakeForge{}, nil, runningRemediation()...)
	res, err := remReconcileErr(t, r, remName)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 || runner.results != 0 {
		t.Errorf("requeue %v, results %d, want neither", res.RequeueAfter, runner.results)
	}
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("run phase = %q, want Running", rem.Status.Phase)
	}
}

// TestRemediationSucceedForgeErrors: a failed push or PR is returned for a
// retry; the finding stays Remediating and the run Running, so the next
// pass pushes again (the PR is found by head, never duplicated).
func TestRemediationSucceedForgeErrors(t *testing.T) {
	tests := []struct {
		name    string
		fw      *errForge
		wantErr string
		pushes  int
	}{
		{"push", &errForge{pushErr: errBoom}, "push remediation branch", 0},
		{"pr", &errForge{prErr: errBoom}, "open remediation PR", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeCRRunner{done: true, events: crdRemediationEvent(true)}
			r, c := remWith(t, runner, tt.fw, nil, runningRemediation()...)
			_, err := remReconcileErr(t, r, remName)
			if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Reconcile error = %v, want %q wrapping %v", err, tt.wantErr, errBoom)
			}
			if len(tt.fw.pushed) != tt.pushes {
				t.Errorf("pushes = %d, want %d", len(tt.fw.pushed), tt.pushes)
			}
			if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunRunning {
				t.Errorf("run phase = %q, want Running", rem.Status.Phase)
			}
			if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseRemediating || f.Status.PullRequest != nil {
				t.Errorf("finding = %q pr %+v, want Remediating with no PR", f.Status.Phase, f.Status.PullRequest)
			}
		})
	}
}

// TestRemediationSucceedWithoutRepository: a success for a finding with no
// repository cannot be pushed anywhere and fails the run.
func TestRemediationSucceedWithoutRepository(t *testing.T) {
	objs := runningRemediation()
	objs[0].(*v1alpha1.Finding).Spec.Repository = nil
	fw := &fakeForge{}
	r, c := remWith(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, nil, objs...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fw.pushed) != 0 {
		t.Errorf("pushed %v, want nothing", fw.pushed)
	}
	f := findingNow(t, c)
	if f.Status.Phase != v1alpha1.PhaseQueued ||
		f.Status.LastFailureReason != "aborted: success without changeset or repository" {
		t.Errorf("finding = %q %q, want Queued with the refusal", f.Status.Phase, f.Status.LastFailureReason)
	}
}

// TestRemediationSucceedFindingGone: a result for a finding that no longer
// exists is dropped without any forge call or error.
func TestRemediationSucceedFindingGone(t *testing.T) {
	objs := runningRemediation()[1:]
	fw := &fakeForge{}
	r, _ := remWith(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, nil, objs...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fw.pushed) != 0 || fw.prCalls != 0 {
		t.Errorf("forge calls %v/%d, want none for a vanished finding", fw.pushed, fw.prCalls)
	}
}

// TestRemediationChangesetRulesLookupErrors: a transient error reading the
// Repository or the Investigation is returned before any forge call, never
// treated as "not found" (which would change the rules).
func TestRemediationChangesetRulesLookupErrors(t *testing.T) {
	for _, kind := range []string{"Repository", "Investigation"} {
		t.Run(kind, func(t *testing.T) {
			getCalls := 0
			funcs := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *v1alpha1.Repository:
					if kind == "Repository" {
						return errBoom
					}
				case *v1alpha1.Investigation:
					if kind == "Investigation" {
						getCalls++
						return errBoom
					}
				}
				return c.Get(ctx, key, obj, opts...)
			}}
			fw := &fakeForge{}
			r, c := remWith(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, funcs,
				runningRemediation()...)
			if _, err := remReconcileErr(t, r, remName); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if len(fw.pushed) != 0 {
				t.Errorf("pushed %v, want nothing before the rules are known", fw.pushed)
			}
			if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunRunning {
				t.Errorf("run phase = %q, want Running", rem.Status.Phase)
			}
		})
	}
}

// TestRemediationLaunchPrerequisites: a launch that cannot gather its
// inputs creates no Job.
func TestRemediationLaunchPrerequisites(t *testing.T) {
	granted := func(mut func(objs []client.Object) []client.Object) []client.Object {
		objs := runningRemediation()
		objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
		return mut(objs)
	}
	tests := []struct {
		name    string
		objs    []client.Object
		wantErr bool
	}{
		{"finding gone", granted(func(o []client.Object) []client.Object { return o[1:] }), false},
		{"repository gone", granted(func(o []client.Object) []client.Object { return o[:3] }), true},
		{"repository without artifact", granted(func(o []client.Object) []client.Object {
			o[3].(*v1alpha1.Repository).Status.Artifact = nil
			return o
		}), true},
		{"investigation gone", granted(func(o []client.Object) []client.Object {
			return []client.Object{o[0], o[1], o[3]}
		}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeCRRunner{}
			r, c := remWith(t, runner, &fakeForge{}, nil, tt.objs...)
			_, err := remReconcileErr(t, r, remName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(runner.created) != 0 {
				t.Errorf("jobs created = %d, want 0", len(runner.created))
			}
			if rem := getRem(t, c); rem.Status.JobRef != nil {
				t.Errorf("jobRef = %+v, want none", rem.Status.JobRef)
			}
		})
	}
}

// TestRemediationLaunchCreateError: a Job that cannot be created leaves the
// run Running without a JobRef, so the next pass launches again.
func TestRemediationLaunchCreateError(t *testing.T) {
	objs := runningRemediation()
	objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
	r, c := remWith(t, &errRunner{fakeCRRunner: &fakeCRRunner{}, createErr: errBoom}, &fakeForge{}, nil, objs...)
	if _, err := remReconcileErr(t, r, remName); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if rem := getRem(t, c); rem.Status.JobRef != nil || rem.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("run = %q jobRef %+v, want Running without a Job", rem.Status.Phase, rem.Status.JobRef)
	}
}

// TestRemediationLaunchRecordsJob: a successful launch records the Job and
// the image Create reported.
func TestRemediationLaunchRecordsJob(t *testing.T) {
	objs := runningRemediation()
	objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
	r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, objs...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	rem := getRem(t, c)
	if rem.Status.JobRef == nil || rem.Status.JobRef.Name != "job-rem-1" {
		t.Errorf("jobRef = %+v, want job-rem-1", rem.Status.JobRef)
	}
	if rem.Status.RunnerImage == nil || rem.Status.RunnerImage.Source != v1alpha1.RunnerImageSourceDefault {
		t.Errorf("runnerImage = %+v, want the default image Create reported", rem.Status.RunnerImage)
	}
}

// TestRemediationRunIgnoresSettledAndMissing: a settled or pending run, and
// one that no longer exists, are no-ops.
func TestRemediationRunIgnoresSettledAndMissing(t *testing.T) {
	for _, phase := range []v1alpha1.RunPhase{v1alpha1.RunPending, v1alpha1.RunComplete, v1alpha1.RunFailed} {
		t.Run(string(phase), func(t *testing.T) {
			objs := runningRemediation()
			objs[1].(*v1alpha1.Remediation).Status.Phase = phase
			runner := &fakeCRRunner{done: true, events: crdRemediationEvent(true)}
			r, c := remWith(t, runner, &fakeForge{}, nil, objs...)
			if _, err := remReconcileErr(t, r, remName); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if runner.results != 0 || len(runner.created) != 0 {
				t.Errorf("results %d, jobs %d, want none", runner.results, len(runner.created))
			}
			if rem := getRem(t, c); rem.Status.Phase != phase {
				t.Errorf("phase = %q, want unchanged %q", rem.Status.Phase, phase)
			}
		})
	}
	r, _ := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil)
	if _, err := remReconcileErr(t, r, "missing"); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// deletingRemediation is the running Remediation marked for deletion.
func deletingRemediation(jobRef bool) []client.Object {
	objs := runningRemediation()
	rem := objs[1].(*v1alpha1.Remediation)
	now := metav1.NewTime(crdClock)
	rem.DeletionTimestamp = &now
	rem.Finalizers = []string{v1alpha1.FinalizerJobs, v1alpha1.FinalizerRollupTotal}
	if !jobRef {
		rem.Status.JobRef = nil
	}
	return objs
}

// TestRemediationFinalize: deleting a run deletes its Job, then drops only
// the jobs finalizer — the rollup's finalizers are not this reconciler's.
func TestRemediationFinalize(t *testing.T) {
	for _, withJob := range []bool{true, false} {
		t.Run(map[bool]string{true: "with job", false: "never launched"}[withJob], func(t *testing.T) {
			runner := &fakeCRRunner{}
			r, c := remWith(t, runner, &fakeForge{}, nil, deletingRemediation(withJob)...)
			r.Now = nil // finalize reads no clock
			if _, err := remReconcileErr(t, r, remName); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			wantDeleted := []string(nil)
			if withJob {
				wantDeleted = []string{"job-rem-1"}
			}
			if !slices.Equal(runner.deleted, wantDeleted) {
				t.Errorf("deleted = %v, want %v", runner.deleted, wantDeleted)
			}
			rem := getRem(t, c)
			if !slices.Equal(rem.Finalizers, []string{v1alpha1.FinalizerRollupTotal}) {
				t.Errorf("finalizers = %v, want only the rollup's", rem.Finalizers)
			}
			// A second pass finds nothing to remove and writes nothing.
			if _, err := remReconcileErr(t, r, remName); err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
		})
	}
}

// TestRemediationFinalizeDeleteError: a Job delete that fails (other than
// not-found) keeps the finalizer, so the Job is never orphaned; a Job that
// is already gone does not block.
func TestRemediationFinalizeDeleteError(t *testing.T) {
	gone := kerrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job-rem-1")
	tests := []struct {
		name     string
		err      error
		wantErr  bool
		wantFins []string
	}{
		{"transient", errBoom, true, []string{v1alpha1.FinalizerJobs, v1alpha1.FinalizerRollupTotal}},
		{"already gone", gone, false, []string{v1alpha1.FinalizerRollupTotal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &errRunner{fakeCRRunner: &fakeCRRunner{}, deleteErr: tt.err}
			r, c := remWith(t, runner, &fakeForge{}, nil, deletingRemediation(true)...)
			if _, err := remReconcileErr(t, r, remName); (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
			if rem := getRem(t, c); !slices.Equal(rem.Finalizers, tt.wantFins) {
				t.Errorf("finalizers = %v, want %v", rem.Finalizers, tt.wantFins)
			}
		})
	}
}

// TestRemediationPullFailedDeleteErrorIsLogged: a stuck repository-image
// Job whose delete fails still fails the run — the delete is best effort.
func TestRemediationPullFailedDeleteErrorIsLogged(t *testing.T) {
	runner := &errRunner{fakeCRRunner: &fakeCRRunner{status: &jobs.Status{
		Active: 1, Created: crdClock.Add(-10 * time.Minute), Waiting: "ErrImagePull",
		WaitingMessage: "manifest unknown", RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}}, deleteErr: errBoom}
	r, c := remWith(t, runner, &fakeForge{}, nil,
		withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.deleted) != 1 {
		t.Errorf("deleted = %v, want one attempt", runner.deleted)
	}
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("run phase = %q, want Failed", rem.Status.Phase)
	}
}

// TestRemediationFailDefaultMaxAttempts: MaxAttempts unset means two, so a
// second failed attempt exhausts the finding (edge 15).
func TestRemediationFailDefaultMaxAttempts(t *testing.T) {
	for _, tt := range []struct {
		attempt int32
		want    v1alpha1.Phase
	}{{1, v1alpha1.PhaseQueued}, {2, v1alpha1.PhaseFailed}} {
		objs := runningRemediation()
		objs[0].(*v1alpha1.Finding).Status.Attempts.Remediation = tt.attempt
		objs[1].(*v1alpha1.Remediation).Spec.Attempt = tt.attempt
		r, c := remWith(t, &fakeCRRunner{done: true}, &fakeForge{}, nil, objs...)
		r.MaxAttempts = 0
		if _, err := remReconcileErr(t, r, remName); err != nil {
			t.Fatalf("attempt %d: Reconcile: %v", tt.attempt, err)
		}
		if got := findingNow(t, c).Status.Phase; got != tt.want {
			t.Errorf("attempt %d: phase = %q, want %q", tt.attempt, got, tt.want)
		}
	}
}

// TestRemediationFailCountError: when the refused attempts cannot be
// counted the run is left Running for the next pass, never stamped Failed
// with the finding unreleased.
func TestRemediationFailCountError(t *testing.T) {
	funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*v1alpha1.RemediationList); ok {
			return errBoom
		}
		return c.List(ctx, list, opts...)
	}}
	r, c := remWith(t, &fakeCRRunner{done: true}, &fakeForge{}, funcs, runningRemediation()...)
	if _, err := remReconcileErr(t, r, remName); !errors.Is(err, errBoom) {
		t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
	}
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("run phase = %q, want Running", rem.Status.Phase)
	}
	if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseRemediating {
		t.Errorf("finding phase = %q, want Remediating", f.Status.Phase)
	}
}

// TestRemediationFailLeavesMovedFinding: a failure for a finding no longer
// Remediating (a human acted meanwhile) stamps the run but leaves the
// finding where the human put it.
func TestRemediationFailLeavesMovedFinding(t *testing.T) {
	objs := runningRemediation()
	objs[0].(*v1alpha1.Finding).Status.Phase = v1alpha1.PhaseQueued
	r, c := remWith(t, &fakeCRRunner{done: true}, &fakeForge{}, nil, objs...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("run phase = %q, want Failed", rem.Status.Phase)
	}
	f := findingNow(t, c)
	if f.Status.Phase != v1alpha1.PhaseQueued || f.Status.Remediation != nil || f.Status.LastFailureReason != "" {
		t.Errorf("finding = %q %+v %q, want untouched", f.Status.Phase, f.Status.Remediation, f.Status.LastFailureReason)
	}
}

// TestRemediationEmptyOutcomeReason: an agent event with no outcome is a
// failure whose Complete condition still carries a non-empty reason.
func TestRemediationEmptyOutcomeReason(t *testing.T) {
	events := []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
		Remediation: &envelope.Remediation{}}}
	r, c := remWith(t, &fakeCRRunner{done: true, events: events}, &fakeForge{}, nil, runningRemediation()...)
	if _, err := remReconcileErr(t, r, remName); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	rem := getRem(t, c)
	if rem.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("run phase = %q, want Failed", rem.Status.Phase)
	}
	var reason string
	for _, cond := range rem.Status.Conditions {
		if cond.Type == v1alpha1.ConditionComplete {
			reason = cond.Reason
		}
	}
	if reason != "Unknown" {
		t.Errorf("Complete reason = %q, want Unknown", reason)
	}
}

func scheduleOnce(t *testing.T, r *RemediationReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: remSchedulerRequest},
	})
}

// pendingRem is a Pending Remediation of finding at prio.
func pendingRem(name, finding string, prio int32) *v1alpha1.Remediation {
	return &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy", CreationTimestamp: metav1.NewTime(crdClock)},
		Spec: v1alpha1.RemediationSpec{
			FindingRef:       v1alpha1.ObjectReference{Name: finding},
			InvestigationRef: v1alpha1.ObjectReference{Name: "i"},
			RepositoryRef:    v1alpha1.LocalObjectReference{Name: "s"},
			Attempt:          1, Priority: prio,
		},
	}
}

// TestRemediationSchedulerGrantMovesFinding: a grant moves the run to
// Running and its Queued finding to Remediating (edge 11) with the run
// recorded as active.
func TestRemediationSchedulerGrantMovesFinding(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, fnd, pendingRem("rem-a", fnd.Name, 50))
	res, err := scheduleOnce(t, r)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if res.RequeueAfter != 5*time.Minute {
		t.Errorf("requeue = %v, want the 5m backstop", res.RequeueAfter)
	}
	f := findingNow(t, c)
	if f.Status.Phase != v1alpha1.PhaseRemediating {
		t.Errorf("finding phase = %q, want Remediating", f.Status.Phase)
	}
	if f.Status.ActiveRun == nil || f.Status.ActiveRun.Name != "rem-a" ||
		f.Status.ActiveRun.Kind != v1alpha1.RunKindRemediation {
		t.Errorf("activeRun = %+v, want rem-a", f.Status.ActiveRun)
	}
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "rem-a"}, &rem); err != nil {
		t.Fatal(err)
	}
	if rem.Status.Phase != v1alpha1.RunRunning || rem.Status.GrantedAt == nil ||
		!rem.Status.GrantedAt.Time.Equal(crdClock) {
		t.Errorf("run = %q grantedAt %v, want Running at %v", rem.Status.Phase, rem.Status.GrantedAt, crdClock)
	}
}

// TestRemediationSchedulerGrantLeavesNonQueuedFinding: a grant whose
// finding is not Queued runs the remediation but leaves the finding's phase
// alone (no illegal edge).
func TestRemediationSchedulerGrantLeavesNonQueuedFinding(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseAwaitingApproval)
	r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, fnd, pendingRem("rem-a", fnd.Name, 50))
	if _, err := scheduleOnce(t, r); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if f := findingNow(t, c); f.Status.Phase != v1alpha1.PhaseAwaitingApproval || f.Status.ActiveRun != nil {
		t.Errorf("finding = %q active %+v, want AwaitingApproval untouched", f.Status.Phase, f.Status.ActiveRun)
	}
}

// TestRemediationSchedulerSlots: running runs consume slots, a run being
// deleted is never granted, and MaxConcurrent <= 0 means one slot.
func TestRemediationSchedulerSlots(t *testing.T) {
	running := pendingRem("rem-running", "f1", 10)
	running.Status.Phase = v1alpha1.RunRunning
	deleting := pendingRem("rem-deleting", "f2", 99)
	now := metav1.NewTime(crdClock)
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{v1alpha1.FinalizerJobs}

	tests := []struct {
		name        string
		maxConc     int
		objs        []client.Object
		wantRunning []string
	}{
		{"running fills the only slot", 1,
			[]client.Object{running, pendingRem("rem-p", "f3", 50)}, []string{"rem-running"}},
		{"zero max means one", 0,
			[]client.Object{pendingRem("rem-a", "f3", 50), pendingRem("rem-b", "f4", 40)}, []string{"rem-a"}},
		{"a second slot admits one more", 2,
			[]client.Object{running, pendingRem("rem-p", "f3", 50), pendingRem("rem-q", "f4", 40)},
			[]string{"rem-p", "rem-running"}},
		{"deleting is skipped", 1,
			[]client.Object{deleting, pendingRem("rem-p", "f3", 50)}, []string{"rem-p"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, len(tt.objs))
			for i, o := range tt.objs {
				objs[i] = o.DeepCopyObject().(client.Object)
			}
			r, c := remWith(t, &fakeCRRunner{}, &fakeForge{}, nil, objs...)
			r.MaxConcurrent = tt.maxConc
			if _, err := scheduleOnce(t, r); err != nil {
				t.Fatalf("schedule: %v", err)
			}
			var list v1alpha1.RemediationList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, rem := range list.Items {
				if rem.Status.Phase == v1alpha1.RunRunning {
					got = append(got, rem.Name)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantRunning) {
				t.Errorf("running = %v, want %v", got, tt.wantRunning)
			}
		})
	}
}

// TestRemediationSchedulerListError: a failed list is returned and grants
// nothing.
func TestRemediationSchedulerListError(t *testing.T) {
	funcs := &interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return errBoom
	}}
	r, _ := remWith(t, &fakeCRRunner{}, &fakeForge{}, funcs)
	if _, err := scheduleOnce(t, r); !errors.Is(err, errBoom) {
		t.Errorf("schedule error = %v, want %v", err, errBoom)
	}
}

// TestRemediationSchedulerGrantError: a failed grant write is returned so
// the scheduler retries.
func TestRemediationSchedulerGrantError(t *testing.T) {
	funcs := &interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object,
		...client.SubResourceUpdateOption) error {
		return errBoom
	}}
	r, _ := remWith(t, &fakeCRRunner{}, &fakeForge{}, funcs, pendingRem("rem-a", "", 50))
	if _, err := scheduleOnce(t, r); !errors.Is(err, errBoom) {
		t.Errorf("schedule error = %v, want %v", err, errBoom)
	}
}

func spawnErr(t *testing.T, r *SpawnerReconciler) error {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"},
	})
	return err
}

// TestSpawnerIgnores: findings the spawner has no edge for — suspended,
// deleting, missing, unapproved holds, hand-offs no human revived, failures
// with no retry, and every other phase — are left untouched with no child.
func TestSpawnerIgnores(t *testing.T) {
	now := metav1.NewTime(crdClock)
	tests := []struct {
		name string
		mut  func(*v1alpha1.Finding)
	}{
		{"suspended", func(f *v1alpha1.Finding) { f.Spec.Suspend = true }},
		{"deleting", func(f *v1alpha1.Finding) {
			f.DeletionTimestamp = &now
			f.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
		}},
		{"awaiting approval without approval", func(f *v1alpha1.Finding) {
			f.Status.Phase = v1alpha1.PhaseAwaitingApproval
		}},
		{"handed off without repository", func(f *v1alpha1.Finding) {
			f.Status.Phase = v1alpha1.PhaseHandedOff
			f.Spec.Approval = &v1alpha1.Approval{By: "a", At: now}
			f.Spec.Repository = nil
		}},
		{"handed off without approval", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseHandedOff }},
		{"failed without retry", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseFailed }},
		{"enhanced", func(f *v1alpha1.Finding) { f.Status.Phase = v1alpha1.PhaseEnhanced }},
		{"queued without investigation", func(f *v1alpha1.Finding) { f.Status.Investigation = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fnd := queuedFinding(v1alpha1.PhaseQueued)
			tt.mut(fnd)
			want := fnd.Status.Phase
			r, c := newSpawner(t, fnd, invChild())
			if err := spawnErr(t, r); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := findingNow(t, c); got.Status.Phase != want || got.Status.Attempts.Remediation != 0 {
				t.Errorf("finding = %q attempts %d, want %q with none", got.Status.Phase,
					got.Status.Attempts.Remediation, want)
			}
			var list v1alpha1.RemediationList
			if err := c.List(t.Context(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Errorf("remediations = %d, want 0", len(list.Items))
			}
		})
	}
	r, _ := newSpawner(t)
	if err := spawnErr(t, r); err != nil {
		t.Errorf("Reconcile(missing) error = %v, want nil", err)
	}
}

// TestSpawnerHoldsWhileAttemptInFlight: a latest attempt still Pending or
// Running must not breed a sibling.
func TestSpawnerHoldsWhileAttemptInFlight(t *testing.T) {
	for _, phase := range []v1alpha1.RunPhase{"", v1alpha1.RunPending, v1alpha1.RunRunning} {
		t.Run(string(phase), func(t *testing.T) {
			fnd := queuedFinding(v1alpha1.PhaseQueued)
			fnd.Status.Attempts.Remediation = 1
			child := pendingRem("finding-aa-1-rem-1", fnd.Name, 10)
			child.Status.Phase = phase
			r, c := newSpawner(t, fnd, invChild(), child)
			if err := spawnErr(t, r); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			var rem v1alpha1.Remediation
			err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-2"}, &rem)
			if !kerrors.IsNotFound(err) {
				t.Errorf("attempt 2 lookup error = %v, want not found", err)
			}
		})
	}
}

// TestSpawnerWithoutInvestigationChild: a missing Investigation child
// spawns with the controller default model and the automated grant.
func TestSpawnerWithoutInvestigationChild(t *testing.T) {
	r, c := newSpawner(t, queuedFinding(v1alpha1.PhaseQueued))
	if err := spawnErr(t, r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"}, &rem); err != nil {
		t.Fatalf("Remediation not created: %v", err)
	}
	p := rem.Spec.Parameters
	if p.Model != "anthropic/claude-sonnet-5" || p.Harness != "claude" || p.MaxTurns != 80 ||
		p.TokenBudget != 400000 || p.Estimate != nil {
		t.Errorf("parameters = %+v, want the default model on the automated grant", p)
	}
	if rem.Spec.ApprovedBy != "" || rem.Spec.Revival {
		t.Errorf("approvedBy/revival = %q/%v, want none", rem.Spec.ApprovedBy, rem.Spec.Revival)
	}
	if len(rem.OwnerReferences) != 1 || rem.OwnerReferences[0].Name != "finding-aa-1" {
		t.Errorf("ownerRefs = %+v, want the finding", rem.OwnerReferences)
	}
	if !slices.Contains(rem.Finalizers, v1alpha1.FinalizerJobs) {
		t.Errorf("finalizers = %v, want the jobs finalizer", rem.Finalizers)
	}
}

// TestSpawnerErrors: transient API errors during spawn are returned, and a
// create failure does not count an attempt.
func TestSpawnerErrors(t *testing.T) {
	tests := []struct {
		name  string
		funcs interceptor.Funcs
		objs  func() []client.Object
	}{
		{"latest child lookup", interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
			key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.Remediation); ok {
				return errBoom
			}
			return c.Get(ctx, key, obj, opts...)
		}}, func() []client.Object {
			f := queuedFinding(v1alpha1.PhaseQueued)
			f.Status.Attempts.Remediation = 1
			return []client.Object{f, invChild()}
		}},
		{"create", interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
			...client.CreateOption) error {
			return errBoom
		}}, func() []client.Object { return []client.Object{queuedFinding(v1alpha1.PhaseQueued), invChild()} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(tt.objs()...).
				WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Remediation{}).
				WithInterceptorFuncs(tt.funcs).Build()
			r, _ := newSpawner(t)
			r.Client = c
			before := findingNow(t, c).Status.Attempts.Remediation
			if err := spawnErr(t, r); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if got := findingNow(t, c).Status.Attempts.Remediation; got != before {
				t.Errorf("attempts = %d, want unchanged %d", got, before)
			}
		})
	}
}

// TestSpawnerAdmitErrors: a failed status write on admission or retry is
// returned, and the phase does not move.
func TestSpawnerAdmitErrors(t *testing.T) {
	approved := queuedFinding(v1alpha1.PhaseAwaitingApproval)
	approved.Spec.Approval = &v1alpha1.Approval{By: "alice", At: metav1.NewTime(crdClock)}
	for name, fnd := range map[string]*v1alpha1.Finding{
		"approval": approved,
		"retry":    failedRemediationFinding(crdClock.Add(-time.Minute)),
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(fnd, invChild()).
				WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Remediation{}).
				WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client,
					string, client.Object, ...client.SubResourceUpdateOption) error {
					return errBoom
				}}).Build()
			r, _ := newSpawner(t)
			r.Client = c
			r.Now = nil // admission works on the real clock too
			if err := spawnErr(t, r); !errors.Is(err, errBoom) {
				t.Fatalf("Reconcile error = %v, want %v", err, errBoom)
			}
			if got := findingNow(t, c).Status.Phase; got != fnd.Status.Phase {
				t.Errorf("phase = %q, want unchanged %q", got, fnd.Status.Phase)
			}
		})
	}
}

// TestSpawnerRetryRecordsCondition: a retry admission records who asked on
// the Retried condition.
func TestSpawnerRetryRecordsCondition(t *testing.T) {
	r, c := newSpawner(t, failedRemediationFinding(crdClock.Add(-time.Minute)), invChild())
	if err := spawnErr(t, r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f := findingNow(t, c)
	var found bool
	for _, cond := range f.Status.Conditions {
		if cond.Type == v1alpha1.ConditionRetried && cond.Status == metav1.ConditionTrue && cond.Message == "dev" {
			found = true
		}
	}
	if !found {
		t.Errorf("conditions = %+v, want Retried by dev", f.Status.Conditions)
	}
}

// TestResolveGrantProperties: the grant is never below the automated floor
// (defaults applied), never above the larger of the manual and automated
// budgets, equals the floor without an approval, and with one is exactly
// the estimate clamped into [floor, ceiling].
func TestResolveGrantProperties(t *testing.T) {
	type in struct {
		AutoTurns, ManualTurns, EstTurns    int32
		AutoBudget, ManualBudget, EstBudget int64
		Approved, NoEstimate                bool
	}
	prop := func(x in) bool {
		r := &SpawnerReconciler{
			AutoMaxTurns: x.AutoTurns % 500, AutoTokenBudget: x.AutoBudget % 5_000_000,
			ManualMaxTurns: x.ManualTurns % 1000, ManualTokenBudget: x.ManualBudget % 10_000_000,
		}
		var est *v1alpha1.AgentEstimate
		if !x.NoEstimate {
			est = &v1alpha1.AgentEstimate{MaxTurns: x.EstTurns % 2000, TokenBudget: x.EstBudget % 20_000_000}
		}
		turns, budget := r.resolveGrant(est, x.Approved)

		floorT, floorB := r.AutoMaxTurns, r.AutoTokenBudget
		if floorT <= 0 {
			floorT = defaultAutoMaxTurns
		}
		if floorB <= 0 {
			floorB = defaultAutoTokenBudget
		}
		ceilT, ceilB := max(r.ManualMaxTurns, floorT), max(r.ManualTokenBudget, floorB)
		if est == nil || !x.Approved {
			return turns == floorT && budget == floorB
		}
		return turns == min(max(est.MaxTurns, floorT), ceilT) && budget == min(max(est.TokenBudget, floorB), ceilB)
	}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20260721))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
