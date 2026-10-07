// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/intentperm"
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
		{name: "forge secret not readable", installErr: fmt.Errorf("get forge secret patchy/other: %w",
			kerrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "other", errors.New("not in resourceNames"))),
			wantReason: ReasonForgeSecretUnreadable},
		{name: "forge secret missing", installErr: fmt.Errorf("get forge secret patchy/gone: %w",
			kerrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "gone")),
			wantReason: ReasonForgeSecretUnreadable},
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

// TestProjectValidationProvesCheckFixReads is the regression test for a
// Project with spec.checks.fix whose App was never granted the reads a
// check-fix round makes: it used to report Ready, then fail on the first
// failing check. Ready now proves checks, statuses and actions read on the
// application repository, and names the one GitHub refused.
func TestProjectValidationProvesCheckFixReads(t *testing.T) {
	notGranted := ghError(http.StatusUnprocessableEntity,
		"The permissions requested are not granted to this installation.")
	tests := []struct {
		name    string
		refused ghclient.TokenPerms
		want    string
	}{
		{name: "checks", refused: ghclient.TokenPerms{Checks: ghclient.PermRead}, want: "checks: read"},
		{name: "statuses", refused: ghclient.TokenPerms{Statuses: ghclient.PermRead}, want: "statuses: read"},
		{name: "actions", refused: ghclient.TokenPerms{Actions: ghclient.PermRead}, want: "actions: read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			p.Spec.Checks.Fix = []string{"test"}
			e := newEnv(t, p)
			e.gh.refused = map[ghclient.TokenPerms]error{tt.refused: notGranted}
			e.reconcileProject()
			c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonAppNotInstalled {
				t.Fatalf("Ready = %+v, want False/%s", c, v1alpha1.ReasonAppNotInstalled)
			}
			if !strings.Contains(c.Message, appRepoURL+" with "+tt.want) || !strings.Contains(c.Message, "spec.checks.fix") {
				t.Errorf("message %q does not name %s on %s and why", c.Message, tt.want, appRepoURL)
			}
		})
	}
}

// TestProjectValidationProvesTheRerunWrite: a Project that re-runs failed
// checks (spec.checks.rerunFailed) is Ready only once the App may re-run
// Actions jobs on its application repository: refused actions write, it is
// not Ready, naming the permission, what it is for and what to grant. A
// Project that fixes checks without re-running them never asks for it.
func TestProjectValidationProvesTheRerunWrite(t *testing.T) {
	refused := map[ghclient.TokenPerms]error{{Actions: ghclient.PermWrite}: ghError(http.StatusUnprocessableEntity,
		"The permissions requested are not granted to this installation.")}
	p := testProject()
	p.Spec.Checks.Fix, p.Spec.Checks.RerunFailed = []string{"test"}, true
	e := newEnv(t, p)
	e.gh.refused = refused
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonAppNotInstalled {
		t.Fatalf("Ready = %+v, want False/%s", c, v1alpha1.ReasonAppNotInstalled)
	}
	for _, want := range []string{appRepoURL + " with actions: write", "spec.checks.rerunFailed",
		"Actions read and write"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q does not say %q", c.Message, want)
		}
	}

	p = testProject()
	p.Spec.Checks.Fix = []string{"test"}
	e = newEnv(t, p)
	e.gh.refused = refused
	e.reconcileProject()
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady); c == nil ||
		c.Reason != ReasonValidated {
		t.Errorf("a Project that only fixes checks: Ready = %+v, want Validated without actions write", c)
	}
}

// TestProjectValidationProvesTheRepositoryReads is the regression test for
// an application repository whose App holds contents and pull requests
// write but no issues read: it used to report Ready, then fail every pass
// once the intent had a pull request, because a reviewer's permission and
// the rate budget are read there with an issues-read token. Ready now
// proves issues read on the application repository, and names it.
func TestProjectValidationProvesTheRepositoryReads(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.refused = map[ghclient.TokenPerms]error{
		{Issues: ghclient.PermRead}: ghError(http.StatusUnprocessableEntity,
			"The permissions requested are not granted to this installation."),
	}
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonAppNotInstalled {
		t.Fatalf("Ready = %+v, want False/%s", c, v1alpha1.ReasonAppNotInstalled)
	}
	if !strings.Contains(c.Message, appRepoURL+" with issues: read") || !strings.Contains(c.Message, "rate budget") {
		t.Errorf("message %q does not name issues: read on %s and why", c.Message, appRepoURL)
	}
}

