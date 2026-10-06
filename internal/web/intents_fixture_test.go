// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// The intents fixture: two Projects, alpha and beta, one intent each, and
// in every place a private string could leak (run detail, condition
// messages, image references) a string the tests then look for.
const (
	leakNode     = "ip-10-0-1-23.ec2.internal"
	leakRegistry = "111122223333.dkr.ecr.us-east-1.amazonaws.com"
	planText     = "---\nsummary: Add a health endpoint\n---\n# Plan\nAdd /healthz.\u202e\n"
)

// tierGranter answers tiers from a per-user table; flip with set.
type tierGranter struct {
	mu    sync.Mutex
	tiers map[string]map[string]authz.Tier
	calls int
}

func (g *tierGranter) Tiers(_ context.Context, id auth.Identity, projects []string) (map[string]authz.Tier, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	out := map[string]authz.Tier{}
	for _, p := range projects {
		out[p] = g.tiers[id.Username][p]
	}
	return out, nil
}

func (g *tierGranter) set(user, project string, t authz.Tier) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tiers[user] == nil {
		g.tiers[user] = map[string]authz.Tier{}
	}
	g.tiers[user][project] = t
}

// The fixture's callers.
var (
	viewerAlpha  = &auth.Identity{Username: "github:viv", DisplayName: "Viv", Session: true} // tier 1 on alpha
	readerAlpha  = &auth.Identity{Username: "github:rex", DisplayName: "Rex", Session: true} // tier 2 on alpha
	viewerBeta   = &auth.Identity{Username: "github:bea", DisplayName: "Bea", Session: true} // tier 1 on beta
	nobodyCaller = &auth.Identity{Username: "github:nob", DisplayName: "Nob", Session: true} // nothing
)

func fixtureGranter() *tierGranter {
	g := &tierGranter{tiers: map[string]map[string]authz.Tier{}}
	g.set(viewerAlpha.Username, "alpha", authz.TierIntents)
	g.set(readerAlpha.Username, "alpha", authz.TierTranscripts)
	g.set(viewerBeta.Username, "beta", authz.TierIntents)
	return g
}

func ctrlRef(kind, name string, uid types.UID) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: uid,
		Controller: &yes}
}

