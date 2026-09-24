// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// TestHappyPath drives one intent from its trigger to its merge: planned
// read-only on the default image from the input snapshot, the plan posted
// verbatim and approved by an approver's label, built on the repository's
// image from the approved plan alone, pushed create-only and opened as a
// pull request, then merged, summarised and closed as completed.
func TestHappyPath(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)

	in := e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	issue := checkSnapshot(t, e, in)
	checkPlanLaunch(t, e, issue)
	checkPlanPosted(t, e, in)

	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	checkBuildLaunch(t, e, in)
	checkPush(t, e, in)
	checkBuildRecords(t, e, in)

	e.gh.closePR(true)
	in = e.drive(name, v1alpha1.IntentMerged, repoImage)
	checkMerged(t, e, in)
}

// checkSnapshot: the input snapshot carries the request and the Project's
// repository, and its digest is the input digest. It returns the snapshot.
func checkSnapshot(t *testing.T, e *env, in *v1alpha1.Intent) string {
	t.Helper()
	if in.Status.Input == nil || in.Status.Input.Revision != 1 {
		t.Fatalf("input = %+v, want revision 1", in.Status.Input)
	}
	var snap corev1.ConfigMap
	key := types.NamespacedName{Namespace: testNS, Name: in.Status.Input.ConfigMap}
	if err := e.c.Get(context.Background(), key, &snap); err != nil {
		t.Fatal(err)
	}
	issue := snap.Data[keyIssue]
	if !strings.Contains(issue, appRepoURL) || !strings.Contains(issue, "GET /version") {
		t.Errorf("snapshot does not carry the request and the project's repository:\n%s", issue)
	}
	if got := digest([]byte(issue)); got != in.Status.Input.Digest {
		t.Errorf("input digest = %s, want the snapshot's %s", in.Status.Input.Digest, got)
	}
	return issue
}

// checkPlanLaunch: the plan ran read-only on the default image, handed the
// snapshot and nothing else, on the plan model and grant.
func checkPlanLaunch(t *testing.T, e *env, issue string) {
	t.Helper()
	plan := e.onlyLaunch(t, "plan")
	if plan.RunnerImage != "" || plan.IssueMarkdown != issue || plan.InvestigationMarkdown != "" {
		t.Errorf("plan launch: image %q, issue %q, investigation %q", plan.RunnerImage, plan.IssueMarkdown,
			plan.InvestigationMarkdown)
	}
	if plan.Kind != KindIntent || plan.Harness != "claude" || plan.Model != "anthropic/claude-sonnet-5" ||
		plan.MaxTurns != 40 || plan.TokenBudget != 200000 {
		t.Errorf("plan launch = %+v", plan)
	}
}

// checkPlanPosted: the plan is stored, posted verbatim once, and recorded as
// GitHub stored it; the status comment exists once.
func checkPlanPosted(t *testing.T, e *env, in *v1alpha1.Intent) {
	t.Helper()
	pl := in.Status.Plan
	if pl == nil || pl.Revision != 1 || pl.Digest != digest([]byte(validPlan)) || pl.CommentID == 0 ||
		pl.PostedAt == nil {
		t.Fatalf("plan = %+v", pl)
	}
	posted := e.gh.withMarker("patchy:plan")
	if len(posted) != 1 || !strings.Contains(posted[0].Body, validPlan) ||
		digest([]byte(posted[0].Body)) != pl.CommentDigest {
		t.Fatalf("plan comments = %d, want one holding the plan verbatim", len(posted))
	}
	if st := e.gh.withMarker("patchy:intent"); len(st) != 1 {
		t.Errorf("status comments = %d, want exactly one", len(st))
	}
}

