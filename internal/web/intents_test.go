// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// leaks are strings that must never reach a viewer: a node name, a private
// registry host with its account ID.
var leaks = []string{leakNode, leakRegistry, "111122223333", "ip-10-"}

func assertNoLeak(t *testing.T, where, body string) {
	t.Helper()
	for _, l := range leaks {
		if strings.Contains(body, l) {
			t.Errorf("%s leaks %q: %s", where, l, body)
		}
	}
}

func TestIntentRoutesRequireASession(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, nil)
	for _, path := range []string{
		"/api/me", "/api/intents", "/api/intents/events", "/api/intents/alpha-7",
		"/api/intents/alpha-7/plans/1", "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1",
		"/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream",
	} {
		if code, _ := get(t, ts, path); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, code)
		}
	}
}

func TestMeListsOnlyGrantedProjects(t *testing.T) {
	s, _ := intentsServer(t, nil)
	cases := []struct {
		id   *auth.Identity
		want []ProjectAccess
	}{
		{viewerAlpha, []ProjectAccess{{Name: "alpha", Tier: "intents"}}},
		{readerAlpha, []ProjectAccess{{Name: "alpha", Tier: "transcripts"}}},
		{viewerBeta, []ProjectAccess{{Name: "beta", Tier: "intents"}}},
		{nobodyCaller, []ProjectAccess{}},
	}
	for _, tc := range cases {
		code, body := get(t, as(t, s, tc.id), "/api/me")
		if code != http.StatusOK {
			t.Fatalf("%s: GET /api/me = %d", tc.id.Username, code)
		}
		me := decode[Me](t, body)
		if me.Name != tc.id.DisplayName || !me.LoggedIn || !slices.Equal(me.Projects, tc.want) {
			t.Errorf("%s: me = %+v, want projects %v", tc.id.Username, me, tc.want)
		}
	}
}

// The board shows each caller only the Projects they hold a tier on; a
// Project they do not see is absent, not listed empty.
func TestBoardIsFilteredByProject(t *testing.T) {
	s, _ := intentsServer(t, nil)
	cases := []struct {
		id       *auth.Identity
		projects []string
		intents  []string
	}{
		{viewerAlpha, []string{"alpha"}, []string{"alpha-7"}},
		{viewerBeta, []string{"beta"}, []string{"beta-3"}},
		{nobodyCaller, nil, nil},
	}
	for _, tc := range cases {
		code, body := get(t, as(t, s, tc.id), "/api/intents")
		if code != http.StatusOK {
			t.Fatalf("%s: GET /api/intents = %d", tc.id.Username, code)
		}
		b := decode[IntentBoard](t, body)
		var projects, intents []string
		for _, p := range b.Projects {
			projects = append(projects, p.Name)
		}
		for _, c := range b.Intents {
			intents = append(intents, c.Name)
		}
		if !slices.Equal(projects, tc.projects) || !slices.Equal(intents, tc.intents) {
			t.Errorf("%s: board projects=%v intents=%v, want %v %v", tc.id.Username, projects, intents,
				tc.projects, tc.intents)
		}
		assertNoLeak(t, "board for "+tc.id.Username, body)
	}
}

func TestBoardCardProjection(t *testing.T) {
	s, _ := intentsServer(t, nil)
	_, body := get(t, as(t, s, viewerAlpha), "/api/intents")
	b := decode[IntentBoard](t, body)
	if len(b.Intents) != 1 {
		t.Fatalf("intents = %+v", b.Intents)
	}
	want := IntentCard{
		Name: "alpha-7", Project: "alpha", Issue: 7, IssueURL: "https://github.com/acme/intents/issues/7",
		Phase: "Building", Column: "building",
		// Agent text is made visible: the zero-width space in the summary
		// arrives as its code point.
		Summary:      "Add a health endpoint[U+200B]",
		Repositories: []string{"acme/app"},
		PullRequests: []IntentPR{{Repository: "acme/app", Number: 12,
			URL: "https://github.com/acme/app/pull/12", State: "open"}},
		Preview: &PreviewLink{Phase: "Ready", URL: "https://alpha-7.preview.example.com/",
			Revision: "dddddddddddd"},
		RunningRuns: []RunningRun{{Name: "alpha-7-bld-r1-app-a2", Stage: "build", Repository: "acme/app",
			Round: 1, Attempt: 2, Phase: "Running", StartedAt: "2026-07-21T11:10:00Z"}},
		Attempt:      &AttemptCount{Stage: "build", Current: 2, Max: 2},
		CostMicroUSD: 1_250_000, RequestedBy: "octo", RequestedAt: "2026-07-21T10:00:00Z",
		PhaseSince: "2026-07-21T11:50:00Z",
	}
	if got := b.Intents[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("card =\n%+v\nwant\n%+v", got, want)
	}
	wantLimits := IntentLimits{MaxRevisions: 3, MaxCheckFixes: 2, MaxCostMicroUSD: 10_000_000, MaxAttempts: 2}
	if b.Projects[0].Limits != wantLimits {
		t.Errorf("limits = %+v", b.Projects[0].Limits)
	}
}

