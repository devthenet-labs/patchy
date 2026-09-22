// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"fmt"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// ledgerTTL is how long an idle pod's counters survive; a Job never outlives
// its deadline by that much.
const ledgerTTL = 24 * time.Hour

// podTotals is what one pod has consumed so far, for the audit line.
type podTotals struct {
	requests int64
	tokens   int64
}

// ledger is the in-memory spend record: per-pod request, in-flight and token
// counters keyed on the identity TokenReview reports, plus the broker-wide
// trailing-hour token window. It always counts, so the audit line can size a
// limit from observed runs; a limit is enforced only when configured above
// zero. Per replica, as the chart runs one.
type ledger struct {
	limits Limits
	now    func() time.Time

	mu        sync.Mutex
	pods      map[string]*podCounters
	hour      hourWindow
	lastSweep time.Time
}

type podCounters struct {
	requests int64
	inflight int64
	tokens   int64
	seen     time.Time
}

func newLedger(limits Limits, now func() time.Time) *ledger {
	return &ledger{limits: limits, now: now, pods: map[string]*podCounters{}}
}

// admit checks every request-time limit for pod and, when all pass, records
// the request and takes an in-flight slot. It returns the slot's release, or
// the refusal message (carrying the fixed prefix the in-pod runtime maps to
// budget_exceeded).
func (l *ledger) admit(pod string) (func(), string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	p := l.podLocked(pod, now)
	lim := l.limits
	switch {
	case lim.TokensPerHour > 0 && l.hour.sum(now) >= lim.TokensPerHour:
		return nil, fmt.Sprintf("%s: broker-wide hourly token ceiling (%d) reached",
			provider.LimitMessagePrefix, lim.TokensPerHour)
	case lim.TokensPerPod > 0 && p.tokens >= lim.TokensPerPod:
		return nil, fmt.Sprintf("%s: tokens per pod (%d) reached", provider.LimitMessagePrefix, lim.TokensPerPod)
	case lim.RequestsPerPod > 0 && p.requests >= lim.RequestsPerPod:
		return nil, fmt.Sprintf("%s: requests per pod (%d) reached", provider.LimitMessagePrefix, lim.RequestsPerPod)
	case lim.ConcurrentPerPod > 0 && p.inflight >= lim.ConcurrentPerPod:
		return nil, fmt.Sprintf("%s: concurrent requests per pod (%d) reached",
			provider.LimitMessagePrefix, lim.ConcurrentPerPod)
	}
	p.requests++
	p.inflight++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		p.inflight--
		p.seen = l.now()
	}, ""
}

// charge adds n tokens to pod's total and the hourly window.
func (l *ledger) charge(pod string, n int64) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.podLocked(pod, now).tokens += n
	l.hour.add(now, n)
}

// totals reports pod's counters.
func (l *ledger) totals(pod string) podTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pods[pod]
	if !ok {
		return podTotals{}
	}
	return podTotals{requests: p.requests, tokens: p.tokens}
}

func (l *ledger) podLocked(pod string, now time.Time) *podCounters {
	p, ok := l.pods[pod]
	if !ok {
		p = &podCounters{}
		l.pods[pod] = p
	}
	p.seen = now
	return p
}

// sweepLocked evicts pods idle for longer than ledgerTTL, at most once a
// minute.
func (l *ledger) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for name, p := range l.pods {
		if p.inflight == 0 && now.Sub(p.seen) > ledgerTTL {
			delete(l.pods, name)
		}
	}
}

// hourWindow is a trailing-hour token sum over sixty one-minute buckets.
type hourWindow struct {
	buckets [60]int64
	minutes [60]int64 // the minute index each bucket holds
}

func (h *hourWindow) add(now time.Time, n int64) {
	m := now.Unix() / 60
	i := m % 60
	if h.minutes[i] != m {
		h.minutes[i], h.buckets[i] = m, 0
	}
	h.buckets[i] += n
}

func (h *hourWindow) sum(now time.Time) int64 {
	m := now.Unix() / 60
	var total int64
	for i := range h.buckets {
		if h.buckets[i] != 0 && m-h.minutes[i] < 60 {
			total += h.buckets[i]
		}
	}
	return total
}
