// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/intentview"
	"github.com/bitwise-media-group/patchy/internal/transcript"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// Stream caps. An intents stream holds a goroutine, a socket and (for a live
// run) a share of the upstream follow budget, so both the whole server's and
// one identity's open streams are bounded.
const (
	// maxIntentStreams bounds the intents streams open at once (run streams
	// and change signals together).
	maxIntentStreams = 256
	// maxIdentityStreams bounds one identity's open intents streams: a
	// board, a run panel and a few tabs, not a scraper.
	maxIdentityStreams = 6
)

// Run stream and signal event names.
const (
	eventActivity       = "activity"
	eventUnavailable    = "unavailable"
	eventIntentsChanged = "intents-changed"
)

// End reasons a stream gives the browser.
const (
	endRevoked   = "revoked"   // the grant it was opened under is gone
	endReconnect = "reconnect" // the stream is old; reconnect through the session
)

// streamLimiter counts open streams per identity and in total.
type streamLimiter struct {
	perMax, totalMax int

	mu    sync.Mutex
	per   map[string]int
	total int
}

func newStreamLimiter(perMax, totalMax int) *streamLimiter {
	return &streamLimiter{perMax: perMax, totalMax: totalMax, per: map[string]int{}}
}

// acquire takes one stream slot for user; release returns it.
func (l *streamLimiter) acquire(user string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.totalMax || l.per[user] >= l.perMax {
		return nil, false
	}
	l.total++
	l.per[user]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.per[user]--; l.per[user] <= 0 {
				delete(l.per, user)
			}
		})
	}, true
}

// sseHeaders starts an intents stream: never cached, never buffered.
func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	return flusher, true
}

// sseSend writes one event whose data is v re-marshalled, never raw bytes
// from a log, so nothing upstream can inject an SSE field. It reports
// whether the client is still there.
func sseSend(w http.ResponseWriter, flusher http.Flusher, event string, v any) bool {
	payload, err := json.Marshal(v)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// sseEnd closes a stream with a reason.
func sseEnd(w http.ResponseWriter, flusher http.Flusher, reason string) {
	sseSend(w, flusher, eventEnd, StreamNotice{Reason: reason})
}

// activityTracker derives a run's activity from its turns, keeping none of
// their text: a turn count, the newest turn's time, and the tool called last
// and still without a result. Turn times are agent-reported, so one that
// does not parse is ignored and one in the future is read as now. Once the
// recorder's closing notice arrives (capNotice) nothing more is recorded,
// so the activity is capped: the tool open then is no longer known to be.
type activityTracker struct {
	now  func() time.Time
	live bool

	turns         int
	lastAt        string
	openTool      string
	openToolSince string
	capped        bool
}

func (a *activityTracker) add(t transcript.Turn) {
	a.turns++
	at := a.clamp(t.At)
	if at != "" {
		a.lastAt = at
	}
	switch {
	case capNotice(t):
		a.capped = true
		a.openTool, a.openToolSince = "", ""
	case t.Kind == transcript.KindToolUse:
		a.openTool, a.openToolSince = intentview.Text(t.Tool, 64), at
	case t.Kind == transcript.KindToolResult, t.Kind == transcript.KindText:
		a.openTool, a.openToolSince = "", ""
	}
}

func (a *activityTracker) clamp(at string) string {
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return ""
	}
	if now := a.now(); ts.After(now) {
		ts = now
	}
	return ts.UTC().Format(time.RFC3339)
}

func (a *activityTracker) snapshot() RunActivity {
	return RunActivity{Turns: a.turns, LastAt: a.lastAt, OpenTool: a.openTool,
		OpenToolSince: a.openToolSince, Live: a.live, Capped: a.capped}
}

// plainTurn is a turn as the intents views ship it: re-marshalled, every
// string made visible (templates.VisibleText), for plain-text rendering.
func plainTurn(t transcript.Turn) Turn {
	w := wireTurn(t)
	w.Tool = intentview.Text(w.Tool, 64)
	w.Text = intentview.Text(w.Text, 0)
	w.At = intentview.Text(w.At, 64)
	return w
}