func intentsFixture(t *testing.T) []client.Object {
	t.Helper()
	at := func(d time.Duration) metav1.Time { return metav1.NewTime(testClock.Add(d)) }
	started, finished := at(-50*time.Minute), at(-40*time.Minute)
	posted := at(-39 * time.Minute)
	mr := int32(3)
	alpha := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "patchy"},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octo"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}},
			Limits:           v1alpha1.ProjectLimits{MaxRevisions: &mr, MaxCostMicroUSD: 10_000_000},
		},
	}
	beta := alpha.DeepCopy()
	beta.Name = "beta"
	beta.Spec.Repositories = []v1alpha1.ProjectRepository{{Name: "web", URL: "https://github.com/acme/web"}}

	inAlpha := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7", Namespace: "patchy", UID: "uid-alpha-7"},
		Spec: v1alpha1.IntentSpec{
			Project: "alpha",
			Issue: v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 7,
				URL: "https://github.com/acme/intents/issues/7"},
			RequestedBy: v1alpha1.IntentRequest{Login: "octo", At: at(-2 * time.Hour), EventID: 1},
		},
		Status: v1alpha1.IntentStatus{
			Phase: v1alpha1.IntentBuilding,
			PhaseTimes: []v1alpha1.IntentPhaseTime{
				{Phase: v1alpha1.IntentPending, At: at(-2 * time.Hour)},
				{Phase: v1alpha1.IntentPlanning, At: at(-55 * time.Minute)},
				{Phase: v1alpha1.IntentAwaitingApproval, At: posted},
				{Phase: v1alpha1.IntentBuilding, At: at(-10 * time.Minute)},
			},
			Input: &v1alpha1.IntentInput{Revision: 1, Digest: "sha256:" + strings.Repeat("a", 64),
				ConfigMap: "alpha-7-input-r1"},
			Plan: &v1alpha1.IntentPlan{Revision: 1, Digest: "sha256:" + strings.Repeat("b", 64),
				ConfigMap: "alpha-7-plan-r1", CommentID: 99, PostedAt: &posted,
				Summary:      "Add a health endpoint\u200b",
				Repositories: []string{"https://github.com/acme/app"}},
			Approval: &v1alpha1.IntentApproval{By: "octo", Source: v1alpha1.IntentActionLabel, EventID: 5,
				At: at(-11 * time.Minute), PlanRevision: 1, PlanDigest: "sha256:" + strings.Repeat("b", 64),
				InputDigest: "sha256:" + strings.Repeat("a", 64)},
			Usage: v1alpha1.IntentUsage{CostMicroUSD: 1_250_000},
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: "https://github.com/acme/app", Number: 12,
				URL: "https://github.com/acme/app/pull/12", State: "open"}},
		},
	}
	inBeta := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-3", Namespace: "patchy", UID: "uid-beta-3"},
		Spec: v1alpha1.IntentSpec{
			Project: "beta",
			Issue: v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 3,
				URL: "https://github.com/acme/intents/issues/3"},
			RequestedBy: v1alpha1.IntentRequest{Login: "octo", At: at(-3 * time.Hour), EventID: 2},
		},
		Status: v1alpha1.IntentStatus{
			Phase: v1alpha1.IntentBlocked,
			PhaseTimes: []v1alpha1.IntentPhaseTime{
				{Phase: v1alpha1.IntentBuilding, At: at(-time.Hour)},
				{Phase: v1alpha1.IntentBlocked, At: at(-30 * time.Minute)},
			},
			Conditions: []metav1.Condition{{
				Type: v1alpha1.ConditionImageRequired, Status: metav1.ConditionTrue, Reason: "RepositoryImageRejected",
				Message: "the build could not run: " + leakRegistry + "/app rejected on " + leakNode,
			}},
		},
	}

	planRun := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7-plan-r1-a1", Namespace: "patchy", UID: "uid-plan-run",
			CreationTimestamp: at(-55 * time.Minute)},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Stage:     v1alpha1.IntentStagePlan, Round: 1, Attempt: 1,
			Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"},
			Grant:      v1alpha1.IntentRunGrant{MaxTurns: 80, TokenBudget: 400000, TimeoutMilliseconds: 1_800_000},
		},
		Status: v1alpha1.IntentRunStatus{
			Phase: v1alpha1.RunComplete, Outcome: "ok", Report: planText,
			Usage:     v1alpha1.UsageSummary{InputTokens: 1000, OutputTokens: 200, CostUSD: "0.75"},
			StartedAt: &started, FinishedAt: &finished,
			Transcript: &v1alpha1.TranscriptRef{Name: "alpha-7-plan-r1-a1-transcript", Turns: 3},
			RunnerImage: &v1alpha1.RunnerImageRef{Source: v1alpha1.RunnerImageSourceDefault,
				Image: leakRegistry + "/patchy/claude@sha256:" + strings.Repeat("c", 64)},
		},
	}
	buildRun := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7-bld-r1-app-a2", Namespace: "patchy", UID: "uid-build-run",
			CreationTimestamp: at(-9 * time.Minute)},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Stage:     v1alpha1.IntentStageBuild, Round: 1, Attempt: 2,
			Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"},
			Inputs:     v1alpha1.IntentRunInputs{PlanRevision: 1},
			Grant:      v1alpha1.IntentRunGrant{MaxTurns: 150, TimeoutMilliseconds: 3_600_000},
		},
		Status: v1alpha1.IntentRunStatus{
			Phase:     v1alpha1.RunRunning,
			JobRef:    &v1alpha1.JobReference{Namespace: "patchy-agents", Name: "job-alpha-7-bld"},
			StartedAt: &started,
			Report:    "a build report\n",
		},
	}
	failedRun := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7-bld-r1-app-a1", Namespace: "patchy", UID: "uid-failed-run",
			CreationTimestamp: at(-10 * time.Minute)},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Stage:     v1alpha1.IntentStageBuild, Round: 1, Attempt: 1,
			Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"},
		},
		Status: v1alpha1.IntentRunStatus{
			Phase: v1alpha1.RunFailed, Outcome: "evicted",
			Detail: "the agent pod was evicted: The node " + leakNode + " was low on resource: memory",
		},
	}
	// A run whose intentRef names alpha-7 by name but another Intent's UID:
	// an earlier alpha-7's, which the views must never show as this one's.
	strayRun := failedRun.DeepCopy()
	strayRun.Name, strayRun.UID = "alpha-7-plan-r9-a1", "uid-stray"
	strayRun.Spec.IntentRef.UID = "uid-older-alpha-7"
	betaRun := failedRun.DeepCopy()
	betaRun.Name, betaRun.UID = "beta-3-bld-r1-web-a1", "uid-beta-run"
	betaRun.Spec.IntentRef = v1alpha1.ObjectReference{Name: "beta-3", UID: "uid-beta-3"}

	raw, err := transcriptstore.Marshal(fixtureTurns())
	if err != nil {
		t.Fatal(err)
	}
	planCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7-plan-r1", Namespace: "patchy",
			Labels:          map[string]string{v1alpha1.LabelIntent: "alpha-7"},
			OwnerReferences: []metav1.OwnerReference{ctrlRef("Intent", "alpha-7", "uid-alpha-7")}},
		Data: map[string]string{"plan.md": planText},
	}
	transcriptCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7-plan-r1-a1-transcript", Namespace: "patchy",
			Labels:          map[string]string{v1alpha1.LabelIntent: "alpha-7"},
			OwnerReferences: []metav1.OwnerReference{ctrlRef("IntentRun", "alpha-7-plan-r1-a1", "uid-plan-run")}},
		BinaryData: map[string][]byte{transcriptstore.DataKey: raw},
	}
	preview := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-7", Namespace: "patchy"},
		Spec:       v1alpha1.PreviewSpec{IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"}},
		Status: v1alpha1.PreviewStatus{Phase: v1alpha1.PreviewReady, URL: "https://alpha-7.preview.example.com/",
			ObservedRevision: strings.Repeat("d", 40)},
	}
	return []client.Object{alpha, beta, inAlpha, inBeta, planRun, buildRun, failedRun, strayRun, betaRun,
		planCM, transcriptCM, preview}
}

