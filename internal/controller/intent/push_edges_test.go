// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// headAfterCommit is the run reconciler's GitHub with one read of the
// intent branch's head failing: the first one after a commit is created
// while armed, the read a revise push makes just before its fast-forward.
type headAfterCommit struct {
	GitHub
	mu        sync.Mutex
	err       error
	armed     bool
	committed bool
}

func (h *headAfterCommit) CreateCommit(ctx context.Context, repoURL string, req ghclient.CommitRequest) (
	string, error) {
	sha, err := h.GitHub.CreateCommit(ctx, repoURL, req)
	if err == nil {
		h.mu.Lock()
		h.committed = h.armed
		h.mu.Unlock()
	}
	return sha, err
}

func (h *headAfterCommit) HeadSHA(ctx context.Context, repoURL, branch string) (string, error) {
	h.mu.Lock()
	fail := h.committed && strings.HasPrefix(branch, "patchy-intent/")
	if fail {
		h.committed, h.armed = false, false
	}
	h.mu.Unlock()
	if fail {
		return "", h.err
	}
	return h.GitHub.HeadSHA(ctx, repoURL, branch)
}

// driveCollectingErrors is drive that keeps going over run reconcile errors, and
// returns them.
func (e *env) driveCollectingErrors(name string, want v1alpha1.IntentPhase) []error {
	e.t.Helper()
	var errs []error
	ctx := context.Background()
	for range 60 {
		if e.get(name).Status.Phase == want {
			return errs
		}
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
			errs = append(errs, err)
		}
		var list v1alpha1.IntentRunList
		if err := e.c.List(ctx, &list, client.InNamespace(testNS)); err != nil {
			e.t.Fatal(err)
		}
		for i := range list.Items {
			if _, err := e.runs.Reconcile(ctx, req(list.Items[i].Name)); err != nil {
				errs = append(errs, err)
			}
		}
		e.clock.Advance(time.Minute)
	}
	e.t.Fatalf("intent %s did not reach %s: phase %s", name, want, e.get(name).Status.Phase)
	return nil
}

// TestRevisePushHeadReadFailures: the read of the intent branch's head just
// before a revise fast-forward fails closed. A branch deleted meanwhile is
// head_moved and the round retries; a refused read is push_refused with
// nothing pushed; and a transient failure is retried until the push lands.
func TestRevisePushHeadReadFailures(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantOutcome string // the first attempt's; "" when it completes
	}{
		{"branch deleted", ghError(http.StatusNotFound, "Not Found"), OutcomeHeadMoved},
		{"read refused", ghError(http.StatusForbidden, "Resource not accessible by integration"), OutcomePushRefused},
		{"transient", errTransient, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			gh := &headAfterCommit{GitHub: e.gh, err: tc.err}
			e.runs.GitHub = gh
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)
			buildHead := e.gh.branches[branchName(name)]
			number := in.Status.PullRequests[0].Number
			e.gh.reviews[number] = []ghclient.Review{{ID: 971, NodeID: "review-971", Author: actorOf(approver),
				State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
			e.clock.Advance(3 * time.Minute)
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			gh.mu.Lock()
			gh.armed = true
			gh.mu.Unlock()

			if tc.wantOutcome == "" {
				errs := e.driveCollectingErrors(name, v1alpha1.IntentInReview)
				if len(errs) != 1 || !errors.Is(errs[0], errTransient) ||
					!strings.Contains(errs[0].Error(), "read intent PR head before advancing") {
					t.Fatalf("run reconcile errors = %v, want the one transient head read", errs)
				}
				runs := e.runsOf(name, v1alpha1.IntentStageRevise)
				if len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete ||
					e.gh.branches[branchName(name)] != runs[0].Status.PushedCommit {
					t.Fatalf("revise attempts = %d, want one complete and pushed", len(runs))
				}
				return
			}
			for range 30 {
				runs := e.runsOf(name, v1alpha1.IntentStageRevise)
				if runs[0].Status.Phase == v1alpha1.RunFailed {
					break
				}
				e.mustIntent(name)
				e.readyRepositories(repoImage)
				e.runRuns()
				e.clock.Advance(time.Minute)
			}
			first := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
			if first.Status.Phase != v1alpha1.RunFailed || first.Status.Outcome != tc.wantOutcome {
				t.Fatalf("first attempt = %s/%s (%s), want Failed/%s", first.Status.Phase, first.Status.Outcome,
					first.Status.Detail, tc.wantOutcome)
			}
			if first.Status.PushedCommit == "" {
				t.Error("the attempt recorded no commit")
			}
			if got := e.gh.branches[branchName(name)]; got != buildHead {
				t.Errorf("intent branch = %s, want still the build's %s: nothing pushed", got, buildHead)
			}
		})
	}
}