// handleRunStream streams one run as Server-Sent Events. Every tier gets
// activity events (counts and the open tool's name, no text); only a tier 2
// reader also gets the turns. The stripping happens here, per subscriber, on
// every send: the follow behind a live run is shared by everyone watching
// it, whatever their tier. A run that has not launched yet is waited for
// (awaitLaunch). The stream re-checks its grant every reauth period and ends
// when it is gone, and ends anyway after maxAge so the browser reconnects
// through its current session.
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	id, in, tier, ok := s.intentRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := s.intentRunFor(ctx, in, r.PathValue("run"))
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelError, "load intent run", slog.String("intent", in.Name), slog.Any("error", err))
		http.Error(w, "failed to load the run", http.StatusInternalServerError)
		return
	}
	release, ok := s.intents.limiter.acquire(id.Username)
	if !ok {
		http.Error(w, "too many open streams", http.StatusTooManyRequests)
		return
	}
	defer release()
	flusher, ok := sseHeaders(w)
	if !ok {
		return
	}
	flusher.Flush()
	if tier == authz.TierTranscripts {
		s.auditRead(ctx, id, in, run.Name, "transcript")
	}
	deadline := time.NewTimer(s.intents.maxAge)
	defer deadline.Stop()
	rs := &runStream{s: s, w: w, flusher: flusher, id: id, in: in, run: run, tier: tier,
		act: &activityTracker{now: s.now}, deadline: deadline.C}
	rs.serve(ctx)
}

// runStream is one viewer's stream of one run.
type runStream struct {
	s       *Server
	w       http.ResponseWriter
	flusher http.Flusher
	id      auth.Identity
	in      *v1alpha1.Intent
	run     *v1alpha1.IntentRun
	tier    authz.Tier
	act     *activityTracker
	// deadline fires maxAge after the stream opened, whether it is still
	// waiting for the run to launch or already following it.
	deadline <-chan time.Time
}

// send emits one turn: activity to everyone, the turn itself to tier 2.
func (rs *runStream) send(t transcript.Turn) bool {
	rs.act.add(t)
	if rs.tier == authz.TierTranscripts && !sseSend(rs.w, rs.flusher, eventTurn, plainTurn(t)) {
		return false
	}
	return sseSend(rs.w, rs.flusher, eventActivity, rs.act.snapshot())
}

func (rs *runStream) serve(ctx context.Context) {
	if !rs.awaitLaunch(ctx) {
		return
	}
	if rs.run.Status.Transcript != nil {
		rs.persisted(ctx)
		return
	}
	running := rs.run.Status.Phase == v1alpha1.RunRunning || rs.run.Status.Phase == v1alpha1.RunPending
	if !running || rs.run.Status.JobRef == nil {
		sseEnd(rs.w, rs.flusher, "")
		return
	}
	rs.live(ctx)
}

// queued reports a run whose agent has not launched: waiting for a run slot
// (Pending, or no status written yet), or granted one (Running) before its
// Job exists. The RunReconciler records the Job only once it has launched it.
func queued(run *v1alpha1.IntentRun) bool {
	if run.Status.Transcript != nil || run.Status.JobRef != nil {
		return false
	}
	switch run.Status.Phase {
	case "", v1alpha1.RunPending, v1alpha1.RunRunning:
		return true
	}
	return false
}

