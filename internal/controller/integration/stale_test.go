// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghas"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/webhook"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// patchy-target's main, oldest first, as it stood when alerts 7 and 9 were
// reopened: #19 merged as base, then #16 (the alert-9 fix) and #14 (the
// alert-7 fix) squash-merged on top of it, then #9. regression stands in
// for a later commit that brings the vulnerable code back.
const (
	base       = "45b1bec980a1aba44367aa7bf871e8b658776d40"
	fixMerge   = "fa82fcdc7efab2777d432ba3385517fa735e0ae0"
	laterMerge = "f4ce2c98005506f003d9d9ff981dea0db4a4b513"
	regression = "3fbab3d1e9b360ca26d757f4823887ebd090f3c1"
	unrelated  = "0000000000000000000000000000000000000001"
)

// history is the linear main above: each commit mapped to its parent.
var history = map[string]string{
	fixMerge:   base,
	laterMerge: fixMerge,
	regression: laterMerge,
}

// fakeCommits is a CommitGraph over a fixed parent map.
type fakeCommits struct {
	parents map[string]string
	err     error
	calls   int
}

func (f *fakeCommits) Precedes(
	_ context.Context, _ *v1alpha1.Integration, _ source.Repo, commit, descendant string,
) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	for c := f.parents[descendant]; c != ""; c = f.parents[c] {
		if c == commit {
			return true, nil
		}
	}
	return false, nil
}

// fakeAlertAPI serves the code-scanning alert the ghas handler fetches after
// a delivery — alert 9 as the API returned it after the reopen.
type fakeAlertAPI struct{}

func (fakeAlertAPI) GetAlert(_ context.Context, _ ghclient.Repo, number int) (*ghclient.Alert, error) {
	return &ghclient.Alert{
		Number:          number,
		RuleID:          "go/reflected-xss",
		RuleName:        "go/reflected-xss",
		RuleDescription: "Reflected cross-site scripting",
		RuleHelp:        "# Reflected cross-site scripting",
		Tags:            []string{"external/cwe/cwe-079", "external/cwe/cwe-116", "security"},
		Severity:        "high",
		State:           "open",
		HTMLURL:         "https://github.com/devthenet-labs/patchy-target/security/code-scanning/9",
		Path:            "echo.go",
		StartLine:       16,
		EndLine:         16,
		MostRecentSHA:   base,
	}, nil
}

// liveIntegration mirrors the live code-scanning Integration: its name is
// part of the family key, so the recorded deliveries resolve to the recorded
// finding names (finding-1678e4a376-*).
func liveIntegration() *v1alpha1.Integration {
	integ := testIntegration()
	integ.Name = "github"
	return integ
}

// remediatedFix is finding-1678e4a376-2 as it stood the moment PR #16 was
// merged: in review, its PR open.
func remediatedFix() *v1alpha1.Finding {
	return &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finding-1678e4a376-2", Namespace: "patchy",
			Labels: map[string]string{v1alpha1.LabelKeyHash: "1678e4a376"},
		},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "github"},
			Source:         "ghas",
			Advisories:     []string{"CWE-79", "CWE-116"},
			Alerts: []v1alpha1.Alert{{
				ID: "9", Source: "ghas",
				URL: "https://github.com/devthenet-labs/patchy-target/security/code-scanning/9",
			}},
		},
		Status: v1alpha1.FindingStatus{
			Phase:       v1alpha1.PhaseInReview,
			PullRequest: &v1alpha1.PullRequestStatus{Number: 16, State: "open"},
		},
	}
}

// staleHarness is the integration-controller's GitHub path over a fake
// cluster: Signals for the PR webhook, the Receiver's ingest for the
// code-scanning one, with the ghas handler in between.
type staleHarness struct {
	receiver *Receiver
	client   client.Client
	commits  *fakeCommits
	logs     *bytes.Buffer
}