// checkBuildLaunch: the approval binds the plan and input digests, and the
// build ran on the repository's image from the approved plan alone, with an
// empty request.
func checkBuildLaunch(t *testing.T, e *env, in *v1alpha1.Intent) {
	t.Helper()
	ap, pl := in.Status.Approval, in.Status.Plan
	if ap == nil || ap.By != approver || ap.Source != v1alpha1.IntentActionLabel || ap.PlanRevision != 1 ||
		ap.PlanDigest != pl.Digest || ap.InputDigest != in.Status.Input.Digest {
		t.Fatalf("approval = %+v", ap)
	}
	build := e.onlyLaunch(t, "build")
	if build.RunnerImage != repoImage || build.IssueMarkdown != "" || build.InvestigationMarkdown != validPlan {
		t.Errorf("build handoff: image %q, issue %q, investigation %q", build.RunnerImage, build.IssueMarkdown,
			build.InvestigationMarkdown)
	}
	if build.Model != "anthropic/claude-opus-5" || build.MaxTurns != 150 || build.TokenBudget != 800000 {
		t.Errorf("build launch = %+v", build)
	}
}

// checkPush: one commit with patchy's own message on the pinned base, the
// intent branch at it, and the pull request, which says "Part of" and closes
// nothing.
func checkPush(t *testing.T, e *env, in *v1alpha1.Intent) {
	t.Helper()
	if len(e.gh.commits) != 1 || strings.Contains(e.gh.commits[0].Message, "agent's message") ||
		!strings.Contains(e.gh.commits[0].Message, "Patchy-Intent: patchy/"+in.Name) ||
		e.gh.commits[0].BaseSHA != baseSHA {
		t.Errorf("commits = %+v", e.gh.commits)
	}
	if sha := e.gh.branches["patchy-intent/"+in.Name]; sha == "" {
		t.Errorf("branch patchy-intent/%s not created", in.Name)
	}
	if len(in.Status.PullRequests) != 1 || in.Status.PullRequests[0].NodeID != "PR_1" ||
		in.Status.Branch != "patchy-intent/"+in.Name {
		t.Fatalf("pull requests = %+v", in.Status.PullRequests)
	}
	if body := e.gh.prs[1].body; !strings.HasPrefix(body, "Part of acme/intents#1") ||
		strings.Contains(strings.ToLower(body), "fixes") {
		t.Errorf("PR body = %q", body)
	}
}

// checkBuildRecords: the build run records the image that ran and the
// commit it pushed; only the build's Repository is kept, without the
// Finding label.
func checkBuildRecords(t *testing.T, e *env, in *v1alpha1.Intent) {
	t.Helper()
	builds := e.runsOf(in.Name, v1alpha1.IntentStageBuild)
	if len(builds) != 1 || builds[0].Status.RunnerImage == nil ||
		builds[0].Status.RunnerImage.Source != v1alpha1.RunnerImageSourceRepository ||
		builds[0].Status.PushedCommit == "" {
		t.Errorf("build runs = %+v", builds)
	}
	var repos v1alpha1.RepositoryList
	if err := e.c.List(context.Background(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos.Items) != 1 || !strings.Contains(repos.Items[0].Name, "-bld-") ||
		repos.Items[0].Labels[v1alpha1.LabelFinding] != "" {
		t.Errorf("repositories = %+v, want the build's alone, without the Finding label", repos.Items)
	}
}

// checkMerged: the intent completed, the issue is closed as completed after
// one summary, usage sums both runs, and an ended intent writes nothing more.
func checkMerged(t *testing.T, e *env, in *v1alpha1.Intent) {
	t.Helper()
	if in.Status.CompletedAt == nil || in.Status.PullRequests[0].State != prMerged {
		t.Errorf("merged intent = %+v", in.Status)
	}
	if e.gh.state() != "closed" || e.gh.closes[1][0] != ghclient.CloseCompleted {
		t.Errorf("issue state %s closes %v, want closed as completed", e.gh.state(), e.gh.closes[1])
	}
	if s := e.gh.withMarker(templates.SummaryKey); len(s) != 1 {
		t.Errorf("summary comments = %d, want one", len(s))
	}
	if in.Status.Usage.CostMicroUSD != 500000 {
		t.Errorf("usage = %+v, want both runs' cost", in.Status.Usage)
	}
	e.mustIntent(in.Name) // the status comment's last update
	before := len(e.gh.ownComments())
	e.clock.Advance(time.Hour)
	e.mustIntent(in.Name)
	if after := len(e.gh.ownComments()); after != before {
		t.Errorf("an ended intent posted %d more comments", after-before)
	}
}
