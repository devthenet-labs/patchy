// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// webRepoURL is a multi-repository Project's second repository, named the
// way an operator may name one: mixed case, a dot and an underscore. patchy
// has no rule about repository names; its Project key is "web".
const (
	webRepoURL = "https://github.com/acme/Acme.Web_App"
	webSlug    = "acme/Acme.Web_App"
)

// multiPlan names both repositories of the multi-repository Project, its
// steps grouped under each as the plan prompt asks.
const multiPlan = `---
summary: "Show the API's version on the web page"
repositories:
  - "https://github.com/acme/app"
  - "https://github.com/acme/Acme.Web_App"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## https://github.com/acme/app

Add GET /api/version returning {"sha": "..."}.

## https://github.com/acme/Acme.Web_App

Fetch /api/version and show the sha in the footer.
`

// testMultiProject is testProject with a second repository, keyed "web".
func testMultiProject() *v1alpha1.Project {
	p := testProject()
	p.Spec.Repositories = append(p.Spec.Repositories, v1alpha1.ProjectRepository{Name: "web", URL: webRepoURL})
	return p
}

// multiRepo sets --intent-multi-repo on every reconciler.
func (e *env) multiRepo(on bool) {
	e.project.Settings.MultiRepo = on
	e.intent.Settings.MultiRepo = on
	e.runs.Settings.MultiRepo = on
}

// newMultiEnv is an env over the multi-repository Project with
// --intent-multi-repo on, whose planner plans over both repositories.
func newMultiEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, testMultiProject())
	e.multiRepo(true)
	e.jobs.output = multiOutput
	return e
}

// multiOutput is defaultOutput with the plan over both repositories.
func multiOutput(spec jobs.Spec) jobs.RunOutput {
	out := defaultOutput(spec)
	if spec.Phase == "plan" {
		out.Events[0].Plan.ReportMarkdown = multiPlan
	}
	return out
}

// passUntil runs the intent reconciler, alone, until done reports true.
func (e *env) passUntil(name string, done func() bool) {
	e.t.Helper()
	for range 20 {
		if done() {
			return
		}
		e.mustIntent(name)
		e.clock.Advance(time.Second)
	}
	e.t.Fatalf("intent %s: the condition never held; phase %s", name, e.get(name).Status.Phase)
}

// repository reads the Repository name, nil when there is none.
func (e *env) repository(name string) *v1alpha1.Repository {
	e.t.Helper()
	var repo v1alpha1.Repository
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &repo)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return &repo
}

// TestMultiRepoProjectValidation: a Project of more than one repository is
// Ready only with --intent-multi-repo, and then every repository is held to
// the checks the one repository always was: it resolves to one Forge, and the
// App can push to it and open pull requests in it. A failing repository is
// named. A repository name with mixed case, a dot and an underscore is as
// good as any.
func TestMultiRepoProjectValidation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		multi      bool
		repoErrs   map[string]error
		wantReason string
		wantIn     string
	}{
		{name: "off", wantReason: ReasonUnsupportedRepositories, wantIn: "--intent-multi-repo"},
		{name: "on", multi: true, wantReason: ReasonValidated},
		{name: "the second repository not installed", multi: true,
			repoErrs:   map[string]error{"Installed acme/acme.web_app": ghError(http.StatusNotFound, "Not Found")},
			wantReason: v1alpha1.ReasonAppNotInstalled, wantIn: webRepoURL},
		{name: "the second repository on no forge", multi: true,
			repoErrs:   map[string]error{"Resolve acme/acme.web_app": fmt.Errorf("x: %w", forge.ErrNoMatch)},
			wantReason: v1alpha1.ReasonForgeUnresolved, wantIn: webRepoURL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testMultiProject())
			e.multiRepo(tt.multi)
			maps.Copy(e.gh.repoErrs, tt.repoErrs)
			e.reconcileProject()
			c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Reason != tt.wantReason || !strings.Contains(c.Message, tt.wantIn) {
				t.Fatalf("Ready = %+v, want reason %s naming %q", c, tt.wantReason, tt.wantIn)
			}
			if want := tt.wantReason == ReasonValidated; (c.Status == metav1.ConditionTrue) != want {
				t.Errorf("Ready status = %s", c.Status)
			}
			if tt.wantReason != ReasonValidated {
				return
			}
			for _, slug := range []string{"acme/app", webSlug} {
				var checks int
				for _, s := range e.gh.installed {
					if strings.HasPrefix(s, slug+" ") {
						checks++
					}
				}
				if checks != 2 {
					t.Errorf("%s: %d installation checks, want contents and pull requests: %v", slug, checks,
						e.gh.installed)
				}
			}
		})
	}
}