// The revision and check-fix counts a card shows, and the limit-reached
// reasons, are the rounds intent-controller holds against the limits: every
// round started, failed ones included, not status.revisions and
// status.checkFixes, which count completed rounds only. Here two failed
// check-fix rounds and one failed review round have blocked an intent whose
// completed-round counters are still zero.
func TestBoardCountsRoundsAsTheLimitsDo(t *testing.T) {
	objs := intentsFixture(t)
	var beta *v1alpha1.Project
	var in *v1alpha1.Intent
	for _, o := range objs {
		switch v := o.(type) {
		case *v1alpha1.Project:
			if v.Name == "beta" {
				beta = v
			}
		case *v1alpha1.Intent:
			if v.Name == "beta-3" {
				in = v
			}
		}
	}
	one, two := int32(1), int32(2)
	beta.Spec.Limits.MaxRevisions, beta.Spec.Limits.MaxCheckFixes = &one, &two
	in.Status.Phase = v1alpha1.IntentBlocked
	in.Status.Revisions, in.Status.CheckFixes = 0, 0
	in.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionRevisionLimitReached, Status: metav1.ConditionTrue, Reason: "MaxRevisions"},
		{Type: v1alpha1.ConditionChecksFailing, Status: metav1.ConditionTrue, Reason: "MaxCheckFixes"},
	}
	revise := func(round, attempt int32, trigger v1alpha1.IntentRunTrigger, outcome string) *v1alpha1.IntentRun {
		return &v1alpha1.IntentRun{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("beta-3-rev-r%d-a%d", round, attempt), Namespace: "patchy",
				UID: types.UID(fmt.Sprintf("uid-rev-%d-%d", round, attempt))},
			Spec: v1alpha1.IntentRunSpec{
				IntentRef: v1alpha1.ObjectReference{Name: "beta-3", UID: "uid-beta-3"},
				Stage:     v1alpha1.IntentStageRevise, Round: round, Attempt: attempt, Trigger: trigger,
				Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/web"},
			},
			Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunFailed, Outcome: outcome},
		}
	}
	objs = append(objs,
		revise(1, 1, v1alpha1.IntentRunTriggerReview, "runtime_error"),
		revise(1, 2, v1alpha1.IntentRunTriggerReview, "timeout"),
		revise(2, 1, v1alpha1.IntentRunTriggerChecks, "timeout"),
		revise(3, 1, v1alpha1.IntentRunTriggerChecks, "runtime_error"),
		// A round refused for want of usable feedback spends no revision.
		revise(4, 1, v1alpha1.IntentRunTriggerCommand, "no_usable_feedback"),
	)
	s, _ := intentsServer(t, nil, objs...)
	_, body := get(t, as(t, s, viewerBeta), "/api/intents")
	c := decode[IntentBoard](t, body).Intents[0]
	if c.Revisions != 1 || c.CheckFixes != 2 {
		t.Errorf("card counts revisions %d, check fixes %d; want 1 and 2, the rounds the limits count",
			c.Revisions, c.CheckFixes)
	}
	want := []string{"the revision limit is reached (1 of 1)", "the check-fix limit is reached (2 of 2)"}
	if !slices.Equal(c.BlockedReasons, want) {
		t.Errorf("blocked reasons = %q, want %q", c.BlockedReasons, want)
	}
}

