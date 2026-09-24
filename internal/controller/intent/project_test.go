// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
)

func (e *env) getProject() *v1alpha1.Project {
	e.t.Helper()
	var p v1alpha1.Project
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &p); err != nil {
		e.t.Fatal(err)
	}
	return &p
}

// TestProjectValidation: a Project is Ready only when it builds in exactly
// one repository, shares its intent repository and trigger with no other
// Project, resolves every repository to one Forge, and the App can act on
// each with the permissions intents use; the labels are created.
func TestProjectValidation(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*v1alpha1.Project)
		other      *v1alpha1.Project
		resolveErr error
		installErr error
		wantReason string
	}{
		{name: "valid", wantReason: ReasonValidated},
		{name: "two repositories", mutate: func(p *v1alpha1.Project) {
			p.Spec.Repositories = append(p.Spec.Repositories, v1alpha1.ProjectRepository{
				Name: "two", URL: "https://github.com/acme/two"})
		}, wantReason: ReasonUnsupportedRepositories},
		{name: "shared intent repository and trigger", other: &v1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNS},
			Spec: v1alpha1.ProjectSpec{IntentRepository: "https://github.com/ACME/intents.git",
				Labels: v1alpha1.ProjectLabels{Trigger: "Patchy:Target"}},
		}, wantReason: v1alpha1.ReasonAmbiguousIntentRepository},
		{name: "no forge", resolveErr: fmt.Errorf("x: %w", forge.ErrNoMatch), wantReason: v1alpha1.ReasonForgeUnresolved},
		{name: "not installed", installErr: ghError(http.StatusNotFound, "Not Found"),
			wantReason: v1alpha1.ReasonAppNotInstalled},
		{name: "repository not in the installation", installErr: ghError(http.StatusUnprocessableEntity, "x"),
			wantReason: v1alpha1.ReasonAppNotInstalled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			if tt.mutate != nil {
				tt.mutate(p)
			}
			objs := []client.Object{p}
			if tt.other != nil {
				objs = append(objs, tt.other)
			}
			e := newEnv(t, objs...)
			e.gh.resolveErr, e.gh.installedErr = tt.resolveErr, tt.installErr
			e.reconcileProject()
			c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Reason != tt.wantReason {
				t.Fatalf("Ready = %+v, want reason %s", c, tt.wantReason)
			}
			if want := tt.wantReason == ReasonValidated; (c.Status == metav1.ConditionTrue) != want {
				t.Errorf("Ready status = %s", c.Status)
			}
			if tt.wantReason == ReasonValidated &&
				(!e.gh.labels["patchy:target"] || !e.gh.labels["patchy:approved"]) {
				t.Errorf("labels created = %v", e.gh.createdLabels)
			}
		})
	}
}

// TestProjectValidationWaitsForGitHub: a transient failure decides nothing.
func TestProjectValidationWaitsForGitHub(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.failNext("Installed", errTransient)
	if _, err := e.project.Reconcile(context.Background(), req("target")); err == nil {
		t.Fatal("no error for a failed installation check")
	}
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady); c != nil {
		t.Errorf("Ready = %+v after a transient failure", c)
	}
}

// TestDiscovery: an open trigger-labelled issue becomes the Intent
// <project>-<issue>, requested by the actor GitHub's events name; an issue
// whose label predates its last close does not.
func TestDiscovery(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.openIssue(1, "One", "body", "alice")
	e.gh.openIssue(2, "Two", "body", approver)
	e.gh.humanClose(2, approver)
	e.gh.mu.Lock()
	e.gh.issues[2].state = "open" // reopened without the label applied again
	e.gh.mu.Unlock()
	e.reconcileProject()

	in := e.get("target-1")
	if in.Spec.RequestedBy.Login != "alice" || in.Spec.Issue.Number != 1 ||
		in.Spec.Issue.Repository != intentRepoURL || in.Spec.RequestedBy.EventID == 0 {
		t.Errorf("intent spec = %+v", in.Spec)
	}
	var none v1alpha1.Intent
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target-2"}, &none); err == nil {
		t.Error("an issue whose trigger predates its last close became an intent")
	}
	p := e.getProject()
	if p.Status.LastPolledAt == nil || p.Status.ActiveIntents != 1 {
		t.Errorf("status = %+v", p.Status)
	}
}

