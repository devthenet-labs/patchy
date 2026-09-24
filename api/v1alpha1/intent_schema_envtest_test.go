// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// The intent kinds' schema tests. They run inside TestSchemaValidation's
// single envtest boot (schema_envtest_test.go), each against its own object
// names in the default namespace.

var (
	schemaNow    = metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	schemaDigest = "sha256:" + strings.Repeat("d", 64)
	schemaSHA    = strings.Repeat("a", 40)
)

// sameJSON compares two values by their JSON encoding. metav1.Time encodes
// as RFC 3339 UTC, so a value read back from the API server (in local time)
// compares equal to the one written — and a field the generated schema
// pruned shows up as a difference.
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	if string(g) != string(w) {
		t.Errorf("%s round-tripped as\n  %s\nwant\n  %s", what, g, w)
	}
}

func schemaProject(name string) *patchyv1.Project {
	return &patchyv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: patchyv1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        patchyv1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories: []patchyv1.ProjectRepository{
				{Name: "shop", URL: "https://github.com/acme/shop"},
			},
		},
	}
}

// testProjectSchema exercises the Project schema: the defaults a minimal
// Project is completed with, every bound at and past its edge, the label
// rule, the name rule, and a status round-trip.
func testProjectSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	testProjectDefaults(ctx, t, c)
	testProjectBounds(ctx, t, c)
	testProjectStatus(ctx, t, c)
}

// testProjectDefaults: a minimal Project is completed with the design's
// defaults, and an explicit false survives defaulting.
func testProjectDefaults(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("a minimal project is defaulted", func(t *testing.T) {
		if err := c.Create(ctx, schemaProject("proj-minimal")); err != nil {
			t.Fatalf("Create(minimal project) = %v, want nil", err)
		}
		got := &patchyv1.Project{}
		if err := c.Get(ctx, client.ObjectKey{Name: "proj-minimal", Namespace: "default"}, got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		s := got.Spec
		if s.Labels.Approve != patchyv1.DefaultApproveLabel || s.Labels.Trigger != "" {
			t.Errorf("labels = %+v, want approve %q and trigger left to the controller", s.Labels, patchyv1.DefaultApproveLabel)
		}
		// The exported defaults and the schema's default markers must agree.
		revisions, checkFixes := patchyv1.DefaultMaxRevisions, patchyv1.DefaultMaxCheckFixes
		wantLimits := patchyv1.ProjectLimits{
			MaxActiveIntents: patchyv1.DefaultMaxActiveIntents,
			MaxRevisions:     &revisions,
			MaxCheckFixes:    &checkFixes,
			MaxCostMicroUSD:  patchyv1.DefaultMaxCostMicroUSD,
		}
		sameJSON(t, "defaulted limits", s.Limits, wantLimits)
		if s.RequireRepositoryImage == nil || !*s.RequireRepositoryImage {
			t.Errorf("requireRepositoryImage = %v, want true", s.RequireRepositoryImage)
		}
		if s.Checks.Timeout == nil || s.Checks.Timeout.Duration != 30*time.Minute || len(s.Checks.Fix) != 0 {
			t.Errorf("checks = %+v, want timeout 30m and no auto-fixed checks", s.Checks)
		}
	})

	t.Run("explicit zero and false values survive defaulting", func(t *testing.T) {
		p := schemaProject("proj-zeroes")
		off, zero := false, int32(0)
		p.Spec.RequireRepositoryImage = &off
		p.Spec.Limits.MaxRevisions = &zero
		p.Spec.Limits.MaxCheckFixes = &zero
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v, want nil", err)
		}
		got := &patchyv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		if got.Spec.RequireRepositoryImage == nil || *got.Spec.RequireRepositoryImage {
			t.Errorf("requireRepositoryImage = %v, want false", got.Spec.RequireRepositoryImage)
		}
		l := got.Spec.Limits
		if l.MaxRevisions == nil || *l.MaxRevisions != 0 || l.MaxCheckFixes == nil || *l.MaxCheckFixes != 0 {
			t.Errorf("limits = %+v, want maxRevisions and maxCheckFixes kept at 0", l)
		}
	})
}

