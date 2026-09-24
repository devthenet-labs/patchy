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
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// fakeCommits is a CommitGraph over a fixed parent map and branch heads.
type fakeCommits struct {
	parents map[string]string
	heads   map[string]string // branch name -> head commit
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

func (f *fakeCommits) Contains(
	_ context.Context, _ *v1alpha1.Integration, _ source.Repo, branch, commit string,
) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	head, ok := f.heads[branch]
	if !ok {
		return false, errors.New("no such branch " + branch)
	}
	for c := head; c != ""; c = f.parents[c] {
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
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://github.com/devthenet-labs/patchy-target",
				Name: "devthenet-labs/patchy-target",
			},
			Advisories: []string{"CWE-79", "CWE-116"},
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

func newStaleHarness(t *testing.T, commits *fakeCommits, opts ...func(*fake.ClientBuilder)) *staleHarness {
	t.Helper()
	fnd := remediatedFix()
	b := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(fnd, liveIntegration()).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Integration{}).
		WithIndex(&v1alpha1.Finding{}, KeyHashIndex, KeyHashIndexer)
	for _, o := range opts {
		o(b)
	}
	c := b.Build()
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
		mainAt      string // main's head; default laterMerge
		payload     string
		wantCreated string // the successor generation, or "" for none
		wantLookups int
		// wantRecorded: the skipped reopen is kept on the remediated
		// generation for the re-check.
		wantRecorded bool
		wantLog      string
	}{
		{
			name:         "recorded stale reopen at the fix's parent is skipped",
			payload:      stale,
			wantLookups:  2,
			wantRecorded: true,
			wantLog:      `msg="stale alert observation skipped" finding=finding-1678e4a376-2 alert=9 commit=` + base,
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
			// Undoing the merge by force-pushing main back to the fix's parent
			// leaves the merge commit resolvable (GitHub keeps it), so the
			// reopen at the parent still precedes it — but the fix is no longer
			// on main, and the vulnerable code is.
			name:        "reopen after main was reset to before the fix opens a successor",
			mainAt:      base,
			payload:     stale,
			wantCreated: "finding-1678e4a376-3",
			wantLookups: 2,
		},
		{
			name: "reopen whose delivery names no branch fails open",
			payload: strings.NewReplacer(`"ref": "refs/heads/main",`, ``,
				`"ref": "refs/heads/main"`, `"ref": ""`).Replace(stale),
			wantCreated: "finding-1678e4a376-3",
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
			mainAt := tc.mainAt
			if mainAt == "" {
				mainAt = laterMerge
			}
			h := newStaleHarness(t, &fakeCommits{
				parents: history, heads: map[string]string{"main": mainAt}, err: tc.commitsErr,
			})
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
			var wantObs []v1alpha1.StaleObservation
			if tc.wantRecorded {
				wantObs = []v1alpha1.StaleObservation{{
					AlertID: "9", Commit: base, Ref: "refs/heads/main", ObservedAt: metav1.NewTime(testClock),
				}}
			}
			if got := get(t, h.client, "finding-1678e4a376-2").Status.StaleObservations; !staleObsEqual(got, wantObs) {
				t.Errorf("stale observations = %+v, want %+v", got, wantObs)
			}
			if tc.wantLog != "" && !strings.Contains(h.logs.String(), tc.wantLog) {
				t.Errorf("logs missing %q:\n%s", tc.wantLog, h.logs.String())
			}
		})
	}
}

// staleObsEqual compares stale observations, timestamps to the second (the
// API server's precision).
func staleObsEqual(a, b []v1alpha1.StaleObservation) bool {
	return slices.EqualFunc(a, b, func(x, y v1alpha1.StaleObservation) bool {
		return x.AlertID == y.AlertID && x.Commit == y.Commit && x.Ref == y.Ref &&
			x.ObservedAt.Unix() == y.ObservedAt.Unix()
	})
}

