// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// TestPRCommandsStartRoundsOnlyWhenEligible: a PR comment starts a revise round only
// when it is an approver's unedited /patchy revise, or a retry with a failed
// round to retry: an outsider's command, an edited one, another verb, a
// retry with nothing failed, or a command older than the pull request's
// review cutoff starts nothing.
func TestPRCommandsStartRoundsOnlyWhenEligible(t *testing.T) {
	cases := []struct {
		name   string
		author string
		body   string
		edited bool
		// before places the comment before the pull request was opened.
		before bool
		// wantRound: the control, an approver's command that does start
		// one.
		wantRound bool
	}{
		{name: "approver's revise", author: approver, body: "/patchy revise do it", wantRound: true},
		{name: "outsider's revise", author: "outsider", body: "/patchy revise do it"},
		{name: "edited revise", author: approver, body: "/patchy revise do it", edited: true},
		{name: "retry with nothing failed", author: approver, body: "/patchy retry"},
		{name: "another verb", author: approver, body: "/patchy approve"},
		{name: "not a command", author: approver, body: "please revise this"},
		{name: "older than the cutoff", author: approver, body: "/patchy revise do it", before: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)
			pr := in.Status.PullRequests[0]
			at := e.clock.Now()
			if tc.before {
				at = at.Add(-24 * time.Hour)
			}
			const id = 950
			e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: id, NodeID: "comment-950",
				UserLogin: tc.author, UserID: actorOf(tc.author).ID, UserType: "User",
				Body: tc.body, CreatedAt: at, UpdatedAt: at}}
			if tc.edited {
				e.gh.edited[id] = true
			}
			for range 4 {
				e.clock.Advance(2 * time.Minute)
				e.mustIntent(name)
				e.runRuns()
			}
			got := e.get(name)
			runs := e.runsOf(name, v1alpha1.IntentStageRevise)
			if tc.wantRound {
				if len(runs) != 1 || runs[0].Spec.Inputs.CommandID != id || got.Status.Rounds != 1 {
					t.Errorf("revise runs = %d, rounds %d; want the command's one round", len(runs), got.Status.Rounds)
				}
				return
			}
			if got.Status.Phase != v1alpha1.IntentInReview {
				t.Errorf("phase = %s, want still %s", got.Status.Phase, v1alpha1.IntentInReview)
			}
			if len(runs) != 0 {
				t.Errorf("revise runs = %d, want none", len(runs))
			}
			if got.Status.Revisions != 0 || got.Status.Rounds != 0 {
				t.Errorf("revisions %d, rounds %d; want none spent", got.Status.Revisions, got.Status.Rounds)
			}
		})
	}
}

// TestReviseHeadMovedBeforeLaunch: a push to the intent branch after the
// revise round pinned it and before its agent launched fails that attempt as
// head_moved with nothing launched, and the retry runs from a fresh pin of
// the new head.
func TestReviseHeadMovedBeforeLaunch(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 961, NodeID: "review-961", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	for range 10 {
		if len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 {
			break
		}
		e.mustIntent(name)
		e.clock.Advance(time.Minute)
	}
	if len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 {
		t.Fatal("no revise run was created")
	}
	launchesBefore := e.jobCount()
	// The round's input and Repository are written, and source-controller
	// pins the Repository at the branch's head, until the run could launch.
	for i := 0; ; i++ {
		run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
		if e.runs.launchable(t.Context(), &run) {
			break
		}
		if i == 10 {
			t.Fatal("the revise run never became launchable")
		}
		e.mustIntent(name)
		e.readyRepositories(repoImage)
	}
	const moved = "3333333333333333333333333333333333333333"
	e.gh.mu.Lock()
	e.gh.branches[branchName(name)] = moved
	e.gh.mu.Unlock()
	e.runRuns()
	first := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if first.Status.Phase != v1alpha1.RunFailed || first.Status.Outcome != OutcomeHeadMoved {
		t.Fatalf("first attempt = %s/%s (%s), want Failed/%s", first.Status.Phase, first.Status.Outcome,
			first.Status.Detail, OutcomeHeadMoved)
	}
	if e.jobCount() != launchesBefore {
		t.Errorf("an agent was launched on a moved head")
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 2 || runs[1].Status.Phase != v1alpha1.RunComplete || runs[1].Status.BaseSHA != moved {
		t.Fatalf("attempts = %d, retry %s from %s; want a completed retry from %s", len(runs),
			runs[len(runs)-1].Status.Phase, runs[len(runs)-1].Status.BaseSHA, moved)
	}
	if in.Status.Revisions != 1 {
		t.Errorf("revisions = %d, want 1", in.Status.Revisions)
	}
}

// jobCount is how many Jobs the fake has created.
func (e *env) jobCount() int {
	e.jobs.mu.Lock()
	defer e.jobs.mu.Unlock()
	return len(e.jobs.specs)
}