func fixtureTurns() []transcript.Turn {
	return []transcript.Turn{
		{Seq: 1, At: "2026-07-21T11:05:00Z", Role: transcript.RoleAssistant, Kind: transcript.KindText,
			Text: "Reading the router.\x1b]8;;https://evil.example\x07"},
		{Seq: 2, At: "2026-07-21T11:06:00Z", Role: transcript.RoleAssistant, Kind: transcript.KindToolUse,
			Tool: "Bash", Text: "go test ./..."},
		{Seq: 3, At: "2026-07-21T11:07:00Z", Role: transcript.RoleUser, Kind: transcript.KindToolResult,
			Text: "ok"},
	}
}

// clientObject keeps the fixture mutators' signatures short.
type clientObject = client.Object

func findConfigMap(objs []client.Object, name string) *corev1.ConfigMap {
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name {
			return cm
		}
	}
	panic("no configmap " + name)
}

func findRun(objs []client.Object, name string) *v1alpha1.IntentRun {
	for _, o := range objs {
		if run, ok := o.(*v1alpha1.IntentRun); ok && run.Name == name {
			return run
		}
	}
	panic("no run " + name)
}

// intentsServer is a server with the intents views on over the fixture.
func intentsServer(t *testing.T, tailer Tailer, objs ...client.Object) (*Server, *tierGranter) {
	t.Helper()
	if objs == nil {
		objs = intentsFixture(t)
	}
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithIndex(&v1alpha1.Investigation{}, RunFindingIndex, RunFindingIndexer).
		WithIndex(&v1alpha1.Remediation{}, RunFindingIndex, RunFindingIndexer).
		WithIndex(&v1alpha1.IntentRun{}, IntentRunIntentIndex, IntentRunIntentIndexer).
		WithIndex(&v1alpha1.Intent{}, IntentProjectIndex, IntentProjectIndexer).
		Build()
	g := fixtureGranter()
	s := NewServer(c, "patchy", stubAuth{}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	s = s.WithTranscripts(c, tailer)
	s = s.WithIntents(IntentsOptions{Projects: g})
	return s, g
}

// as serves s to one caller, with an optional Sec-Fetch-Site.
func as(t *testing.T, s *Server, id *auth.Identity) *httptest.Server {
	t.Helper()
	s.auth = stubAuth{id: id}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// get fetches path with the browser's same-origin header, returning the
// status and body.
func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

// sseEvents splits an SSE body into (event, data) pairs.
func sseEvents(body string) [][2]string {
	var out [][2]string
	for block := range strings.SplitSeq(body, "\n\n") {
		var event, data string
		for line := range strings.SplitSeq(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event != "" {
			out = append(out, [2]string{event, data})
		}
	}
	return out
}

// captureLog records every log line as text.
type captureLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.buf.Write(p)
	}), nil))
}

func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