// testProjectBounds creates one Project per case, each a valid Project with
// one field moved to (accepted) or just past (rejected) its bound.
func testProjectBounds(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	repos := func(n int) []patchyv1.ProjectRepository {
		out := make([]patchyv1.ProjectRepository, n)
		for i := range out {
			out[i] = patchyv1.ProjectRepository{
				Name: fmt.Sprintf("app%d", i), URL: fmt.Sprintf("https://github.com/acme/app%d", i),
			}
		}
		return out
	}
	logins := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("user%d", i)
		}
		return out
	}
	tests := []struct {
		name    string
		mutate  func(*patchyv1.Project)
		wantErr bool
	}{
		{"eight repositories", func(p *patchyv1.Project) { p.Spec.Repositories = repos(8) }, false},
		{"nine repositories", func(p *patchyv1.Project) { p.Spec.Repositories = repos(9) }, true},
		{"no repositories", func(p *patchyv1.Project) { p.Spec.Repositories = nil }, true},
		{"duplicate repository key", func(p *patchyv1.Project) {
			p.Spec.Repositories = append(repos(1),
				patchyv1.ProjectRepository{Name: "app0", URL: "https://github.com/acme/other"})
		}, true},
		// One entry per repository: pull requests are keyed by URL, and
		// every repository shares the intent branch.
		{"two keys for one repository url", func(p *patchyv1.Project) {
			p.Spec.Repositories = append(repos(1),
				patchyv1.ProjectRepository{Name: "again", URL: "https://github.com/acme/app0"})
		}, true},
		{"two repository urls differing only in case", func(p *patchyv1.Project) {
			p.Spec.Repositories = append(repos(1),
				patchyv1.ProjectRepository{Name: "again", URL: "https://GitHub.com/Acme/App0"})
		}, true},
		{"a repository url and its .git form", func(p *patchyv1.Project) {
			p.Spec.Repositories = append(repos(1),
				patchyv1.ProjectRepository{Name: "again", URL: "https://github.com/acme/app0.git"})
		}, true},
		{"a repository whose name embeds .git", func(p *patchyv1.Project) {
			p.Spec.Repositories = append(repos(1),
				patchyv1.ProjectRepository{Name: "pages", URL: "https://github.com/acme/app0.github.io"})
		}, false},
		{"repository key not a DNS label", func(p *patchyv1.Project) { p.Spec.Repositories[0].Name = "Shop" }, true},
		{"repository key at the name budget", func(p *patchyv1.Project) {
			p.Spec.Repositories[0].Name = strings.Repeat("a", patchyv1.MaxRepositoryKeyLength)
		}, false},
		{"repository key past the name budget", func(p *patchyv1.Project) {
			p.Spec.Repositories[0].Name = strings.Repeat("a", patchyv1.MaxRepositoryKeyLength+1)
		}, true},
		{"plain http repository", func(p *patchyv1.Project) {
			p.Spec.Repositories[0].URL = "http://github.com/acme/shop"
		}, true},
		{"credentials in the repository url", func(p *patchyv1.Project) {
			p.Spec.Repositories[0].URL = "https://x:token@github.com/acme/shop"
		}, true},
		{"repository url past owner/name", func(p *patchyv1.Project) {
			p.Spec.Repositories[0].URL = "https://github.com/acme/shop/tree/main"
		}, true},
		{"intent repository not a url", func(p *patchyv1.Project) { p.Spec.IntentRepository = "acme/intents" }, true},
		{"twenty approvers", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = logins(20) }, false},
		{"twenty-one approvers", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = logins(21) }, true},
		{"no approvers", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = nil }, true},
		{"duplicate approver", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = []string{"octocat", "octocat"} }, true},
		{"approver with a space", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = []string{"octo cat"} }, true},
		{"a bot approver", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = []string{"dependabot[bot]"} }, true},
		{"an EMU approver", func(p *patchyv1.Project) { p.Spec.Approvers.Logins = []string{"octocat_acme"} }, false},
		{"trigger equals approve", func(p *patchyv1.Project) {
			p.Spec.Labels = patchyv1.ProjectLabels{Trigger: "patchy:go", Approve: "patchy:go"}
		}, true},
		{"trigger equals the defaulted approve label, case-insensitively", func(p *patchyv1.Project) {
			p.Spec.Labels = patchyv1.ProjectLabels{Trigger: "Patchy:Approved"}
		}, true},
		// The derived trigger (patchy:<name>) must not be the approve label.
		{"a project named approved with a derived trigger", func(p *patchyv1.Project) { p.Name = "approved" }, true},
		{"a project named approved with its own trigger", func(p *patchyv1.Project) {
			p.Name, p.Spec.Labels.Trigger = "approved", "patchy:target"
		}, false},
		{"a derived trigger equal to a custom approve label, case-insensitively", func(p *patchyv1.Project) {
			p.Name, p.Spec.Labels.Approve = "ship", "Patchy:Ship"
		}, true},
		{"a derived trigger apart from a custom approve label", func(p *patchyv1.Project) {
			p.Name, p.Spec.Labels.Approve = "ship", "patchy:ok"
		}, false},
		{"a 50-character label", func(p *patchyv1.Project) { p.Spec.Labels.Trigger = strings.Repeat("l", 50) }, false},
		{"a 51-character label", func(p *patchyv1.Project) { p.Spec.Labels.Trigger = strings.Repeat("l", 51) }, true},
		{"cost ceiling at its cap", func(p *patchyv1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1000000000 }, false},
		{"cost ceiling past its cap", func(p *patchyv1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1000000001 }, true},
		{"negative cost ceiling", func(p *patchyv1.Project) { p.Spec.Limits.MaxCostMicroUSD = -1 }, true},
		{"twenty revisions", func(p *patchyv1.Project) { n := int32(20); p.Spec.Limits.MaxRevisions = &n }, false},
		{"twenty-one revisions", func(p *patchyv1.Project) { n := int32(21); p.Spec.Limits.MaxRevisions = &n }, true},
		{"negative check fixes", func(p *patchyv1.Project) { n := int32(-1); p.Spec.Limits.MaxCheckFixes = &n }, true},
		{"twenty-one active intents", func(p *patchyv1.Project) { p.Spec.Limits.MaxActiveIntents = 21 }, true},
		{"stage turns past the bound", func(p *patchyv1.Project) { p.Spec.Limits.Build.MaxTurns = 1001 }, true},
		{"stage tokens past the bound", func(p *patchyv1.Project) { p.Spec.Limits.Revise.TokenBudget = 100000001 }, true},
		{"thirty-three fixed checks", func(p *patchyv1.Project) {
			p.Spec.Checks.Fix = logins(33)
		}, true},
		{"check timeout under a minute", func(p *patchyv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: 30 * time.Second}
		}, true},
		{"check timeout over six hours", func(p *patchyv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: 7 * time.Hour}
		}, true},
		{"check timeout of one hour", func(p *patchyv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: time.Hour}
		}, false},
		// The name budget: every Intent and IntentRun name derived from the
		// Project's must fit in a label value.
		{"a name at the name budget", func(p *patchyv1.Project) {
			p.Name = strings.Repeat("p", patchyv1.MaxProjectNameLength)
		}, false},
		{"a name past the name budget", func(p *patchyv1.Project) {
			p.Name = strings.Repeat("p", patchyv1.MaxProjectNameLength+1)
		}, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := schemaProject(fmt.Sprintf("proj-bounds-%d", i))
			tt.mutate(p)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// testProjectStatus writes a populated status through the subresource and
// reads it back, so a field the generated schema prunes fails here.
func testProjectStatus(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("status round-trips and is bounded", func(t *testing.T) {
		p := schemaProject("proj-status")
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		want := patchyv1.ProjectStatus{
			Conditions: []metav1.Condition{{
				Type: patchyv1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1,
				LastTransitionTime: schemaNow, Reason: patchyv1.ReasonAmbiguousIntentRepository,
				Message: "project other shares https://github.com/acme/intents with the same trigger",
			}},
			ObservedGeneration: 1,
			ActiveIntents:      2,
			LastPolledAt:       schemaNow.DeepCopy(),
		}
		p.Status = *want.DeepCopy()
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatalf("Status().Update(project) = %v, want nil", err)
		}
		got := &patchyv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		sameJSON(t, "project status", got.Status, want)
		got.Status.ActiveIntents = -1
		if err := c.Status().Update(ctx, got); err == nil {
			t.Error("Status().Update(activeIntents=-1) = nil, want minimum rejection")
		}
	})
}