// TestMultiRepoOffHoldsTheIntent: with --intent-multi-repo off, an Intent of
// a multi-repository Project is held Blocked (UnsupportedRepositories), saying
// why, whatever it was doing: the plan run already leased is never launched,
// and no other is created. Turning the flag on resumes it where it stood,
// with that same run, so turning the flag off is a real rollback.
func TestMultiRepoOffHoldsTheIntent(t *testing.T) {
	e := newMultiEnv(t)
	name := e.newIntent(approver)
	e.passUntil(name, func() bool {
		ar := e.get(name).Status.ActiveRun
		return ar != nil && len(e.runsOf(name, v1alpha1.IntentStagePlan)) == 1
	})
	leased := e.runsOf(name, v1alpha1.IntentStagePlan)[0]

	e.multiRepo(false)
	// The run pool grants the held run no slot, even once its trees are in.
	e.readyRepositories(repoImage)
	if _, err := e.runs.Reconcile(context.Background(), req(runSchedulerRequest)); err != nil {
		t.Fatal(err)
	}
	if r := e.runsOf(name, v1alpha1.IntentStagePlan)[0]; r.Status.Phase == v1alpha1.RunRunning {
		t.Fatal("the run pool granted a slot to a run of a multi-repository project with the flag off")
	}
	e.mustIntent(name)
	in := e.get(name)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories)
	if in.Status.Phase != v1alpha1.IntentBlocked || c == nil || c.Status != metav1.ConditionTrue ||
		c.Reason != ReasonMultiRepositoryOff {
		t.Fatalf("phase %s, UnsupportedRepositories %+v; want Blocked on it", in.Status.Phase, c)
	}
	for range 5 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if n := len(e.jobs.launched()); n != 0 {
		t.Fatalf("%d Jobs launched while multi-repository intents are off", n)
	}
	if runs := e.intentRuns(name); len(runs) != 1 || runs[0].Status.Phase == v1alpha1.RunRunning {
		t.Fatalf("runs = %d (the leased one %s); want the one, never granted", len(runs), runs[0].Status.Phase)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentBlocked {
		t.Fatalf("phase %s while the flag is off, want Blocked", in.Status.Phase)
	}
	st := e.gh.withMarker("patchy:intent")
	if len(st) != 1 || !strings.Contains(st[0].Body, "--intent-multi-repo") {
		t.Errorf("the status comment does not say what holds the intent: %+v", st)
	}

	e.multiRepo(true)
	in = e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plans := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(plans) != 1 || plans[0].UID != leased.UID {
		t.Errorf("plan runs = %d, want the one leased before the hold, resumed", len(plans))
	}
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories) {
		t.Error("UnsupportedRepositories is still True")
	}
}

// TestMultiRepoOffBesideAnotherBlock: an intent already Blocked on another
// reason gains UnsupportedRepositories while the flag is off, and loses it
// once the flag is on again, while the other block still holds it.
func TestMultiRepoOffBesideAnotherBlock(t *testing.T) {
	p := testMultiProject()
	p.Spec.Limits.MaxCostMicroUSD = 100000
	e := newEnv(t, p)
	e.multiRepo(true)
	e.jobs.output = multiOutput
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("conditions = %+v, want BudgetExhausted", in.Status.Conditions)
	}
	e.multiRepo(false)
	e.settleActions(name)
	in = e.get(name)
	if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories) {
		t.Fatalf("UnsupportedRepositories = %+v with the flag off",
			meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories))
	}
	e.multiRepo(true)
	e.settleActions(name)
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentBlocked ||
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionUnsupportedRepositories) ||
		!meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Errorf("phase %s, conditions %+v; want Blocked on the budget alone", in.Status.Phase,
			in.Status.Conditions)
	}
}

