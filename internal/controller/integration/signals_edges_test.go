// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
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
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// A credential GitHub refuses the collaborator-permission lookup (403)
// fails closed: the command is refused, not applied and not retried.
func TestCommandRefusedWhenPermissionUnreadable(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionAdmin // what the lookup would have said
	tracker.permErrs = []error{forbidden("collaborator permission")}
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))

	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	assertRefused(t, tracker, f, 41, maintainer, "you may not use `/patchy approve` here")
	if f.Spec.Approval != nil {
		t.Errorf("approval = %+v, want none: an unreadable permission must not authorise", f.Spec.Approval)
	}
	if len(tracker.permReads) != 1 {
		t.Errorf("permission reads = %v, want exactly one (a 403 is an answer, not a transient)", tracker.permReads)
	}
}

// Malformed deliveries are errors; actions the pipeline does not act on are
// ignored without touching the finding.
func TestSignalsHandleEdges(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		payload string
		wantErr string
	}{
		{name: "malformed issues event", typ: "issues", payload: `{"action":`, wantErr: "decode issues event"},
		{name: "malformed comment event", typ: "issue_comment", payload: `[]`, wantErr: "decode issue_comment event"},
		{
			name: "malformed pull request event", typ: "pull_request", payload: `"x"`,
			wantErr: "decode pull_request event",
		},
		{
			name: "an issue label change", typ: "issues",
			payload: `{"action":"labeled","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`,
		},
		{
			name: "a deleted comment", typ: "issue_comment",
			payload: `{"action":"deleted",` +
				`"issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"},` +
				`"comment":{"id":41,"body":"/patchy approve"}}`,
		},
		{
			name: "a pull request opened", typ: "pull_request",
			payload: `{"action":"opened","pull_request":{"number":11,"head":{"ref":"patchy/finding-aa-1"}}}`,
		},
		{
			name: "a closed pull request off patchy's branches", typ: "pull_request",
			payload: `{"action":"closed","pull_request":{"number":11,"merged":true,"head":{"ref":"feature/x"}}}`,
		},
		{name: "an issue with no URL", typ: "issues", payload: `{"action":"closed","issue":{"number":7}}`},
		{name: "an event type patchy does not consume", typ: "push", payload: `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, c := newSignals(t, trackedFinding(v1alpha1.PhaseQueued))
			before := get(t, c, "finding-aa-1")
			// A nil Integration is legal: the approve alias falls back to
			// the parser's default.
			err := s.Handle(t.Context(), nil, event(tt.typ, tt.payload))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Handle() = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Handle() = %v, want nil", err)
			}
			after := get(t, c, "finding-aa-1")
			if after.Status.Phase != before.Status.Phase || after.Status.Commands != nil {
				t.Errorf("finding changed (phase %s -> %s, commands %+v); want untouched",
					before.Status.Phase, after.Status.Phase, after.Status.Commands)
			}
		})
	}
}

// A tracking-URL lookup failure is returned, so the delivery is counted as
// a handler error rather than silently dropped.
func TestSignalsLookupFailure(t *testing.T) {
	boom := errors.New("index unavailable")
	s, _ := newSignalsWith(t, func(b *fake.ClientBuilder) {
		b.WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
		})
	}, trackedFinding(v1alpha1.PhaseQueued))
	payload := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`
	if err := s.Handle(t.Context(), testIntegration(), event("issues", payload)); !errors.Is(err, boom) {
		t.Errorf("Handle() = %v, want the lookup error", err)
	}
}

func TestSameActor(t *testing.T) {
	tests := []struct {
		name string
		a, b v1alpha1.CommandActor
		want bool
	}{
		{
			name: "same id, renamed login", a: v1alpha1.CommandActor{Login: "old", ID: 7},
			b: v1alpha1.CommandActor{Login: "new", ID: 7}, want: true,
		},
		{
			name: "different ids, same login", a: v1alpha1.CommandActor{Login: "x", ID: 7},
			b: v1alpha1.CommandActor{Login: "x", ID: 8}, want: false,
		},
		{
			name: "no ids, logins compared without case", a: v1alpha1.CommandActor{Login: "Alice"},
			b: v1alpha1.CommandActor{Login: "alice"}, want: true,
		},
		{
			name: "one id missing falls back to login", a: v1alpha1.CommandActor{Login: "alice", ID: 7},
			b: v1alpha1.CommandActor{Login: "bob"}, want: false,
		},
	}
	for _, tt := range tests {
		if got := sameActor(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: sameActor = %v, want %v", tt.name, got, tt.want)
		}
		if got := sameActor(tt.b, tt.a); got != tt.want {
			t.Errorf("%s: sameActor is not symmetric", tt.name)
		}
	}
}

func TestApproveAlias(t *testing.T) {
	withAlias := testIntegration()
	withAlias.Spec.GitHub.Issues.ApproveComment = "/lgtm"
	noIssues := testIntegration()
	noIssues.Spec.GitHub.Issues = nil
	tests := []struct {
		name  string
		integ *v1alpha1.Integration
		want  string
	}{
		{name: "nil integration", integ: nil, want: ""},
		{name: "no github block", integ: &v1alpha1.Integration{}, want: ""},
		{name: "no issues block", integ: noIssues, want: ""},
		{name: "default alias", integ: testIntegration(), want: ""},
		{name: "configured alias", integ: withAlias, want: "/lgtm"},
	}
	for _, tt := range tests {
		if got := approveAlias(tt.integ); got != tt.want {
			t.Errorf("%s: approveAlias = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestIsBot(t *testing.T) {
	tests := []struct {
		actor v1alpha1.CommandActor
		want  bool
	}{
		{actor: v1alpha1.CommandActor{Login: "dependabot[bot]", Type: "Bot"}, want: true},
		{actor: v1alpha1.CommandActor{Login: "patchy[bot]", Type: "User"}, want: true},
		{actor: v1alpha1.CommandActor{Login: "robot", Type: "Bot"}, want: true},
		{actor: v1alpha1.CommandActor{Login: "botanist", Type: "User"}, want: false},
	}
	for _, tt := range tests {
		if got := isBot(tt.actor); got != tt.want {
			t.Errorf("isBot(%+v) = %v, want %v", tt.actor, got, tt.want)
		}
	}
}

// appliedBy recognises a command's own effect on the spec: same author, same
// decision second, per verb; anything else is someone else's action.
func TestAppliedBy(t *testing.T) {
	at := testClock
	req := func(by string, when time.Time) *v1alpha1.ActionRequest {
		return &v1alpha1.ActionRequest{By: by, At: metav1.NewTime(when)}
	}
	approval := func(by string, when time.Time) *v1alpha1.Approval {
		return &v1alpha1.Approval{By: by, At: metav1.NewTime(when)}
	}
	cmd := func(verb string) *v1alpha1.FindingCommand {
		return &v1alpha1.FindingCommand{Verb: verb, Actor: v1alpha1.CommandActor{Login: maintainer}}
	}
	tests := []struct {
		name string
		spec v1alpha1.FindingSpec
		verb string
		want bool
	}{
		{
			name: "own approval", spec: v1alpha1.FindingSpec{Approval: approval(maintainer, at)},
			verb: action.VerbApprove, want: true,
		},
		{
			name: "own approval, sub-second skew",
			spec: v1alpha1.FindingSpec{Approval: approval(maintainer, at.Add(300*time.Millisecond))},
			verb: action.VerbApprove, want: true,
		},
		{
			name: "someone else's approval", spec: v1alpha1.FindingSpec{Approval: approval("other", at)},
			verb: action.VerbApprove, want: false,
		},
		{
			name: "an earlier approval",
			spec: v1alpha1.FindingSpec{Approval: approval(maintainer, at.Add(-time.Minute))}, verb: action.VerbApprove,
			want: false,
		},
		{name: "no approval", verb: action.VerbApprove, want: false},
		{name: "own retry", spec: v1alpha1.FindingSpec{Retry: req(maintainer, at)}, verb: action.VerbRetry, want: true},
		{
			name: "no retry", spec: v1alpha1.FindingSpec{Approval: approval(maintainer, at)}, verb: action.VerbRetry,
			want: false,
		},
		{
			name: "own expedite", spec: v1alpha1.FindingSpec{Expedite: req(maintainer, at)}, verb: action.VerbExpedite,
			want: true,
		},
		{name: "no expedite", verb: action.VerbExpedite, want: false},
		{
			name: "a toggle verb is never recognised", spec: v1alpha1.FindingSpec{Approval: approval(maintainer, at)},
			verb: action.VerbSuspend, want: false,
		},
	}
	for _, tt := range tests {
		f := &v1alpha1.Finding{Spec: tt.spec}
		if got := appliedBy(f, cmd(tt.verb), at); got != tt.want {
			t.Errorf("%s: appliedBy = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The projection leaves a finding being deleted alone and surfaces a read
// failure for the backoff.
func TestProjectionReadEdges(t *testing.T) {
	t.Run("a finding being deleted is not projected", func(t *testing.T) {
		fnd := projectable(v1alpha1.PhaseOpened)
		now := metav1.NewTime(testClock)
		fnd.DeletionTimestamp = &now
		fnd.Finalizers = []string{"patchy.bitwisemedia.uk/test"}
		tracker := newFakeTracker()
		r, _ := newProjector(t, tracker, testIntegration(), fnd)
		res, err := r.Reconcile(t.Context(), findingRequest(fnd.Name))
		if err != nil || res != (ctrl.Result{}) {
			t.Fatalf("Reconcile() = %+v, %v; want zero, nil", res, err)
		}
		if len(tracker.issues) != 0 || len(tracker.comments) != 0 {
			t.Errorf("tracker touched: %d issues, %d comments", len(tracker.issues), len(tracker.comments))
		}
	})

	t.Run("a read failure is returned", func(t *testing.T) {
		boom := errors.New("cache unavailable")
		c := receiverClient(&interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		})
		r := &FindingReconciler{Client: c, Namespace: "patchy"}
		_, err := r.Reconcile(t.Context(), findingRequest("finding-aa-1"))
		if !errors.Is(err, boom) {
			t.Errorf("Reconcile() = %v, want the read error", err)
		}
	})

	t.Run("a missing finding is a no-op", func(t *testing.T) {
		r := &FindingReconciler{Client: receiverClient(nil), Namespace: "patchy"}
		res, err := r.Reconcile(t.Context(), findingRequest("gone"))
		if err != nil || res != (ctrl.Result{}) {
			t.Errorf("Reconcile() = %+v, %v; want zero, nil", res, err)
		}
	})
}

// findingRequest is the reconcile request for the named finding.
func findingRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: name}}
}
