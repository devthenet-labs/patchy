// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

const issueURL = "https://github.test/acme/orders/issues/7"

// execRoot runs the full command tree against the harness's fake cluster.
// NewRoot rebinds the persistent flags, so the output format comes from args.
func (h *harness) execRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRoot(h.opts)
	root.SetOut(h.out)
	root.SetErr(h.errOut)
	root.SetArgs(append([]string{"--no-color"}, args...))
	return root.ExecuteContext(context.Background())
}

func tracked(f *v1alpha1.Finding) {
	f.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7, URL: issueURL}
}

func testInv(name, finding string, mutate ...func(*v1alpha1.Investigation)) *v1alpha1.Investigation {
	inv := &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			Labels:            map[string]string{v1alpha1.LabelFinding: finding},
			CreationTimestamp: metav1.NewTime(testClock.Add(-time.Hour)),
		},
		Spec: v1alpha1.InvestigationSpec{FindingRef: v1alpha1.ObjectReference{Name: finding}, Attempt: 1},
		Status: v1alpha1.InvestigationStatus{
			Recommendation: v1alpha1.RecommendationRemediate,
			Confidence:     "0.9",
			Report:         "---\nverdict: remediate\n---\n\n# Analysis\n\nUnsanitised input.",
			Stage: &v1alpha1.StageResult{Outcome: "ok", Model: "m", NumTurns: 3,
				Usage: v1alpha1.UsageSummary{InputTokens: 100, OutputTokens: 20, CostUSD: "0.25"}},
		},
	}
	for _, m := range mutate {
		m(inv)
	}
	return inv
}

// testRem builds a remediation of fnd-1.
func testRem(name string, mutate ...func(*v1alpha1.Remediation)) *v1alpha1.Remediation {
	const finding = "fnd-1"
	rem := &v1alpha1.Remediation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			Labels:            map[string]string{v1alpha1.LabelFinding: finding},
			CreationTimestamp: metav1.NewTime(testClock.Add(-time.Hour)),
		},
		Spec: v1alpha1.RemediationSpec{FindingRef: v1alpha1.ObjectReference{Name: finding}, Attempt: 1},
		Status: v1alpha1.RemediationStatus{
			Success: true,
			Branch:  "patchy/fix",
			Report:  "# Fix\n\nEscaped the argument.",
			Stage: &v1alpha1.StageResult{Outcome: "ok", Model: "m", NumTurns: 5,
				Usage: v1alpha1.UsageSummary{InputTokens: 300, OutputTokens: 40, CostUSD: "0.5"}},
		},
	}
	for _, m := range mutate {
		m(rem)
	}
	return rem
}

func withPR(rem *v1alpha1.Remediation) {
	rem.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 9, URL: "https://github.test/acme/orders/pull/9"}
}