// TestMultiRepoOffHoldsAGrantedRun: a run granted its slot just before the
// flag went off launches nothing: it hands its slot back and waits.
func TestMultiRepoOffHoldsAGrantedRun(t *testing.T) {
	e := newMultiEnv(t)
	name, run := e.leasePlan()
	e.readyRepositories(repoImage)
	ctx := context.Background()
	if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
		t.Fatal(err)
	}
	if r := e.runsOf(name, v1alpha1.IntentStagePlan)[0]; r.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("plan run %s, want granted", r.Status.Phase)
	}
	e.multiRepo(false)
	if _, err := e.runs.Reconcile(ctx, req(run.Name)); err != nil {
		t.Fatal(err)
	}
	if r := e.runsOf(name, v1alpha1.IntentStagePlan)[0]; r.Status.Phase != v1alpha1.RunPending ||
		r.Status.JobRef != nil || len(e.jobs.launched()) != 0 {
		t.Errorf("plan run %s, job %+v, %d Jobs; want it pending again, nothing launched", r.Status.Phase,
			r.Status.JobRef, len(e.jobs.launched()))
	}
}

// TestMultiRepoOffHoldsThePushAndTheReview: a build that finished just as the
// flag went off pushes nothing (PushHeld, MultiRepositoryOff), and an Intent
// in review is held too; with the flag on again the push is made and the
// intent carries on to review.
func TestMultiRepoOffHoldsThePushAndTheReview(t *testing.T) {
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
	if _, err := e.runs.Reconcile(context.Background(), req(launched.Name)); err != nil {
		t.Fatal(err)
	}
	var run v1alpha1.IntentRun
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(launched), &run); err != nil {
		t.Fatal(err)
	}
	held := meta.FindStatusCondition(run.Status.Conditions, v1alpha1.ConditionPushHeld)
	if held == nil || held.Status != metav1.ConditionTrue || held.Reason != ReasonMultiRepositoryOff ||
		len(e.gh.commits) != 0 {
		t.Fatalf("PushHeld = %+v, %d commits; want the push held on MultiRepositoryOff", held, len(e.gh.commits))
	}

	e.multiRepo(true)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if len(e.gh.commits) != 2 {
		t.Fatalf("commits = %d, want one per repository", len(e.gh.commits))
	}
	e.multiRepo(false)
	e.mustIntent(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentBlocked {
		t.Fatalf("phase %s in review with the flag off, want Blocked", in.Status.Phase)
	}
	e.multiRepo(true)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
}

// TestOneRepositoryUnchangedByTheFlag: a one-repository Project takes exactly
// the same path with --intent-multi-repo on as off: the same runs, specs and
// Jobs (a plan with no trees), the same GitHub calls, the same pull request.
func TestOneRepositoryUnchangedByTheFlag(t *testing.T) {
	type life struct {
		runs  map[string]v1alpha1.IntentRunSpec
		jobs  map[string]jobs.Spec
		calls map[string]int
		body  string
		state string
	}
	live := func(multi bool) life {
		e := newEnv(t, testProject())
		e.multiRepo(multi)
		name := e.awaiting()
		e.gh.label(1, "patchy:approved", approver)
		e.drive(name, v1alpha1.IntentInReview, repoImage)
		e.gh.closePR(true)
		e.drive(name, v1alpha1.IntentMerged, repoImage)
		l := life{runs: map[string]v1alpha1.IntentRunSpec{}, jobs: map[string]jobs.Spec{},
			calls: maps.Clone(e.gh.calls), body: e.gh.prs[1].body, state: e.gh.withMarker("patchy:intent")[0].Body}
		for _, r := range e.intentRuns(name) {
			l.runs[r.Name] = r.Spec
		}
		for _, s := range e.jobs.launched() {
			l.jobs[s.Finding] = s
			if s.Phase == "plan" && (len(s.Trees) != 0 || s.RepoKey != "" || s.RepoURL != "") {
				t.Errorf("multi=%v: a one-repository plan Job carries trees: %+v", multi, s)
			}
		}
		for _, r := range e.runsOf(name, v1alpha1.IntentStagePlan) {
			if len(r.Spec.Trees) != 0 || len(r.Status.Trees) != 0 {
				t.Errorf("multi=%v: a one-repository plan run has trees: %+v", multi, r)
			}
		}
		return l
	}
	off, on := live(false), live(true)
	if !reflect.DeepEqual(off.runs, on.runs) {
		t.Errorf("run specs differ:\noff %+v\non  %+v", off.runs, on.runs)
	}
	if !reflect.DeepEqual(off.jobs, on.jobs) {
		t.Errorf("Job specs differ:\noff %+v\non  %+v", off.jobs, on.jobs)
	}
	if !reflect.DeepEqual(off.calls, on.calls) {
		// The GitHub calls are the intent's whole conversation with GitHub:
		// equal counts per method mean the flag added no read and no write.
		t.Errorf("GitHub calls differ:\noff %v\non  %v", off.calls, on.calls)
	}
	if off.body != on.body || off.state != on.state {
		t.Errorf("the pull request body or status comment differs:\n%s\n%s", off.body, on.body)
	}
}

