// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package render_test

import (
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/render"
)

// assertContains fails for every wanted substring missing from got, and every
// absent one present.
func assertContains(t *testing.T, got string, want, absent []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
	for _, a := range absent {
		if strings.Contains(got, a) {
			t.Errorf("output wrongly contains %q:\n%s", a, got)
		}
	}
}

func TestFindingSummary(t *testing.T) {
	f := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: "fnd-1"},
		Spec: v1alpha1.FindingSpec{
			Title:      "Command injection",
			Advisories: []string{"GHSA-1", "CVE-2"},
			Severity:   v1alpha1.LevelCritical,
		},
		Status: v1alpha1.FindingStatus{
			Phase:       v1alpha1.PhaseInReview,
			Priority:    v1alpha1.LevelHigh,
			Tracking:    &v1alpha1.TrackingStatus{URL: "https://example.test/issues/7"},
			PullRequest: &v1alpha1.PullRequestStatus{Number: 9, URL: "https://example.test/pull/9"},
		},
	}
	got := doc(func(d *printer.Doc) { render.FindingSummary(d, f) })
	assertContains(t, got, []string{
		"## Finding fnd-1",
		"- **Title:** Command injection",
		"- **Advisories:** GHSA-1, CVE-2",
		"- **Phase:** InReview",
		"- **Severity:** critical",
		"- **Priority:** high",
		"- **Issue:** https://example.test/issues/7",
		"- **Pull request:** https://example.test/pull/9",
	}, nil)

	bare := doc(func(d *printer.Doc) {
		render.FindingSummary(d, &v1alpha1.Finding{ObjectMeta: metav1.ObjectMeta{Name: "fnd-2"}})
	})
	assertContains(t, bare, nil, []string{"Issue", "Pull request"})
}

// TestFindingDetailHumanTrail: who approved, retried and expedited a finding,
// and when, is its audit trail; a detail view that loses it hides why the
// finding moved.
func TestFindingDetailHumanTrail(t *testing.T) {
	at := metav1.NewTime(testClock.Add(-30 * time.Minute))
	merged := metav1.NewTime(testClock.Add(-5 * time.Minute))
	f := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: "fnd-1", CreationTimestamp: metav1.NewTime(testClock.Add(-time.Hour))},
		Spec: v1alpha1.FindingSpec{
			Title: "t",
			Repository: &v1alpha1.FindingRepository{
				Type: "github", URL: "https://example.test/acme/app", Name: "acme/app", DefaultBranch: "main",
			},
			Approval: &v1alpha1.Approval{By: "alice@acme.test", At: at},
			Retry:    &v1alpha1.ActionRequest{By: "bob@acme.test", At: at},
			Expedite: &v1alpha1.ActionRequest{By: "carol@acme.test", At: at},
			Alerts: []v1alpha1.Alert{
				{ID: "a1", URL: "https://example.test/alert/1"},
				{ID: "a2", Locations: []v1alpha1.Location{{Path: "go.mod"}}},
			},
		},
		Status: v1alpha1.FindingStatus{
			Phase:             v1alpha1.PhaseInReview,
			LastFailureReason: "push rejected",
			FirstObservedAt:   &at,
			PhaseTimes:        []v1alpha1.PhaseTime{{Phase: v1alpha1.PhaseQueued, At: at}},
			Tracking:          &v1alpha1.TrackingStatus{IssueNumber: 7, URL: "https://example.test/issues/7", State: "open"},
			PullRequest: &v1alpha1.PullRequestStatus{
				Number: 9, URL: "https://example.test/pull/9", State: "merged", MergedAt: &merged,
			},
			Investigation: &v1alpha1.InvestigationSummary{Name: "fnd-1-inv-1", Attempt: 1},
			Remediation:   &v1alpha1.RemediationSummary{Name: "fnd-1-rem-2", Attempt: 2, Success: true, Branch: "patchy/fix"},
			ActiveRun:     &v1alpha1.ActiveRun{Kind: v1alpha1.RunKindRemediation, Name: "fnd-1-rem-2"},
		},
	}
	got := doc(func(d *printer.Doc) { render.FindingDetail(d, f, testClock, "", nil) })
	assertContains(t, got, []string{
		"- **Repository:** acme/app",
		"- **Branch:** main",
		"- **Failure:** push rejected",
		"- **Approved by:** alice@acme.test at 2026-07-24T11:30:00Z (30m ago)",
		"- **Retry requested:** bob@acme.test",
		"- **Expedited by:** carol@acme.test",
		"- **First observed:** 2026-07-24T11:30:00Z",
		"- **Queued:** 2026-07-24T11:30:00Z",
		"- **Issue:** #7 https://example.test/issues/7",
		"- **State:** open",
		"- **Pull request:** #9 https://example.test/pull/9",
		"- **Merged:** 2026-07-24T11:55:00Z (5m ago)",
		// A missing verdict/confidence reads as a dash, not a gap.
		"fnd-1-inv-1 (attempt 1) verdict - confidence -",
		"fnd-1-rem-2 (attempt 2) success=true branch patchy/fix",
		"- **Running now:** remediation fnd-1-rem-2",
		"- **a1:** https://example.test/alert/1",
		"- **a2:** go.mod",
	}, []string{"Spend", "Suspended"})
}