// A Blocked intent's reasons are public wording, never the condition's
// message, which here names a node and a private registry.
func TestBlockedReasonsArePublic(t *testing.T) {
	s, _ := intentsServer(t, nil)
	_, body := get(t, as(t, s, viewerBeta), "/api/intents")
	b := decode[IntentBoard](t, body)
	c := b.Intents[0]
	if c.Column != "building" || c.BlockedFrom != "Building" ||
		!slices.Equal(c.BlockedReasons, []string{"the repository's agent image was rejected"}) {
		t.Errorf("blocked card = %+v", c)
	}
	assertNoLeak(t, "blocked board", body)
}

// An intent in a Project the caller may not see is a 404 indistinguishable
// from one that does not exist, on every route.
func TestCrossProjectDenial(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, viewerAlpha)
	_, missing := get(t, ts, "/api/intents/alpha-404")
	for _, path := range []string{
		"/api/intents/beta-3",
		"/api/intents/beta-3/plans/1",
		"/api/intents/beta-3/runs/beta-3-bld-r1-web-a1",
		"/api/intents/beta-3/runs/beta-3-bld-r1-web-a1/stream",
	} {
		code, body := get(t, ts, path)
		if code != http.StatusNotFound || body != missing {
			t.Errorf("GET %s as an alpha viewer = %d %q, want the same 404 as a missing intent (%q)",
				path, code, body, missing)
		}
	}
	// Naming another Project's run under a visible intent's path finds
	// nothing either: runs are checked against the intent by name and UID.
	if code, _ := get(t, ts, "/api/intents/alpha-7/runs/beta-3-bld-r1-web-a1"); code != http.StatusNotFound {
		t.Errorf("another intent's run under alpha-7 = %d, want 404", code)
	}
	// A run naming alpha-7 by name but an earlier alpha-7 by UID is not
	// this intent's.
	if code, _ := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-plan-r9-a1"); code != http.StatusNotFound {
		t.Errorf("a stale-UID run = %d, want 404", code)
	}
	_, body := get(t, ts, "/api/intents/alpha-7")
	if strings.Contains(body, "alpha-7-plan-r9-a1") {
		t.Error("the timeline lists a run whose intentRef UID is another intent's")
	}
}

func TestTimelineProjection(t *testing.T) {
	s, _ := intentsServer(t, nil)
	code, body := get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7")
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d %s", code, body)
	}
	d := decode[IntentDetail](t, body)
	if d.Tier != "intents" || len(d.PhaseTimes) != 4 || d.Input == nil {
		t.Fatalf("detail = %+v", d)
	}
	wantPlan := &IntentPlanView{Revision: 1, Digest: "sha256:" + strings.Repeat("b", 64),
		Summary: "Add a health endpoint[U+200B]", Repositories: []string{"acme/app"},
		PostedAt: "2026-07-21T11:21:00Z", CommentURL: "https://github.com/acme/intents/issues/7#issuecomment-99"}
	if !reflect.DeepEqual(d.Plan, wantPlan) {
		t.Errorf("plan = %+v, want %+v", d.Plan, wantPlan)
	}
	wantApproval := &IntentApproval{By: "octo", Source: "label", At: "2026-07-21T11:49:00Z", PlanRevision: 1,
		PlanDigest: "sha256:" + strings.Repeat("b", 64), InputDigest: "sha256:" + strings.Repeat("a", 64)}
	if !reflect.DeepEqual(d.Approval, wantApproval) {
		t.Errorf("approval = %+v, want %+v", d.Approval, wantApproval)
	}
	names := make([]string, 0, len(d.Runs))
	for _, r := range d.Runs {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"alpha-7-plan-r1-a1", "alpha-7-bld-r1-app-a1", "alpha-7-bld-r1-app-a2"}) {
		t.Errorf("runs = %v, want oldest first, the stray run left out", names)
	}
	plan := d.Runs[0]
	wantRow := IntentRunRow{Name: "alpha-7-plan-r1-a1", Stage: "plan", Repository: "acme/app", Round: 1, Attempt: 1,
		Phase: "Complete", Outcome: "ok", Reason: "ok: completed", CreatedAt: "2026-07-21T11:05:00Z",
		StartedAt: "2026-07-21T11:10:00Z", FinishedAt: "2026-07-21T11:20:00Z", CostMicroUSD: 750_000,
		Usage:       &Usage{InputTokens: 1000, OutputTokens: 200, CostMicroUSD: 750_000},
		ImageSource: "default", ImageDigest: "cccccccccccc", Transcript: &TranscriptSummary{Turns: 3},
		Grant: &RunGrant{MaxTurns: 80, TokenBudget: 400000, TimeoutMilliseconds: 1_800_000}}
	if !reflect.DeepEqual(plan, wantRow) {
		t.Errorf("plan run row =\n%+v\nwant\n%+v", plan, wantRow)
	}
	if evicted := d.Runs[1]; evicted.Reason != "evicted: the agent pod was evicted" {
		t.Errorf("evicted reason = %q", evicted.Reason)
	}
	// Tier 1 lists no plan texts to open.
	if len(d.PlanTexts) != 0 {
		t.Errorf("tier 1 plan texts = %v", d.PlanTexts)
	}
	assertNoLeak(t, "timeline", body)

	_, body = get(t, as(t, s, readerAlpha), "/api/intents/alpha-7")
	if d := decode[IntentDetail](t, body); d.Tier != "transcripts" || !slices.Equal(d.PlanTexts, []int32{1}) {
		t.Errorf("tier 2 detail tier=%q planTexts=%v", d.Tier, d.PlanTexts)
	}
}