// staleAlert is alert 9 as the API reports it once a later analysis has
// moved it: open (unless edited), latest instance at commit on ref.
func staleAlert(commit, ref string) *ghclient.Alert {
	a, _ := fakeAlertAPI{}.GetAlert(context.Background(), ghclient.Repo{}, 9)
	a.MostRecentSHA, a.MostRecentRef = commit, ref
	return a
}

// TestStaleObservationRecheck covers what a skipped stale reopen leaves
// behind: alert 9 stays open on GitHub, and GitHub sends code_scanning_alert
// events only on state changes. When the next analysis of main is of a
// commit that brings the vulnerable code back, the alert is already open, so
// no event arrives — the projection reconciler has to notice on its own by
// re-reading the alert, and open the successor generation then.
func TestStaleObservationRecheck(t *testing.T) {
	const interval = 5 * time.Minute
	cases := []struct {
		name        string
		mainAt      string              // main's head when the re-check runs
		alert       *ghclient.Alert     // what GetAlert answers; nil is a 404
		edit        func(*staleHarness) // cluster changes before the re-check
		wantCreated string              // the successor generation, or ""
		wantPending bool                // the observation is still recorded
		wantAfter   time.Duration       // the requeue
		check       func(*testing.T, *staleHarness)
	}{
		{
			name:        "alert silently regressed on main opens the successor",
			mainAt:      regression,
			alert:       staleAlert(regression, "refs/heads/main"),
			wantCreated: "finding-1678e4a376-3",
		},
		{
			name:        "alert still at the stale commit stays pending",
			mainAt:      laterMerge,
			alert:       staleAlert(base, "refs/heads/main"),
			wantPending: true,
			wantAfter:   interval,
		},
		{
			// The force-push case once more, but with the alert already open:
			// the analysis of the reset head changes nothing GitHub reports,
			// so only the re-check can see that the fix left main.
			name:        "alert still at the stale commit after main was reset opens the successor",
			mainAt:      base,
			alert:       staleAlert(base, "refs/heads/main"),
			wantCreated: "finding-1678e4a376-3",
		},
		{
			name:   "alert fixed on main settles",
			mainAt: laterMerge,
			alert: func() *ghclient.Alert {
				a := staleAlert(laterMerge, "refs/heads/main")
				a.State = "fixed"
				return a
			}(),
		},
		{
			name:   "alert dismissed settles",
			mainAt: laterMerge,
			alert: func() *ghclient.Alert {
				a := staleAlert(base, "refs/heads/main")
				a.State = "dismissed"
				return a
			}(),
		},
		{
			name:   "alert gone settles",
			mainAt: laterMerge,
		},
		{
			name:        "latest instance on a pull-request ref says nothing about main",
			mainAt:      laterMerge,
			alert:       staleAlert("0000000000000000000000000000000000000002", "refs/pull/21/merge"),
			wantPending: true,
			wantAfter:   interval,
		},
		{
			name:   "a newer generation carrying the alert settles it without ingesting",
			mainAt: regression,
			alert:  staleAlert(regression, "refs/heads/main"),
			edit: func(h *staleHarness) {
				h.scan(t, strings.ReplaceAll(fixtureFile(t, "code_scanning_alert.reopened.stale.json"), base, regression))
			},
			check: func(t *testing.T, h *staleHarness) {
				if n := len(listFindings(t, h.client)); n != 2 {
					t.Errorf("findings = %d, want the remediated one and the webhook's successor only", n)
				}
			},
		},
		{
			// The observation is recorded and will be read again; a transient
			// failure must not fail open into the duplicate the check exists
			// to prevent.
			name:   "ancestry lookup failure keeps the observation pending",
			mainAt: laterMerge,
			alert:  staleAlert(base, "refs/heads/main"),
			edit: func(h *staleHarness) {
				h.commits.err = errors.New("compare: 502")
			},
			wantPending: true,
			wantAfter:   interval,
		},
		{
			name:   "suspended integration looks again later",
			mainAt: regression,
			alert:  staleAlert(regression, "refs/heads/main"),
			edit: func(h *staleHarness) {
				var integ v1alpha1.Integration
				key := types.NamespacedName{Namespace: "patchy", Name: "github"}
				if err := h.client.Get(t.Context(), key, &integ); err != nil {
					t.Fatalf("get integration: %v", err)
				}
				integ.Spec.Suspend = true
				if err := h.client.Update(t.Context(), &integ); err != nil {
					t.Fatalf("suspend integration: %v", err)
				}
			},
			wantPending: true,
			wantAfter:   interval,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commits := &fakeCommits{parents: history, heads: map[string]string{"main": laterMerge}}
			h := newStaleHarness(t, commits)
			h.merge(t, nil)
			h.scan(t, fixtureFile(t, "code_scanning_alert.reopened.stale.json"))
			if n := len(listFindings(t, h.client)); n != 1 {
				t.Fatalf("findings = %d after the stale reopen, want only the remediated one", n)
			}
			recorded := get(t, h.client, "finding-1678e4a376-2").Status.StaleObservations
			if tc.edit != nil {
				tc.edit(h)
			}

			// The next analysis of main: no webhook, only the alert moving.
			commits.heads["main"] = tc.mainAt
			tracker := newFakeTracker()
			tracker.alerts = map[int]*ghclient.Alert{}
			if tc.alert != nil {
				tracker.alerts[9] = tc.alert
			}
			r := &FindingReconciler{
				Client: h.client, Namespace: "patchy", Now: func() time.Time { return testClock },
				ClientFor: func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
					return tracker, nil
				},
				Ingest: h.receiver.Ingest, StaleRecheck: interval, Log: h.receiver.Log,
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-1678e4a376-2"}}
			res, err := r.Reconcile(t.Context(), req)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if tc.wantCreated != "" {
				succ := get(t, h.client, tc.wantCreated)
				if len(succ.Spec.Related) != 1 || succ.Spec.Related[0].To != "finding-1678e4a376-2" {
					t.Errorf("successor related = %+v, want successor-of finding-1678e4a376-2", succ.Spec.Related)
				}
			} else if tc.check == nil {
				if n := len(listFindings(t, h.client)); n != 1 {
					t.Errorf("findings = %d, want only the remediated one", n)
				}
			}
			if tc.check != nil {
				tc.check(t, h)
			}
			var wantObs []v1alpha1.StaleObservation
			if tc.wantPending {
				wantObs = recorded
			}
			if got := get(t, h.client, "finding-1678e4a376-2").Status.StaleObservations; !staleObsEqual(got, wantObs) {
				t.Errorf("stale observations = %+v, want %+v", got, wantObs)
			}
			if res.RequeueAfter != tc.wantAfter {
				t.Errorf("requeue after = %v, want %v", res.RequeueAfter, tc.wantAfter)
			}
		})
	}
}

// TestStaleObservationUnrecordedFailsOpen: a skip that cannot be recorded
// could never be re-checked, so it is not taken — the reopen ingests as it
// did before the stale check existed.
func TestStaleObservationUnrecordedFailsOpen(t *testing.T) {
	h := newStaleHarness(t, &fakeCommits{parents: history, heads: map[string]string{"main": laterMerge}},
		func(b *fake.ClientBuilder) {
			b.WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
					opts ...client.SubResourceUpdateOption) error {
					if f, ok := obj.(*v1alpha1.Finding); ok && len(f.Status.StaleObservations) > 0 {
						return errors.New("etcd unavailable")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})
		})
	h.merge(t, nil)
	h.scan(t, fixtureFile(t, "code_scanning_alert.reopened.stale.json"))

	get(t, h.client, "finding-1678e4a376-3")
	if want := `msg="stale alert observation not recorded; ingesting"`; !strings.Contains(h.logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, h.logs.String())
	}
}