// TestDiscoveryConditional: an unchanged listing (304) re-lists nothing and
// creates nothing new, but retries the issues that were waiting.
func TestDiscoveryConditional(t *testing.T) {
	p := testProject()
	p.Spec.Limits.MaxActiveIntents = 1
	e := newEnv(t, p)
	e.gh.openIssue(1, "One", "body", approver)
	e.gh.openIssue(2, "Two", "body", approver)
	e.reconcileProject()
	if got := e.getProject().Status.ActiveIntents; got != 1 {
		t.Fatalf("active = %d, want the limit of 1", got)
	}
	events := e.gh.calls["ListIssueEvents"]
	// Issue 1's intent ends; nothing on GitHub changed.
	in := e.get("target-1")
	in.Status.Phase = v1alpha1.IntentClosed
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.reconcileProject()
	if e.gh.calls["ListIssueEvents"] != events+1 {
		t.Errorf("a 304 re-read %d issues' events, want only the waiting one", e.gh.calls["ListIssueEvents"]-events)
	}
	e.get("target-2")
}

// TestDiscoveryNameConflict: an issue whose Intent name another repository's
// issue holds is reported, and waits.
func TestDiscoveryNameConflict(t *testing.T) {
	held := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS},
		Spec: v1alpha1.IntentSpec{Project: "target",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/old-intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: approver, EventID: 1}},
	}
	e := newEnv(t, testProject(), held)
	e.gh.openIssue(1, "One", "body", approver)
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionIntentNameConflict)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "issue #1 (Intent target-1)") {
		t.Fatalf("IntentNameConflict = %+v", c)
	}
	if got := e.get("target-1").Spec.Issue.Repository; got != "https://github.com/acme/old-intents" {
		t.Errorf("the held Intent was changed: %s", got)
	}
	// Once the holder is gone, the issue gets its Intent.
	if err := e.c.Delete(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.reconcileProject()
	if got := e.get("target-1").Spec.Issue.Repository; got != intentRepoURL {
		t.Errorf("repository = %s", got)
	}
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions,
		v1alpha1.ConditionIntentNameConflict); c.Status != metav1.ConditionFalse {
		t.Errorf("IntentNameConflict = %+v after the holder went", c)
	}
}

// TestDiscoveryPausedUnderTheRateFloor: below the floor nothing is listed.
func TestDiscoveryPausedUnderTheRateFloor(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.remaining = 10
	e.gh.openIssue(1, "One", "body", approver)
	e.reconcileProject()
	if e.gh.calls["ListIssues"] != 0 || e.getProject().Status.LastPolledAt != nil {
		t.Errorf("listed %d times under the floor", e.gh.calls["ListIssues"])
	}
}

// TestSuspendedProjectDiscoversNothing.
func TestSuspendedProjectDiscoversNothing(t *testing.T) {
	p := testProject()
	p.Spec.Suspend = true
	e := newEnv(t, p)
	e.gh.openIssue(1, "One", "body", approver)
	e.reconcileProject()
	if e.gh.calls["ListIssues"] != 0 {
		t.Error("a suspended project was polled")
	}
}

// TestRevival: an approver applying the trigger label again to a failed
// intent's issue revives it to Planning at a new input revision; the same
// from anyone else, or on a merged intent, gets one notice and the label is
// removed again.
func TestRevival(t *testing.T) {
	tests := []struct {
		name      string
		end       v1alpha1.IntentPhase
		actor     string
		wantPhase v1alpha1.IntentPhase
		wantText  string
	}{
		{"approver revives a failed intent", v1alpha1.IntentFailed, approver, v1alpha1.IntentPlanning, ""},
		{"a non-approver cannot", v1alpha1.IntentFailed, "mallory", v1alpha1.IntentFailed, "only the project"},
		{"a merged intent has ended", v1alpha1.IntentMerged, approver, v1alpha1.IntentMerged, "has ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			if tt.end == v1alpha1.IntentFailed {
				e.jobs.output = failingBuild
			}
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			if tt.end == v1alpha1.IntentFailed {
				e.drive(name, v1alpha1.IntentFailed, repoImage)
			} else {
				e.drive(name, v1alpha1.IntentInReview, repoImage)
				e.gh.closePR(true)
				e.drive(name, v1alpha1.IntentMerged, repoImage)
				e.gh.mu.Lock()
				e.gh.issues[1].state = "open" // reopened
				e.gh.mu.Unlock()
			}
			e.mustIntent(name)
			e.jobs.output = defaultOutput
			e.clock.Advance(time.Minute)
			if tt.end == v1alpha1.IntentMerged {
				e.gh.unlabel(1, "patchy:target", approver)
			}
			id := e.gh.label(1, "patchy:target", tt.actor)
			e.reconcileProject()
			e.mustIntent(name)
			in := e.get(name)
			if in.Status.Phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s", in.Status.Phase, tt.wantPhase)
			}
			if in.Status.LastTrigger == nil || in.Status.LastTrigger.EventID != id {
				t.Errorf("lastTrigger = %+v, want the new label event consumed", in.Status.LastTrigger)
			}
			if tt.wantPhase == v1alpha1.IntentPlanning {
				if in.Status.Input.Revision != 2 || in.Status.CompletedAt != nil {
					t.Errorf("revived: input %+v completedAt %v", in.Status.Input, in.Status.CompletedAt)
				}
				return
			}
			n := e.gh.withMarker("event-" + itoa(id))
			if len(n) != 1 || !strings.Contains(n[0].Body, tt.wantText) {
				t.Fatalf("notices = %+v", n)
			}
			if e.gh.hasLabel("patchy:target") {
				t.Error("the trigger label was left on an ended intent's issue")
			}
			// Handed over again, nothing more is posted.
			e.nudger.Nudge(testNS, name)
			e.mustIntent(name)
			if n := e.gh.withMarker("event-" + itoa(id)); len(n) != 1 {
				t.Errorf("notices = %d after a second hand-off", len(n))
			}
		})
	}
}