func TestInvestigationReviewHold(t *testing.T) {
	inv := testInvestigation()
	inv.Status.AwaitApproval = true
	inv.Status.HoldReasons = []v1alpha1.HoldReason{v1alpha1.HoldLowConfidence, v1alpha1.HoldExceedsAutomatedTokens}
	inv.Status.Stage.NumTurns = 12
	got := doc(func(d *printer.Doc) { render.InvestigationReview(d, inv, false) })
	assertContains(t, got, []string{
		"## Investigation fnd-1-inv-1 (attempt 1)",
		// Several reasons are joined, not ranked.
		"awaiting approval — confidence is below the automation threshold; " +
			"the fix is predicted to need more output tokens than run unattended",
		"- **Cost:** $0.031 on anthropic/claude-sonnet-5 (12 turns)",
	}, []string{"verdict: remediate"})
}

func TestHoldTextVariants(t *testing.T) {
	cases := []struct {
		name    string
		reasons []v1alpha1.HoldReason
		params  *v1alpha1.AgentParameters
		want    string
	}{
		{"token hold with estimate", []v1alpha1.HoldReason{v1alpha1.HoldExceedsAutomatedTokens},
			&v1alpha1.AgentParameters{Estimate: &v1alpha1.AgentEstimate{TokenBudget: 700000}},
			"predicted to need 700000 output tokens"},
		{"token hold without estimate", []v1alpha1.HoldReason{v1alpha1.HoldExceedsAutomatedTokens}, nil,
			"more output tokens than run unattended"},
		{"turn hold without estimate", []v1alpha1.HoldReason{v1alpha1.HoldExceedsAutomatedTurns},
			&v1alpha1.AgentParameters{},
			"more turns than run unattended"},
		{"unknown reason shows its name", []v1alpha1.HoldReason{"SomethingNew"}, nil, "awaiting approval — SomethingNew"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := testInvestigation()
			inv.Status.AwaitApproval = true
			inv.Status.HoldReasons = tc.reasons
			inv.Status.RemediationParameters = tc.params
			got := doc(func(d *printer.Doc) { render.InvestigationReview(d, inv, false) })
			assertContains(t, got, []string{tc.want}, nil)
		})
	}
}

func testRemediation() *v1alpha1.Remediation {
	granted := metav1.NewTime(testClock.Add(-10 * time.Minute))
	return &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Name: "fnd-1-rem-1", CreationTimestamp: metav1.NewTime(testClock.Add(-time.Hour))},
		Spec: v1alpha1.RemediationSpec{
			FindingRef:       v1alpha1.ObjectReference{Name: "fnd-1"},
			InvestigationRef: v1alpha1.ObjectReference{Name: "fnd-1-inv-1"},
			Attempt:          1,
			Priority:         73,
			ApprovedBy:       "alice@acme.test",
			Revival:          true,
		},
		Status: v1alpha1.RemediationStatus{
			Success:      true,
			Confidence:   "0.8",
			GrantedAt:    &granted,
			Branch:       "patchy/fnd-1",
			PushedCommit: "abc123",
			PullRequest:  &v1alpha1.PullRequestRef{Number: 9, URL: "https://example.test/pull/9"},
			Report:       "---\nsuccess: true\n---\n\n# Fix\n\nSanitised the input.",
			Stage: &v1alpha1.StageResult{Outcome: "ok", Model: "m", NumTurns: 4,
				Usage: v1alpha1.UsageSummary{CostUSD: "0.10"}},
		},
	}
}

func TestRemediationDetail(t *testing.T) {
	got := doc(func(d *printer.Doc) { render.RemediationDetail(d, testRemediation(), testClock) })
	assertContains(t, got, []string{
		"- **Investigation:** fnd-1-inv-1",
		"- **Queue priority:** 73",
		"- **Approved by:** alice@acme.test",
		"- **Granted:** 2026-07-24T11:50:00Z (10m ago)",
		"- **Revival:** yes",
		"- **Branch:** patchy/fnd-1",
		"- **Commit:** abc123",
		"- **Pull request:** #9 https://example.test/pull/9",
		"## Agent run",
	}, []string{"## Budget"}) // no parameters at all: no budget section
}

func TestRemediationBudgetWithoutEstimate(t *testing.T) {
	rem := testRemediation()
	rem.Spec.Parameters = v1alpha1.AgentParameters{MaxTurns: 40, TokenBudget: 1000}
	rem.Status.Stage = nil
	got := doc(func(d *printer.Doc) { render.RemediationDetail(d, rem, testClock) })
	assertContains(t, got, []string{
		"- **Turns:** 0 of 40 granted (no estimate)",
		"- **Output tokens:** 0 of 1000 granted (no estimate)",
	}, []string{"est."})
}