// awaitLaunch holds the stream of a queued run open until the run launches
// or ends, re-reading it every runPoll, so a viewer who opened it while it
// waited for a slot sees it start rather than a stream that ended before
// there was anything to follow. It keeps the stream's other rules while it
// waits: pings, the grant re-checked, the maxAge reconnect. It reports
// whether the stream goes on; false once it has ended.
func (rs *runStream) awaitLaunch(ctx context.Context) bool {
	if !queued(rs.run) {
		return true
	}
	poll := time.NewTicker(rs.s.intents.runPoll)
	defer poll.Stop()
	ping := time.NewTicker(keepalivePeriod)
	defer ping.Stop()
	reauth := time.NewTicker(rs.s.intents.reauth)
	defer reauth.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-poll.C:
			run, err := rs.s.intentRunFor(ctx, rs.in, rs.run.Name)
			switch {
			case errors.Is(err, errNotVisible):
				sseEnd(rs.w, rs.flusher, "")
				return false
			case err != nil:
				// A failed read is retried on the next tick.
				continue
			}
			rs.run = run
			if !queued(run) {
				return true
			}
		case <-reauth.C:
			if !rs.s.stillGranted(ctx, rs.id, rs.in.Spec.Project, rs.tier) {
				sseEnd(rs.w, rs.flusher, endRevoked)
				return false
			}
		case <-rs.deadline:
			sseEnd(rs.w, rs.flusher, endReconnect)
			return false
		case <-ping.C:
			if _, err := fmt.Fprint(rs.w, ": ping\n\n"); err != nil {
				return false
			}
			rs.flusher.Flush()
		}
	}
}

// persisted replays a collected run's stored transcript and ends.
func (rs *runStream) persisted(ctx context.Context) {
	turns, err := rs.s.runTranscript(ctx, rs.in, rs.run)
	if err != nil && !errors.Is(err, errNotVisible) {
		rs.s.log.LogAttrs(ctx, slog.LevelError, "load intent transcript", slog.String("run", rs.run.Name),
			slog.Any("error", err))
	}
	for _, t := range turns {
		if !rs.send(t) {
			return
		}
	}
	if len(turns) == 0 {
		sseSend(rs.w, rs.flusher, eventActivity, rs.act.snapshot())
	}
	sseEnd(rs.w, rs.flusher, "")
}

// live follows a running run through the shared tail hub until it ends, the
// viewer leaves, the grant goes, or the stream ages out.
func (rs *runStream) live(ctx context.Context) {
	// Say so rather than end quietly when the run cannot be followed: a
	// running intent run has no persisted transcript yet, so an empty stream
	// would read as a blank run.
	if rs.s.tails == nil {
		sseSend(rs.w, rs.flusher, eventUnavailable, StreamNotice{
			Reason: "live view unavailable: this server does not follow agent logs"})
		sseEnd(rs.w, rs.flusher, "")
		return
	}
	sub, err := rs.s.tails.subscribe(rs.run.Status.JobRef.Name)
	if err != nil {
		sseSend(rs.w, rs.flusher, eventUnavailable, StreamNotice{
			Reason: fmt.Sprintf("live view unavailable: %d runs are already followed", maxLiveTails)})
		sseEnd(rs.w, rs.flusher, "")
		return
	}
	defer sub.Close()
	rs.act.live = true
	for _, t := range sub.Replay {
		if !rs.send(t) {
			return
		}
	}
	sseSend(rs.w, rs.flusher, eventActivity, rs.act.snapshot())

	ping := time.NewTicker(keepalivePeriod)
	defer ping.Stop()
	reauth := time.NewTicker(rs.s.intents.reauth)
	defer reauth.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-sub.Turns:
			if !ok {
				rs.act.live = false
				sseSend(rs.w, rs.flusher, eventActivity, rs.act.snapshot())
				sseEnd(rs.w, rs.flusher, "")
				return
			}
			if !rs.send(t) {
				return
			}
		case <-reauth.C:
			if !rs.s.stillGranted(ctx, rs.id, rs.in.Spec.Project, rs.tier) {
				sseEnd(rs.w, rs.flusher, endRevoked)
				return
			}
		case <-rs.deadline:
			sseEnd(rs.w, rs.flusher, endReconnect)
			return
		case <-ping.C:
			if _, err := fmt.Fprint(rs.w, ": ping\n\n"); err != nil {
				return
			}
			rs.flusher.Flush()
		}
	}
}

// stillGranted re-checks that the caller holds at least tier on project. An
// error counts as revoked: a stream must not outlive a grant it cannot
// confirm.
func (s *Server) stillGranted(ctx context.Context, id auth.Identity, project string, tier authz.Tier) bool {
	tiers, err := s.intents.projects.Tiers(ctx, id, []string{project})
	return err == nil && tiers[project] >= tier
}