// TestMultiRepoPlanReadsEveryTree: a plan run of a multi-repository Project
// plans from the first repository and reads every other one as a tree, each
// from a Repository the run owns. Its Job is handed each tree's pinned,
// digest-verified tarball and the manifest keys; the run records what each
// tree was at launch; and every tree Repository is deleted with the plan
// Repository once the run is collected. The plan comment names, in the
// Project's spelling, each repository patchy will open a pull request in.
func TestMultiRepoPlanReadsEveryTree(t *testing.T) {
	e := newMultiEnv(t)
	name, run := e.leasePlan()
	treeRepo := checkTreeLeased(t, e, run)

	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plan := e.onlyLaunch(t, "plan")
	wantTrees := []jobs.Tree{{Key: "web", URL: webRepoURL, ArtifactURL: "http://artifacts/" + treeRepo + ".tar.gz",
		ArtifactDigest: strings.TrimPrefix(digest([]byte(treeRepo)), "sha256:"), BaseSHA: baseSHA}}
	if plan.RepoKey != "app" || plan.RepoURL != appRepoURL || !reflect.DeepEqual(plan.Trees, wantTrees) ||
		plan.RunnerImage != "" {
		t.Fatalf("plan Job: key %q url %q trees %+v image %q", plan.RepoKey, plan.RepoURL, plan.Trees,
			plan.RunnerImage)
	}
	run = e.runsOf(name, v1alpha1.IntentStagePlan)[0]
	wantStatus := []v1alpha1.IntentRunTreeStatus{{Name: "web", BaseSHA: baseSHA,
		ArtifactDigest: wantTrees[0].ArtifactDigest}}
	if !reflect.DeepEqual(run.Status.Trees, wantStatus) {
		t.Errorf("run status.trees = %+v, want %+v", run.Status.Trees, wantStatus)
	}
	for _, n := range []string{run.Spec.Repository.RepositoryRef.Name, treeRepo} {
		if repo := e.repository(n); repo != nil && repo.DeletionTimestamp.IsZero() {
			t.Errorf("Repository %s outlived the plan run's collection", n)
		}
	}
	posted := e.gh.withMarker("patchy:plan")
	if len(posted) != 1 || !strings.Contains(posted[0].Body, "`acme/app`") ||
		!strings.Contains(posted[0].Body, "`"+webSlug+"`") || !strings.Contains(posted[0].Body, multiPlan) {
		t.Errorf("the plan comment does not name both repositories beside the verbatim plan: %+v", posted)
	}
}

// checkTreeLeased: a plan run of the multi-repository Project plans from the
// first repository and reads the other as a tree, from a Repository the run
// owns at the tree's default branch. It returns that Repository's name.
func checkTreeLeased(t *testing.T, e *env, run v1alpha1.IntentRun) string {
	t.Helper()
	treeRepo := run.Name + "-src-web"
	want := []v1alpha1.IntentRunTree{{Name: "web", URL: webRepoURL,
		RepositoryRef: v1alpha1.LocalObjectReference{Name: treeRepo}}}
	if !reflect.DeepEqual(run.Spec.Trees, want) || run.Spec.Repository.URL != appRepoURL {
		t.Fatalf("plan run trees = %+v from %s, want %+v from the first repository", run.Spec.Trees,
			run.Spec.Repository.URL, want)
	}
	for _, n := range []string{run.Spec.Repository.RepositoryRef.Name, treeRepo} {
		repo := e.repository(n)
		if repo == nil || !controlledBy(repo.OwnerReferences, run.UID) ||
			repo.Labels[v1alpha1.LabelIntentRun] != run.Name {
			t.Fatalf("Repository %s = %+v, want one the plan run owns", n, repo)
		}
	}
	if repo := e.repository(treeRepo); repo.Spec.URL != webRepoURL || repo.Spec.Ref.Branch != "" {
		t.Errorf("tree Repository spec = %+v, want the web repository's default branch", repo.Spec)
	}
	return treeRepo
}

