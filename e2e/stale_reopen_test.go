// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
)

// The recorded patchy-target history: the fix's squash merge sits directly
// on the commit whose late CodeQL analysis reopened the alert; regressed
// stands in for a later commit that brings the vulnerable code back.
const (
	staleCommit = "45b1bec980a1aba44367aa7bf871e8b658776d40"
	fixMerge    = "fa82fcdc7efab2777d432ba3385517fa735e0ae0" // pull_request.merged.json's merge_commit_sha
	regressed   = "3fbab3d1e9b360ca26d757f4823887ebd090f3c1"
	staleAlert  = 7 // code_scanning_alert.reopened.stale.json's alert
	mainRef     = "refs/heads/main"
)

// staleFlow is the shipped integration-controller carried to the moment the
// duplicate-finding regression recorded on patchy-target began: an alert's
// finding remediated by a merged PR whose merge commit is recorded.
type staleFlow struct {
	cl   *cluster
	gh   *fakegithub.Server
	url  string
	gen1 v1alpha1.Finding
}

func newStaleFlow(t *testing.T, args ...string) *staleFlow {
	t.Helper()
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	gh.SetParents(map[string]string{fixMerge: staleCommit, regressed: fixMerge})
	gh.SetBranch("main", fixMerge)
	ctx := context.Background()

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "integration-controller", append([]string{"--listen-addr", listen}, args...)...)
	s := &staleFlow{cl: cl, gh: gh, url: "http://" + listen + "/github/webhooks"}

	// 1. The alert opens generation 1.
	deliver(t, s.url, "code_scanning_alert", fixture(t, "code_scanning_alert.created.json"))
	eventually(t, "the alert to open a finding", func() bool {
		items := s.findings(t)
		if len(items) != 1 {
			return false
		}
		s.gen1 = items[0]
		return true
	})

	// 2. The pipeline carries it to review (fabricated: no job controllers
	//    run here) with its remediation PR open.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(&s.gen1), &cur); err != nil {
			return err
		}
		cur.Status.Phase = v1alpha1.PhaseInReview
		cur.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 901, State: "open"}
		return cl.client.Status().Update(ctx, &cur)
	})
	if err != nil {
		t.Fatalf("fabricate InReview: %v", err)
	}

	// 3. The fix merges; the finding records the merge commit.
	merged := bytes.ReplaceAll(fixture(t, "pull_request.merged.json"),
		[]byte("finding-cccccccccc-1"), []byte(s.gen1.Name))
	deliver(t, s.url, "pull_request", merged)
	eventually(t, "the merge to remediate the finding and record its commit", func() bool {
		cur := s.get(t)
		return cur.Status.Phase == v1alpha1.PhaseRemediated &&
			cur.Status.PullRequest != nil && cur.Status.PullRequest.MergeCommitSHA == fixMerge
	})
	return s
}

func (s *staleFlow) findings(t *testing.T) []v1alpha1.Finding {
	t.Helper()
	var list v1alpha1.FindingList
	if err := s.cl.client.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list findings: %v", err)
	}
	return list.Items
}

// get re-reads generation 1.
func (s *staleFlow) get(t *testing.T) v1alpha1.Finding {
	t.Helper()
	var cur v1alpha1.Finding
	if err := s.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&s.gen1), &cur); err != nil {
		t.Fatalf("get %s: %v", s.gen1.Name, err)
	}
	return cur
}

// successor reports whether a generation succeeding generation 1 exists.
func (s *staleFlow) successor(t *testing.T) bool {
	t.Helper()
	for _, f := range s.findings(t) {
		if f.Name != s.gen1.Name && len(f.Spec.Related) == 1 && f.Spec.Related[0].To == s.gen1.Name {
			return true
		}
	}
	return false
}