func TestBrowsePrintURL(t *testing.T) {
	repo := describeRepository("u", nil)
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{"finding opens its issue", []string{"finding", "fnd-1"}, issueURL, ""},
		{"investigation opens its finding's issue", []string{"inv", "fnd-1-inv-1"}, issueURL, ""},
		{"remediation opens its pull request", []string{"rem", "fnd-1-rem-1"}, "https://github.test/acme/orders/pull/9", ""},
		{"remediation without a PR falls back to the issue", []string{"rem", "fnd-1-rem-2"}, issueURL, ""},
		{"repository opens its URL", []string{"repo", "fnd-1-src"}, "https://github.com/acme/orders", ""},
		{"untracked finding has nothing to open", []string{"finding", "fnd-2"}, "", "has no URL to open yet"},
		{"config kinds have no page", []string{"forge", "gh"}, "", "nothing to open"},
		{"missing object", []string{"finding", "absent"}, "", "not found"},
		{"orphaned investigation", []string{"inv", "orphan-inv-1"}, "", "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t,
				testFinding("fnd-1", v1alpha1.PhaseInReview, tracked),
				testFinding("fnd-2", v1alpha1.PhaseOpened),
				testInv("fnd-1-inv-1", "fnd-1"),
				testInv("orphan-inv-1", "gone"),
				testRem("fnd-1-rem-1", withPR),
				testRem("fnd-1-rem-2"),
				repo,
				&v1alpha1.Forge{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: testNamespace}},
			)
			err := h.execRoot(t, append([]string{"browse"}, append(tc.args, "--print-url")...)...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if h.out.Len() != 0 {
					t.Errorf("a failed browse printed %q", h.out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("browse: %v", err)
			}
			if got := strings.TrimSpace(h.out.String()); got != tc.want {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrowseUnknownNounIsUsage(t *testing.T) {
	h := newHarness(t)
	err := h.execRoot(t, "browse", "widget", "x", "--print-url")
	if err == nil || exitCode(err) != ExitUsage {
		t.Fatalf("err = %v (exit %d), want a usage error", err, exitCode(err))
	}
}

func TestReviewFindingShowsBothStages(t *testing.T) {
	fnd := testFinding("fnd-1", v1alpha1.PhaseInReview, tracked, func(f *v1alpha1.Finding) {
		f.Status.Investigation = &v1alpha1.InvestigationSummary{Name: "fnd-1-inv-1", Attempt: 1}
		f.Status.Remediation = &v1alpha1.RemediationSummary{Name: "fnd-1-rem-1", Attempt: 1}
	})
	h := newHarness(t, fnd, testInv("fnd-1-inv-1", "fnd-1"), testRem("fnd-1-rem-1", withPR))
	if err := h.execRoot(t, "review", "finding", "fnd-1", "-o", "markdown"); err != nil {
		t.Fatalf("review: %v", err)
	}
	got := h.out.String()
	for _, want := range []string{
		"## Finding fnd-1",
		"- **Issue:** " + issueURL,
		"## Investigation fnd-1-inv-1 (attempt 1)",
		"- **Verdict:** remediate",
		"# Analysis",
		"## Remediation fnd-1-rem-1 (attempt 1)",
		"- **Pull request:** #9 https://github.test/acme/orders/pull/9",
		"Escaped the argument.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The frontmatter is a contract between stages, not prose.
	if strings.Contains(got, "verdict: remediate") {
		t.Errorf("frontmatter leaked without --raw:\n%s", got)
	}

	h.out.Reset()
	if err := h.execRoot(t, "review", "finding", "fnd-1", "-o", "markdown", "--raw"); err != nil {
		t.Fatalf("review --raw: %v", err)
	}
	if !strings.Contains(h.out.String(), "verdict: remediate") {
		t.Errorf("--raw dropped the frontmatter:\n%s", h.out.String())
	}
}

func TestReviewFindingWithoutRuns(t *testing.T) {
	// The summaries name children that have since been collected.
	fnd := testFinding("fnd-1", v1alpha1.PhaseQueued, func(f *v1alpha1.Finding) {
		f.Status.Investigation = &v1alpha1.InvestigationSummary{Name: "gone-inv-1"}
		f.Status.Remediation = &v1alpha1.RemediationSummary{Name: "gone-rem-1"}
	})
	h := newHarness(t, fnd)
	if err := h.execRoot(t, "review", "finding", "fnd-1"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if !strings.Contains(h.errOut.String(), "No agent has run on fnd-1 yet (phase Queued)") {
		t.Errorf("stderr = %q, want the no-run note", h.errOut.String())
	}
	if !strings.Contains(h.out.String(), "Finding fnd-1") {
		t.Errorf("stdout lost the finding summary:\n%s", h.out.String())
	}
}

func TestReviewRuns(t *testing.T) {
	fnd := testFinding("fnd-1", v1alpha1.PhaseInReview, tracked, func(f *v1alpha1.Finding) {
		f.Status.Investigation = &v1alpha1.InvestigationSummary{Name: "fnd-1-inv-2", Attempt: 2}
		f.Status.Remediation = &v1alpha1.RemediationSummary{Name: "fnd-1-rem-1", Attempt: 1}
	})
	objs := func() *harness {
		return newHarness(t, fnd.DeepCopy(),
			testInv("fnd-1-inv-1", "fnd-1"),
			testInv("fnd-1-inv-2", "fnd-1", func(i *v1alpha1.Investigation) {
				i.Spec.Attempt = 2
				i.Status.Recommendation = v1alpha1.RecommendationManual
			}),
			testRem("fnd-1-rem-1", withPR),
			testRem("fnd-1-rem-2"),
		)
	}
	cases := []struct {
		name    string
		args    []string
		want    []string
		wantErr string
	}{
		{"investigation by name", []string{"inv", "fnd-1-inv-1"},
			[]string{"Investigation fnd-1-inv-1 (attempt 1)", "remediate"}, ""},
		{"latest investigation by finding", []string{"inv", "--finding", "fnd-1"},
			[]string{"Investigation fnd-1-inv-2 (attempt 2)", "manual"}, ""},
		{"explicit attempt", []string{"inv", "--finding", "fnd-1", "--attempt", "1"},
			[]string{"Investigation fnd-1-inv-1 (attempt 1)"}, ""},
		{"remediation by finding", []string{"rem", "--finding", "fnd-1"},
			[]string{"Remediation fnd-1-rem-1 (attempt 1)", "Success:", "true"}, ""},
		{"investigation web url is the issue", []string{"inv", "fnd-1-inv-1", "--print-url"},
			[]string{issueURL}, ""},
		{"remediation web url is its PR", []string{"rem", "fnd-1-rem-1", "--print-url"},
			[]string{"https://github.test/acme/orders/pull/9"}, ""},
		{"remediation without a PR falls back to the issue", []string{"rem", "fnd-1-rem-2", "--print-url"},
			[]string{issueURL}, ""},
		{"finding web url", []string{"finding", "fnd-1", "--print-url"}, []string{issueURL}, ""},
		{"missing attempt", []string{"inv", "--finding", "fnd-1", "--attempt", "7"}, nil, "not found"},
		{"needs a name or --finding", []string{"inv"}, nil, "give a investigation name or --finding"},
		{"not reviewable", []string{"forge", "gh"}, nil, "cannot review forges"},
		{"unknown noun", []string{"widget", "x"}, nil, "widget"},
		{"bad output format", []string{"inv", "fnd-1-inv-1", "-o", "xml"}, nil, "unknown output format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := objs()
			err := h.execRoot(t, append([]string{"review"}, tc.args...)...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(h.out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, h.out.String())
				}
			}
		})
	}
}

func TestReviewRunURLNeedsTheFinding(t *testing.T) {
	// The investigation's finding is gone, so there is no issue to fall back to.
	h := newHarness(t, testInv("orphan-inv-1", "gone"))
	err := h.execRoot(t, "review", "inv", "orphan-inv-1", "--print-url")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the missing finding reported", err)
	}
}

func TestOpenURLRefusesEmpty(t *testing.T) {
	h := newHarness(t)
	err := openURL(h.opts, &reviewFlags{}, "", "finding x")
	if err == nil || err.Error() != "finding x has no URL to open yet" {
		t.Fatalf("err = %v", err)
	}
}

// TestOpenURLValidatesBeforeLaunching: without --print-url the URL goes to the
// platform launcher, which must never see a non-http(s) value.
func TestOpenURLValidatesBeforeLaunching(t *testing.T) {
	h := newHarness(t)
	h.opts.Verbose = true
	err := openURL(h.opts, &reviewFlags{}, "file:///etc/passwd", "finding x")
	if err == nil || !strings.Contains(err.Error(), "not an http(s) URL") {
		t.Fatalf("err = %v, want the scheme refusal", err)
	}
	if !strings.Contains(h.errOut.String(), "patchy: opening file:///etc/passwd") {
		t.Errorf("--verbose did not narrate: %q", h.errOut.String())
	}
}

func TestDescribeViews(t *testing.T) {
	withUID := func(f *v1alpha1.Finding) { f.UID = "fnd-uid-1" }
	h := newHarness(t,
		testFinding("fnd-1", v1alpha1.PhaseInReview, withUID, tracked),
		testInv("fnd-1-inv-1", "fnd-1"),
		testRem("fnd-1-rem-1", withPR),
		// A child with an unparseable cost still counts its tokens.
		testRem("fnd-1-rem-2", func(r *v1alpha1.Remediation) { r.Status.Stage.Usage.CostUSD = "n/a" }),
		testRem("fnd-1-rem-3", func(r *v1alpha1.Remediation) { r.Status.Stage = nil }),
		&v1alpha1.Forge{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: testNamespace},
			Spec: v1alpha1.ForgeSpec{Provider: "github", BaseURL: "https://github.test"}},
	)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"finding", "fnd-1"}, []string{
			"Finding fnd-1",
			// 100+300+300 in, 20+40+40 out, 0.25+0.5 dollars, across 4 runs.
			"700 in / 100 out tokens, $0.7500 across 4 runs",
		}},
		{[]string{"inv", "fnd-1-inv-1"}, []string{"Investigation fnd-1-inv-1", "Verdict:", "remediate"}},
		{[]string{"rem", "fnd-1-rem-1"}, []string{"Remediation fnd-1-rem-1", "patchy/fix", "#9"}},
		{[]string{"forge", "gh", "-o", "yaml"}, []string{"baseURL: https://github.test"}},
		{[]string{"finding", "fnd-1", "-o", "json"}, []string{`"name": "fnd-1"`, `"phase": "InReview"`}},
		{[]string{"finding", "fnd-1", "-o", "name"}, []string{"findings.patchy.bitwisemedia.uk/fnd-1"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			h.out.Reset()
			if err := h.execRoot(t, append([]string{"describe"}, tc.args...)...); err != nil {
				t.Fatalf("describe: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(h.out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, h.out.String())
				}
			}
		})
	}
}

