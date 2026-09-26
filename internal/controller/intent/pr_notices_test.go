// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

func emptyReviewRound(t *testing.T) (*env, string, int64) {
	t.Helper()
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.reviews[number] = []ghclient.Review{{ID: 2501, NodeID: "review-2501",
		Author: actorOf(approver), State: "CHANGES_REQUESTED", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	return e, name, number
}

func TestRoundNoticeRefusalIsNotSuccess(t *testing.T) {
	e, name, number := emptyReviewRound(t)
	e.gh.prComments[number] = nil
	p := &pass{r: e.intent, in: e.get(name), proj: testProject()}
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	e.gh.failNext("CreateIssueComment", ghError(http.StatusForbidden, "Resource not accessible by integration"))
	if err := p.finishPRRound(context.Background(), &run); err == nil {
		t.Fatal("a refused round notice was treated as delivered")
	}
}

func TestLegacyMissingRoundNoticeRecoveredAfterRestart(t *testing.T) {
	e, name, number := emptyReviewRound(t)
	// Model v0.12.5: the round settled but its notice was refused and lost.
	e.gh.prComments[number] = nil
	in := e.get(name)
	in.Status.RoundNoticesThrough = 0
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	jobsBefore := len(e.jobs.launched())
	for range 4 {
		e.restart()
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	count := 0
	for _, c := range e.gh.prComments[number] {
		if strings.Contains(c.Body, "patchy:intent-pr-round:") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recovered round notices = %d, want exactly one", count)
	}
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.Revisions != 0 ||
		len(e.jobs.launched()) != jobsBefore || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 1 {
		t.Fatal("notice recovery launched work or charged the failed round")
	}
}

func TestRoundNoticeDeliverySurvivesFailures(t *testing.T) {
	for _, method := range []string{"ListIssueComments", "BotLogin", "CreateIssueComment"} {
		t.Run(method, func(t *testing.T) {
			e, name, number := emptyReviewRound(t)
			e.gh.prComments[number] = nil
			for range 2 {
				e.restart()
				e.gh.failNext(method, ghError(http.StatusForbidden, "Resource not accessible by integration"))
				e.clock.Advance(time.Minute)
				if err := e.reconcileIntent(name); err == nil {
					t.Fatal("refused notice delivery did not schedule an error retry")
				}
				if e.get(name).Status.RoundNoticesThrough != 0 {
					t.Fatal("cursor advanced past a failed delivery")
				}
			}
			// A successful post followed by a failed status write must be adopted.
			e.failStatusIf = func(in *v1alpha1.Intent) bool { return in.Status.RoundNoticesThrough == 1 }
			e.clock.Advance(time.Minute)
			if err := e.reconcileIntent(name); !errors.Is(err, errTransient) {
				t.Fatalf("cursor write failure = %v", err)
			}
			for range 4 {
				e.restart()
				e.clock.Advance(time.Minute)
				e.mustIntent(name)
			}
			if len(e.gh.prComments[number]) != 1 || e.get(name).Status.RoundNoticesThrough != 1 {
				t.Fatalf("delivery not exactly once: %d comments, cursor %d",
					len(e.gh.prComments[number]), e.get(name).Status.RoundNoticesThrough)
			}
		})
	}
}

type lostPRCommentResponse struct {
	GitHub
	lost bool
}

func (g *lostPRCommentResponse) CreatePullRequestComment(ctx context.Context, repo string, n int64, body string) (
	*ghclient.Comment, error) {
	c, err := g.GitHub.CreatePullRequestComment(ctx, repo, n, body)
	if err == nil && !g.lost {
		g.lost = true
		return nil, errTransient
	}
	return c, err
}

func TestRoundNoticeLostResponseAdoptsBotMarker(t *testing.T) {
	e, name, number := emptyReviewRound(t)
	e.gh.prComments[number] = nil
	// A human copying the exact marker cannot suppress the bot's delivery.
	e.gh.prComments[number] = []*ghclient.Comment{{Body: "<!-- patchy:intent-pr-round:" + name + ":1 -->",
		UserLogin: approver, CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.intent.GitHub = &lostPRCommentResponse{GitHub: e.gh}
	e.clock.Advance(time.Minute)
	if err := e.reconcileIntent(name); !errors.Is(err, errTransient) {
		t.Fatalf("lost post response = %v", err)
	}
	e.restart()
	e.clock.Advance(time.Minute)
	e.mustIntent(name)
	if len(e.gh.prComments[number]) != 2 || e.get(name).Status.RoundNoticesThrough != 1 {
		t.Fatal("lost response was duplicated, or a human marker was trusted")
	}
}

func TestPendingRoundNoticeDoesNotBlockMergeOrExpire(t *testing.T) {
	e, name, number := emptyReviewRound(t)
	e.gh.prComments[number] = nil
	e.gh.closePR(true)
	e.clock.Advance(time.Minute)
	e.drive(name, v1alpha1.IntentMerged, repoImage)
	e.clock.Advance(15 * 24 * time.Hour)
	if _, err := e.ttl.Reconcile(context.Background(), req(name)); err != nil {
		t.Fatal(err)
	}
	e.get(name) // still retained for delivery
	e.gh.failNext("CreateIssueComment", ghError(http.StatusForbidden, "Resource not accessible by integration"))
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("terminal notice refusal was lost")
	}
	e.restart()
	e.mustIntent(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentMerged || in.Status.RoundNoticesThrough != 1 {
		t.Fatalf("terminal delivery = %+v", in.Status)
	}
	if _, err := e.ttl.Reconcile(context.Background(), req(name)); err != nil {
		t.Fatal(err)
	}
}

func TestRoundNoticeRecoveryRespectsRateFloor(t *testing.T) {
	for _, ended := range []bool{false, true} {
		e, name, number := emptyReviewRound(t)
		e.gh.prComments[number] = nil
		if ended {
			e.gh.closePR(true)
			e.clock.Advance(time.Minute)
			e.drive(name, v1alpha1.IntentMerged, repoImage)
		}
		e.gh.appRemaining = new(int)
		e.restart()
		e.clock.Advance(time.Minute)
		for range 3 {
			res, err := e.intent.Reconcile(context.Background(), req(name))
			if err != nil || res.RequeueAfter < time.Second {
				t.Fatalf("under-floor recovery ended=%t: %+v, %v", ended, res, err)
			}
		}
		if len(e.gh.prComments[number]) != 0 || e.get(name).Status.RoundNoticesThrough != 0 {
			t.Fatal("notice delivered below the repository's rate floor")
		}
		e.gh.appRemaining = nil
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
		if len(e.gh.prComments[number]) != 1 {
			t.Fatal("notice was not delivered after rate budget recovered")
		}
	}
}
