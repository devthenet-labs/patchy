// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/resourceclass"
)

// testClasses are the operator's menu in these tests: large, the docs'
// example (no CPU limit, a memory limit above the request), and medium.
func testClasses(t *testing.T) resourceclass.Set {
	t.Helper()
	s, err := resourceclass.Parse(`{
		"large":  {"requests": {"cpu": 4, "memory": "8Gi"}, "limits": {"memory": "10Gi"}},
		"medium": {"requests": {"cpu": "2", "memory": "4Gi"}, "limits": {"memory": "5Gi", "cpu": "3"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	largeResources  = &jobs.Resources{Class: "large", CPURequest: "4", MemoryRequest: "8Gi", MemoryLimit: "10Gi"}
	mediumResources = &jobs.Resources{Class: "medium", CPURequest: "2", MemoryRequest: "4Gi", CPULimit: "3",
		MemoryLimit: "5Gi"}
)

// classes gives every reconciler the operator's classes.
func (e *env) classes(s resourceclass.Set) {
	e.project.Classes, e.intent.Classes, e.runs.Classes = s, s, s
}

// pick sets the resource class the Project picks for the repository keyed
// key, as an operator edits it (a new generation).
func (e *env) pick(key, class string) {
	e.t.Helper()
	p := e.getProject()
	for i := range p.Spec.Repositories {
		if p.Spec.Repositories[i].Name == key {
			p.Spec.Repositories[i].AgentResourceClass = class
		}
	}
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

// specOf is the spec run's Job was created from.
func (e *env) specOf(run v1alpha1.IntentRun) jobs.Spec {
	e.t.Helper()
	if run.Status.JobRef == nil {
		e.t.Fatalf("run %s launched no Job", run.Name)
	}
	e.jobs.mu.Lock()
	defer e.jobs.mu.Unlock()
	spec, ok := e.jobs.specs[run.Status.JobRef.Name]
	if !ok {
		e.t.Fatalf("no Job %s was created", run.Status.JobRef.Name)
	}
	return spec
}

// sameResources compares a Job's resources with those wanted, nil the
// controller's default.
func sameResources(got, want *jobs.Resources) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestRunResources: which runs a repository's class reaches. Plans never;
// builds, revise rounds and check-fix rounds of that repository do; a
// repository that picks none, or that left the Project, gets the default;
// a class the controller does not define is named, never guessed at.
func TestRunResources(t *testing.T) {
	classes := testClasses(t)
	proj := testMultiProject()
	proj.Spec.Repositories[0].AgentResourceClass = "large"
	proj.Spec.Repositories[1].AgentResourceClass = "xl"
	run := func(stage v1alpha1.IntentStage, trigger v1alpha1.IntentRunTrigger, url string) *v1alpha1.IntentRun {
		return &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: stage, Trigger: trigger,
			Repository: v1alpha1.IntentRunRepository{URL: url}}}
	}
	tests := []struct {
		name        string
		run         *v1alpha1.IntentRun
		want        *jobs.Resources
		wantUnknown string
	}{
		{"a plan", run(v1alpha1.IntentStagePlan, "", appRepoURL), nil, ""},
		{"a build", run(v1alpha1.IntentStageBuild, "", appRepoURL), largeResources, ""},
		{"a build, spelled differently", run(v1alpha1.IntentStageBuild, "", "https://GitHub.com/Acme/App.git"),
			largeResources, ""},
		{"a revise round", run(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerReview, appRepoURL),
			largeResources, ""},
		{"a check-fix round", run(v1alpha1.IntentStageRevise, v1alpha1.IntentRunTriggerChecks, appRepoURL),
			largeResources, ""},
		{"an unknown class", run(v1alpha1.IntentStageBuild, "", webRepoURL), nil, "xl"},
		{"a plan of a repository picking an unknown class", run(v1alpha1.IntentStagePlan, "", webRepoURL), nil, ""},
		{"a repository that left the project", run(v1alpha1.IntentStageBuild, "", "https://github.com/acme/gone"),
			nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unknown := runResources(classes, proj, tt.run)
			if !sameResources(got, tt.want) || unknown != tt.wantUnknown {
				t.Errorf("runResources = %+v, %q; want %+v, %q", got, unknown, tt.want, tt.wantUnknown)
			}
		})
	}
	if got, unknown := runResources(nil, proj, run(v1alpha1.IntentStageBuild, "", appRepoURL)); got != nil ||
		unknown != "large" {
		t.Errorf("with no classes defined = %+v, %q; want the class named unknown", got, unknown)
	}
}

// TestResourceClassPerRepository: in a multi-repository intent, the
// repository that picks a class builds and revises on it, the one that
// picks none and the plan run on the default.
func TestResourceClassPerRepository(t *testing.T) {
	e := newMultiEnv(t)
	e.runs.MaxConcurrent = 2
	e.classes(testClasses(t))
	e.pick("web", "large")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)

	plan := e.runsOf(name, v1alpha1.IntentStagePlan)[0]
	if got := e.specOf(plan).Resources; got != nil {
		t.Errorf("the plan ran on %+v, want the default", got)
	}
	if got := e.specOf(e.buildsIn(name, appRepoURL)[0]).Resources; got != nil {
		t.Errorf("the app build ran on %+v, want the default", got)
	}
	if got := e.specOf(e.buildsIn(name, webRepoURL)[0]).Resources; !sameResources(got, largeResources) {
		t.Errorf("the web build ran on %+v, want large", got)
	}

	// A revise round on the web pull request runs on large too.
	var web int64
	for _, pr := range in.Status.PullRequests {
		if sameRepo(pr.Repository, webRepoURL) {
			web = pr.Number
		}
	}
	e.reviewOn(web, 901, "Please show the short sha.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("revise runs = %+v, want one complete", runs)
	}
	if got := e.specOf(runs[0]).Resources; !sameResources(got, largeResources) {
		t.Errorf("the web revise round ran on %+v, want large", got)
	}
}

// TestResourceClassReachesCheckFix: a check-fix round of a repository that
// picks a class runs on it, like its build.
func TestResourceClassReachesCheckFix(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	project.Spec.Repositories[0].AgentResourceClass = "medium"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if got := e.specOf(e.runsOf(name, v1alpha1.IntentStageBuild)[0]).Resources; !sameResources(got,
		mediumResources) {
		t.Errorf("the build ran on %+v, want medium", got)
	}
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head, Status: "completed",
		Conclusion: "failure", AppSlug: "github-actions", Output: ghclient.CheckOutput{Title: "Go failed"}}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks {
		t.Fatalf("revise runs = %+v, want one check-fix round", runs)
	}
	if got := e.specOf(runs[0]).Resources; !sameResources(got, mediumResources) {
		t.Errorf("the check-fix round ran on %+v, want medium", got)
	}
}

// TestUnknownClassHoldsAReviseRound: a Project edited to pick a class
// intent-controller does not define, after its build ran, holds the next
// revise round Pending with no Job and the Intent Blocked from Revising,
// naming the round; once the pick names a defined class, the same round
// resumes active and runs on it, spending one revision.
func TestUnknownClassHoldsAReviseRound(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "large"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)

	e.pick("app", "xl")
	e.reviewOn(in.Status.PullRequests[0].Number, 903, "Please add a test.")
	e.clock.Advance(3 * time.Minute)
	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable)
	if c == nil || c.Reason != ReasonUnknownResourceClass || !strings.Contains(c.Message, "revise round") ||
		!strings.Contains(c.Message, `"xl"`) || v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentRevising {
		t.Fatalf("ResourcesUnavailable = %+v from %s, want the revise round and its class named", c,
			v1alpha1.IntentBlockedFrom(in))
	}
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.JobRef != nil {
		t.Fatalf("revise runs = %+v, want one waiting with no Job", runs)
	}

	e.pick("app", "medium")
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs = e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete || runs[0].Spec.Attempt != 1 {
		t.Fatalf("revise runs = %+v, want the one round, complete on its first attempt", runs)
	}
	if got := e.specOf(runs[0]).Resources; !sameResources(got, mediumResources) {
		t.Errorf("the revise round ran on %+v, want medium", got)
	}
	if in.Status.Revisions != 1 {
		t.Errorf("revisions = %d, want 1", in.Status.Revisions)
	}
}

// wantCondition fails t unless conds holds condType with status and reason,
// its message saying every one of want.
func wantCondition(t *testing.T, conds []metav1.Condition, condType string, status metav1.ConditionStatus,
	reason string, want ...string) {
	t.Helper()
	c := meta.FindStatusCondition(conds, condType)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Fatalf("%s = %+v, want %s with reason %s", condType, c, status, reason)
	}
	for _, w := range want {
		if !strings.Contains(c.Message, w) {
			t.Fatalf("%s message %q does not say %q", condType, c.Message, w)
		}
	}
}

// TestUnknownClassHoldsOnlyItsRepository: a repository picking a class
// intent-controller does not define builds nothing: its run waits Pending,
// never granted a slot (no status write pass after pass), no Job and no
// attempt spent, while the other repository's build runs and pushes. The
// Intent is Blocked naming the class, the Project warns without losing
// Ready, and once the class is known the same run launches on it.
func TestUnknownClassHoldsOnlyItsRepository(t *testing.T) {
	e := newMultiEnv(t)
	e.classes(testClasses(t))
	e.pick("web", "xl")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	wantCondition(t, in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable, metav1.ConditionTrue,
		ReasonUnknownResourceClass, `"xl"`, "in "+webSlug, "defined: large, medium")
	if v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentBuilding {
		t.Errorf("blocked from %s, want Building", v1alpha1.IntentBlockedFrom(in))
	}
	e.checkClassWait(name)

	// The Project warns, and stays Ready: discovery and plans go on.
	e.reconcileProject()
	p := e.getProject()
	wantCondition(t, p.Status.Conditions, v1alpha1.ConditionResourceClassesResolved, metav1.ConditionFalse,
		ReasonUnknownResourceClass, `repository web (`+webRepoURL+`) picks "xl"`)
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("the Project is not Ready over an unknown class: %+v", p.Status.Conditions)
	}

	// The class is changed to one the operator defines: the same run
	// launches on it, its attempt never spent.
	e.pick("web", "large")
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	web := e.buildsIn(name, webRepoURL)
	if len(web) != 1 || web[0].Spec.Attempt != 1 || web[0].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("web builds = %+v, want the one attempt, complete", web)
	}
	if got := e.specOf(web[0]).Resources; !sameResources(got, largeResources) {
		t.Errorf("the web build ran on %+v, want large", got)
	}
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable) {
		t.Error("ResourcesUnavailable is still True")
	}
	e.reconcileProject()
	if !meta.IsStatusConditionTrue(e.getProject().Status.Conditions, v1alpha1.ConditionResourceClassesResolved) {
		t.Error("ResourceClassesResolved is not True once the class is defined")
	}
}

// checkClassWait asserts, of a Blocked multi-repository intent whose web
// repository picks an unknown class, that the app build runs and pushes
// while the web build waits: one attempt, Pending, no Job ever created for
// it, and no scheduler pass writing to it.
func (e *env) checkClassWait(name string) {
	e.t.Helper()
	for range 4 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if app := e.buildsIn(name, appRepoURL); len(app) != 1 || app[0].Status.PushedCommit == "" {
		e.t.Fatalf("app builds = %+v, want the one pushed beside the waiting web build", app)
	}
	web := e.buildsIn(name, webRepoURL)
	waiting := web[0].Status.Phase == v1alpha1.RunPending || web[0].Status.Phase == ""
	if len(web) != 1 || web[0].Status.JobRef != nil || web[0].Spec.Attempt != 1 || !waiting {
		e.t.Fatalf("web builds = %+v, want one attempt waiting, with no Job", web)
	}
	for _, s := range e.jobs.launched() {
		if s.Phase == "build" && s.Repo == webSlug {
			e.t.Fatal("a Job was created for the web build on an unknown class")
		}
	}
	version := web[0].ResourceVersion
	for range 5 {
		e.runRuns()
	}
	if got := e.buildsIn(name, webRepoURL)[0].ResourceVersion; got != version {
		e.t.Errorf("the waiting run was written by the scheduler (resourceVersion %s → %s)", version, got)
	}
}

// TestUnknownClassNeverTakesTheSlot: a build waiting on an unknown class
// outranks a plan in the pool, and is never granted: the plan, lower in
// priority, takes the one slot, and the build's run is never written.
func TestUnknownClassNeverTakesTheSlot(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "xl"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	ctx := context.Background()
	e.pendingRun("target-9-plan-r1-a1", "target-9", v1alpha1.IntentStagePlan)
	e.pendingRun("target-8-bld-r1-app-a1", "target-8", v1alpha1.IntentStageBuild)
	e.readyRepositories(repoImage)
	before := e.runsOf("target-8", v1alpha1.IntentStageBuild)[0].ResourceVersion
	for range 3 {
		if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
			t.Fatal(err)
		}
	}
	if plan := e.runsOf("target-9", v1alpha1.IntentStagePlan)[0]; plan.Status.Phase != v1alpha1.RunRunning {
		t.Errorf("the plan is %q, want Running in the slot the waiting build cannot use", plan.Status.Phase)
	}
	build := e.runsOf("target-8", v1alpha1.IntentStageBuild)[0]
	if build.Status.Phase != "" || build.ResourceVersion != before {
		t.Errorf("the waiting build is %q (resourceVersion %s → %s), want it never granted", build.Status.Phase,
			before, build.ResourceVersion)
	}
}

// TestLaunchBackstopForUnknownClass: a revise round granted a moment before
// its class became unknown hands the slot back before reading anything
// from GitHub (its PR head), and launches nothing.
func TestLaunchBackstopForUnknownClass(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "xl"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	ctx := context.Background()
	e.pendingRun("target-8-rev-r1-a1", "target-8", v1alpha1.IntentStageRevise)
	e.readyRepositories(repoImage)
	// The round's pull request is open: a round on none has ended, and
	// never reaches its launch.
	in := e.get("target-8")
	in.Status.Phase = v1alpha1.IntentRevising
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{Repository: appRepoURL, Number: 5, State: prOpen}}
	if err := e.c.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	run := e.runsOf("target-8", v1alpha1.IntentStageRevise)[0]
	run.Status.Phase = v1alpha1.RunRunning
	if err := e.c.Status().Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	heads := e.gh.calls["HeadSHA"]
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	got := e.runsOf("target-8", v1alpha1.IntentStageRevise)[0]
	if got.Status.Phase != v1alpha1.RunPending || got.Status.JobRef != nil {
		t.Errorf("run = %q with Job %+v, want Pending with none", got.Status.Phase, got.Status.JobRef)
	}
	if n := len(e.jobs.launched()); n != 0 {
		t.Errorf("%d Jobs created", n)
	}
	if e.gh.calls["HeadSHA"] != heads {
		t.Error("the launch read the PR head before finding its class unknown")
	}
}

// unschedulableFor answers Status for the Jobs of stage as a pod no node
// fits since the first look, the scheduler saying why; every other Job
// finished.
func (e *env) unschedulableFor(stage string) {
	var since time.Time
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if spec.Phase != stage {
			return jobs.Status{}, false
		}
		if since.IsZero() {
			since = e.clock.Now()
		}
		st := jobs.Status{Active: 1, Created: since, Unschedulable: "0/1 nodes are available: 1 Insufficient cpu.",
			UnschedulableSince: since, Resources: "no requests or limits"}
		if spec.Resources != nil {
			st.ResourceClass = spec.Resources.Class
			st.Resources = "requests cpu " + spec.Resources.CPURequest + ", memory " + spec.Resources.MemoryRequest
		}
		return st, true
	}
}

// TestUnschedulableBuildStopsOnce: a build whose pod no node fits is looked
// at while it waits, stopped once the grace has passed (its Job deleted),
// and settled unschedulable, uncounted; the Intent is Blocked, saying so
// without the scheduler's words, and stays so until the Project changes,
// when the next attempt runs on the class picked then.
func TestUnschedulableBuildStopsOnce(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "large"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	e.unschedulableFor("build")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	got := e.stopUnschedulable(name, e.buildLaunched(name))
	if !strings.Contains(got.Status.Detail, "resource class large") ||
		!strings.Contains(got.Status.Detail, "Insufficient cpu") {
		t.Fatalf("the build's detail = %q, want the class and the scheduler's words", got.Status.Detail)
	}

	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	wantCondition(t, in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable, metav1.ConditionTrue,
		ReasonUnschedulable, got.Name)
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable); strings.Contains(
		c.Message, "Insufficient") {
		t.Fatalf("ResourcesUnavailable = %q, which carries the scheduler's words", c.Message)
	}
	e.mustIntent(name)
	for _, comment := range e.gh.withMarker("patchy:intent") {
		if strings.Contains(comment.Body, "Insufficient") || !strings.Contains(comment.Body, "could not be scheduled") {
			t.Errorf("status comment = %q", comment.Body)
		}
	}
	// Nothing changed: it stays blocked, and nothing is retried.
	e.settleActions(name)
	if n := len(e.runsOf(name, v1alpha1.IntentStageBuild)); n != 1 {
		t.Fatalf("build runs = %d while nothing changed, want 1", n)
	}

	// The operator picks a smaller class: the next attempt runs on it,
	// told nothing of the last (its agent never ran).
	e.jobs.statusFn = nil
	e.pick("app", "medium")
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) != 2 || runs[1].Spec.Attempt != 2 || runs[1].Spec.PreviousAttempt != nil {
		t.Fatalf("build runs = %+v, want a second attempt with no previous attempt", runs)
	}
	if got := e.specOf(runs[1]).Resources; !sameResources(got, mediumResources) {
		t.Errorf("the second attempt ran on %+v, want medium", got)
	}
}

// stopUnschedulable drives run, a launched build whose pod no node fits,
// through the grace: looked at again within the poll while it waits, still
// Running a second short of the grace, then stopped once past it (its Job
// deleted) and settled unschedulable. It returns the settled run.
func (e *env) stopUnschedulable(name string, run v1alpha1.IntentRun) v1alpha1.IntentRun {
	e.t.Helper()
	ctx := context.Background()
	res, err := e.runs.Reconcile(ctx, req(run.Name))
	if err != nil {
		e.t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > unschedulablePoll {
		e.t.Errorf("an unscheduled pod is looked at again after %s, want within %s", res.RequeueAfter,
			unschedulablePoll)
	}
	e.clock.Advance(UnschedulableGrace - time.Second)
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		e.t.Fatal(err)
	}
	if got := e.runsOf(name, v1alpha1.IntentStageBuild)[0]; got.Status.Phase != v1alpha1.RunRunning {
		e.t.Fatalf("the build settled %s within the grace", got.Status.Outcome)
	}
	e.clock.Advance(2 * time.Second)
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		e.t.Fatal(err)
	}
	got := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	if got.Status.Phase != v1alpha1.RunFailed || got.Status.Outcome != OutcomeUnschedulable {
		e.t.Fatalf("the build = %s %s: %s", got.Status.Phase, got.Status.Outcome, got.Status.Detail)
	}
	if !slices.Contains(e.jobs.deleted, got.Status.JobRef.Name) {
		e.t.Error("the unschedulable Job was not deleted")
	}
	return got
}

// TestUnschedulableStatusWriteBeforeDelete: if the API server refuses the
// terminal write, the Job must still be there for the next pass to reach
// the same uncounted outcome. A missing Job would instead settle aborted
// and spend the attempt, even though the agent never ran.
func TestUnschedulableStatusWriteBeforeDelete(t *testing.T) {
	e := newEnv(t, testProject())
	e.unschedulableFor("build")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	run := e.buildLaunched(name)
	ctx := context.Background()
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(UnschedulableGrace + time.Second)
	e.failRunEvery = 1
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err == nil {
		t.Fatal("terminal status write succeeded despite the injected API failure")
	}
	if slices.Contains(e.jobs.deleted, run.Status.JobRef.Name) {
		t.Fatal("the Job was deleted before its uncounted outcome was durable")
	}
	if got := e.runsOf(name, v1alpha1.IntentStageBuild)[0]; got.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("run phase = %s after the refused write, want Running", got.Status.Phase)
	}
	e.failRunEvery = 0
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	got := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	if got.Status.Outcome != OutcomeUnschedulable || !uncounted(&got) ||
		!slices.Contains(e.jobs.deleted, run.Status.JobRef.Name) {
		t.Fatalf("retry = %s, uncounted %v, Job deleted %v; want unschedulable, true, true",
			got.Status.Outcome, uncounted(&got), slices.Contains(e.jobs.deleted, run.Status.JobRef.Name))
	}
}

// TestUnschedulableDeleteRetries: a delete failure after settlement does not
// leave an unschedulable pod occupying capacity until its Job deadline.
func TestUnschedulableDeleteRetries(t *testing.T) {
	e := newEnv(t, testProject())
	e.unschedulableFor("build")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	run := e.buildLaunched(name)
	ctx := context.Background()
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(UnschedulableGrace + time.Second)
	e.jobs.deleteErr = errTransient
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err == nil {
		t.Fatal("Job delete succeeded despite the injected failure")
	}
	if got := e.runsOf(name, v1alpha1.IntentStageBuild)[0]; got.Status.Outcome != OutcomeUnschedulable {
		t.Fatalf("run outcome = %s after the failed delete, want unschedulable", got.Status.Outcome)
	}
	e.jobs.deleteErr = nil
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.jobs.deleted, run.Status.JobRef.Name) {
		t.Fatal("terminal reconcile did not retry the Job delete")
	}
}

// TestUnschedulablePlanBlocks: a plan no node fits (the default resources
// larger than any node) blocks the Intent rather than spend its attempts,
// and resumes once the Project changes.
func TestUnschedulablePlanBlocks(t *testing.T) {
	e := newEnv(t, testProject())
	e.unschedulableFor("plan")
	name := e.newIntent(approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionResourcesUnavailable)
	if c == nil || c.Reason != ReasonUnschedulable || v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentPlanning {
		t.Fatalf("ResourcesUnavailable = %+v from %s", c, v1alpha1.IntentBlockedFrom(in))
	}
	plans := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(plans) != 1 || plans[0].Status.Outcome != OutcomeUnschedulable {
		t.Fatalf("plan runs = %+v, want one unschedulable", plans)
	}
	e.jobs.statusFn = nil
	e.pick("app", "")
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != 2 {
		t.Errorf("plan runs = %d, want the retry", n)
	}
}

// TestUnschedulableReviseRoundEnds: a revise round whose pod no node fits
// ends, as one with no image does: no revision spent, and its pull request
// told why in words that name nothing of the cluster.
func TestUnschedulableReviseRoundEnds(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "large"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.unschedulableFor("build") // revise runs are build-phase Jobs
	e.reviewOn(number, 902, "Please add a test.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Outcome != OutcomeUnschedulable || in.Status.Revisions != 0 {
		t.Fatalf("revise runs = %+v, revisions %d; want one unschedulable round, none spent", runs,
			in.Status.Revisions)
	}
	var notice string
	for _, c := range e.gh.prComments[number] {
		if strings.Contains(c.Body, "intent-pr-round") {
			notice = c.Body
		}
	}
	if !strings.Contains(notice, "no node in the cluster could fit its agent") ||
		strings.Contains(notice, "Insufficient") {
		t.Errorf("round notice = %q", notice)
	}
}

// TestOOMKilledBuildSaysWhy: a build whose agent was OOM-killed reports no
// result; its detail names the kill, the memory limit and the class, and
// what to change, rather than a bare missing result.
func TestOOMKilledBuildSaysWhy(t *testing.T) {
	project := testProject()
	project.Spec.Repositories[0].AgentResourceClass = "large"
	e := newEnv(t, project)
	e.classes(testClasses(t))
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return defaultOutput(spec)
		}
		return jobs.RunOutput{}
	}
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if spec.Phase != "build" {
			return jobs.Status{}, false
		}
		code := int32(137)
		return jobs.Status{Done: true, Failed: 1, AgentStarted: true, AgentTerminated: "OOMKilled",
			AgentExitCode: &code, MemoryLimit: "10Gi", ResourceClass: spec.Resources.Class}, true
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	run := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	for _, want := range []string{"no build event", "OOM-killed (exit 137)", "memory limit of 10Gi",
		"resource class large", "agentResourceClass"} {
		if !strings.Contains(run.Status.Detail, want) {
			t.Errorf("detail %q does not say %q", run.Status.Detail, want)
		}
	}
}

// TestEvictionDetailStaysOnRun: a pod message can name private cluster
// infrastructure. Operators need it on the run, but the public intent
// issue sees only a short cause and a pointer to that run.
func TestEvictionDetailStaysOnRun(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return defaultOutput(spec)
		}
		return jobs.RunOutput{}
	}
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if spec.Phase != "build" {
			return jobs.Status{}, false
		}
		return jobs.Status{Done: true, Failed: 1, PodReason: "Evicted",
			PodMessage: "private-node-identifier was low on memory"}, true
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) != 2 || runs[1].Status.Outcome != OutcomeEvicted ||
		!strings.Contains(runs[1].Status.Detail, "private-node-identifier") {
		t.Fatalf("build runs = %+v, want two evictions with the pod message on their detail", runs)
	}
	e.mustIntent(name)
	for _, c := range e.gh.withMarker("patchy:intent") {
		if strings.Contains(c.Body, "private-node-identifier") ||
			!strings.Contains(c.Body, "agent pod was evicted") {
			t.Errorf("intent status comment = %q, want a redacted eviction cause", c.Body)
		}
	}
}

// TestUnschedulableJudgement: when a not-yet-finished Job's pod is looked
// at again, and when it is stopped.
func TestUnschedulableJudgement(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		st          jobs.Status
		wantStop    bool
		wantRequeue time.Duration
	}{
		{"finished", jobs.Status{Done: true}, false, 0},
		{"agent running", jobs.Status{AgentStarted: true}, false, 0},
		{"scheduled, pulling", jobs.Status{Scheduled: true}, false, 0},
		{"no pod yet", jobs.Status{}, false, unschedulablePoll},
		{"unschedulable a minute", jobs.Status{Unschedulable: "no fit", UnschedulableSince: now.Add(-time.Minute)},
			false, unschedulablePoll},
		{"unschedulable nearly the grace", jobs.Status{Unschedulable: "no fit",
			UnschedulableSince: now.Add(-UnschedulableGrace + 10*time.Second)}, false, 10 * time.Second},
		{"unschedulable past the grace", jobs.Status{Unschedulable: "no fit",
			UnschedulableSince: now.Add(-UnschedulableGrace)}, true, 0},
	}
	for _, tt := range tests {
		detail, requeue := unschedulable(tt.st, now)
		if (detail != "") != tt.wantStop || requeue != tt.wantRequeue {
			t.Errorf("%s: unschedulable = %q, %s; want stop %v, requeue %s", tt.name, detail, requeue,
				tt.wantStop, tt.wantRequeue)
		}
	}
}

// TestProjectResourceClassesCondition: the Project's warning is absent while
// it picks no class, True naming its picks when each is defined, and False
// naming each unknown one; it never touches Ready.
func TestProjectResourceClassesCondition(t *testing.T) {
	classes := testClasses(t)
	tests := []struct {
		name       string
		picks      map[string]string
		wantStatus metav1.ConditionStatus // "" for absent
		wantIn     []string
	}{
		{name: "no picks"},
		{name: "a defined class", picks: map[string]string{"web": "large"}, wantStatus: metav1.ConditionTrue,
			wantIn: []string{"web picks large"}},
		{name: "an unknown class", picks: map[string]string{"web": "large", "app": "xl"},
			wantStatus: metav1.ConditionFalse,
			wantIn:     []string{`repository app (` + appRepoURL + `) picks "xl"`, "defined: large, medium"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testMultiProject()
			for i := range p.Spec.Repositories {
				p.Spec.Repositories[i].AgentResourceClass = tt.picks[p.Spec.Repositories[i].Name]
			}
			// A stale condition from an earlier pick is replaced or removed.
			p.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionResourceClassesResolved,
				Status: metav1.ConditionUnknown, Reason: "Stale"}}
			setResourceClasses(p, classes)
			c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionResourceClassesResolved)
			if tt.wantStatus == "" {
				if c != nil {
					t.Fatalf("condition = %+v, want none", c)
				}
				return
			}
			if c == nil || c.Status != tt.wantStatus {
				t.Fatalf("condition = %+v, want %s", c, tt.wantStatus)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(c.Message, want) {
					t.Errorf("message %q does not say %q", c.Message, want)
				}
			}
			if meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady) != nil {
				t.Error("the warning wrote Ready")
			}
		})
	}
}

// TestFailedRunReasonKeepsTheSchedulerOffGitHub: an intent that ends Failed
// on an unschedulable run says so on its issue without the scheduler's
// words, which describe the cluster's nodes; every other failure shows its
// detail as before.
func TestFailedRunReasonKeepsTheSchedulerOffGitHub(t *testing.T) {
	run := func(outcome, detail string) *v1alpha1.IntentRun {
		r := &v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunFailed, Outcome: outcome,
			Detail: detail}}
		r.Name = "target-8-bld-r1-app-a3"
		return r
	}
	got := failedRunReason(run(OutcomeUnschedulable, "no node could fit the agent pod for 10m0s: "+
		"0/3 nodes are available: 3 Insufficient cpu."))
	if strings.Contains(got, "Insufficient") || !strings.Contains(got, "target-8-bld-r1-app-a3") {
		t.Errorf("unschedulable reason = %q, want the run named and no scheduler words", got)
	}
	if got := failedRunReason(run(OutcomeAborted, "agent job produced no build event")); got !=
		"aborted: agent job produced no build event" {
		t.Errorf("aborted reason = %q", got)
	}
	if got := failedRunReason(run("timeout", "")); got != "timeout" {
		t.Errorf("bare reason = %q", got)
	}
}
