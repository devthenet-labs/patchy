// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
)

// TestMergeIssueCloseFirst: merging a remediation PR sends two deliveries —
// the PR's close and, through the body's "Fixes #N", its tracking issue's
// close — and the controller handles them unordered. When the issue's close
// lands first, the shipped integration-controller asks GitHub for the PR and
// settles the finding Remediated with the merge commit recorded, instead of
// handing it off; the PR's own delivery, arriving second, changes nothing.
func TestMergeIssueCloseFirst(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	ctx := context.Background()

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "integration-controller", "--listen-addr", listen)
	url := "http://" + listen + "/github/webhooks"

	get := func(name string) v1alpha1.Finding {
		t.Helper()
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cur); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return cur
	}

	// 1. An alert opens a finding, projected to its tracking issue.
	deliver(t, url, "code_scanning_alert", fixture(t, "code_scanning_alert.created.json"))
	var fnd v1alpha1.Finding
	eventually(t, "the alert's finding to be projected to an issue", func() bool {
		var list v1alpha1.FindingList
		if err := cl.client.List(ctx, &list, client.InNamespace(namespace)); err != nil || len(list.Items) != 1 {
			return false
		}
		fnd = list.Items[0]
		return fnd.Status.Tracking != nil && fnd.Status.Tracking.URL != ""
	})

	// 2. The pipeline carries it to review of PR #901 (fabricated: no job
	//    controllers run here), and the PR merges on GitHub.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := get(fnd.Name)
		cur.Status.Phase = v1alpha1.PhaseInReview
		cur.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 901, State: "open"}
		return cl.client.Status().Update(ctx, &cur)
	})
	if err != nil {
		t.Fatalf("fabricate InReview: %v", err)
	}
	gh.MergePull(901, "patchy/"+fnd.Name, fixMerge)

	// 3. The merge's issue close lands first.
	closeIssue, err := json.Marshal(map[string]any{
		"action": "closed",
		"issue": map[string]any{
			"number":   fnd.Status.Tracking.IssueNumber,
			"state":    "closed",
			"html_url": fnd.Status.Tracking.URL,
		},
		"repository": map[string]any{"name": "shop", "full_name": "acme/shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, url, "issues", closeIssue)
	eventually(t, "the issue's close to settle the merge, not hand the finding off", func() bool {
		cur := get(fnd.Name)
		if cur.Status.Phase == v1alpha1.PhaseHandedOff {
			t.Fatal("the merge's issue close handed the finding off")
		}
		return cur.Status.Phase == v1alpha1.PhaseRemediated && cur.Status.PullRequest != nil &&
			cur.Status.PullRequest.State == "merged" && cur.Status.PullRequest.MergeCommitSHA == fixMerge
	})
	settled := get(fnd.Name)

	// 4. The PR's own delivery, arriving second, changes nothing.
	merged := bytes.ReplaceAll(fixture(t, "pull_request.merged.json"),
		[]byte("finding-cccccccccc-1"), []byte(fnd.Name))
	deliver(t, url, "pull_request", merged)
	consistently(t, "the late PR delivery to leave the settled finding alone", func() bool {
		cur := get(fnd.Name)
		return cur.Status.Phase == v1alpha1.PhaseRemediated &&
			cur.Status.PullRequest.MergeCommitSHA == fixMerge &&
			cur.Status.CompletedAt.Equal(settled.Status.CompletedAt)
	})
}