// TestProjectValidationMintsTheTable: Ready mints one token per grant of
// the intentperm table, each with that one permission on its repository;
// the check-fix reads only when spec.checks.fix names a check, and actions
// write only when spec.checks.rerunFailed is set beside it.
// TestEveryTokenIsInTheTable is the other half: every token an intent asks
// for is a grant of that table.
func TestProjectValidationMintsTheTable(t *testing.T) {
	base := []installCheck{
		{intentRepoURL, ghclient.TokenPerms{Issues: ghclient.PermWrite}},
		{appRepoURL, ghclient.TokenPerms{Contents: ghclient.PermWrite}},
		{appRepoURL, ghclient.TokenPerms{PullRequests: ghclient.PermWrite}},
		{appRepoURL, ghclient.TokenPerms{Issues: ghclient.PermRead}},
	}
	checkFix := append(slices.Clone(base),
		installCheck{appRepoURL, ghclient.TokenPerms{Checks: ghclient.PermRead}},
		installCheck{appRepoURL, ghclient.TokenPerms{Statuses: ghclient.PermRead}},
		installCheck{appRepoURL, ghclient.TokenPerms{Actions: ghclient.PermRead}},
	)
	rerun := append(slices.Clone(checkFix), installCheck{appRepoURL, ghclient.TokenPerms{Actions: ghclient.PermWrite}})
	for _, tt := range []struct {
		name  string
		fix   []string
		rerun bool
		want  []installCheck
	}{
		{name: "no check fixes", want: base},
		{name: "check fixes", fix: []string{"test", "lint"}, want: checkFix},
		{name: "check fixes and re-runs", fix: []string{"test"}, rerun: true, want: rerun},
		{name: "re-runs with no check to fix", rerun: true, want: base},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			p.Spec.Checks.Fix, p.Spec.Checks.RerunFailed = tt.fix, tt.rerun
			e := newEnv(t, p)
			e.reconcileProject()
			if c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady); c == nil ||
				c.Reason != ReasonValidated {
				t.Fatalf("Ready = %+v", c)
			}
			e.gh.mu.Lock()
			got := slices.Clone(e.gh.installs)
			e.gh.mu.Unlock()
			if !slices.Equal(got, tt.want) {
				t.Errorf("installation checks =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// TestTokenPermsRefusesWhatNoTokenCanProve: a grant the token request
// cannot express, or one it must not (a write of a read-only permission),
// is an error rather than a token minted with something else.
func TestTokenPermsRefusesWhatNoTokenCanProve(t *testing.T) {
	for _, g := range []intentperm.Grant{
		{Permission: "administration", Access: intentperm.Write},
		{Permission: intentperm.Checks, Access: intentperm.Write},
		{Permission: intentperm.Issues, Access: "admin"},
	} {
		if p, err := tokenPerms(g); err == nil {
			t.Errorf("tokenPerms(%s) = %+v, want an error", g, p)
		}
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
	e.gh.humanClose(2)
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

func TestSharedIntentIssueHasOneProject(t *testing.T) {
	other := testProject()
	other.Name, other.UID = "other", "other-uid"
	for _, tt := range []struct {
		name       string
		labelFirst bool
		want       int
	}{
		{name: "both triggers before discovery", want: 0},
		{name: "second trigger after discovery", labelFirst: true, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject(), other.DeepCopy())
			e.gh.openIssue(1, "One", "body", approver)
			if tt.labelFirst {
				e.reconcileProject()
			}
			e.gh.label(1, "patchy:other", approver)
			e.clock.Advance(time.Minute)
			e.reconcileProject()
			if _, err := e.project.Reconcile(context.Background(), req("other")); err != nil {
				t.Fatal(err)
			}
			var intents v1alpha1.IntentList
			if err := e.c.List(context.Background(), &intents, client.InNamespace(testNS)); err != nil {
				t.Fatal(err)
			}
			if len(intents.Items) != tt.want {
				t.Fatalf("discovered %d Intents, want %d", len(intents.Items), tt.want)
			}
			var project v1alpha1.Project
			if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "other"},
				&project); err != nil {
				t.Fatal(err)
			}
			c := meta.FindStatusCondition(project.Status.Conditions, v1alpha1.ConditionIntentNameConflict)
			if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "issue #1") {
				t.Errorf("other Project's issue conflict = %+v", c)
			}
			if tt.labelFirst {
				e.drive("target-1", v1alpha1.IntentAwaitingApproval, repoImage)
				e.gh.label(1, "patchy:approved", approver)
				e.drive("target-1", v1alpha1.IntentInReview, repoImage)
				if len(e.gh.prs) != 1 {
					t.Errorf("pull requests = %d, want only the first Project's", len(e.gh.prs))
				}
			}
		})
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

// TestSkippedPollsArePaced: a Project that has polled and then cannot (the
// App uninstalled, so not Ready; suspended; the rate budget under the floor)
// is looked at once per poll interval, not in a loop: its requeue is a whole
// interval, and the passes an event starts in between call GitHub for
// nothing.
func TestSkippedPollsArePaced(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		break_ func(e *env)
		calls  string
	}{
		{"not Ready", func(e *env) {
			e.gh.installedErr = ghError(http.StatusNotFound, "Not Found")
			e.clock.Advance(revalidateEvery)
		}, "Installed"},
		{"suspended", func(e *env) {
			p := e.getProject()
			p.Spec.Suspend = true
			if err := e.c.Update(ctx, p); err != nil {
				e.t.Fatal(err)
			}
			e.clock.Advance(time.Minute)
		}, "ListIssues"},
		{"under the rate floor", func(e *env) {
			e.gh.remaining = 10
			e.clock.Advance(time.Minute)
		}, "RateRemaining"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.reconcileProject()
			if e.getProject().Status.LastPolledAt == nil {
				t.Fatal("the Project never polled")
			}
			tt.break_(e)
			for i := range 5 {
				res, err := e.project.Reconcile(ctx, req("target"))
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 && tt.name == "not Ready" && meta.IsStatusConditionTrue(e.getProject().Status.Conditions,
					v1alpha1.ConditionReady) {
					t.Fatal("the Project is still Ready")
				}
				// The interval from the skipped poll, less the seconds since.
				if want := time.Minute - time.Duration(i)*time.Second; res.RequeueAfter != want {
					t.Fatalf("pass %d: requeue after %s, want %s", i, res.RequeueAfter, want)
				}
				if i == 0 {
					e.gh.calls = map[string]int{}
				}
				e.clock.Advance(time.Second)
			}
			if n := e.gh.calls[tt.calls]; n != 0 {
				t.Errorf("%s called %d times between polls", tt.calls, n)
			}
		})
	}
}

