// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// buildsIn are the Intent's build runs in repoURL, by name.
func (e *env) buildsIn(name, repoURL string) []v1alpha1.IntentRun {
	var out []v1alpha1.IntentRun
	for _, r := range e.runsOf(name, v1alpha1.IntentStageBuild) {
		if sameRepo(r.Spec.Repository.URL, repoURL) {
			out = append(out, r)
		}
	}
	return out
}

// siblingComments are the siblings comments on pull request n.
func (e *env) siblingComments(n int64) []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[n] {
		if strings.Contains(markerOf(c.Body), " "+templates.SiblingsKey+" ") {
			out = append(out, c.Body)
		}
	}
	return out
}

// buildJob is the name of the agent Job of the first attempt of the build
// of key in round 1.
func buildJob(key string) string {
	return jobs.NameFor(v1alpha1.IntentRunName("target-1", v1alpha1.IntentStageBuild, 1, key, 1), KindIntent, 1)
}

// TestMultiRepoBuildFanOut drives a multi-repository intent from approval to
// merge. One pass creates one build per approved repository, each in its own
// repository and image, from the approved plan alone; no pull request opens
// until every build has pushed; the pull requests are recorded one per pass
// and the last record moves the intent to InReview; each body says which
// repositories the intent spans. In review each pull request gets the
// comment linking the others, once, and the intent merges when both have.
func TestMultiRepoBuildFanOut(t *testing.T) {
	e := newMultiEnv(t)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	checkFanOut(t, e, name)
	driveFanOutToReview(t, e, name)
	checkFanOutRecords(t, e, name)
	checkSiblingsLinked(t, e, name)
	e.gh.closePRn(1, true)
	e.gh.closePRn(2, true)
	e.drive(name, v1alpha1.IntentMerged, repoImage)
}

// checkFanOut: the pass that starts the build round creates one build per
// approved repository, first attempts of the approved plan's round.
func checkFanOut(t *testing.T, e *env, name string) {
	t.Helper()
	e.passUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageBuild)) > 0 })
	builds := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(builds) != 2 || len(e.buildsIn(name, appRepoURL)) != 1 || len(e.buildsIn(name, webRepoURL)) != 1 {
		t.Fatalf("builds after one pass = %+v, want one per repository", builds)
	}
	digest := e.get(name).Status.Approval.PlanDigest
	for _, b := range builds {
		if b.Spec.Round != 1 || b.Spec.Attempt != 1 || b.Spec.Inputs.PlanDigest != digest {
			t.Errorf("build %s spec = %+v", b.Name, b.Spec)
		}
	}
}

// fanOutInReview checks, after any pass of either reconciler, that no pull
// request is open unless both builds are complete and that the intent is in
// review exactly when both pull requests are recorded; it reports InReview.
func fanOutInReview(t *testing.T, e *env, name string) bool {
	t.Helper()
	in := e.get(name)
	complete := 0
	for _, b := range e.runsOf(name, v1alpha1.IntentStageBuild) {
		if b.Status.Phase == v1alpha1.RunComplete {
			complete++
		}
	}
	if len(e.gh.prs) > 0 && complete < 2 {
		t.Fatalf("a pull request opened while only %d of 2 builds were complete", complete)
	}
	if in.Status.Phase == v1alpha1.IntentBuilding && len(in.Status.PullRequests) == 2 {
		t.Fatal("both pull requests recorded and the intent still Building")
	}
	if in.Status.Phase == v1alpha1.IntentInReview && len(in.Status.PullRequests) != 2 {
		t.Fatalf("InReview with pull requests %+v, want both", in.Status.PullRequests)
	}
	return in.Status.Phase == v1alpha1.IntentInReview
}