func TestRemediationReview(t *testing.T) {
	rem := testRemediation()
	got := doc(func(d *printer.Doc) { render.RemediationReview(d, rem, false) })
	assertContains(t, got, []string{
		"## Remediation fnd-1-rem-1 (attempt 1)",
		"- **Success:** true",
		"- **Confidence:** 0.8",
		"- **Outcome:** ok",
		"- **Cost:** $0.10 on m (4 turns)",
		"- **Pull request:** #9 https://example.test/pull/9",
		"# Fix",
	}, []string{"success: true"})

	raw := doc(func(d *printer.Doc) { render.RemediationReview(d, rem, true) })
	assertContains(t, raw, []string{"success: true"}, nil)

	rem.Status.PullRequest = nil
	rem.Status.Stage = nil
	noPR := doc(func(d *printer.Doc) { render.RemediationReview(d, rem, false) })
	assertContains(t, noPR, nil, []string{"Pull request", "Cost"})
}

func TestSkewPercent(t *testing.T) {
	cases := []struct {
		predicted, actual int64
		want              string
	}{
		{0, 10, "-"},
		{-5, 10, "-"},
		{10, 10, "on target"},
		{10, 25, "+150%"},
		{10, 5, "-50%"},
		{100, 100 - 0, "on target"},
	}
	for _, tc := range cases {
		if got := render.SkewPercent(tc.predicted, tc.actual); got != tc.want {
			t.Errorf("SkewPercent(%d, %d) = %q, want %q", tc.predicted, tc.actual, got, tc.want)
		}
	}
}

// TestSkewPercentSign is the property the doc comment promises: the sign says
// which way the estimate was wrong.
func TestSkewPercentSign(t *testing.T) {
	prop := func(p, a uint16) bool {
		predicted, actual := int64(p)+1, int64(a)
		got := render.SkewPercent(predicted, actual)
		pct := (actual - predicted) * 100 / predicted
		switch {
		case pct > 0:
			return strings.HasPrefix(got, "+") && strings.HasSuffix(got, "%")
		case pct < 0:
			return strings.HasPrefix(got, "-") && strings.HasSuffix(got, "%")
		default:
			return got == "on target"
		}
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(1))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestCostAndDash(t *testing.T) {
	if got := render.Cost(nil); got != "" {
		t.Errorf("Cost(nil) = %q", got)
	}
	if got := render.Cost(&v1alpha1.StageResult{Model: "m"}); got != "" {
		t.Errorf("Cost without a figure = %q, want empty", got)
	}
	if got := render.Dash(""); got != "-" {
		t.Errorf("Dash(empty) = %q", got)
	}
	if got := render.Dash("x"); got != "x" {
		t.Errorf("Dash(x) = %q", got)
	}
	if got := render.Timestamp(nil, testClock); got != "" {
		t.Errorf("Timestamp(nil) = %q", got)
	}
	if got := render.Timestamp(&metav1.Time{}, testClock); got != "" {
		t.Errorf("Timestamp(zero) = %q", got)
	}
}

func TestRepositoryDetail(t *testing.T) {
	fetched := metav1.NewTime(testClock.Add(-2 * time.Minute))
	repo := &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fnd-1-src",
			Labels:            map[string]string{v1alpha1.LabelFinding: "fnd-1"},
			CreationTimestamp: metav1.NewTime(testClock.Add(-time.Hour)),
		},
		Spec: v1alpha1.RepositorySpec{
			URL: "https://example.test/acme/app",
			Ref: v1alpha1.RepositoryRef{Branch: "main"},
		},
		Status: v1alpha1.RepositoryStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Fetched", Message: "artifact stored"},
				{Type: v1alpha1.ConditionStalled, Status: metav1.ConditionFalse},
			},
			ResolvedSHA: "deadbeef",
			Forge:       &v1alpha1.LocalObjectReference{Name: "github"},
			Artifact:    &v1alpha1.Artifact{Digest: "sha256:aa", SizeBytes: 1234, LastFetchedAt: &fetched},
		},
	}
	got := doc(func(d *printer.Doc) { render.RepositoryDetail(d, repo, testClock) })
	assertContains(t, got, []string{
		"## Repository fnd-1-src",
		"- **URL:** https://example.test/acme/app",
		"- **Branch:** main",
		"- **Finding:** fnd-1",
		"- **Ready:** True Fetched — artifact stored",
		"- **Stalled:** False\n",
		"- **Commit:** deadbeef",
		"- **Forge:** github",
		"## Artifact",
		"- **Size:** 1234 bytes",
		"- **Fetched:** 2026-07-24T11:58:00Z (2m ago)",
	}, []string{"## Runner image"})

	bare := doc(func(d *printer.Doc) {
		render.RepositoryDetail(d, &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "r"}}, testClock)
	})
	assertContains(t, bare, nil, []string{"Ready", "Forge", "Artifact"})
}