// TestHandOffSurvivesAFailure: a hand-off whose GitHub call fails is kept
// for the retry, which answers it, though discovery's next listing of the
// unchanged issue is a 304 and nudges nothing.
func TestHandOffSurvivesAFailure(t *testing.T) {
	tests := []struct {
		name      string
		end       v1alpha1.IntentPhase
		fail      string
		wantPhase v1alpha1.IntentPhase
	}{
		{"a revival whose issue read fails", v1alpha1.IntentFailed, "GetIssue", v1alpha1.IntentPlanning},
		{"an ended intent's notice whose label removal fails", v1alpha1.IntentMerged, "RemoveLabel",
			v1alpha1.IntentMerged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			if tt.end == v1alpha1.IntentFailed {
				e.jobs.output = failingBuild
			}
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			if tt.end == v1alpha1.IntentFailed {
				e.drive(name, v1alpha1.IntentFailed, repoImage)
			} else {
				e.drive(name, v1alpha1.IntentInReview, repoImage)
				e.gh.closePR(true)
				e.drive(name, v1alpha1.IntentMerged, repoImage)
				e.gh.mu.Lock()
				e.gh.issues[1].state = "open" // reopened
				e.gh.mu.Unlock()
				e.gh.unlabel(1, "patchy:target", approver)
			}
			e.mustIntent(name)
			e.jobs.output = defaultOutput
			e.clock.Advance(time.Minute)
			id := e.gh.label(1, "patchy:target", approver)
			e.reconcileProject() // a full listing: the nudge
			e.gh.failNext(tt.fail, errTransient)
			if err := e.reconcileIntent(name); err == nil {
				t.Fatal("the failed hand-off returned no error")
			}
			e.clock.Advance(time.Minute)
			e.reconcileProject() // a 304: no nudge
			if err := e.reconcileIntent(name); err != nil {
				t.Fatalf("the retried hand-off: %v", err)
			}
			in := e.get(name)
			if in.Status.Phase != tt.wantPhase || in.Status.LastTrigger == nil || in.Status.LastTrigger.EventID != id {
				t.Fatalf("phase %s lastTrigger %+v, want %s with the label %d consumed", in.Status.Phase,
					in.Status.LastTrigger, tt.wantPhase, id)
			}
			if tt.end == v1alpha1.IntentMerged {
				if n := e.gh.withMarker("event-" + itoa(id)); len(n) != 1 || e.gh.hasLabel("patchy:target") {
					t.Errorf("notices %d, label left %v; want one notice and the label removed", len(n),
						e.gh.hasLabel("patchy:target"))
				}
			}
		})
	}
}

// TestTTL: an ended intent is deleted, in the foreground, its TTL after it
// completed; an active one never is.
func TestTTL(t *testing.T) {
	done := metav1.NewTime(time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)) // a day before the clock
	ended := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS, UID: "u1"},
		Status:     v1alpha1.IntentStatus{Phase: v1alpha1.IntentMerged, CompletedAt: &done},
	}
	active := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-2", Namespace: testNS, UID: "u2"},
		Status:     v1alpha1.IntentStatus{Phase: v1alpha1.IntentBlocked},
	}
	e := newEnv(t, ended, active)
	res, err := e.ttl.Reconcile(context.Background(), req("target-1"))
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("before expiry: %+v, %v", res, err)
	}
	e.clock.Advance(14 * 24 * time.Hour)
	for _, n := range []string{"target-1", "target-2"} {
		if _, err := e.ttl.Reconcile(context.Background(), req(n)); err != nil {
			t.Fatal(err)
		}
	}
	var gone v1alpha1.Intent
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(ended), &gone); err == nil {
		t.Error("the expired intent was not deleted")
	}
	e.get("target-2")
}