// schemaIntent is a valid Intent for issue `issue` of Project target, named
// as the schema requires (<project>-<issue>).
func schemaIntent(issue int64) *patchyv1.Intent {
	return &patchyv1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: patchyv1.IntentName("target", issue), Namespace: "default"},
		Spec: patchyv1.IntentSpec{
			Project: "target",
			Issue: patchyv1.IntentIssue{
				Repository: "https://github.com/acme/intents",
				Number:     issue,
				URL:        fmt.Sprintf("https://github.com/acme/intents/issues/%d", issue),
			},
			RequestedBy: patchyv1.IntentRequest{Login: "octocat", At: schemaNow, EventID: 42},
		},
	}
}

// fullIntentStatus populates every Intent status field at a legal value.
func fullIntentStatus() patchyv1.IntentStatus {
	return patchyv1.IntentStatus{
		Phase: patchyv1.IntentInReview,
		PhaseTimes: []patchyv1.IntentPhaseTime{
			{Phase: patchyv1.IntentPending, At: schemaNow},
			{Phase: patchyv1.IntentInReview, At: schemaNow},
		},
		Conditions: []metav1.Condition{{
			Type: patchyv1.ConditionRevisionLimitReached, Status: metav1.ConditionFalse,
			LastTransitionTime: schemaNow, Reason: "WithinLimit", Message: "1 of 3 revisions",
		}},
		ObservedGeneration: 1,
		Input:              &patchyv1.IntentInput{Revision: 1, Digest: schemaDigest, ConfigMap: "target-1-input-r1"},
		Plan: &patchyv1.IntentPlan{
			Revision: 1, Digest: schemaDigest, ConfigMap: "target-1-plan-r1",
			CommentID: 1001, CommentDigest: schemaDigest, PostedAt: schemaNow.DeepCopy(),
			Summary: "Add GET /version", Repositories: []string{"https://github.com/acme/shop"},
		},
		Approval: &patchyv1.IntentApproval{
			By: "octocat", Source: patchyv1.IntentActionLabel, EventID: 43, At: schemaNow, PlanRevision: 1,
			PlanDigest: schemaDigest, InputDigest: schemaDigest,
		},
		LastTrigger: &patchyv1.IntentAction{
			Source: patchyv1.IntentActionCommand, EventID: 44, Login: "octocat", At: schemaNow,
		},
		Branch: "patchy-intent/target-1",
		PullRequests: []patchyv1.IntentPullRequest{{
			Repository: "https://github.com/acme/shop", Number: 7,
			URL: "https://github.com/acme/shop/pull/7", NodeID: "PR_kwDOAbCdEf", HeadSHA: schemaSHA,
			State: "merged", MergedAt: schemaNow.DeepCopy(), MergeCommitSHA: schemaSHA,
		}},
		Rounds:     3,
		Revisions:  1,
		CheckFixes: 1,
		Usage: patchyv1.IntentUsage{
			CostMicroUSD: 1234567, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40,
		},
		Tracking:    &patchyv1.IntentTracking{StatusCommentID: 1000, StatusDigest: schemaDigest},
		ActiveRun:   &patchyv1.ObjectReference{Name: "target-1-rev1-shop-a1", UID: "u-1"},
		CompletedAt: schemaNow.DeepCopy(),
	}
}