// Tier 2 content refuses a tier 1 caller who can see the intent: the plan
// with 403, the report by leaving it out.
func TestTier2DeniedToTier1(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, viewerAlpha)
	code, body := get(t, ts, "/api/intents/alpha-7/plans/1")
	if code != http.StatusForbidden || !strings.Contains(body, `may not read plans and transcripts in project "alpha"`) {
		t.Errorf("tier 1 plan = %d %q, want 403", code, body)
	}
	code, body = get(t, ts, "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1")
	if code != http.StatusOK {
		t.Fatalf("tier 1 run = %d", code)
	}
	if run := decode[IntentRunDetail](t, body); run.Report != "" || run.Tier != "intents" {
		t.Errorf("tier 1 run report = %q tier %q, want none", run.Report, run.Tier)
	}

	ts2 := as(t, s, readerAlpha)
	code, body = get(t, ts2, "/api/intents/alpha-7/plans/1")
	if code != http.StatusOK {
		t.Fatalf("tier 2 plan = %d %s", code, body)
	}
	p := decode[IntentPlanText](t, body)
	if !p.Current || p.Revision != 1 || !strings.Contains(p.Text, "Add /healthz.[U+202E]") {
		t.Errorf("plan = %+v, want the current plan with the bidi control made visible", p)
	}
	code, body = get(t, ts2, "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1")
	if run := decode[IntentRunDetail](t, body); code != http.StatusOK || !strings.Contains(run.Report, "# Plan") ||
		strings.Contains(run.Report, "summary:") {
		t.Errorf("tier 2 run = %d report %q, want the report body without its frontmatter", code, run.Report)
	}
}

// The attempt a card and a run panel show is the attempt as
// intent-controller counts it toward the round's two, not the run's ordinal:
// a first attempt no node could fit did not count, so the build's attempt 2
// is still its first that counts, and its failure would be retried, not
// fail the intent. The fixture's evicted first attempt, by contrast, counts.
func TestAttemptsAreCountedAsTheControllerCounts(t *testing.T) {
	objs := intentsFixture(t)
	findRun(objs, "alpha-7-bld-r1-app-a1").Status.Outcome = "unschedulable"
	s, _ := intentsServer(t, nil, objs...)
	ts := as(t, s, viewerAlpha)
	_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2")
	run := decode[IntentRunDetail](t, body)
	if run.LastAttempt || run.CountedAttempt != 1 || run.Attempt != 2 {
		t.Errorf("run panel attempt %d (counted %d), last %v; want ordinal 2, counted 1, not the last",
			run.Attempt, run.CountedAttempt, run.LastAttempt)
	}
	_, body = get(t, ts, "/api/intents")
	if a := decode[IntentBoard](t, body).Intents[0].Attempt; a == nil || a.Current != 1 || a.Max != 2 {
		t.Errorf("card attempt = %+v, want 1 of 2", a)
	}

	// The fixture as it is: attempt 1 was evicted, which counts.
	s, _ = intentsServer(t, nil)
	_, body = get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2")
	if run := decode[IntentRunDetail](t, body); !run.LastAttempt || run.CountedAttempt != 2 {
		t.Errorf("attempt 2 after a counted failure: counted %d, last %v; want 2 and the last",
			run.CountedAttempt, run.LastAttempt)
	}
}

