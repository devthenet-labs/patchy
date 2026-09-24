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

// reviewFlow is the shipped integration-controller with one finding carried
// to review of PR #901, from its branch in acme/shop.
type reviewFlow struct {
	cl  *cluster
	gh  *fakegithub.Server
	url string
	fnd v1alpha1.Finding
}

// startReview opens a finding from an alert, waits for its tracking issue,
// and fabricates its review of PR #901 (no job controllers run here). The PR
// itself is the test's to put on the fake.
func startReview(t *testing.T) *reviewFlow {
	t.Helper()
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.controller(t, "integration-controller", "--listen-addr", listen)
	f := &reviewFlow{cl: cl, gh: gh, url: "http://" + listen + "/github/webhooks"}

	deliver(t, f.url, "code_scanning_alert", fixture(t, "code_scanning_alert.created.json"))
	eventually(t, "the alert's finding to be projected to an issue", func() bool {
		var list v1alpha1.FindingList
		if err := cl.client.List(context.Background(), &list, client.InNamespace(namespace)); err != nil ||
			len(list.Items) != 1 {
			return false
		}
		f.fnd = list.Items[0]
		return f.fnd.Status.Tracking != nil && f.fnd.Status.Tracking.URL != ""
	})

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := f.get(t)
		cur.Status.Phase = v1alpha1.PhaseInReview
		cur.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 901, State: "open"}
		return cl.client.Status().Update(context.Background(), &cur)
	})
	if err != nil {
		t.Fatalf("fabricate InReview: %v", err)
	}
	return f
}

// get reads the finding as it is now.
func (f *reviewFlow) get(t *testing.T) v1alpha1.Finding {
	t.Helper()
	var cur v1alpha1.Finding
	key := client.ObjectKey{Namespace: namespace, Name: f.fnd.Name}
	if err := f.cl.client.Get(context.Background(), key, &cur); err != nil {
		t.Fatalf("get %s: %v", f.fnd.Name, err)
	}
	return cur
}

// closeIssue delivers the tracking issue's issues.closed.
func (f *reviewFlow) closeIssue(t *testing.T) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"action": "closed",
		"issue": map[string]any{
			"number":   f.fnd.Status.Tracking.IssueNumber,
			"state":    "closed",
			"html_url": f.fnd.Status.Tracking.URL,
		},
		"repository": map[string]any{"name": "shop", "full_name": "acme/shop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, f.url, "issues", payload)
}

// prMerged is PR #901's merge delivery for the finding, as recorded.
func (f *reviewFlow) prMerged(t *testing.T) []byte {
	t.Helper()
	return bytes.ReplaceAll(fixture(t, "pull_request.merged.json"),
		[]byte("finding-cccccccccc-1"), []byte(f.fnd.Name))
}

// TestMergeIssueCloseFirst: merging a remediation PR sends two deliveries —
// the PR's close and, through the body's "Fixes #N", its tracking issue's
// close — and the controller handles them unordered. When the issue's close
// lands first, the shipped integration-controller asks GitHub for the PR and
// settles the finding Remediated with the merge commit recorded, instead of
// handing it off; the PR's own delivery, arriving second, changes nothing.
func TestMergeIssueCloseFirst(t *testing.T) {
	f := startReview(t)
	f.gh.MergePull(901, "patchy/"+f.fnd.Name, fixMerge)

	f.closeIssue(t)
	eventually(t, "the issue's close to settle the merge, not hand the finding off", func() bool {
		cur := f.get(t)
		if cur.Status.Phase == v1alpha1.PhaseHandedOff {
			t.Fatal("the merge's issue close handed the finding off")
		}
		return cur.Status.Phase == v1alpha1.PhaseRemediated && cur.Status.PullRequest != nil &&
			cur.Status.PullRequest.State == "merged" && cur.Status.PullRequest.MergeCommitSHA == fixMerge
	})
	settled := f.get(t)

	deliver(t, f.url, "pull_request", f.prMerged(t))
	consistently(t, "the late PR delivery to leave the settled finding alone", func() bool {
		cur := f.get(t)
		return cur.Status.Phase == v1alpha1.PhaseRemediated &&
			cur.Status.PullRequest.MergeCommitSHA == fixMerge &&
			cur.Status.CompletedAt.Equal(settled.Status.CompletedAt)
	})
}

// TestHumanCloseDuringReviewLookupFails: a human closes the tracking issue
// to take the finding over while its PR is still open, and GitHub fails the
// first reads of that PR. The webhook's delivery is never redelivered once
// answered, so the close must be kept and the PR read again until GitHub
// answers: the finding is then handed off, not left in review.
func TestHumanCloseDuringReviewLookupFails(t *testing.T) {
	f := startReview(t)
	f.gh.OpenPull(901, "patchy/"+f.fnd.Name)
	f.gh.FailPullReads(3)

	f.closeIssue(t)
	eventually(t, "the human's close to hand the finding off once the PR can be read", func() bool {
		cur := f.get(t)
		return cur.Status.Phase == v1alpha1.PhaseHandedOff &&
			cur.Status.Tracking != nil && cur.Status.Tracking.State == "closed"
	})
}

// TestMergeFromRenamedRepository: the finding's repository is renamed while
// its PR is in review, so the merge's delivery names the new repository,
// not the recorded one. The controller confirms against the recorded PR —
// GitHub redirects the old name — and settles the merge rather than
// ignoring it.
func TestMergeFromRenamedRepository(t *testing.T) {
	f := startReview(t)
	f.gh.MergePull(901, "patchy/"+f.fnd.Name, fixMerge)

	renamed := bytes.ReplaceAll(f.prMerged(t), []byte(`"acme/shop"`), []byte(`"acme/storefront"`))
	deliver(t, f.url, "pull_request", renamed)
	eventually(t, "the renamed repository's merge to settle the finding", func() bool {
		cur := f.get(t)
		return cur.Status.Phase == v1alpha1.PhaseRemediated && cur.Status.PullRequest != nil &&
			cur.Status.PullRequest.State == "merged" && cur.Status.PullRequest.MergeCommitSHA == fixMerge
	})
}
