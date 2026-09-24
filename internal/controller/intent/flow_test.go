// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// onlyLaunch is the one Job launched for phase.
func (e *env) onlyLaunch(t *testing.T, phase string) jobs.Spec {
	t.Helper()
	var found []jobs.Spec
	for _, s := range e.jobs.launched() {
		if s.Phase == phase {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s Jobs launched, want 1", len(found), phase)
	}
	return found[0]
}

// TestStageEnv: a plan Job carries the investigate stage's wall clock and
// ceilings plus the build grant it is sized for, with the automated budget no
// higher than the manual; a build Job the remediate stage's.
func TestStageEnv(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	var planEnv map[string]string
	for job, s := range e.jobs.specs {
		if s.Phase == "plan" {
			planEnv = e.jobs.envs[job]
		}
	}
	want := map[string]string{
		"PATCHY_INVESTIGATE_TIMEOUT":           "20m0s",
		"PATCHY_INVESTIGATE_MAX_TURNS":         "40",
		"PATCHY_INVESTIGATE_TOKEN_BUDGET":      "200000",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "150",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "800000",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "150",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "800000",
	}
	for k, v := range want {
		if planEnv[k] != v {
			t.Errorf("plan env %s = %q, want %q", k, planEnv[k], v)
		}
	}
	build := stageEnv(v1alpha1.IntentStageBuild, testSettings(), v1alpha1.IntentRunGrant{})
	if build["PATCHY_REMEDIATE_TIMEOUT"] != "1h0m0s" || build["PATCHY_REMEDIATE_MANUAL_MAX_TURNS"] != "150" ||
		build["PATCHY_REMEDIATE_AUTO_MAX_TURNS"] != "150" {
		t.Errorf("build env = %v", build)
	}
}

// TestProjectLimitsLowerTheGrant: a Project's per-stage limits lower a run's
// grant, and a limit above the controller's ceiling is clamped to it.
func TestProjectLimitsLowerTheGrant(t *testing.T) {
	p := testProject()
	p.Spec.Limits.Plan = v1alpha1.StageLimits{MaxTurns: 10, TokenBudget: 999999999}
	g := testSettings().grant(p, v1alpha1.IntentStagePlan)
	if g.MaxTurns != 10 || g.TokenBudget != 200000 || g.TimeoutMilliseconds != (20*time.Minute).Milliseconds() {
		t.Errorf("grant = %+v", g)
	}
}

// TestStatusCommentExactlyOnce: a restart between posting the status comment
// and recording it adopts the posted one instead of posting a second.
func TestStatusCommentExactlyOnce(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	// The status comment is posted, and the write recording it fails.
	e.failStatusIf = func(in *v1alpha1.Intent) bool { return in.Status.Tracking != nil }
	for range 6 {
		_ = e.reconcileIntent(name)
		if e.failed > 0 {
			break
		}
	}
	if e.failed == 0 {
		t.Fatal("the status comment was never recorded")
	}
	if st := e.gh.withMarker("patchy:intent"); len(st) != 1 || e.get(name).Status.Tracking != nil {
		t.Fatalf("after the failed record: status comments = %d, tracking %+v", len(st), e.get(name).Status.Tracking)
	}
	// A restarted controller adopts the comment.
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: testSettings(),
		Images: e.intent.Images, Nudger: e.nudger, Now: e.clock.Now}
	e.mustIntent(name)
	e.mustIntent(name)
	st := e.gh.withMarker("patchy:intent")
	if len(st) != 1 {
		t.Fatalf("status comments = %d, want exactly one", len(st))
	}
	if in := e.get(name); in.Status.Tracking == nil || in.Status.Tracking.StatusCommentID != st[0].ID {
		t.Errorf("tracking = %+v", in.Status.Tracking)
	}
}

// TestPlanCommentExactlyOnce: a failure between posting the plan and
// recording it, and a restart of the controller, post it once.
func TestPlanCommentExactlyOnce(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	// The plan is posted, and the write recording it fails.
	e.failStatusIf = func(in *v1alpha1.Intent) bool {
		return in.Status.Plan != nil && in.Status.Plan.CommentID != 0
	}
	for range 30 {
		if e.failed > 0 {
			break
		}
		_ = e.reconcileIntent(name)
		e.readyRepositories("")
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if e.failed == 0 {
		t.Fatal("the plan comment was never recorded")
	}
	if n := len(e.gh.withMarker("patchy:plan")); n != 1 || e.get(name).Status.Plan.CommentID != 0 {
		t.Fatalf("after the failed record: plan comments = %d, plan %+v", n, e.get(name).Status.Plan)
	}
	// A restarted controller (nothing in memory) finishes the step.
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: testSettings(),
		Images: e.intent.Images, Nudger: e.nudger, Now: e.clock.Now}
	e.drive(name, v1alpha1.IntentAwaitingApproval, "")
	if n := len(e.gh.withMarker("patchy:plan")); n != 1 {
		t.Errorf("plan comments = %d, want exactly one", n)
	}
}

// TestApprovalDuringPlanRecordRetry: an approver's approve label added after
// the plan was posted, while recording the plan failed, is an approval: the
// retry that finds the posted plan leaves the label alone.
func TestApprovalDuringPlanRecordRetry(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.failStatusIf = func(in *v1alpha1.Intent) bool {
		return in.Status.Plan != nil && in.Status.Plan.CommentID != 0
	}
	for range 30 {
		if e.failed > 0 {
			break
		}
		_ = e.reconcileIntent(name)
		e.readyRepositories("")
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if e.failed == 0 {
		t.Fatal("the plan comment was never recorded")
	}
	e.clock.Advance(time.Second)
	id := e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentBuilding, repoImage)
	if ap := in.Status.Approval; ap == nil || ap.EventID != id || ap.Source != v1alpha1.IntentActionLabel {
		t.Fatalf("approval = %+v, want the label %d", ap, id)
	}
	if n := len(e.gh.withMarker("patchy:plan")); n != 1 {
		t.Errorf("plan comments = %d, want exactly one", n)
	}
}

// TestEstimateNotice: a plan whose estimate exceeds the build grant is
// flagged before it is posted, once.
func TestEstimateNotice(t *testing.T) {
	p := testProject()
	p.Spec.Limits.Build = v1alpha1.StageLimits{MaxTurns: 20}
	e := newEnv(t, p)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, "")
	notes := e.gh.withMarker("estimate-r1")
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "40 turns") {
		t.Fatalf("estimate notices = %+v", notes)
	}
	plans := e.gh.withMarker("patchy:plan")
	if notes[0].ID > plans[0].ID {
		t.Error("the estimate notice follows the plan; the approver must read it first")
	}
}

// TestApprovedByCommand: /patchy approve works as the label does, and gets
// the eyes reaction and exactly one done reply.
func TestApprovedByCommand(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/patchy approve")
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if ap := in.Status.Approval; ap == nil || ap.Source != v1alpha1.IntentActionCommand || ap.EventID != id {
		t.Fatalf("approval = %+v", ap)
	}
	if e.gh.reactions[id] == 0 {
		t.Error("the command got no reaction")
	}
	if replies := e.gh.withMarker("comment-" + itoa(id)); len(replies) != 1 ||
		!strings.Contains(replies[0].Body, "is done") {
		t.Errorf("replies = %+v, want one done reply", replies)
	}
}

// TestUsageAndConditionReadback: the approval writes ApprovalRejected False.
func TestApprovalClearsRejection(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentBuilding, repoImage)
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionApprovalRejected) {
		t.Error("ApprovalRejected is True after an accepted approval")
	}
}
