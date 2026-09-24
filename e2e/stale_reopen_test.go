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
)

// TestStaleReopenAfterMergedFix drives the shipped integration-controller
// through the duplicate-finding regression recorded on patchy-target: an
// alert's finding is remediated by a merged PR, then CodeQL reopens the alert
// from an analysis of the merge's parent that uploaded last. That reopen
// observes code the fix replaced and must not open a successor generation;
// a later reopen at a commit after the fix must.
func TestStaleReopenAfterMergedFix(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	gh.SetParents(map[string]string{fixMerge: staleCommit, regressed: fixMerge})
	ctx := context.Background()

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "integration-controller", "--listen-addr", listen)
	webhookURL := "http://" + listen + "/github/webhooks"

	findings := func() []v1alpha1.Finding {
		var list v1alpha1.FindingList
		if err := cl.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			t.Fatalf("list findings: %v", err)
		}
		return list.Items
	}

	// 1. The alert opens generation 1.
	deliver(t, webhookURL, "code_scanning_alert", fixture(t, "code_scanning_alert.created.json"))
	var gen1 v1alpha1.Finding
	eventually(t, "the alert to open a finding", func() bool {
		items := findings()
		if len(items) != 1 {
			return false
		}
		gen1 = items[0]
		return true
	})

	// 2. The pipeline carries it to review (fabricated: no job controllers
	//    run here) with its remediation PR open.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(&gen1), &cur); err != nil {
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
		[]byte("finding-cccccccccc-1"), []byte(gen1.Name))
	deliver(t, webhookURL, "pull_request", merged)
	eventually(t, "the merge to remediate the finding and record its commit", func() bool {
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(&gen1), &cur); err != nil {
			return false
		}
		return cur.Status.Phase == v1alpha1.PhaseRemediated &&
			cur.Status.PullRequest != nil && cur.Status.PullRequest.MergeCommitSHA == fixMerge
	})

	// 4. The stale reopen — at the merge's parent — is recognized and
	//    skipped: the controller asks GitHub for the ancestry, and no
	//    successor appears.
	stale := fixture(t, "code_scanning_alert.reopened.stale.json")
	deliver(t, webhookURL, "code_scanning_alert", stale)
	eventually(t, "the stale reopen's ancestry lookup", func() bool { return gh.Compares() >= 1 })
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if items := findings(); len(items) != 1 {
			t.Fatalf("findings = %d after the stale reopen, want only the remediated one", len(items))
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 5. A reopen at a commit after the fix is a regression: it opens the
	//    successor generation.
	deliver(t, webhookURL, "code_scanning_alert",
		bytes.ReplaceAll(stale, []byte(staleCommit), []byte(regressed)))
	eventually(t, "the regression to open a successor", func() bool {
		for _, f := range findings() {
			if f.Name != gen1.Name && len(f.Spec.Related) == 1 && f.Spec.Related[0].To == gen1.Name {
				return true
			}
		}
		return false
	})
}