// driveFanOutToReview drives the intent to InReview, checking fanOutInReview
// after every pass.
func driveFanOutToReview(t *testing.T, e *env, name string) {
	t.Helper()
	for range 60 {
		if fanOutInReview(t, e, name) {
			return
		}
		e.mustIntent(name)
		if fanOutInReview(t, e, name) {
			return
		}
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	t.Fatalf("the intent never reached InReview: %s", e.get(name).Status.Phase)
}

// checkFanOutRecords: the records and the GitHub writes of the round. Each
// repository was built once, in its own image from the approved plan alone,
// and pushed to its own intent branch; the pull requests are recorded in plan
// order; every body, and the status comment, names both repositories.
func checkFanOutRecords(t *testing.T, e *env, name string) {
	t.Helper()
	in := e.get(name)
	if in.Status.PullRequests[0].Repository != appRepoURL || in.Status.PullRequests[1].Repository != webRepoURL ||
		in.Status.Branch != branchName(name) || in.Status.ActiveRun != nil {
		t.Errorf("status: pull requests %+v, branch %q, activeRun %+v", in.Status.PullRequests, in.Status.Branch,
			in.Status.ActiveRun)
	}
	repos := map[string]bool{}
	for _, s := range e.jobs.launched() {
		if s.Phase != "build" {
			continue
		}
		repos[s.Repo] = true
		if s.RunnerImage != repoImage || s.InvestigationMarkdown != multiPlan || s.IssueMarkdown != "" ||
			len(s.Trees) != 0 {
			t.Errorf("build Job %s: image %q, trees %d", s.Finding, s.RunnerImage, len(s.Trees))
		}
	}
	if !repos["acme/app"] || !repos[webSlug] || len(repos) != 2 {
		t.Errorf("build Jobs ran in %v, want each repository once", repos)
	}
	checkFanOutWrites(t, e, name)
}

// checkFanOutWrites: one commit and one intent branch in each repository,
// and pull request bodies and a status comment naming both.
func checkFanOutWrites(t *testing.T, e *env, name string) {
	t.Helper()
	if !slices.Equal(e.gh.commitRepos, []string{"acme/app", webSlug}) &&
		!slices.Equal(e.gh.commitRepos, []string{webSlug, "acme/app"}) {
		t.Errorf("commits made in %v, want one per repository", e.gh.commitRepos)
	}
	if e.gh.branches[branchName(name)] == "" || e.gh.branches[webSlug+":"+branchName(name)] == "" {
		t.Errorf("branches = %v, want the intent branch in each repository", e.gh.branches)
	}
	for n := int64(1); n <= 2; n++ {
		body := e.gh.prs[n].body
		if !strings.HasPrefix(body, "Part of acme/intents#1") || !strings.Contains(body, "acme/app") ||
			!strings.Contains(body, webSlug) {
			t.Errorf("pull request %d body does not span both repositories:\n%s", n, body)
		}
	}
	if st := e.gh.withMarker("patchy:intent"); len(st) != 1 || !strings.Contains(st[0].Body, webSlug) {
		t.Errorf("the status comment does not name the repositories: %+v", st)
	}
}

// checkSiblingsLinked: in review, each pull request carries one comment
// linking the other, never itself, SiblingsLinked is True, and later passes
// post nothing more.
func checkSiblingsLinked(t *testing.T, e *env, name string) {
	t.Helper()
	e.settleActions(name)
	for n, other := range map[int64]string{1: e.gh.prs[2].url, 2: e.gh.prs[1].url} {
		got := e.siblingComments(n)
		if len(got) != 1 || !strings.Contains(got[0], other) || strings.Contains(got[0], e.gh.prs[n].url) {
			t.Errorf("pull request %d siblings comments = %q, want one linking %s", n, got, other)
		}
	}
	if c := meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Errorf("SiblingsLinked = %+v, want True", c)
	}
	e.settleActions(name)
	if len(e.siblingComments(1)) != 1 || len(e.siblingComments(2)) != 1 {
		t.Error("a siblings comment was posted again")
	}
}

// inReviewMulti drives a new multi-repository intent to InReview.
func (e *env) inReviewMulti() string {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	return name
}

// TestSiblingCommentExactlyOnceAcrossALostWrite: the status write that
// records the cross-links fails once; the next pass finds both comments by
// the bot's marker and posts neither again.
func TestSiblingCommentExactlyOnceAcrossALostWrite(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewMulti()
	e.failStatusIf = func(in *v1alpha1.Intent) bool {
		return meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionSiblingsLinked)
	}
	for range 5 {
		_ = e.reconcileIntent(name)
		e.clock.Advance(time.Minute)
	}
	if e.failed != 1 {
		t.Fatalf("injected failures = %d, want the one", e.failed)
	}
	if len(e.siblingComments(1)) != 1 || len(e.siblingComments(2)) != 1 {
		t.Errorf("siblings comments = %d and %d, want one each", len(e.siblingComments(1)),
			len(e.siblingComments(2)))
	}
	if !meta.IsStatusConditionTrue(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked) {
		t.Error("SiblingsLinked is not True")
	}
}

