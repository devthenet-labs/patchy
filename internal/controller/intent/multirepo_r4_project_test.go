// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestGrantedRunOfADeletedProjectHandsBackItsSlot is the round-4 regression
// of a run granted a slot whose Project is deleted before it launches. launch
// returned the Project's NotFound as an error, so the run stayed Running with
// no Job, holding its slot for as long as the Project stayed gone: in a pool
// of one, every other Project's intents stalled behind it, for good if the
// Project never came back. Now the run hands its slot back and waits Pending,
// another Project's run takes the slot, and once its Project is created again
// it is granted again.
func TestGrantedRunOfADeletedProjectHandsBackItsSlot(t *testing.T) {
	other := testProject()
	other.Name, other.UID = "other", "other-project-uid"
	e := newEnv(t, testProject(), other)
	ctx := context.Background()
	const run, otherRun = "target-9-plan-r1-a1", "other-8-plan-r1-a1"
	e.pendingRun(run, "target-9", v1alpha1.IntentStagePlan)
	e.readyRepositories(repoImage)
	if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
		t.Fatal(err)
	}
	if got := e.irun(run).Status.Phase; got != v1alpha1.RunRunning {
		t.Fatalf("run %s is %q, want granted", run, got)
	}

	e.deleteProject()
	if _, err := e.runs.Reconcile(ctx, req(run)); err != nil {
		t.Errorf("launching a run whose project is gone: %v; want its slot handed back", err)
	}
	if got := e.irun(run); got.Status.Phase != v1alpha1.RunPending || got.Status.JobRef != nil {
		t.Errorf("run %s is %q with job %v, want Pending with none", run, got.Status.Phase, got.Status.JobRef)
	}

	// Another Project's intent takes the one slot.
	if err := e.c.Create(ctx, &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "other-8", Namespace: testNS},
		Spec: v1alpha1.IntentSpec{Project: "other", Issue: v1alpha1.IntentIssue{Repository: intentRepoURL, Number: 8},
			RequestedBy: v1alpha1.IntentRequest{Login: approver, EventID: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	e.pendingRun(otherRun, "other-8", v1alpha1.IntentStagePlan)
	e.readyRepositories(repoImage)
	if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
		t.Fatal(err)
	}
	if got := e.irun(otherRun).Status.Phase; got != v1alpha1.RunRunning {
		t.Errorf("another project's run is %q beside a run whose project is gone, want granted", got)
	}
	if got := e.irun(run).Status.Phase; got != v1alpha1.RunPending {
		t.Errorf("run %s is %q while its project is gone, want still Pending", run, got)
	}

	// Its Project created again, the run is granted again.
	e.runs.MaxConcurrent = 2
	if err := e.c.Create(ctx, testProject()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
		t.Fatal(err)
	}
	if got := e.irun(run).Status.Phase; got != v1alpha1.RunRunning {
		t.Errorf("run %s is %q once its project is back, want granted again", run, got)
	}
}

// TestHoldReasonFollowsItsCause is the round-4 regression of a held build
// whose hold changes cause. Its intent was suspended when it finished, and its
// Project was then deleted, and the suspension lifted. The PushHeld mark kept
// its first reason, IntentSuspended, so the run named a hold that no longer
// held it, and when its Job expired before the Project came back, hold_expired
// said the intent's suspension outlasted the Job. Now the mark names the cause
// the push waits on (ProjectGone), and the expiry says the project was gone.
func TestHoldReasonFollowsItsCause(t *testing.T) {
	e := newEnv(t, testProject())
	ctx := context.Background()
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	build := e.buildLaunched(name)
	e.suspend(name, true)
	if _, err := e.runs.Reconcile(ctx, req(build.Name)); err != nil {
		t.Fatal(err)
	}
	if run := e.irun(build.Name); !heldOn(run, ReasonIntentSuspended) {
		t.Fatalf("build conditions %+v, want its push held on the suspension", run.Status.Conditions)
	}

	e.deleteProject()
	e.suspend(name, false)
	for range 2 {
		e.clock.Advance(time.Minute)
		if _, err := e.runs.Reconcile(ctx, req(build.Name)); err != nil {
			t.Fatal(err)
		}
	}
	if run := e.irun(build.Name); !heldOn(run, ReasonProjectGone) {
		t.Fatalf("build conditions %+v, want its push held on the project being gone", run.Status.Conditions)
	}
	if len(e.gh.commits) != 0 {
		t.Fatalf("%d commits while the project is gone, want none", len(e.gh.commits))
	}

	e.jobs.gone = map[string]bool{build.Status.JobRef.Name: true}
	if err := e.c.Create(ctx, testProject()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runs.Reconcile(ctx, req(build.Name)); err != nil {
		t.Fatal(err)
	}
	run := e.irun(build.Name)
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeHoldExpired ||
		!strings.Contains(run.Status.Detail, "its project was gone") {
		t.Errorf("build %s %s %q, want hold_expired saying its project was gone", run.Status.Phase,
			run.Status.Outcome, run.Status.Detail)
	}
}