// markReady pins the Repository name as source-controller would.
func (e *env) markReady(name string) {
	e.t.Helper()
	repo := e.repository(name)
	repo.Status.ResolvedSHA = baseSHA
	repo.Status.Artifact = &v1alpha1.Artifact{URL: "http://artifacts/" + name + ".tar.gz",
		Digest: strings.TrimPrefix(digest([]byte(name)), "sha256:")}
	repo.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Ready", LastTransitionTime: metav1.NewTime(e.clock.Now())}}
	if err := e.c.Status().Update(context.Background(), repo); err != nil {
		e.t.Fatal(err)
	}
}

// stall marks the Repository name Stalled for reason, pinned and stored
// when stored is set.
func (e *env) stall(name, reason string, stored bool) {
	e.t.Helper()
	repo := e.repository(name)
	if stored {
		repo.Status.ResolvedSHA = baseSHA
		repo.Status.Artifact = &v1alpha1.Artifact{URL: "http://artifacts/" + name + ".tar.gz",
			Digest: strings.TrimPrefix(digest([]byte(name)), "sha256:")}
	}
	now := metav1.NewTime(e.clock.Now())
	repo.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionStalled, Status: metav1.ConditionTrue, Reason: reason,
			Message: "the tree cannot be used: " + reason, LastTransitionTime: now},
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, LastTransitionTime: now},
	}
	if err := e.c.Status().Update(context.Background(), repo); err != nil {
		e.t.Fatal(err)
	}
}

// leasePlan runs a new intent's passes until its plan run and its children
// exist, and returns the run.
func (e *env) leasePlan() (string, v1alpha1.IntentRun) {
	e.t.Helper()
	name := e.newIntent(approver)
	e.passUntil(name, func() bool { return e.get(name).Status.ActiveRun != nil })
	return name, e.runsOf(name, v1alpha1.IntentStagePlan)[0]
}

// TestPlanWaitsForEveryTree: the plan launches only once every tree's
// artifact is stored, the slowest included; until then it holds no slot.
func TestPlanWaitsForEveryTree(t *testing.T) {
	e := newMultiEnv(t)
	_, run := e.leasePlan()
	e.markReady(run.Spec.Repository.RepositoryRef.Name)
	for range 3 {
		e.runRuns()
	}
	if n := len(e.jobs.launched()); n != 0 {
		t.Fatalf("%d Jobs launched before the tree was stored", n)
	}
	var got v1alpha1.IntentRun
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&run), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1alpha1.RunRunning {
		t.Fatal("the plan was granted a slot while a tree was still being fetched")
	}
	e.markReady(run.Spec.Trees[0].RepositoryRef.Name)
	e.runRuns()
	e.runRuns()
	if plan := e.onlyLaunch(t, "plan"); len(plan.Trees) != 1 {
		t.Errorf("plan Job trees = %+v", plan.Trees)
	}
}

// TestStalledTreeAbortsThePlan: a tree is held to the planning repository's
// rule. A declared image the policy refuses does not stop a plan, which runs
// the default image whatever any tree declares; any other stall aborts the
// run, naming the tree, rather than leave the intent planning forever.
func TestStalledTreeAbortsThePlan(t *testing.T) {
	t.Run("image rejected: the plan runs", func(t *testing.T) {
		e := newMultiEnv(t)
		_, run := e.leasePlan()
		e.markReady(run.Spec.Repository.RepositoryRef.Name)
		e.stall(run.Spec.Trees[0].RepositoryRef.Name, v1alpha1.ReasonRunnerImageRejected, true)
		e.runRuns()
		e.runRuns()
		if plan := e.onlyLaunch(t, "plan"); len(plan.Trees) != 1 || plan.RunnerImage != "" {
			t.Errorf("plan Job = %+v", plan)
		}
	})
	t.Run("any other stall: the run aborts, naming the tree", func(t *testing.T) {
		e := newMultiEnv(t)
		_, run := e.leasePlan()
		e.markReady(run.Spec.Repository.RepositoryRef.Name)
		e.stall(run.Spec.Trees[0].RepositoryRef.Name, "ArchiveTooLarge", false)
		e.runRuns()
		var got v1alpha1.IntentRun
		if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&run), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != v1alpha1.RunFailed || got.Status.Outcome != OutcomeAborted ||
			!strings.Contains(got.Status.Detail, "tree web") || !strings.Contains(got.Status.Detail, webRepoURL) {
			t.Errorf("run = %s %s %q, want aborted naming the tree", got.Status.Phase, got.Status.Outcome,
				got.Status.Detail)
		}
		if n := len(e.jobs.launched()); n != 0 {
			t.Errorf("%d Jobs launched beside a stalled tree", n)
		}
	})
}