// TestForgeChangeRevalidates: a Project that is not Ready is re-checked at
// the next poll interval, or at once when a Forge changes.
func TestForgeChangeRevalidates(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.installedErr = ghError(http.StatusNotFound, "Not Found")
	e.reconcileProject()
	e.gh.installedErr = nil // the App is installed
	e.clock.Advance(time.Second)
	e.reconcileProject()
	if meta.IsStatusConditionTrue(e.getProject().Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatal("re-checked within the interval with nothing changed")
	}
	e.project.forgeChanges.Add(1) // the Forge watch saw a change
	e.reconcileProject()
	if !meta.IsStatusConditionTrue(e.getProject().Status.Conditions, v1alpha1.ConditionReady) {
		t.Error("a Forge change did not re-check the Project")
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
				e.gh.removeTrigger()
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
				e.gh.removeTrigger()
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

// staleIntent is a cache that has not yet seen an Intent's revival: it shows
// the Intent Failed, completed at done.
type staleIntent struct {
	client.Client
	done metav1.Time
}

func (s staleIntent) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := s.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if in, ok := obj.(*v1alpha1.Intent); ok {
		in.Status.Phase, in.Status.CompletedAt = v1alpha1.IntentFailed, &s.done
	}
	return nil
}

// TestTTLSparesARevivedIntent: an Intent the cache still shows Failed and
// expired, but that an approver revived just now, is not deleted.
func TestTTLSparesARevivedIntent(t *testing.T) {
	revived := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "target-1", Namespace: testNS, UID: "u1"},
		Status:     v1alpha1.IntentStatus{Phase: v1alpha1.IntentPlanning},
	}
	e := newEnv(t, revived)
	e.clock.Advance(14 * 24 * time.Hour)
	e.ttl.Client = staleIntent{Client: e.c, done: metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))}
	if _, err := e.ttl.Reconcile(context.Background(), req("target-1")); err != nil {
		t.Fatal(err)
	}
	e.get("target-1")
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