// TestSiblingRefusalNeverHoldsReview: GitHub refusing the cross-link on one
// pull request (a locked conversation) holds nothing back: the intent is in
// review, the other pull request has its comment, and SiblingsLinked says
// which one was refused, once. Once the conversation is unlocked the comment
// is posted and the condition turns True.
func TestSiblingRefusalNeverHoldsReview(t *testing.T) {
	e := newMultiEnv(t)
	e.gh.lockedPRs[2] = true
	name := e.inReviewMulti()
	e.settleActions(name)
	in := e.get(name)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionSiblingsLinked)
	if in.Status.Phase != v1alpha1.IntentInReview || c == nil || c.Status != metav1.ConditionFalse ||
		c.Reason != ReasonSiblingsRefused || !strings.Contains(c.Message, webSlug+"#2") {
		t.Fatalf("phase %s, SiblingsLinked %+v; want InReview with the refusal named", in.Status.Phase, c)
	}
	if len(e.siblingComments(1)) != 1 || len(e.siblingComments(2)) != 0 {
		t.Errorf("siblings comments = %d and %d, want PR 1's alone", len(e.siblingComments(1)),
			len(e.siblingComments(2)))
	}
	writes := e.writes
	e.settleActions(name)
	if e.writes != writes {
		t.Errorf("%d status writes repeating the same refusal", e.writes-writes)
	}

	e.gh.mu.Lock()
	e.gh.lockedPRs[2] = false
	e.gh.mu.Unlock()
	e.settleActions(name)
	if !meta.IsStatusConditionTrue(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked) ||
		len(e.siblingComments(1)) != 1 || len(e.siblingComments(2)) != 1 {
		t.Errorf("after the unlock: SiblingsLinked %+v, comments %d and %d",
			meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionSiblingsLinked),
			len(e.siblingComments(1)), len(e.siblingComments(2)))
	}
}

// TestRepositoryAttemptsSpentFailsTheIntent: one repository whose build
// attempts are spent fails the whole intent at once; the sibling build still
// running is aborted and its Job deleted, and no pull request is opened
// anywhere.
func TestRepositoryAttemptsSpentFailsTheIntent(t *testing.T) {
	e := newMultiEnv(t)
	e.runs.MaxConcurrent = 2
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "build" && spec.Repo == webSlug {
			return failingBuild(spec)
		}
		return multiOutput(spec)
	}
	e.jobs.status[buildJob("app")] = jobs.Status{}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	e.runRuns()
	web := e.buildsIn(name, webRepoURL)
	if len(web) != 2 || web[1].Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("web builds = %+v, want both attempts failed", web)
	}
	app := e.buildsIn(name, appRepoURL)
	if len(app) != 1 || app[0].Status.Phase != v1alpha1.RunFailed || app[0].Status.Outcome != OutcomeAborted {
		t.Fatalf("app builds = %+v, want the one aborted", app)
	}
	if !slices.Contains(e.jobs.deleted, buildJob("app")) {
		t.Errorf("deleted Jobs = %v, want the app build's", e.jobs.deleted)
	}
	if len(e.gh.prs) != 0 || len(e.gh.commits) != 0 {
		t.Errorf("pull requests %d, commits %d; want none", len(e.gh.prs), len(e.gh.commits))
	}
}

// TestStaleRoundBranchBlocksBeforeAnyBuild: a revived intent's new round
// meets the branch an earlier round pushed in one repository (its sibling
// failed, so it never got a pull request). The branch is patchy's own but not
// this round's: the intent blocks on it, naming the repository and the run
// that pushed it, before any build of the new round is created, and builds
// once the branch is deleted.
func TestStaleRoundBranchBlocksBeforeAnyBuild(t *testing.T) {
	e := newMultiEnv(t)
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "build" && spec.Repo == webSlug {
			return failingBuild(spec)
		}
		return multiOutput(spec)
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	app := e.buildsIn(name, appRepoURL)
	if len(app) != 1 || app[0].Status.PushedCommit == "" {
		t.Fatalf("app builds = %+v, want the one pushed before the intent failed", app)
	}
	stale := app[0].Status.PushedCommit

	e.jobs.output = multiOutput
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:target", approver)
	e.reconcileProject()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionBranchConflict)
	if c == nil || c.Reason != ReasonStaleRoundBranch || !strings.Contains(c.Message, "acme/app") ||
		!strings.Contains(c.Message, stale) || !strings.Contains(c.Message, app[0].Name) ||
		!strings.Contains(c.Message, "safe") {
		t.Fatalf("BranchConflict = %+v, want the stale round's branch named, safe to delete", c)
	}
	for _, b := range e.runsOf(name, v1alpha1.IntentStageBuild) {
		if b.Spec.Round == in.Status.Approval.PlanRevision {
			t.Fatalf("build %s of the new round created beside the stale branch", b.Name)
		}
	}
	if got := e.gh.branches[branchName(name)]; got != stale {
		t.Errorf("the stale branch was moved to %s", got)
	}

	delete(e.gh.branches, branchName(name))
	delete(e.gh.branches, webSlug+":"+branchName(name))
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if len(in.Status.PullRequests) != 2 {
		t.Errorf("pull requests = %+v, want both", in.Status.PullRequests)
	}
}