// TestForeignTreeRepositoryIsNeverUsed: an object someone else owns under a
// tree Repository's derived name is never adopted, and its tarball never
// reaches a Job: the attempt ends launching nothing, the next attempt (a
// name of its own) plans, and the foreign object is left as it was.
func TestForeignTreeRepositoryIsNeverUsed(t *testing.T) {
	foreignName := v1alpha1.IntentRunTreeRepositoryName(
		v1alpha1.IntentRunName("target-1", v1alpha1.IntentStagePlan, 1, "", 1), "web")
	foreign := &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: foreignName, Namespace: testNS,
			Labels: map[string]string{v1alpha1.LabelIntentRun: "target-1-plan-r1-a1"}},
		Spec: v1alpha1.RepositorySpec{URL: webRepoURL},
	}
	e := newEnv(t, testMultiProject(), foreign)
	e.multiRepo(true)
	e.jobs.output = multiOutput
	e.markReady(foreignName)
	name := e.newIntent(approver)
	in := e.driveTolerant(name, v1alpha1.IntentAwaitingApproval)
	plans := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(plans) != 2 || plans[0].Status.Outcome != OutcomeAborted ||
		!strings.Contains(plans[0].Status.Detail, "not this run's own") {
		t.Fatalf("plan runs = %+v, want the first aborted on the foreign tree and a second", plans)
	}
	for _, s := range e.jobs.launched() {
		for _, tree := range s.Trees {
			if strings.Contains(tree.ArtifactURL, foreignName+".tar.gz") {
				t.Errorf("the foreign Repository's tarball reached Job %s", s.Finding)
			}
		}
	}
	got := e.repository(foreignName)
	if got == nil || len(got.OwnerReferences) != 0 {
		t.Errorf("the foreign Repository was changed or deleted: %+v", got)
	}
	if in.Status.Plan == nil {
		t.Error("no plan was recorded")
	}
}

// driveTolerant is drive, tolerating reconcile errors as the controller's
// backoff would.
func (e *env) driveTolerant(name string, want v1alpha1.IntentPhase) *v1alpha1.Intent {
	e.t.Helper()
	ctx := context.Background()
	for range 60 {
		if in := e.get(name); in.Status.Phase == want {
			return in
		}
		_ = e.reconcileIntent(name)
		e.readyRepositories(repoImage)
		_, _ = e.runs.Reconcile(ctx, req(runSchedulerRequest))
		for _, r := range e.intentRuns(name) {
			_, _ = e.runs.Reconcile(ctx, req(r.Name))
		}
		e.clock.Advance(time.Minute)
	}
	in := e.get(name)
	e.t.Fatalf("intent %s did not reach %s: phase %s", name, want, in.Status.Phase)
	return nil
}

// TestRefusedTreesSettleTheLaunch: a Job create the jobs package refuses for
// its trees settles the run launch_refused, a counted attempt, instead of
// holding its slot retrying a launch that cannot succeed.
func TestRefusedTreesSettleTheLaunch(t *testing.T) {
	e := newMultiEnv(t)
	e.jobs.createErr = fmt.Errorf("%w: trees[0] key %q is used twice", jobs.ErrTreesRefused, "app")
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	plans := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(plans) != 2 || slices.ContainsFunc(plans, func(r v1alpha1.IntentRun) bool {
		return r.Status.Outcome != OutcomeLaunchRefused
	}) {
		t.Errorf("plan runs = %+v, want both attempts launch_refused", plans)
	}
}
