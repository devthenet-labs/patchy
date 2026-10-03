// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// irun reads the IntentRun name.
func (e *env) irun(name string) *v1alpha1.IntentRun {
	e.t.Helper()
	var run v1alpha1.IntentRun
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &run); err != nil {
		e.t.Fatal(err)
	}
	return &run
}

// deleteProject deletes the test Project.
func (e *env) deleteProject() {
	e.t.Helper()
	if err := e.c.Delete(context.Background(), e.getProject()); err != nil {
		e.t.Fatal(err)
	}
}

// heldOn reports run held PushHeld, with reason.
func heldOn(run *v1alpha1.IntentRun, reason string) bool {
	c := meta.FindStatusCondition(run.Status.Conditions, v1alpha1.ConditionPushHeld)
	return run.Status.Phase == v1alpha1.RunRunning && c != nil && c.Status == metav1.ConditionTrue &&
		c.Reason == reason
}

// TestProjectDeletedMidRoundHoldsThePush is the round-3 regression of a
// Project deleted while a round runs. The push gate read the missing Project
// as nothing to hold, so the round's commit was created and web's open pull
// request branch fast-forwarded, by a token no Project vouched for any more
// (removing web alone refused the same push). Now the push is held
// (PushHeld, ProjectGone): no commit, the branch unmoved, as many passes as
// the Project stays gone. Created again, the Project resumes the round, which
// pushes once.
func TestProjectDeletedMidRoundHoldsThePush(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inRound(2, "web")
	commits, webHead := len(e.gh.commits), e.branchIn(name, webRepoURL)
	e.deleteProject()
	e.releaseJob(jobs.NameFor(v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, "web", 1), KindIntent, 1))
	for range 3 {
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if run := e.reviseRun(name, 1); !heldOn(run, ReasonProjectGone) {
		t.Fatalf("round %s %s %q, conditions %+v; want its push held on ProjectGone", run.Status.Phase,
			run.Status.Outcome, run.Status.Detail, run.Status.Conditions)
	}
	if len(e.gh.commits) != commits || e.branchIn(name, webRepoURL) != webHead {
		t.Fatalf("commits %d -> %d, web branch %s -> %s; want nothing pushed while the project is gone", commits,
			len(e.gh.commits), webHead, e.branchIn(name, webRepoURL))
	}

	if err := e.c.Create(context.Background(), testMultiProject()); err != nil {
		t.Fatal(err)
	}
	e.runRuns()
	run := e.reviseRun(name, 1)
	if run.Status.Phase != v1alpha1.RunComplete || len(e.gh.commits) != commits+1 ||
		e.branchIn(name, webRepoURL) != run.Status.PushedCommit {
		t.Fatalf("round %s, %d new commits, web branch %s; want the held push made once the project is back",
			run.Status.Phase, len(e.gh.commits)-commits, e.branchIn(name, webRepoURL))
	}
}

// TestFlagOffHeldBuildStaysHeldWhenItsProjectIsDeleted: a build whose push
// was held because --intent-multi-repo went off stays held when its
// multi-repository Project is then deleted (a patchy-config uninstall during
// a rollback, say). Before the fix the missing Project released it: the
// commit was made and the patchy-intent branch created with the flag still
// off, against the documented rollback guarantee that nothing is pushed. The
// hold's reason is the cause it waits on now (round 4): ProjectGone while the
// Project is gone, MultiRepositoryOff again once it is back with the flag
// still off.
func TestFlagOffHeldBuildStaysHeldWhenItsProjectIsDeleted(t *testing.T) {
	e := newMultiEnv(t)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	var launched *v1alpha1.IntentRun
	for range 30 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		for _, r := range e.runsOf(name, v1alpha1.IntentStageBuild) {
			if r.Status.JobRef != nil {
				launched = &r
			}
		}
		if launched != nil {
			break
		}
		e.clock.Advance(time.Minute)
	}
	if launched == nil {
		t.Fatal("no build launched")
	}
	e.multiRepo(false)
	e.runRuns()
	if run := e.irun(launched.Name); !heldOn(run, ReasonMultiRepositoryOff) || len(e.gh.commits) != 0 {
		t.Fatalf("build conditions %+v, %d commits; want its push held on the flag", run.Status.Conditions,
			len(e.gh.commits))
	}

	e.deleteProject()
	for range 3 {
		e.clock.Advance(time.Minute)
		e.runRuns()
	}
	if run := e.irun(launched.Name); !heldOn(run, ReasonProjectGone) {
		t.Errorf("build %s, conditions %+v; want it still held, on the project being gone", run.Status.Phase,
			run.Status.Conditions)
	}
	if len(e.gh.commits) != 0 || e.branchIn(name, launched.Spec.Repository.URL) != "" {
		t.Errorf("%d commits, branch %q; want nothing pushed with the flag off and the project gone",
			len(e.gh.commits), e.branchIn(name, launched.Spec.Repository.URL))
	}

	if err := e.c.Create(context.Background(), testMultiProject()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.clock.Advance(time.Minute)
		e.runRuns()
	}
	if run := e.irun(launched.Name); !heldOn(run, ReasonMultiRepositoryOff) {
		t.Errorf("build %s, conditions %+v; want it still held, on the flag, once the project is back",
			run.Status.Phase, run.Status.Conditions)
	}
	if len(e.gh.commits) != 0 || e.branchIn(name, launched.Spec.Repository.URL) != "" {
		t.Errorf("%d commits, branch %q; want nothing pushed with the flag still off",
			len(e.gh.commits), e.branchIn(name, launched.Spec.Repository.URL))
	}
}