func TestPlanRevisionInput(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, readerAlpha)
	for path, want := range map[string]int{
		"/api/intents/alpha-7/plans/0":          http.StatusBadRequest,
		"/api/intents/alpha-7/plans/1000":       http.StatusBadRequest,
		"/api/intents/alpha-7/plans/x":          http.StatusBadRequest,
		"/api/intents/alpha-7/plans/4294967297": http.StatusBadRequest,
		"/api/intents/alpha-7/plans/2":          http.StatusNotFound,
	} {
		if code, _ := get(t, ts, path); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
}

// A ConfigMap read under a derived or status-named name must carry the
// intent's label and a controller reference to the very owner, or it is not
// served: status-server may get any ConfigMap in its namespace.
func TestConfigMapGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(objs []clientObject)
	}{
		{"plan with another intent's label", func(objs []clientObject) {
			cm := findConfigMap(objs, "alpha-7-plan-r1")
			cm.Labels["patchy.bitwisemedia.uk/intent"] = "beta-3"
		}},
		{"plan with no label", func(objs []clientObject) {
			cm := findConfigMap(objs, "alpha-7-plan-r1")
			cm.Labels = nil
		}},
		{"plan owned by another UID", func(objs []clientObject) {
			cm := findConfigMap(objs, "alpha-7-plan-r1")
			cm.OwnerReferences[0].UID = "uid-older-alpha-7"
		}},
		{"plan owned but not as controller", func(objs []clientObject) {
			cm := findConfigMap(objs, "alpha-7-plan-r1")
			cm.OwnerReferences[0].Controller = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := intentsFixture(t)
			tc.mutate(objs)
			s, _ := intentsServer(t, nil, objs...)
			if code, _ := get(t, as(t, s, readerAlpha), "/api/intents/alpha-7/plans/1"); code != http.StatusNotFound {
				t.Errorf("plan = %d, want 404", code)
			}
		})
	}

	transcriptCases := []struct {
		name   string
		mutate func(objs []clientObject)
	}{
		{"transcript owned by another run", func(objs []clientObject) {
			findConfigMap(objs, "alpha-7-plan-r1-a1-transcript").OwnerReferences[0].UID = "uid-build-run"
		}},
		{"transcript with another intent's label", func(objs []clientObject) {
			findConfigMap(objs, "alpha-7-plan-r1-a1-transcript").Labels["patchy.bitwisemedia.uk/intent"] = "beta-3"
		}},
		{"status naming a ConfigMap that is not the run's own", func(objs []clientObject) {
			run := findRun(objs, "alpha-7-plan-r1-a1")
			run.Status.Transcript.Name = "alpha-7-plan-r1"
		}},
	}
	for _, tc := range transcriptCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := intentsFixture(t)
			tc.mutate(objs)
			s, _ := intentsServer(t, nil, objs...)
			code, body := get(t, as(t, s, readerAlpha), "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream")
			if code != http.StatusOK {
				t.Fatalf("stream = %d", code)
			}
			for _, ev := range sseEvents(body) {
				if ev[0] == eventTurn {
					t.Errorf("a refused transcript streamed a turn: %s", ev[1])
				}
			}
		})
	}
}

// A collected run's stream: tier 2 gets every turn, made visible; tier 1
// the activity alone, with no text.
func TestRunStreamTiersOnPersistedTranscript(t *testing.T) {
	s, _ := intentsServer(t, nil)
	_, body := get(t, as(t, s, readerAlpha), "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream")
	events := sseEvents(body)
	var turns []Turn
	for _, ev := range events {
		if ev[0] == eventTurn {
			turns = append(turns, decode[Turn](t, ev[1]))
		}
	}
	if len(turns) != 3 || events[len(events)-1][0] != eventEnd {
		t.Fatalf("tier 2 events = %v", events)
	}
	if strings.ContainsRune(turns[0].Text, 0x1b) || !strings.Contains(turns[0].Text, "[U+001B]") {
		t.Errorf("turn text = %q, want the terminal escape shown as its code point", turns[0].Text)
	}

	_, body = get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream")
	for _, ev := range sseEvents(body) {
		if ev[0] == eventTurn {
			t.Fatalf("tier 1 received a turn: %s", ev[1])
		}
	}
	if strings.Contains(body, "go test") || strings.Contains(body, "Reading the router") {
		t.Errorf("tier 1 stream carries transcript text: %s", body)
	}
	last := lastActivity(t, body)
	if last.Turns != 3 || last.LastAt != "2026-07-21T11:07:00Z" || last.OpenTool != "" || last.Live {
		t.Errorf("tier 1 activity = %+v", last)
	}
}