// TestImageBlockNamesItsRepository: a repository whose build has no accepted
// image blocks the intent on ImageRequired, naming it, while the sibling
// already running finishes and pushes. Once the Project changes, the resume
// launches only the blocked repository's next attempt.
func TestImageBlockNamesItsRepository(t *testing.T) {
	e := newMultiEnv(t)
	e.runs.MaxConcurrent = 2
	webImage := false
	imageOf := func(repo *v1alpha1.Repository) string {
		if sameRepo(repo.Spec.URL, webRepoURL) && !webImage {
			return ""
		}
		return repoImage
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.driveEach(name, v1alpha1.IntentBlocked, imageOf)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionImageRequired)
	if c == nil || c.Reason != ReasonNoRepositoryImage || !strings.Contains(c.Message, "in "+webSlug) {
		t.Fatalf("ImageRequired = %+v, want the web repository named", c)
	}
	for range 4 {
		e.mustIntent(name)
		e.readyEach(imageOf)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if app := e.buildsIn(name, appRepoURL); len(app) != 1 || app[0].Status.Phase != v1alpha1.RunComplete ||
		app[0].Status.PushedCommit == "" {
		t.Fatalf("app builds = %+v, want the one pushed while the intent is Blocked", app)
	}

	webImage = true
	p := e.getProject()
	p.Spec.Labels.Approve = "patchy:go"
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.driveEach(name, v1alpha1.IntentInReview, imageOf)
	if app := e.buildsIn(name, appRepoURL); len(app) != 1 {
		t.Errorf("app builds = %d after the resume, want the one", len(app))
	}
	if web := e.buildsIn(name, webRepoURL); len(web) != 2 || web[1].Status.Phase != v1alpha1.RunComplete {
		t.Errorf("web builds = %+v, want a second attempt, complete", web)
	}
}

// TestActiveRunIsSticky: with both builds in flight, activeRun names one and
// stays on it, pass after pass, with no status write; once that run settles
// it moves, in one write, to the build still running.
func TestActiveRunIsSticky(t *testing.T) {
	e := newMultiEnv(t)
	e.runs.MaxConcurrent = 2
	e.jobs.status[buildJob("app")] = jobs.Status{}
	e.jobs.status[buildJob("web")] = jobs.Status{}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	for range 10 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	first := e.get(name).Status.ActiveRun
	if first == nil || len(e.jobs.launched()) != 3 {
		t.Fatalf("activeRun %+v, %d Jobs; want both builds launched beside the plan", first, len(e.jobs.launched()))
	}
	writes := e.writes
	for range 5 {
		e.mustIntent(name)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if ar := e.get(name).Status.ActiveRun; *ar != *first || e.writes != writes {
		t.Fatalf("activeRun moved to %+v, or %d writes, while both builds ran", ar, e.writes-writes)
	}

	key, other := "app", "web"
	if strings.Contains(first.Name, "-web-") {
		key, other = "web", "app"
	}
	e.jobs.mu.Lock()
	delete(e.jobs.status, buildJob(key))
	e.jobs.mu.Unlock()
	e.runRuns()
	e.mustIntent(name)
	e.mustIntent(name)
	ar := e.get(name).Status.ActiveRun
	if ar == nil || !strings.Contains(ar.Name, "-"+other+"-") || e.writes-writes > 2 {
		t.Errorf("activeRun = %+v after %d writes, want the build still running", ar, e.writes-writes)
	}
}

// TestMultiRepoBuildInvariantsSeeded is a seeded property over random
// per-repository build histories, three repositories, a run pool of one to
// three slots, random interleavings of the reconcilers, failed status writes
// and restarts: no repository ever has two builds in flight; the intent is
// never in review unless every approved repository has its pull request; no
// pull request opens while a build is incomplete; and every history ends in
// review or failed.
func TestMultiRepoBuildInvariantsSeeded(t *testing.T) {
	const threeRepoPlan = `---
summary: "Version everywhere"
repositories:
  - "https://github.com/acme/app"
  - "https://github.com/acme/Acme.Web_App"
  - "https://github.com/acme/lib"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

Three repositories.
`
	for seed := range int64(24) {
		rng := rand.New(rand.NewSource(0x5ee3 + seed))
		p := testMultiProject()
		p.Spec.Repositories = append(p.Spec.Repositories,
			v1alpha1.ProjectRepository{Name: "lib", URL: "https://github.com/acme/lib"})
		e := newEnv(t, p)
		e.multiRepo(true)
		e.runs.MaxConcurrent = 1 + rng.Intn(3)
		outcomes := map[string]bool{} // run name → fails
		e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
			if spec.Phase == "plan" {
				out := defaultOutput(spec)
				out.Events[0].Plan.ReportMarkdown = threeRepoPlan
				return out
			}
			if outcomes[spec.Finding] {
				return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
					Remediation: &envelope.Remediation{Stage: envelope.Stage{Outcome: envelope.OutcomeRuntimeError,
						Detail: "the CLI crashed"}}}}}
			}
			return defaultOutput(spec)
		}
		name := e.awaiting()
		e.gh.label(1, "patchy:approved", approver)
		ctx := context.Background()
		ended := false
		for step := 0; step < 1500 && !ended; step++ {
			for _, b := range e.runsOf(name, v1alpha1.IntentStageBuild) {
				if _, ok := outcomes[b.Name]; !ok {
					outcomes[b.Name] = rng.Intn(4) == 0
				}
			}
			e.failEvery = 0
			if rng.Intn(5) == 0 {
				e.failEvery = 2 + rng.Intn(3)
			}
			switch n := rng.Intn(12); {
			case n < 3:
				_ = e.reconcileIntent(name)
			case n < 4:
				e.readyRepositories(repoImage)
			case n < 6:
				_, _ = e.runs.Reconcile(ctx, req(runSchedulerRequest))
			case n < 10:
				if runs := e.intentRuns(name); len(runs) > 0 {
					_, _ = e.runs.Reconcile(ctx, req(runs[rng.Intn(len(runs))].Name))
				}
			case n < 11:
				e.clock.Advance(time.Duration(1+rng.Intn(60)) * time.Second)
			default:
				if rng.Intn(4) == 0 {
					e.restart()
					e.multiRepo(true)
					e.runs.MaxConcurrent = 1 + rng.Intn(3)
				}
			}
			e.failEvery = 0
			in := e.get(name)
			checkBuildInvariants(t, seed, step, e, in)
			ended = in.Status.Phase == v1alpha1.IntentInReview || in.Status.Phase == v1alpha1.IntentFailed
		}
		if !ended {
			t.Errorf("seed %d: the intent ended neither in review nor failed: %s", seed, e.get(name).Status.Phase)
		}
	}
}