func TestDescribeFindingWithoutSpend(t *testing.T) {
	h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseOpened))
	if err := h.execRoot(t, "describe", "finding", "fnd-1", "-o", "markdown"); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if strings.Contains(h.out.String(), "Spend") {
		t.Errorf("a finding with no runs shows spend:\n%s", h.out.String())
	}
}

func TestDescribeErrors(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"describe", "widget", "x"}, "widget"},
		{[]string{"describe", "finding", "absent"}, "not found"},
		{[]string{"describe", "finding", "x", "-o", "xml"}, "unknown output format"},
	}
	for _, tc := range cases {
		err := h.execRoot(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestDescribeListFailureIsNotFatal(t *testing.T) {
	h := newHarness(t, testFinding("fnd-1", v1alpha1.PhaseOpened))
	h.opts.Verbose = true
	failing := &failingListClient{Client: h.client, err: errors.New("list refused")}
	h.opts.env.Client = failing
	if err := h.execRoot(t, "describe", "finding", "fnd-1", "-v"); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(h.out.String(), "Finding fnd-1") {
		t.Errorf("finding not rendered:\n%s", h.out.String())
	}
	for _, want := range []string{"could not total spend for fnd-1: list refused",
		"could not read the runner image of fnd-1: list refused"} {
		if !strings.Contains(h.errOut.String(), want) {
			t.Errorf("stderr missing %q: %q", want, h.errOut.String())
		}
	}
}

// failingListClient fails every List, as a cluster that grants get but not
// list does.
type failingListClient struct {
	client.Client
	err error
}

func (f *failingListClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return f.err
}
