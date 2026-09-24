// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
)

// TestProjectionPostsOnce runs the real integration-controller, informer
// cache and all, against a finding whose investigation finished and is held
// for approval: the issue gets exactly one report comment — recorded by id on
// status.tracking.comments — and one approval notice, however many
// reconciles the projection's own writes and a burst of status churn queue
// behind the one that posted them.
func TestProjectionPostsOnce(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	ctx := context.Background()

	const name = "finding-eeeeeeeeee-1"
	const reportMarker = "<!-- patchy:report Investigation/1 -->"
	inv := &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-inv-1", Namespace: namespace,
			Labels: map[string]string{v1alpha1.LabelFinding: name},
		},
		Spec: v1alpha1.InvestigationSpec{FindingRef: v1alpha1.ObjectReference{Name: name}, Attempt: 1},
	}
	if err := cl.client.Create(ctx, inv); err != nil {
		t.Fatal(err)
	}
	inv.Status.Phase = v1alpha1.RunComplete
	inv.Status.Report = "The sink is reachable from the request handler; escape it."
	if err := cl.client.Status().Update(ctx, inv); err != nil {
		t.Fatal(err)
	}

	fnd := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "github"},
			TrackingRef:    &v1alpha1.LocalObjectReference{Name: "github"},
			Source:         "ghas",
			Advisories:     []string{"CWE-79"},
			Title:          "Reflected cross-site scripting",
			Severity:       v1alpha1.LevelHigh,
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://127.0.0.1/acme/shop", Name: "acme/shop", DefaultBranch: "main",
			},
		},
	}
	if err := cl.client.Create(ctx, fnd); err != nil {
		t.Fatal(err)
	}
	fnd.Status.Phase = v1alpha1.PhaseAwaitingApproval
	fnd.Status.Investigation = &v1alpha1.InvestigationSummary{
		Name: inv.Name, Attempt: 1, Recommendation: v1alpha1.RecommendationRemediate, AwaitApproval: true,
	}
	if err := cl.client.Status().Update(ctx, fnd); err != nil {
		t.Fatal(err)
	}

	cl.controller(t, "integration-controller", "--listen-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)))

	var issue int
	eventually(t, "the report comment to be recorded on the finding", func() bool {
		var cur v1alpha1.Finding
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return false
		}
		tr := cur.Status.Tracking
		if tr == nil || cur.GetAnnotations()["patchy.bitwisemedia.uk/projected-notice"] == "" {
			return false
		}
		for _, c := range tr.Comments {
			if c.Marker == reportMarker && c.ID != 0 {
				issue = int(tr.IssueNumber)
				return true
			}
		}
		return false
	})

	// Churn: every status write queues another projection of the finding.
	for i := range 20 {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur v1alpha1.Finding
			if err := cl.client.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
				return err
			}
			cur.Status.LastFailureReason = fmt.Sprintf("churn %d", i)
			return cl.client.Status().Update(ctx, &cur)
		}); err != nil {
			t.Fatal(err)
		}
	}

	count := func(needle string) int {
		n := 0
		for _, body := range gh.Comments(issue) {
			if strings.Contains(body, needle) {
				n++
			}
		}
		return n
	}
	consistently(t, "one report comment and one approval notice", func() bool {
		return count(reportMarker) == 1 && count("holding this remediation for human approval") == 1
	})
	if n := len(gh.Issues()); n != 1 {
		t.Errorf("issues = %d, want 1", n)
	}
}