// runTranscript reads a collected run's persisted transcript: only from the
// ConfigMap the run's status names, only when that is the run's own
// derived name, and only when it carries the Intent's label and a controller
// reference to this very run.
func (s *Server) runTranscript(ctx context.Context, in *v1alpha1.Intent, run *v1alpha1.IntentRun) (
	[]transcript.Turn, error) {
	ref := run.Status.Transcript
	if ref == nil || ref.Name != transcriptstore.NameFor(run.Name) {
		return nil, errNotVisible
	}
	cm, err := s.guardedConfigMap(ctx, ref.Name, in.Name, run.UID)
	if err != nil {
		return nil, err
	}
	return transcriptstore.FromConfigMap(cm)
}

// projectBroker fans the content-free intents-changed signal out to the
// subscribers allowed to see the Project that changed, and to no one else,
// so a viewer of one Project cannot time another's activity.
type projectBroker struct {
	max int

	mu   sync.Mutex
	subs map[*projectSub]struct{}
}

// projectSub is one signal subscriber and the Projects it may hear about.
type projectSub struct {
	ch chan struct{}

	mu      sync.Mutex
	allowed map[string]bool
}

func (p *projectSub) setAllowed(allowed map[string]bool) {
	p.mu.Lock()
	p.allowed = allowed
	p.mu.Unlock()
}

func (p *projectSub) hears(projects map[string]bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name := range projects {
		if p.allowed[name] {
			return true
		}
	}
	return false
}

func newProjectBroker(limit int) *projectBroker {
	return &projectBroker{max: limit, subs: map[*projectSub]struct{}{}}
}

func (b *projectBroker) subscribe(allowed map[string]bool) (*projectSub, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) >= b.max {
		return nil, false
	}
	sub := &projectSub{ch: make(chan struct{}, 1), allowed: allowed}
	b.subs[sub] = struct{}{}
	return sub, true
}

func (b *projectBroker) unsubscribe(sub *projectSub) {
	b.mu.Lock()
	delete(b.subs, sub)
	b.mu.Unlock()
}

// publish signals every subscriber allowed to see any of projects. A full
// buffer drops the signal: every signal means the same "refetch".
func (b *projectBroker) publish(projects map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if !sub.hears(projects) {
			continue
		}
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

// grantedSet is the Projects a tier map shows at all.
func grantedSet(tiers map[string]authz.Tier) map[string]bool {
	out := map[string]bool{}
	for name, t := range tiers {
		if t != authz.TierNone {
			out[name] = true
		}
	}
	return out
}

// handleIntentEvents is the intents views' change signal: authenticated,
// content-free, and filtered to the caller's Projects. Unlike /events it is
// never public. It re-resolves the caller's Projects every reauth period and
// ends after maxAge so the browser reconnects through its session.
func (s *Server) handleIntentEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	_, tiers, err := s.projectTiers(ctx, id)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "project tiers", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}
	release, ok := s.intents.limiter.acquire(id.Username)
	if !ok {
		http.Error(w, "too many open streams", http.StatusTooManyRequests)
		return
	}
	defer release()
	sub, ok := s.intents.signals.subscribe(grantedSet(tiers))
	if !ok {
		http.Error(w, "too many open streams", http.StatusServiceUnavailable)
		return
	}
	defer s.intents.signals.unsubscribe(sub)
	flusher, ok := sseHeaders(w)
	if !ok {
		return
	}
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(keepalivePeriod)
	defer ping.Stop()
	reauth := time.NewTicker(s.intents.reauth)
	defer reauth.Stop()
	deadline := time.NewTimer(s.intents.maxAge)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.ch:
			if !sseSend(w, flusher, eventIntentsChanged, struct{}{}) {
				return
			}
		case <-reauth.C:
			_, tiers, err := s.projectTiers(ctx, id)
			if err != nil {
				sseEnd(w, flusher, endRevoked)
				return
			}
			sub.setAllowed(grantedSet(tiers))
		case <-deadline.C:
			sseEnd(w, flusher, endReconnect)
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
