// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/webhook"
)

// trackedFinding is a Finding in the given phase whose tracking issue URL is
// linked, so the URL index resolves it.
func trackedFinding(phase v1alpha1.Phase) *v1alpha1.Finding {
	const name = "finding-aa-1"
	return &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"},
			Source:         "ghas",
			Advisories:     []string{"CVE-2026-0001"},
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://github.com/acme/orders",
				Name: "acme/orders",
			},
		},
		Status: v1alpha1.FindingStatus{
			Phase: phase,
			Tracking: &v1alpha1.TrackingStatus{
				Integration: "gh",
				IssueNumber: 7,
				URL:         "https://github.com/acme/orders/issues/7",
				State:       "open",
			},
		},
	}
}

func newSignals(t *testing.T, objs ...client.Object) (*Signals, client.Client) {
	t.Helper()
	return newSignalsWith(t, nil, objs...)
}

// newSignalsWith is newSignals over a fake client configure adjusts; nil
// adjusts nothing.
func newSignalsWith(
	t *testing.T, configure func(*fake.ClientBuilder), objs ...client.Object,
) (*Signals, client.Client) {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithIndex(&v1alpha1.Finding{}, TrackingURLIndex, func(obj client.Object) []string {
			f := obj.(*v1alpha1.Finding)
			if f.Status.Tracking == nil {
				return nil
			}
			return []string{f.Status.Tracking.URL}
		})
	if configure != nil {
		configure(b)
	}
	c := b.Build()
	return &Signals{
		Client:    c,
		Namespace: "patchy",
		Now:       func() time.Time { return testClock },
	}, c
}

func event(typ, payload string) webhook.Event {
	return webhook.Event{Type: typ, Payload: []byte(payload)}
}

func TestSignalsIssueClosedHandsOff(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Phase != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff", f.Status.Phase)
	}
	if f.Status.Tracking.State != "closed" {
		t.Errorf("tracking state = %q, want closed", f.Status.Tracking.State)
	}
}

func TestSignalsIssueClosedTerminalNoop(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseRemediated))
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseRemediated {
		t.Errorf("phase = %q, want Remediated unchanged", got)
	}
}

