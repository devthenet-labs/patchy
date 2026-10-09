// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// lostCreates wraps e's reconcilers' clients so the first create of every
// object lands but answers AlreadyExists, as a create whose response was
// lost before a retry, or one made by a pass that crashed before recording
// it, would look. It returns the kinds so answered.
func lostCreates(e *env) map[string]int {
	lost := map[string]int{}
	seen := map[string]bool{}
	wc, ok := e.c.(client.WithWatch)
	if !ok {
		e.t.Fatal("the env's client does not watch")
	}
	c := interceptor.NewClient(wc, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			kind := fmt.Sprintf("%T", obj)
			key := kind + "/" + obj.GetNamespace() + "/" + obj.GetName()
			if obj.GetName() == "" || seen[key] {
				return c.Create(ctx, obj, opts...)
			}
			seen[key] = true
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			lost[kind]++
			return kerrors.NewAlreadyExists(schema.GroupResource{Resource: kind}, obj.GetName())
		},
	})
	e.intent.Client = c
	e.runs.Client = c
	e.project.Client = c
	return lost
}

// TestCreatesAreAdopted: every object a pass creates is adopted when the API
// server already holds it as that pass would have made it: the intent runs
// from its trigger to its merge just as when every create answers.
func TestCreatesAreAdopted(t *testing.T) {
	e := newEnv(t, testProject())
	lost := lostCreates(e)
	e.reconcileProject()
	name := e.newIntent(approver)

	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if len(in.Status.PullRequests) == 0 {
		t.Errorf("no pull request recorded: %+v", in.Status)
	}
	e.gh.closePR(true)
	e.drive(name, v1alpha1.IntentMerged, repoImage)

	for _, kind := range []string{"*v1alpha1.IntentRun", "*v1alpha1.Repository", "*v1.ConfigMap"} {
		if lost[kind] == 0 {
			t.Errorf("no %s create was answered AlreadyExists; lost = %v", kind, lost)
		}
	}
	// One plan run and one build run, each created once.
	runs := e.intentRuns(name)
	if len(runs) != 2 {
		names := make([]string, 0, len(runs))
		for _, r := range runs {
			names = append(names, r.Name)
		}
		t.Errorf("runs = %v, want one plan and one build", names)
	}
}

// TestReviseCreatesAreAdopted: a revise round's run and input, and its
// retry after the head moved, are adopted when the API server already holds
// them: one round, two attempts, one revision, as when every create answers.
func TestReviseCreatesAreAdopted(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	lost := lostCreates(e)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 901, NodeID: "review-901", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 2 || runs[0].Status.Outcome != OutcomeHeadMoved || runs[1].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("revise attempts = %+v, want head_moved then complete", runs)
	}
	if runs[0].Spec.Round != 1 || runs[1].Spec.Round != 1 || in.Status.Revisions != 1 {
		t.Errorf("rounds %d, %d and revisions %d; want one round of two attempts and one revision",
			runs[0].Spec.Round, runs[1].Spec.Round, in.Status.Revisions)
	}
	if lost["*v1alpha1.IntentRun"] < 2 {
		t.Errorf("revise run creates answered AlreadyExists = %d, want both attempts", lost["*v1alpha1.IntentRun"])
	}
}

// TestCommandRoundCreatesAreAdopted: a round an approver's PR command
// started, adopted from a create that answered AlreadyExists, still
// acknowledges the command once and runs one round.
func TestCommandRoundCreatesAreAdopted(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	lostCreates(e)
	pr := in.Status.PullRequests[0]
	e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: 940, NodeID: "comment-940",
		UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
		Body:      "/patchy revise Please check the response header.",
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.clock.Advance(time.Second)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerCommand ||
		runs[0].Spec.Inputs.CommandID != 940 {
		t.Fatalf("PR command runs = %+v, want one command-triggered round", runs)
	}
	if e.gh.reactions[940] != 1 {
		t.Errorf("eyes reactions to command = %d, want one", e.gh.reactions[940])
	}
}

// TestCheckFixCreatesAreAdopted: a check-fix round's run and input, adopted
// from creates that answered AlreadyExists, still carry the failed check and
// count one fix.
func TestCheckFixCreatesAreAdopted(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	lostCreates(e)
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		DetailsURL: "https://github.com/acme/app/actions/runs/81/jobs/91",
		Output:     ghclient.CheckOutput{Title: "Go failed", Summary: "failed"}}}
	e.gh.workflowJobs[81] = []ghclient.WorkflowJob{{ID: 91, CheckRunID: 71, HeadSHA: head,
		Name: "go test", Conclusion: "failure"}}
	e.gh.jobLogs[91] = "go test ./...\nFAIL version test"
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		len(runs[0].Spec.Inputs.CheckRunIDs) != 1 || runs[0].Spec.Inputs.CheckRunIDs[0] != 71 {
		t.Fatalf("check-fix run = %+v, want failed check run 71", runs)
	}
	if in.Status.CheckFixes != 1 || in.Status.Revisions != 0 {
		t.Errorf("round counters = fixes %d, revisions %d; want 1, 0", in.Status.CheckFixes, in.Status.Revisions)
	}
}

// TestDiscoveredIntentCreateIsAdopted: discovery adopts an Intent the API
// server already holds for the issue, as it would have created it: the same
// one Intent as when the create answers, which then plans as usual.
func TestDiscoveredIntentCreateIsAdopted(t *testing.T) {
	discover := func(lose bool) (*env, v1alpha1.IntentSpec) {
		e := newEnv(t, testProject())
		var lost map[string]int
		if lose {
			lost = lostCreates(e)
		}
		e.gh.openIssue(1, "One", "Please add GET /version.", approver)
		e.clock.Advance(time.Minute)
		e.reconcileProject()
		var intents v1alpha1.IntentList
		if err := e.c.List(context.Background(), &intents, client.InNamespace(testNS)); err != nil {
			t.Fatal(err)
		}
		if len(intents.Items) != 1 {
			t.Fatalf("discovered %d Intents, want 1", len(intents.Items))
		}
		if lose && lost["*v1alpha1.Intent"] != 1 {
			t.Fatalf("Intent creates answered AlreadyExists = %d, want 1", lost["*v1alpha1.Intent"])
		}
		return e, intents.Items[0].Spec
	}
	_, want := discover(false)
	e, got := discover(true)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("adopted Intent spec = %+v, want %+v", got, want)
	}
	// Discovery again neither duplicates it nor fails.
	e.clock.Advance(time.Minute)
	e.reconcileProject()
	var intents v1alpha1.IntentList
	if err := e.c.List(context.Background(), &intents, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	if len(intents.Items) != 1 {
		t.Errorf("Intents after a second discovery = %d, want 1", len(intents.Items))
	}
	e.drive(v1alpha1.IntentName("target", 1), v1alpha1.IntentAwaitingApproval, repoImage)
}