func lastActivity(t *testing.T, body string) RunActivity {
	t.Helper()
	var last RunActivity
	found := false
	for _, ev := range sseEvents(body) {
		if ev[0] == eventActivity {
			last, found = decode[RunActivity](t, ev[1]), true
		}
	}
	if !found {
		t.Fatalf("no activity event in %s", body)
	}
	return last
}

// One upstream follow, shared by a tier 1 and a tier 2 viewer of the same
// live run: the stripping happens per subscriber, so whichever arrives
// first, the tier 1 viewer never receives a turn and the tier 2 one gets
// them all.
func TestLiveStreamStripsPerSubscriber(t *testing.T) {
	for _, order := range [][]*auth.Identity{{viewerAlpha, readerAlpha}, {readerAlpha, viewerAlpha}} {
		hold := make(chan struct{})
		tailer := &fakeTailer{turns: fixtureTurns(), hold: hold}
		s, _ := intentsServer(t, tailer)
		bodies := map[string]chan string{}
		for _, id := range order {
			ch := make(chan string, 1)
			bodies[id.Username] = ch
			ts := as(t, s, id)
			go func() {
				_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
				ch <- body
			}()
			// Let each subscribe before the next, so the second is a
			// late joiner on the shared follow.
			waitFor(t, func() bool { return s.tails.activeTails() == 1 })
			time.Sleep(50 * time.Millisecond)
		}
		close(hold)
		tier1, tier2 := <-bodies[viewerAlpha.Username], <-bodies[readerAlpha.Username]
		for _, ev := range sseEvents(tier1) {
			if ev[0] == eventTurn {
				t.Errorf("order %s first: tier 1 received a turn", order[0].Username)
			}
		}
		n := 0
		for _, ev := range sseEvents(tier2) {
			if ev[0] == eventTurn {
				n++
			}
		}
		if n != 3 {
			t.Errorf("order %s first: tier 2 received %d turns, want 3", order[0].Username, n)
		}
		if a := lastActivity(t, tier1); a.Turns != 3 || a.Live {
			t.Errorf("tier 1 final activity = %+v", a)
		}
	}
}

// Once the transcript recorder reaches its cap it records nothing more,
// while the agent goes on working. The activity says so rather than freeze
// on the tool open at the cap, which would read as a stuck run.
func TestLiveActivityMarksTheTranscriptCap(t *testing.T) {
	turns := append(fixtureTurns()[:2], transcript.Turn{Seq: 3, At: "2026-07-21T11:08:00Z",
		Role: transcript.RoleSystem, Kind: transcript.KindNotice, Truncated: true,
		Text: "transcript truncated: 500 turn cap reached"})
	s, _ := intentsServer(t, &fakeTailer{turns: turns})
	_, body := get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
	var capped []bool
	for _, ev := range sseEvents(body) {
		if ev[0] == eventActivity {
			capped = append(capped, strings.Contains(ev[1], `"capped":true`))
		}
	}
	if len(capped) == 0 || !capped[len(capped)-1] {
		t.Errorf("activity never reported the transcript cap: %s", body)
	}
	if a := lastActivity(t, body); a.OpenTool != "" || a.OpenToolSince != "" {
		t.Errorf("activity after the cap = %+v, want no open tool (it is unknown)", a)
	}
}