func newStaleHarness(t *testing.T, commits *fakeCommits) *staleHarness {
	t.Helper()
	fnd := remediatedFix()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(fnd).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithIndex(&v1alpha1.Finding{}, KeyHashIndex, KeyHashIndexer).
		Build()
	// The fake drops status on create; write it the way the pipeline did.
	fnd.Status = remediatedFix().Status
	if err := c.Status().Update(t.Context(), fnd); err != nil {
		t.Fatalf("seed finding status: %v", err)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	return &staleHarness{
		receiver: &Receiver{
			Reader:    c,
			Namespace: "patchy",
			Ingest: &Ingestor{
				Client: c, Namespace: "patchy", Now: func() time.Time { return testClock },
				Log: log, Commits: commits,
			},
			Signals: &Signals{Client: c, Namespace: "patchy", Now: func() time.Time { return testClock }},
			Log:     log,
		},
		client:  c,
		commits: commits,
		logs:    logs,
	}
}

// merge delivers the PR webhook that merged #16, as recorded, optionally
// edited.
func (h *staleHarness) merge(t *testing.T, edit func(string) string) {
	t.Helper()
	payload := fixtureFile(t, "pull_request.closed.merged.json")
	if edit != nil {
		payload = edit(payload)
	}
	e := webhook.Event{Type: "pull_request", DeliveryID: "a412f700-b7a1-11f1-9f92-f9fb9e1e5841", Payload: []byte(payload)}
	if err := h.receiver.Signals.Handle(t.Context(), liveIntegration(), e); err != nil {
		t.Fatalf("deliver PR merge: %v", err)
	}
	if got := get(t, h.client, "finding-1678e4a376-2").Status.Phase; got != v1alpha1.PhaseRemediated {
		t.Fatalf("after the merge phase = %q, want Remediated", got)
	}
}

// scan delivers a code_scanning_alert webhook through the ghas handler.
func (h *staleHarness) scan(t *testing.T, payload string) {
	t.Helper()
	e := webhook.Event{Type: ghas.EventType, DeliveryID: "dcffb800-b7a1-11f1-8a23-2281daf6cda1", Payload: []byte(payload)}
	if err := h.receiver.ingestAll(t.Context(), liveIntegration(), ghas.New(fakeAlertAPI{}), e); err != nil {
		t.Fatalf("deliver code scanning alert: %v", err)
	}
}

func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// TestStaleReopenAfterMergedFix reproduces the duplicate finding
// finding-1678e4a376-5: PR #16 fixed alert 9 and merged as fa82fcd, then an
// out-of-order CodeQL analysis of fa82fcd's parent (45b1bec) reopened the
// alert. That reopen observes code the merged fix replaced and must not open
// a successor generation; a reopen at the fix or after it still must.
func TestStaleReopenAfterMergedFix(t *testing.T) {
	stale := fixtureFile(t, "code_scanning_alert.reopened.stale.json")
	reopenedAt := func(commit string) string { return strings.ReplaceAll(stale, base, commit) }
	noMergeCommit := func(p string) string { return strings.ReplaceAll(p, fixMerge, "") }

	cases := []struct {
		name        string
		mergeEdit   func(string) string
		commitsErr  error
		payload     string
		wantCreated string // the successor generation, or "" for none
		wantLookups int
		wantLog     string
	}{
		{
			name:        "recorded stale reopen at the fix's parent is skipped",
			payload:     stale,
			wantLookups: 1,
			wantLog:     `msg="stale alert observation skipped" finding=finding-1678e4a376-2 alert=9 commit=` + base,
		},
		{
			name:        "reopen at a later commit that brings the code back opens a successor",
			payload:     reopenedAt(regression),
			wantCreated: "finding-1678e4a376-3",
			wantLookups: 1,
			wantLog:     `msg="finding created" finding=finding-1678e4a376-3`,
		},
		{
			name:        "reopen at the merge commit itself opens a successor without a lookup",
			payload:     reopenedAt(fixMerge),
			wantCreated: "finding-1678e4a376-3",
		},
		{
			name:        "reopen on a history the fix is not in opens a successor",
			payload:     reopenedAt(unrelated),
			wantCreated: "finding-1678e4a376-3",
			wantLookups: 1,
		},
		{
			name:        "fix merged without a recorded merge commit fails open",
			mergeEdit:   noMergeCommit,
			payload:     stale,
			wantCreated: "finding-1678e4a376-3",
		},
		{
			name:        "ancestry lookup failure fails open",
			commitsErr:  errors.New("compare: 502"),
			payload:     stale,
			wantCreated: "finding-1678e4a376-3",
			wantLookups: 1,
			wantLog:     `msg="commit ancestry lookup failed; ingesting"`,
		},
		{
			name: "a new alert raised at an old commit still opens a finding",
			payload: strings.NewReplacer(`"action": "reopened"`, `"action": "created"`,
				`"number": 9,`, `"number": 13,`).Replace(stale),
			wantCreated: "finding-1678e4a376-3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newStaleHarness(t, &fakeCommits{parents: history, err: tc.commitsErr})
			h.merge(t, tc.mergeEdit)
			h.scan(t, tc.payload)

			items := listFindings(t, h.client)
			switch {
			case tc.wantCreated == "" && len(items) != 1:
				t.Errorf("findings = %d, want only the remediated one (no successor for a stale reopen)", len(items))
			case tc.wantCreated != "":
				f := get(t, h.client, tc.wantCreated)
				if f.Status.Phase != v1alpha1.PhaseOpened {
					t.Errorf("%s phase = %q, want Opened", tc.wantCreated, f.Status.Phase)
				}
				if len(f.Spec.Related) != 1 || f.Spec.Related[0].To != "finding-1678e4a376-2" {
					t.Errorf("%s related = %+v, want successor-of finding-1678e4a376-2", tc.wantCreated, f.Spec.Related)
				}
			}
			if h.commits.calls != tc.wantLookups {
				t.Errorf("ancestry lookups = %d, want %d", h.commits.calls, tc.wantLookups)
			}
			if tc.wantLog != "" && !strings.Contains(h.logs.String(), tc.wantLog) {
				t.Errorf("logs missing %q:\n%s", tc.wantLog, h.logs.String())
			}
		})
	}
}