// checkBuildInvariants checks the build round's invariants on the intent as
// one step of the seeded property left it.
func checkBuildInvariants(t *testing.T, seed int64, step int, e *env, in *v1alpha1.Intent) {
	t.Helper()
	inFlight := map[string]int{}
	complete := map[string]bool{}
	for _, b := range e.runsOf(in.Name, v1alpha1.IntentStageBuild) {
		switch b.Status.Phase {
		case v1alpha1.RunComplete:
			complete[b.Spec.Repository.URL] = true
		case v1alpha1.RunFailed:
		default:
			inFlight[b.Spec.Repository.URL]++
		}
	}
	for repo, n := range inFlight {
		if n > 1 {
			t.Fatalf("seed %d step %d: %d builds in flight in %s", seed, step, n, repo)
		}
	}
	if len(e.gh.prs) > 0 && len(complete) < 3 {
		t.Fatalf("seed %d step %d: a pull request opened with %d of 3 builds complete", seed, step, len(complete))
	}
	if in.Status.Phase == v1alpha1.IntentInReview {
		if len(in.Status.PullRequests) != 3 {
			t.Fatalf("seed %d step %d: InReview with %d pull requests", seed, step, len(in.Status.PullRequests))
		}
		for _, pr := range in.Status.PullRequests {
			if !complete[pr.Repository] {
				t.Fatalf("seed %d step %d: a pull request in %s with no complete build", seed, step, pr.Repository)
			}
		}
	}
}