func TestSignalsReopenAfterDismissal(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseDismissed))
	payload := `{"action":"reopened","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := get(t, c, "finding-aa-1").Status.Phase; got != v1alpha1.PhaseHandedOff {
		t.Errorf("phase = %q, want HandedOff (edge 19)", got)
	}
}

// inReview is trackedFinding in review of its recorded remediation PR,
// acme/orders#11.
func inReview() *v1alpha1.Finding {
	fnd := trackedFinding(v1alpha1.PhaseInReview)
	fnd.Status.PullRequest = &v1alpha1.PullRequestStatus{
		Number: 11, URL: "https://github.com/acme/orders/pull/11", State: "open",
	}
	return fnd
}

// prClosed is a pull_request.closed delivery for PR number in repo, from the
// finding's remediation branch (patchy/finding-aa-1) in headRepo.
func prClosed(repo string, number int, headRepo string, merged bool) string {
	return fmt.Sprintf(
		`{"action":"closed","pull_request":{"number":%d,"merged":%v,`+
			`"merged_at":"2026-07-21T13:00:00Z","merge_commit_sha":"fa82fcdc7efab2777d432ba3385517fa735e0ae0",`+
			`"head":{"ref":"patchy/finding-aa-1","repo":{"full_name":%q}},"base":{"ref":"main","repo":{"full_name":%q}}},`+
			`"repository":{"full_name":%q}}`,
		number, merged, headRepo, repo, repo)
}

func TestSignalsPullRequest(t *testing.T) {
	cases := []struct {
		name      string
		merged    bool
		wantPhase v1alpha1.Phase
		wantState string
	}{
		{"merged remediates", true, v1alpha1.PhaseRemediated, "merged"},
		{"closed unmerged fails", false, v1alpha1.PhaseFailed, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			payload := prClosed("acme/orders", 11, "acme/orders", tc.merged)
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if f.Status.PullRequest.State != tc.wantState {
				t.Errorf("pr state = %q, want %q", f.Status.PullRequest.State, tc.wantState)
			}
			if tc.merged && (f.Status.CompletedAt == nil || f.Status.PullRequest.MergedAt == nil) {
				t.Error("merged PR left completedAt/mergedAt unset")
			}
			// The merge commit is what ingest measures later scanner
			// observations against; an unmerged close records none.
			wantSHA := ""
			if tc.merged {
				wantSHA = "fa82fcdc7efab2777d432ba3385517fa735e0ae0"
			}
			if got := f.Status.PullRequest.MergeCommitSHA; got != wantSHA {
				t.Errorf("mergeCommitSHA = %q, want %q", got, wantSHA)
			}
		})
	}
}

// TestSignalsPullRequestNotRecorded: the head ref names the finding, but
// anyone who can open a pull request can name a branch patchy/<finding>. A
// close settles the finding only when it is the recorded remediation PR:
// the same number, in the finding's repository, from a branch there. One
// that differs only in its repository — a rename or transfer looks so — is
// kept pending for the recorded PR's own state to settle; the rest are
// ignored outright.
func TestSignalsPullRequestNotRecorded(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantPhase   v1alpha1.Phase
		wantPending bool
	}{
		{"recorded PR merges", prClosed("acme/orders", 11, "acme/orders", true), v1alpha1.PhaseRemediated, false},
		{"repository matches case-insensitively", prClosed("Acme/Orders", 11, "Acme/Orders", true),
			v1alpha1.PhaseRemediated, false},
		{"another repository", prClosed("acme/billing", 11, "acme/billing", true), v1alpha1.PhaseInReview, true},
		{"another number", prClosed("acme/orders", 12, "acme/orders", true), v1alpha1.PhaseInReview, false},
		{"another number closed unmerged", prClosed("acme/orders", 12, "acme/orders", false),
			v1alpha1.PhaseInReview, false},
		{"from a fork", prClosed("acme/orders", 11, "mallory/orders", true), v1alpha1.PhaseInReview, false},
		{"from a deleted fork", prClosed("acme/orders", 11, "", false), v1alpha1.PhaseInReview, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newSignals(t, inReview())
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", tc.payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			f := get(t, c, "finding-aa-1")
			if f.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tc.wantPhase)
			}
			if tc.wantPhase == v1alpha1.PhaseInReview && f.Status.PullRequest.State != "open" {
				t.Errorf("pr state = %q, want open (an unsettled close records nothing)", f.Status.PullRequest.State)
			}
			pending := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionReviewClosePending)
			switch {
			case tc.wantPending && (pending == nil || pending.Reason != v1alpha1.ReasonUnrecordedRepository):
				t.Errorf("pending close = %+v, want %s", pending, v1alpha1.ReasonUnrecordedRepository)
			case !tc.wantPending && pending != nil:
				t.Errorf("pending close = %+v, want none", pending)
			}
		})
	}
}

// TestSignalsPullRequestRepositoryFallback: a record without a URL is
// matched against the finding's own repository, where remediation opens
// every PR.
func TestSignalsPullRequestRepositoryFallback(t *testing.T) {
	cases := []struct {
		name      string
		repo      string
		wantPhase v1alpha1.Phase
	}{
		{"finding's repository", "acme/orders", v1alpha1.PhaseRemediated},
		{"another repository", "acme/billing", v1alpha1.PhaseInReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fnd := inReview()
			fnd.Status.PullRequest.URL = ""
			s, c := newSignals(t, fnd)
			payload := prClosed(tc.repo, 11, tc.repo, true)
			if err := s.Handle(t.Context(), testIntegration(), event("pull_request", payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := get(t, c, "finding-aa-1").Status.Phase; got != tc.wantPhase {
				t.Errorf("phase = %q, want %q", got, tc.wantPhase)
			}
		})
	}
}

func TestSignalsForeignIssueIgnored(t *testing.T) {
	s, _ := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
	payload := `{"action":"closed","issue":{"number":99,"html_url":"https://github.com/acme/other/issues/99"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// staleCache stands in for an informer cache that lags the Finding's latest
// write for longer than a handler's retries last: every Get of the Finding
// returns the version it held when the cache fell behind. Every other read
// passes through.
type staleCache struct {
	client.Client
	snapshot *v1alpha1.Finding
}

func (s *staleCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if f, ok := obj.(*v1alpha1.Finding); ok && key == client.ObjectKeyFromObject(s.snapshot) {
		s.snapshot.DeepCopyInto(f)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// TestSignalsWriteReadsPastTheCache: a delivery is answered before it is
// handled, so the handler's write is the only record of what it carried. A
// cache still showing the Finding as it was before the projection's last
// write must not make every retry conflict and lose the command: each
// attempt re-reads the Finding through the APIReader.
func TestSignalsWriteReadsPastTheCache(t *testing.T) {
	s, c := newSignals(t, trackedFinding(v1alpha1.PhaseQueued), testIntegration())
	before := get(t, c, "finding-aa-1")
	settled := before.DeepCopy()
	settled.Status.Commands = &v1alpha1.FindingCommands{Consumed: []int64{40}} // the projection's last write
	if err := c.Status().Update(t.Context(), settled); err != nil {
		t.Fatalf("status update: %v", err)
	}
	s.Client, s.APIReader = &staleCache{Client: c, snapshot: before}, c

	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy suspend"))
	f := get(t, c, "finding-aa-1")
	if pendingCommand(f, 41) == nil || !slices.Contains(f.Status.Commands.Consumed, 40) {
		t.Errorf("commands = %+v, want 41 recorded beside the projection's write", f.Status.Commands)
	}
}

// TestSignalsOutlastConflictBurst: while a finding's commands settle, the
// projection writes its status several times per command and the webhook
// workers write it for each delivery, so a handler's write can conflict
// several times in a row; one that runs out of retries loses its command
// for good. The handler outlasts more conflicts in a row than client-go's
// DefaultRetry allows.
func TestSignalsOutlastConflictBurst(t *testing.T) {
	conflicts := retry.DefaultRetry.Steps
	s, c := newSignalsWith(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if conflicts > 0 {
					conflicts--
					return kerrors.NewConflict(v1alpha1.GroupVersion.WithResource("findings").GroupResource(),
						obj.GetName(), errors.New("the object has been modified"))
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		})
	}, trackedFinding(v1alpha1.PhaseQueued), testIntegration())

	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy suspend"))
	if pendingCommand(get(t, c, "finding-aa-1"), 41) == nil {
		t.Errorf("commands = %+v, want 41 recorded after %d conflicts",
			get(t, c, "finding-aa-1").Status.Commands, retry.DefaultRetry.Steps)
	}
}