// A live run whose follow budget is spent says so instead of ending blank.
func TestLiveStreamUnavailableWhenFollowsExhausted(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	s, _ := intentsServer(t, &fakeTailer{hold: hold})
	for i := range maxLiveTails {
		sub, err := s.tails.subscribe("other-job-" + string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
	}
	_, body := get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
	events := sseEvents(body)
	if len(events) < 2 || events[0][0] != eventUnavailable || !strings.Contains(events[0][1], "live view unavailable") {
		t.Errorf("events = %v, want an unavailable notice", events)
	}
}

// An open stream re-checks its grant: revoke it, and the stream ends with
// reason revoked instead of following the run to its end.
func TestOpenStreamIsReauthorised(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	s, g := intentsServer(t, &fakeTailer{turns: fixtureTurns()[:1], hold: hold})
	s.intents.reauth = 30 * time.Millisecond
	done := make(chan string, 1)
	ts := as(t, s, readerAlpha)
	go func() {
		_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
		done <- body
	}()
	waitFor(t, func() bool { return s.tails.activeTails() == 1 })
	g.set(readerAlpha.Username, "alpha", authz.TierNone)
	select {
	case body := <-done:
		events := sseEvents(body)
		if last := events[len(events)-1]; last[0] != eventEnd || !strings.Contains(last[1], endRevoked) {
			t.Errorf("last event = %v, want end revoked", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its revoked grant")
	}

	// A downgrade from tier 2 to tier 1 ends a tier 2 stream too: it must
	// not keep streaming turns its viewer may no longer read.
	g.set(readerAlpha.Username, "alpha", authz.TierTranscripts)
	waitFor(t, func() bool { return s.tails.activeTails() == 0 })
	go func() {
		_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
		done <- body
	}()
	waitFor(t, func() bool { return s.tails.activeTails() == 1 })
	time.Sleep(20 * time.Millisecond)
	g.set(readerAlpha.Username, "alpha", authz.TierIntents)
	select {
	case body := <-done:
		if !strings.Contains(body, endRevoked) {
			t.Errorf("downgraded stream body = %s, want end revoked", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a downgraded stream kept going")
	}
}

func TestStreamMaxAgeEndsForReconnect(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	s, _ := intentsServer(t, &fakeTailer{hold: hold})
	s.intents.maxAge = 50 * time.Millisecond
	_, body := get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
	if !strings.Contains(body, endReconnect) {
		t.Errorf("body = %s, want an end with reason reconnect", body)
	}
}

// open is how many streams the limiter holds (tests).
func (l *streamLimiter) open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// A run still waiting for a run slot when its panel opens (Pending, no Job
// yet) has nothing to follow. Its stream waits for the Job instead of
// ending, then follows the run live once it launches, so a viewer who opened
// a queued run sees it start without reloading.
func TestRunStreamWaitsForLaunch(t *testing.T) {
	objs := intentsFixture(t)
	queued := findRun(objs, "alpha-7-bld-r1-app-a2")
	queued.Status.Phase, queued.Status.JobRef, queued.Status.StartedAt = v1alpha1.RunPending, nil, nil
	s, _ := intentsServer(t, &fakeTailer{turns: fixtureTurns()}, objs...)
	s.intents.runPoll = 10 * time.Millisecond
	ts := as(t, s, readerAlpha)
	done := make(chan string, 1)
	go func() {
		_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
		done <- body
	}()
	// The stream holds its slot while the run waits.
	waitFor(t, func() bool { return s.intents.limiter.open() == 1 })
	time.Sleep(50 * time.Millisecond)
	select {
	case body := <-done:
		t.Fatalf("the stream of a queued run ended before it launched: %s", body)
	default:
	}

	// The run launches: the RunReconciler grants it a slot, then its Job.
	ctx := context.Background()
	var run v1alpha1.IntentRun
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(queued), &run); err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = v1alpha1.RunRunning
	run.Status.JobRef = &v1alpha1.JobReference{Namespace: "patchy-agents", Name: "job-alpha-7-bld"}
	if err := s.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-done:
		turns := 0
		for _, ev := range sseEvents(body) {
			if ev[0] == eventTurn {
				turns++
			}
		}
		if turns != 3 {
			t.Errorf("the launched run streamed %d turns, want 3: %s", turns, body)
		}
		if a := lastActivity(t, body); a.Turns != 3 {
			t.Errorf("final activity = %+v, want 3 turns", a)
		}
		if !strings.Contains(body, `"live":true`) {
			t.Errorf("the stream never reported the run live: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not follow the run once it launched")
	}
}

// A queued run that ends without ever launching (its launch refused) ends
// its stream too, rather than waiting for a Job that will never exist.
func TestRunStreamOfQueuedRunEndsWhenRunEnds(t *testing.T) {
	objs := intentsFixture(t)
	queued := findRun(objs, "alpha-7-bld-r1-app-a2")
	queued.Status.Phase, queued.Status.JobRef, queued.Status.StartedAt = v1alpha1.RunPending, nil, nil
	s, _ := intentsServer(t, &fakeTailer{}, objs...)
	s.intents.runPoll = 10 * time.Millisecond
	ts := as(t, s, viewerAlpha)
	done := make(chan string, 1)
	go func() {
		_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2/stream")
		done <- body
	}()
	waitFor(t, func() bool { return s.intents.limiter.open() == 1 })
	ctx := context.Background()
	var run v1alpha1.IntentRun
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(queued), &run); err != nil {
		t.Fatal(err)
	}
	run.Status.Phase, run.Status.Outcome = v1alpha1.RunFailed, "launch_refused"
	if err := s.client.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-done:
		events := sseEvents(body)
		if len(events) == 0 || events[len(events)-1][0] != eventEnd {
			t.Errorf("events = %v, want a final end", events)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of a run that ended unlaunched kept waiting")
	}
}

func TestPerIdentityStreamCap(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	s, _ := intentsServer(t, &fakeTailer{hold: hold})
	const stream = "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream"
	releases := make([]func(), 0, maxIdentityStreams)
	for range maxIdentityStreams {
		release, ok := s.intents.limiter.acquire(viewerAlpha.Username)
		if !ok {
			t.Fatal("limiter refused below its cap")
		}
		releases = append(releases, release)
	}
	if code, _ := get(t, as(t, s, viewerAlpha), stream); code != http.StatusTooManyRequests {
		t.Errorf("stream past the per-identity cap = %d, want 429", code)
	}
	// Another identity is unaffected.
	if code, _ := get(t, as(t, s, readerAlpha), stream); code != http.StatusOK {
		t.Errorf("another identity's stream = %d, want 200", code)
	}
	for _, r := range releases {
		r()
	}
	if code, _ := get(t, as(t, s, viewerAlpha), stream); code != http.StatusOK {
		t.Errorf("stream after release = %d, want 200", code)
	}
}

// The change signal reaches only subscribers allowed to see the Project that
// changed, so a viewer of one Project cannot time another's activity.
func TestIntentSignalsAreFilteredByProject(t *testing.T) {
	b := newProjectBroker(4)
	alpha, _ := b.subscribe(map[string]bool{"alpha": true})
	beta, _ := b.subscribe(map[string]bool{"beta": true})
	b.publish(map[string]bool{"beta": true})
	select {
	case <-alpha.ch:
		t.Error("an alpha subscriber heard a beta change")
	default:
	}
	select {
	case <-beta.ch:
	default:
		t.Error("the beta subscriber missed a beta change")
	}
	beta.setAllowed(map[string]bool{})
	b.publish(map[string]bool{"beta": true})
	select {
	case <-beta.ch:
		t.Error("a subscriber whose grant went heard a change")
	default:
	}
	b.subscribe(nil)
	b.subscribe(nil)
	if _, ok := b.subscribe(nil); ok {
		t.Error("broker accepted a subscriber past its cap")
	}
}

// A tier 2 read leaves one audit line naming who read what; a tier 1 view
// leaves none, and no line carries content.
func TestTier2ReadsAreAudited(t *testing.T) {
	s, _ := intentsServer(t, nil)
	var logs captureLog
	s.log = logs.logger()
	get(t, as(t, s, viewerAlpha), "/api/intents")
	get(t, as(t, s, viewerAlpha), "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1")
	if strings.Contains(logs.String(), "intent content read") {
		t.Errorf("tier 1 views were audited: %s", logs.String())
	}
	get(t, as(t, s, readerAlpha), "/api/intents/alpha-7/plans/1")
	out := logs.String()
	const line = `msg="intent content read" user=github:rex project=alpha intent=alpha-7 run="" what="plan r1"`
	if !strings.Contains(out, line) {
		t.Errorf("audit line missing or wrong: %s", out)
	}
	if strings.Contains(out, "healthz") {
		t.Errorf("an audit line carried content: %s", out)
	}
}