// deliverStale delivers the recorded stale reopen, at commit.
func (s *staleFlow) deliverStale(t *testing.T, commit string) {
	t.Helper()
	stale := fixture(t, "code_scanning_alert.reopened.stale.json")
	deliver(t, s.url, "code_scanning_alert", bytes.ReplaceAll(stale, []byte(staleCommit), []byte(commit)))
}

// TestStaleReopenAfterMergedFix drives the shipped integration-controller
// through the duplicate-finding regression recorded on patchy-target: an
// alert's finding is remediated by a merged PR, then CodeQL reopens the alert
// from an analysis of the merge's parent that uploaded last. That reopen
// observes code the fix replaced and must not open a successor generation —
// it is recorded for the re-check instead; a later reopen at a commit after
// the fix must open one.
func TestStaleReopenAfterMergedFix(t *testing.T) {
	s := newStaleFlow(t)

	// 4. The stale reopen — at the merge's parent, with main still carrying
	//    the fix — is recognized and set aside: the controller asks GitHub
	//    for the ancestry, records the observation, and opens no successor.
	s.deliverStale(t, staleCommit)
	eventually(t, "the stale reopen to be recorded for the re-check", func() bool {
		obs := s.get(t).Status.StaleObservations
		return len(obs) == 1 && obs[0].AlertID == fmt.Sprint(staleAlert) &&
			obs[0].Commit == staleCommit && obs[0].Ref == mainRef
	})
	if n := s.gh.Compares(); n < 2 {
		t.Errorf("compare calls = %d, want the ancestry and the branch lookups", n)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if items := s.findings(t); len(items) != 1 {
			t.Fatalf("findings = %d after the stale reopen, want only the remediated one", len(items))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 5. A reopen at a commit after the fix is a regression: it opens the
	//    successor generation.
	s.deliverStale(t, regressed)
	eventually(t, "the regression to open a successor", func() bool { return s.successor(t) })
}

// TestSilentRegressionAfterStaleReopen: the stale reopen leaves the alert
// open on GitHub, which reports alerts only when their state changes. When
// the next analysis of main is of a commit that brings the vulnerable code
// back, no webhook arrives at all — the controller's re-check of the alert
// must find it and open the successor.
func TestSilentRegressionAfterStaleReopen(t *testing.T) {
	s := newStaleFlow(t, "--stale-recheck-interval", "250ms")

	// GitHub's view after the stale reopen: open, latest at the stale
	// commit on main.
	s.gh.SetAlert(staleAlert, "open", mainRef, staleCommit)
	s.deliverStale(t, staleCommit)
	eventually(t, "the stale reopen to be recorded for the re-check", func() bool {
		return len(s.get(t).Status.StaleObservations) == 1
	})
	consistently(t, "no successor while the alert stays where the fix superseded it", func() bool {
		return len(s.findings(t)) == 1 && len(s.get(t).Status.StaleObservations) == 1
	})

	// The next push reverts the fix; its analysis finds the alert, already
	// open, at the new head — and GitHub sends nothing.
	s.gh.SetBranch("main", regressed)
	s.gh.SetAlert(staleAlert, "open", mainRef, regressed)
	eventually(t, "the re-check to open the successor", func() bool { return s.successor(t) })
	eventually(t, "the settled observation to be cleared", func() bool {
		return len(s.get(t).Status.StaleObservations) == 0
	})
}

// TestReopenAfterMainResetBeforeFix: undoing the merge by force-pushing main
// back to the fix's parent keeps the orphaned merge commit resolvable, so a
// reopen at the parent still precedes it — but the vulnerable code is back
// on main, and the reopen must open the successor.
func TestReopenAfterMainResetBeforeFix(t *testing.T) {
	s := newStaleFlow(t)
	s.gh.SetBranch("main", staleCommit)
	s.deliverStale(t, staleCommit)
	eventually(t, "the reopen on the reset main to open a successor", func() bool { return s.successor(t) })
	if obs := s.get(t).Status.StaleObservations; len(obs) != 0 {
		t.Errorf("stale observations = %+v, want none", obs)
	}
}
