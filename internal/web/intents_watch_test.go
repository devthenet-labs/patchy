// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

func TestProjectOf(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ctx := t.Context()
	cases := []struct {
		name string
		obj  any
		want string
	}{
		{"a project", &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}, "alpha"},
		{"an intent", &v1alpha1.Intent{Spec: v1alpha1.IntentSpec{Project: "beta"}}, "beta"},
		{"a run, through its intent", &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7"}}}, "alpha"},
		{"a preview, through its intent", &v1alpha1.Preview{Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "beta-3"}}}, "beta"},
		{"a run whose intent is gone", &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "gone-1"}}}, ""},
		{"anything else", &v1alpha1.Finding{}, ""},
	}
	for _, tc := range cases {
		if got := s.projectOf(ctx, tc.obj); got != tc.want {
			t.Errorf("%s: projectOf = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A deletion tombstone is unwrapped by the watch handler first.
	tomb := toolscache.DeletedFinalStateUnknown{Obj: &v1alpha1.Intent{Spec: v1alpha1.IntentSpec{Project: "alpha"}}}
	if got := s.projectOf(ctx, tomb.Obj); got != "alpha" {
		t.Errorf("tombstone projectOf = %q", got)
	}
}

// A burst of changes across Projects coalesces into one round that reaches
// only the subscribers of the Projects that changed.
func TestIntentsDebounceLoopPublishesPerProject(t *testing.T) {
	s, _ := intentsServer(t, nil)
	s.debounce = 20 * time.Millisecond
	alpha, _ := s.intents.signals.subscribe(map[string]bool{"alpha": true})
	beta, _ := s.intents.signals.subscribe(map[string]bool{"beta": true})
	changed := &changedProjects{signal: make(chan struct{}, 1), set: map[string]bool{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.intentsDebounceLoop(ctx, changed)

	for range 5 {
		changed.add("alpha")
	}
	changed.add("")
	select {
	case <-alpha.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the alpha subscriber heard nothing")
	}
	select {
	case <-beta.ch:
		t.Error("the beta subscriber heard an alpha change")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-alpha.ch:
		t.Error("a burst produced a second round")
	default:
	}
}

// The intents change signal: authenticated, content-free, and only for the
// caller's Projects.
func TestIntentEventsStream(t *testing.T) {
	s, _ := intentsServer(t, nil)
	s.intents.maxAge = 300 * time.Millisecond
	ts := as(t, s, viewerAlpha)
	done := make(chan string, 1)
	go func() {
		_, body := get(t, ts, "/api/intents/events")
		done <- body
	}()
	waitFor(t, func() bool {
		s.intents.signals.mu.Lock()
		defer s.intents.signals.mu.Unlock()
		return len(s.intents.signals.subs) == 1
	})
	s.intents.signals.publish(map[string]bool{"beta": true})
	s.intents.signals.publish(map[string]bool{"alpha": true})
	body := <-done
	events := sseEvents(body)
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev[0])
	}
	if strings.Join(names, ",") != eventIntentsChanged+","+eventEnd {
		t.Errorf("events = %v, want one intents-changed (alpha's, not beta's) then end", names)
	}
	if strings.Contains(body, "alpha") || strings.Contains(body, "beta") {
		t.Errorf("the signal carried content: %s", body)
	}
}

type countingFacts struct {
	calls atomic.Int32
	facts jobs.Facts
	err   error
}

func (c *countingFacts) Facts(context.Context, string) (jobs.Facts, error) {
	c.calls.Add(1)
	return c.facts, c.err
}

// A live run's panel carries its Job clock, read through a short cache, and
// the idle limit of its own stage.
func TestRunPanelJobClock(t *testing.T) {
	s, _ := intentsServer(t, nil)
	facts := &countingFacts{facts: jobs.Facts{
		Created: testClock.Add(-9 * time.Minute), Started: testClock.Add(-8 * time.Minute),
		DeadlineSeconds: 5400, InvestigateIdle: 10 * time.Minute, RemediateIdle: 20 * time.Minute,
	}}
	s.intents.facts = &factsCache{src: facts, now: s.now, entries: map[string]factsEntry{}}
	ts := as(t, s, viewerAlpha)
	for range 3 {
		code, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2")
		if code != http.StatusOK {
			t.Fatalf("run = %d", code)
		}
		run := decode[IntentRunDetail](t, body)
		if run.Job == nil || run.Job.DeadlineSeconds != 5400 || run.Job.IdleTimeoutSeconds != 1200 ||
			run.Job.StartedAt != "2026-07-21T11:52:00Z" || !run.LastAttempt || !run.Running {
			t.Fatalf("run = %+v job %+v", run, run.Job)
		}
	}
	if n := facts.calls.Load(); n != 1 {
		t.Errorf("Job reads = %d, want 1 (cached)", n)
	}
	// A collected run has no Job clock to show.
	_, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-plan-r1-a1")
	if run := decode[IntentRunDetail](t, body); run.Job != nil {
		t.Errorf("collected run job clock = %+v", run.Job)
	}
	// A Job that cannot be read leaves the clock out rather than failing.
	s.intents.facts = &factsCache{src: &countingFacts{err: errors.New("forbidden")}, now: s.now,
		entries: map[string]factsEntry{}}
	code, body := get(t, ts, "/api/intents/alpha-7/runs/alpha-7-bld-r1-app-a2")
	if run := decode[IntentRunDetail](t, body); code != http.StatusOK || run.Job != nil {
		t.Errorf("unreadable job: %d %+v", code, run.Job)
	}
}

// With the envelope hardened, a live findings transcript re-checks its
// viewer's grant too, and ends without it.
func TestFindingsTranscriptReauthorisedWhenHardened(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	s := transcriptServer(t, &fakeTailer{turns: storedTurns()[:1], hold: hold}, runningInvestigation()...)
	granter := &flipGranter{}
	granter.view.Store(true)
	s.granter = granter
	s = s.WithIntents(IntentsOptions{Projects: authz.FullProjects{}, ReauthPeriod: 30 * time.Millisecond})
	ts := as(t, s, operator)
	done := make(chan string, 1)
	go func() {
		_, body := get(t, ts, transcriptPath)
		done <- body
	}()
	waitFor(t, func() bool { return s.tails.activeTails() == 1 })
	granter.view.Store(false)
	select {
	case body := <-done:
		if !strings.Contains(body, "event: end") {
			t.Errorf("body = %s, want the stream ended", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a findings transcript outlived its viewer's revoked grant")
	}
}

type flipGranter struct{ view atomic.Bool }

func (g *flipGranter) Grants(context.Context, auth.Identity) (authz.Grants, error) {
	return authz.Grants{View: g.view.Load()}, nil
}