// testIntentSchema exercises the Intent schema: spec immutability except
// suspend (every other field refused one at a time), the spec bounds, and a
// full status round-trip plus each status bound.
func testIntentSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	key := client.ObjectKey{Name: "target-1", Namespace: "default"}
	if err := c.Create(ctx, schemaIntent(1)); err != nil {
		t.Fatalf("Create(intent) = %v, want nil", err)
	}
	fresh := func(t *testing.T) *patchyv1.Intent {
		t.Helper()
		i := &patchyv1.Intent{}
		if err := c.Get(ctx, key, i); err != nil {
			t.Fatalf("Get(intent) = %v", err)
		}
		return i
	}

	t.Run("suspend is mutable", func(t *testing.T) {
		i := fresh(t)
		i.Spec.Suspend = true
		if err := c.Update(ctx, i); err != nil {
			t.Fatalf("Update(spec.suspend=true) = %v, want nil", err)
		}
		if !fresh(t).Spec.Suspend {
			t.Error("spec.suspend = false after update, want true")
		}
	})

	for _, tt := range []struct {
		name   string
		mutate func(*patchyv1.IntentSpec)
	}{
		{"project", func(s *patchyv1.IntentSpec) { s.Project = "other" }},
		{"issue number", func(s *patchyv1.IntentSpec) { s.Issue.Number = 2 }},
		{"issue repository", func(s *patchyv1.IntentSpec) { s.Issue.Repository = "https://github.com/acme/other" }},
		{"issue url", func(s *patchyv1.IntentSpec) { s.Issue.URL = "https://github.com/acme/intents/issues/2" }},
		{"issue url removed", func(s *patchyv1.IntentSpec) { s.Issue.URL = "" }},
		{"requester login", func(s *patchyv1.IntentSpec) { s.RequestedBy.Login = "mallory" }},
		{"requester event", func(s *patchyv1.IntentSpec) { s.RequestedBy.EventID = 99 }},
		{"requester time", func(s *patchyv1.IntentSpec) { s.RequestedBy.At = metav1.NewTime(schemaNow.Add(time.Hour)) }},
	} {
		t.Run("spec "+tt.name+" is immutable", func(t *testing.T) {
			i := fresh(t)
			tt.mutate(&i.Spec)
			if err := c.Update(ctx, i); err == nil {
				t.Errorf("Update(spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}

	// Each case mutates a valid spec and is then named from it as the
	// schema requires, unless it names itself.
	for n, tt := range []struct {
		name    string
		mutate  func(*patchyv1.IntentSpec)
		rename  string
		wantErr bool
	}{
		{"a bot requester", func(s *patchyv1.IntentSpec) { s.RequestedBy.Login = "github-actions[bot]" }, "", false},
		{"a requester with a space", func(s *patchyv1.IntentSpec) { s.RequestedBy.Login = "octo cat" }, "", true},
		{"issue number zero", func(s *patchyv1.IntentSpec) { s.Issue.Number = 0 }, "", true},
		{"requester event zero", func(s *patchyv1.IntentSpec) { s.RequestedBy.EventID = 0 }, "", true},
		{"issue repository not a url", func(s *patchyv1.IntentSpec) { s.Issue.Repository = "acme/intents" }, "", true},
		{"issue url not https", func(s *patchyv1.IntentSpec) { s.Issue.URL = "javascript:alert(1)" }, "", true},
		// The name budget: the Intent name is at most 33 characters.
		{"the longest project and issue", func(s *patchyv1.IntentSpec) {
			s.Project, s.Issue.Number = strings.Repeat("p", patchyv1.MaxProjectNameLength), patchyv1.MaxIntentIssueNumber
		}, "", false},
		{"a project past the name budget", func(s *patchyv1.IntentSpec) {
			s.Project = strings.Repeat("p", patchyv1.MaxProjectNameLength+1)
		}, "", true},
		{"an issue number past seven digits", func(s *patchyv1.IntentSpec) {
			s.Issue.Number = patchyv1.MaxIntentIssueNumber + 1
		}, "", true},
		{"a name other than <project>-<issue>", func(*patchyv1.IntentSpec) {}, "target-other", true},
		{"a name for another issue", func(*patchyv1.IntentSpec) {}, "target-2", true},
		{"a name for another project", func(*patchyv1.IntentSpec) {}, "targets-1", true},
		{"a name with no issue", func(*patchyv1.IntentSpec) {}, "target-", true},
		{"a name padding the issue with a zero", func(s *patchyv1.IntentSpec) { s.Issue.Number = 7 }, "target-07", true},
		{"a name carrying a suffix", func(s *patchyv1.IntentSpec) { s.Issue.Number = 8 }, "target-8-x", true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			i := schemaIntent(int64(100 + n))
			tt.mutate(&i.Spec)
			i.Name = patchyv1.IntentName(i.Spec.Project, i.Spec.Issue.Number)
			if tt.rename != "" {
				i.Name = tt.rename
			}
			if err := c.Create(ctx, i); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	t.Run("status round-trips", func(t *testing.T) {
		i := fresh(t)
		i.Status = fullIntentStatus()
		if err := c.Status().Update(ctx, i); err != nil {
			t.Fatalf("Status().Update(full intent status) = %v, want nil", err)
		}
		sameJSON(t, "intent status", fresh(t).Status, fullIntentStatus())
	})

	phaseTimes := func(n int) []patchyv1.IntentPhaseTime {
		out := make([]patchyv1.IntentPhaseTime, n)
		for k := range out {
			out[k] = patchyv1.IntentPhaseTime{Phase: patchyv1.IntentPlanning, At: schemaNow}
		}
		return out
	}
	prs := func(n int) []patchyv1.IntentPullRequest {
		out := make([]patchyv1.IntentPullRequest, n)
		for k := range out {
			out[k] = patchyv1.IntentPullRequest{Repository: fmt.Sprintf("https://github.com/acme/app%d", k), Number: 1}
		}
		return out
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*patchyv1.IntentStatus)
		wantErr bool
	}{
		{"an unknown phase", func(s *patchyv1.IntentStatus) { s.Phase = "Bogus" }, true},
		{"a finding phase", func(s *patchyv1.IntentStatus) { s.Phase = "Remediated" }, true},
		{"64 phase times", func(s *patchyv1.IntentStatus) { s.PhaseTimes = phaseTimes(patchyv1.MaxIntentPhaseTimes) }, false},
		{"65 phase times", func(s *patchyv1.IntentStatus) {
			s.PhaseTimes = phaseTimes(patchyv1.MaxIntentPhaseTimes + 1)
		}, true},
		{"8 pull requests", func(s *patchyv1.IntentStatus) { s.PullRequests = prs(8) }, false},
		{"9 pull requests", func(s *patchyv1.IntentStatus) { s.PullRequests = prs(9) }, true},
		{"two pull requests in one repository", func(s *patchyv1.IntentStatus) {
			s.PullRequests = append(prs(1), patchyv1.IntentPullRequest{Repository: "https://github.com/acme/app0", Number: 2})
		}, true},
		{"a pull request state outside the enum", func(s *patchyv1.IntentStatus) { s.PullRequests[0].State = "draft" }, true},
		{"a short head sha", func(s *patchyv1.IntentStatus) { s.PullRequests[0].HeadSHA = "abc123" }, true},
		{"a sha-256 head", func(s *patchyv1.IntentStatus) { s.PullRequests[0].HeadSHA = strings.Repeat("b", 64) }, false},
		{"a finding branch", func(s *patchyv1.IntentStatus) { s.Branch = "patchy/finding-abc" }, true},
		{"a branch outside the intent prefix", func(s *patchyv1.IntentStatus) { s.Branch = "main" }, true},
		{"a bare-hex digest", func(s *patchyv1.IntentStatus) { s.Input.Digest = strings.Repeat("d", 64) }, true},
		{"a bot approval", func(s *patchyv1.IntentStatus) { s.Approval.By = "renovate[bot]" }, true},
		// An event id is meaningless without the id space it is from.
		{"an approval without its source", func(s *patchyv1.IntentStatus) { s.Approval.Source = "" }, true},
		{"an approval from an unknown source", func(s *patchyv1.IntentStatus) { s.Approval.Source = "review" }, true},
		{"an approval by command", func(s *patchyv1.IntentStatus) {
			s.Approval.Source = patchyv1.IntentActionCommand
		}, false},
		{"a trigger without its source", func(s *patchyv1.IntentStatus) { s.LastTrigger.Source = "" }, true},
		{"a trigger without its event", func(s *patchyv1.IntentStatus) { s.LastTrigger.EventID = 0 }, true},
		{"a refused bot trigger", func(s *patchyv1.IntentStatus) { s.LastTrigger.Login = "dependabot[bot]" }, false},
		{"a trigger by a malformed login", func(s *patchyv1.IntentStatus) { s.LastTrigger.Login = "octo cat" }, true},
		{"a 201-character plan summary", func(s *patchyv1.IntentStatus) { s.Plan.Summary = strings.Repeat("s", 201) }, true},
		{"nine planned repositories", func(s *patchyv1.IntentStatus) {
			s.Plan.Repositories = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		}, true},
		{"a negative cost", func(s *patchyv1.IntentStatus) { s.Usage.CostMicroUSD = -1 }, true},
		{"a plan revision of zero", func(s *patchyv1.IntentStatus) { s.Plan.Revision = 0 }, true},
		// Rounds and revisions name runs, so they stay inside the name budget.
		{"rounds at the bound", func(s *patchyv1.IntentStatus) { s.Rounds = patchyv1.MaxIntentRound }, false},
		{"rounds past the bound", func(s *patchyv1.IntentStatus) { s.Rounds = patchyv1.MaxIntentRound + 1 }, true},
		{"an input revision past the bound", func(s *patchyv1.IntentStatus) {
			s.Input.Revision = patchyv1.MaxIntentRound + 1
		}, true},
		{"a plan revision past the bound", func(s *patchyv1.IntentStatus) {
			s.Plan.Revision = patchyv1.MaxIntentRound + 1
		}, true},
		{"an approved revision past the bound", func(s *patchyv1.IntentStatus) {
			s.Approval.PlanRevision = patchyv1.MaxIntentRound + 1
		}, true},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			i := fresh(t)
			i.Status = fullIntentStatus()
			tt.mutate(&i.Status)
			if err := c.Status().Update(ctx, i); (err != nil) != tt.wantErr {
				t.Errorf("Status().Update(intent: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// schemaIntentRun is a valid run of the given stage, each at round 1: a plan
// run at input revision 1, a build run of approved plan revision 1, a revise
// run pinning that plan and taking its image from the build round.
func schemaIntentRun(name string, stage patchyv1.IntentStage) *patchyv1.IntentRun {
	r := &patchyv1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: patchyv1.IntentRunSpec{
			IntentRef: patchyv1.ObjectReference{Name: "target-1", UID: "u-intent"},
			Stage:     stage,
			Repository: patchyv1.IntentRunRepository{
				URL:           "https://github.com/acme/shop",
				RepositoryRef: patchyv1.LocalObjectReference{Name: name + "-src"},
			},
			Round:   1,
			Attempt: 1,
			Inputs:  patchyv1.IntentRunInputs{ConfigMap: name + "-input", InputDigest: schemaDigest},
			Grant:   patchyv1.IntentRunGrant{MaxTurns: 40, TokenBudget: 200000, TimeoutMilliseconds: 1200000},
		},
	}
	switch stage {
	case patchyv1.IntentStageBuild:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
	case patchyv1.IntentStageRevise:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
		r.Spec.Trigger = patchyv1.IntentRunTriggerReview
		r.Spec.Inputs.ReviewIDs = []int64{5001}
		r.Spec.ImageFrom = &patchyv1.ObjectReference{Name: "target-1-bld-r1-shop-a1", UID: "u-r0"}
	}
	return r
}

// testIntentRunSchema exercises the IntentRun schema: spec immutability, the
// per-stage invariants (round, plan pin, trigger, imageFrom), the slice 1b
// consumption records (each trigger's own, required, and only on its kind of
// round) and their bounds, the name backstop, and a full status round-trip.
func testIntentRunSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	stages := []patchyv1.IntentStage{patchyv1.IntentStagePlan, patchyv1.IntentStageBuild, patchyv1.IntentStageRevise}
	for _, stage := range stages {
		if err := c.Create(ctx, schemaIntentRun("target-1-"+string(stage), stage)); err != nil {
			t.Fatalf("Create(valid %s run) = %v, want nil", stage, err)
		}
	}
	key := client.ObjectKey{Name: "target-1-revise", Namespace: "default"}
	fresh := func(t *testing.T) *patchyv1.IntentRun {
		t.Helper()
		r := &patchyv1.IntentRun{}
		if err := c.Get(ctx, key, r); err != nil {
			t.Fatalf("Get(intent run) = %v", err)
		}
		return r
	}

	for _, tt := range []struct {
		name   string
		mutate func(*patchyv1.IntentRunSpec)
	}{
		{"round", func(s *patchyv1.IntentRunSpec) { s.Round = 2 }},
		{"consumed reviews", func(s *patchyv1.IntentRunSpec) { s.Inputs.ReviewIDs = append(s.Inputs.ReviewIDs, 5002) }},
		{"image source", func(s *patchyv1.IntentRunSpec) { s.ImageFrom.Name = "target-1-rev1-shop-a1-src" }},
		{"grant", func(s *patchyv1.IntentRunSpec) { s.Grant.MaxTurns = 1000 }},
		{"previous attempt added", func(s *patchyv1.IntentRunSpec) {
			s.PreviousAttempt = &patchyv1.PreviousAttempt{Name: "x", Attempt: 1, Outcome: "timeout"}
		}},
	} {
		t.Run("spec "+tt.name+" is immutable", func(t *testing.T) {
			r := fresh(t)
			tt.mutate(&r.Spec)
			if err := c.Update(ctx, r); err == nil {
				t.Errorf("Update(intent run spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}

	ids := func(n int) []int64 {
		out := make([]int64, n)
		for k := range out {
			out[k] = int64(k + 1)
		}
		return out
	}
	for n, tt := range []struct {
		name    string
		stage   patchyv1.IntentStage
		mutate  func(*patchyv1.IntentRunSpec)
		wantErr bool
	}{
		{"an unknown stage", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) { s.Stage = "deploy" }, true},
		{"an unknown trigger", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) { s.Trigger = "manual" }, true},
		// Each trigger records what it consumed, and each record belongs
		// to its own kind of round: the exactly-once guarantee.
		{"a command round consuming the reviews since the last round", patchyv1.IntentStageRevise,
			func(s *patchyv1.IntentRunSpec) {
				s.Trigger, s.Inputs.CommandID = patchyv1.IntentRunTriggerCommand, 9001
			}, false},
		{"a command round with no reviews", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger, s.Inputs.CommandID, s.Inputs.ReviewIDs = patchyv1.IntentRunTriggerCommand, 9001, nil
		}, false},
		{"a checks trigger", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CheckRunIDs = patchyv1.IntentRunTriggerChecks, nil, ids(32)
		}, false},
		{"a review round without its reviews", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = nil
		}, true},
		{"a checks round without its check runs", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs = patchyv1.IntentRunTriggerChecks, nil
		}, true},
		{"a command round without its command", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger = patchyv1.IntentRunTriggerCommand
		}, true},
		{"a checks round consuming reviews", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger, s.Inputs.CheckRunIDs = patchyv1.IntentRunTriggerChecks, ids(1)
		}, true},
		{"a review round consuming check runs", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.CheckRunIDs = ids(1)
		}, true},
		{"a review round consuming a command", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.CommandID = 9001
		}, true},
		{"a plan run consuming reviews", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(1)
		}, true},
		{"a build run consuming check runs", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.CheckRunIDs = ids(1)
		}, true},
		{"a build run consuming a command", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.CommandID = 9001
		}, true},
		{"a plan run at round 0", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) { s.Round = 0 }, true},
		{"a build run at round 0", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Round, s.Inputs.PlanRevision = 0, 0
		}, true},
		// A build's round is its plan revision, so a build after a revival
		// and a new approval is a new round, never the first build's name.
		{"a build run of a later plan revision", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Round, s.Inputs.PlanRevision = 3, 3
		}, false},
		{"a build run at a round other than its plan revision", patchyv1.IntentStageBuild,
			func(s *patchyv1.IntentRunSpec) { s.Round = 2 }, true},
		{"a revise run at round 0", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) { s.Round = 0 }, true},
		{"a round at the bound", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Round = patchyv1.MaxIntentRound
		}, false},
		{"a round past the bound", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Round = patchyv1.MaxIntentRound + 1
		}, true},
		{"a plan revision past the bound", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.PlanRevision = patchyv1.MaxIntentRound + 1
		}, true},
		{"a build run without the plan digest", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.PlanDigest = ""
		}, true},
		{"a build run without the plan revision", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.PlanRevision = 0
		}, true},
		{"a revise run without the plan", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.PlanRevision, s.Inputs.PlanDigest = 0, ""
		}, true},
		{"a revise run without a trigger", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger = ""
		}, true},
		{"a revise run without the build image", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.ImageFrom = nil
		}, true},
		{"a build run with a trigger", patchyv1.IntentStageBuild, func(s *patchyv1.IntentRunSpec) {
			s.Trigger = patchyv1.IntentRunTriggerReview
		}, true},
		{"a plan run with an image source", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.ImageFrom = &patchyv1.ObjectReference{Name: "r0"}
		}, true},
		{"attempt zero", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) { s.Attempt = 0 }, true},
		{"attempt seventeen", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) { s.Attempt = 17 }, true},
		{"32 consumed reviews", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(32)
		}, false},
		{"33 consumed reviews", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(33)
		}, true},
		{"33 consumed check runs", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CheckRunIDs = patchyv1.IntentRunTriggerChecks, nil, ids(33)
		}, true},
		{"a review consumed twice", patchyv1.IntentStageRevise, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = []int64{7, 7}
		}, true},
		{"a timeout past a day", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Grant.TimeoutMilliseconds = 86400001
		}, true},
		{"a bare-hex input digest", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Inputs.InputDigest = strings.Repeat("d", 64)
		}, true},
		{"credentials in the repository url", patchyv1.IntentStagePlan, func(s *patchyv1.IntentRunSpec) {
			s.Repository.URL = "https://x:token@github.com/acme/shop"
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			r := schemaIntentRun(fmt.Sprintf("target-1-case-%d", n), tt.stage)
			tt.mutate(&r.Spec)
			if err := c.Create(ctx, r); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	// The backstop behind the name budget: a run name is a label value.
	for _, tt := range []struct {
		size    int
		wantErr bool
	}{{63, false}, {64, true}} {
		t.Run(fmt.Sprintf("create with a %d-character name", tt.size), func(t *testing.T) {
			name := "target-1-plan-r1-a1-" + strings.Repeat("n", tt.size-len("target-1-plan-r1-a1-"))
			if err := c.Create(ctx, schemaIntentRun(name, patchyv1.IntentStagePlan)); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run named %d characters) = %v, wantErr %v", tt.size, err, tt.wantErr)
			}
		})
	}

	fullStatus := func() patchyv1.IntentRunStatus {
		return patchyv1.IntentRunStatus{
			Phase:  patchyv1.RunComplete,
			JobRef: &patchyv1.JobReference{Namespace: "patchy-agents", Name: "patchy-abc-int-a1", UID: "u-job"},
			RunnerImage: &patchyv1.RunnerImageRef{
				Image:    "ghcr.io/acme/go-agent-env@sha256:" + strings.Repeat("a", 64),
				Source:   patchyv1.RunnerImageSourceRepository,
				Manifest: ".patchy/agent.yaml",
			},
			BaseSHA:      schemaSHA,
			PushedCommit: strings.Repeat("c", 40),
			Outcome:      "ok",
			Report:       "## Changes\n",
			Detail:       "pushed",
			Usage:        patchyv1.UsageSummary{InputTokens: 1, OutputTokens: 2, CostUSD: "0.123456"},
			Transcript:   &patchyv1.TranscriptRef{Name: "target-1-revise-transcript", Turns: 12},
			StartedAt:    schemaNow.DeepCopy(),
			FinishedAt:   schemaNow.DeepCopy(),
			Conditions: []metav1.Condition{{
				Type: patchyv1.ConditionComplete, Status: metav1.ConditionTrue,
				LastTransitionTime: schemaNow, Reason: "ok", Message: "run complete",
			}},
			ObservedGeneration: 1,
		}
	}
	t.Run("status round-trips", func(t *testing.T) {
		r := fresh(t)
		r.Status = fullStatus()
		if err := c.Status().Update(ctx, r); err != nil {
			t.Fatalf("Status().Update(full intent run status) = %v, want nil", err)
		}
		sameJSON(t, "intent run status", fresh(t).Status, fullStatus())
	})
	for _, tt := range []struct {
		name    string
		mutate  func(*patchyv1.IntentRunStatus)
		wantErr bool
	}{
		{"a 64 KiB report", func(s *patchyv1.IntentRunStatus) { s.Report = strings.Repeat("r", 65536) }, false},
		{"a report past 64 KiB", func(s *patchyv1.IntentRunStatus) { s.Report = strings.Repeat("r", 65537) }, true},
		{"a detail past 4 KiB", func(s *patchyv1.IntentRunStatus) { s.Detail = strings.Repeat("d", 4097) }, true},
		{"an outcome past 64", func(s *patchyv1.IntentRunStatus) { s.Outcome = strings.Repeat("o", 65) }, true},
		{"a malformed pushed commit", func(s *patchyv1.IntentRunStatus) { s.PushedCommit = "HEAD" }, true},
		{"an unknown run phase", func(s *patchyv1.IntentRunStatus) { s.Phase = "Bogus" }, true},
		{"an unknown image source", func(s *patchyv1.IntentRunStatus) { s.RunnerImage.Source = "bogus" }, true},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			r := fresh(t)
			r.Status = fullStatus()
			tt.mutate(&r.Status)
			if err := c.Status().Update(ctx, r); (err != nil) != tt.wantErr {
				t.Errorf("Status().Update(intent run: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}
