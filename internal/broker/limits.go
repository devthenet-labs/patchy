// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
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
	reserved  int64 // outstanding reservations broker-wide
	lastSweep time.Time
}

type podCounters struct {
	requests int64
	inflight int64
	tokens   int64
	reserved int64 // outstanding reservations of in-flight requests
	seen     time.Time
	// freed is closed, and cleared, when one of the pod's in-flight slots is
	// released: requests waiting for a slot select on it. Nil while nobody
	// waits.
	freed chan struct{}
}

func newLedger(limits Limits, now func() time.Time) *ledger {
	return &ledger{limits: limits, now: now, pods: map[string]*podCounters{}}
}

// admit checks every request-time limit for pod and, when all pass, records
// the request, takes an in-flight slot and reserves the request's worst-case
// tokens against both token limits. The token checks count every
// outstanding reservation, all under one lock, so parallel requests cannot
// each pass before any of them is charged: a request is refused when the
// committed total (charged plus reserved) has reached the limit or its own
// reservation would carry it over. It returns the release — which frees the
// slot and returns the reservation once the request has been charged its
// actual usage — or the refusal message (carrying the fixed prefix the
// in-pod runtime maps to budget_exceeded).
//
// When the in-flight cap is the only limit refusing the request, admit first
// waits up to Limits.ConcurrencyWait for one of the pod's slots to be
// released, or until ctx ends, and then checks every limit again. A caller
// can hold its whole response before the handler that served it has
// returned and released the slot (see Server.proxy), so its next request
// would otherwise be refused for a slot that is about to free. Every other
// limit refuses at once, and the ledger lock is not held while waiting.
func (l *ledger) admit(ctx context.Context, pod string, reserve int64) (func(), string) {
	reserve = max(reserve, 0)
	release, reason, freed := l.tryAdmit(pod, reserve)
	if freed == nil {
		return release, reason
	}
	deadline := time.NewTimer(l.limits.ConcurrencyWait)
	defer deadline.Stop()
	for freed != nil {
		select {
		case <-freed:
		case <-deadline.C:
			return nil, reason
		case <-ctx.Done():
			return nil, reason
		}
		release, reason, freed = l.tryAdmit(pod, reserve)
	}
	return release, reason
}

// tryAdmit is one pass of admit's checks under the lock. freed is non-nil
// only when the request was refused by the in-flight cap alone and waiting
// is enabled: it is closed when one of the pod's slots is released.
func (l *ledger) tryAdmit(pod string, reserve int64) (release func(), reason string, freed <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	p := l.podLocked(pod, now)
	lim := l.limits
	switch {
	case lim.TokensPerHour > 0 && over(l.hour.sum(now)+l.reserved, reserve, lim.TokensPerHour):
		return nil, fmt.Sprintf("%s: broker-wide hourly token ceiling (%d) reached",
			provider.LimitMessagePrefix, lim.TokensPerHour), nil
	case lim.TokensPerPod > 0 && over(p.tokens+p.reserved, reserve, lim.TokensPerPod):
		return nil, fmt.Sprintf("%s: tokens per pod (%d) reached", provider.LimitMessagePrefix, lim.TokensPerPod), nil
	case lim.RequestsPerPod > 0 && p.requests >= lim.RequestsPerPod:
		return nil, fmt.Sprintf("%s: requests per pod (%d) reached", provider.LimitMessagePrefix, lim.RequestsPerPod), nil
	case lim.ConcurrentPerPod > 0 && p.inflight >= lim.ConcurrentPerPod:
		reason := fmt.Sprintf("%s: concurrent requests per pod (%d) reached",
			provider.LimitMessagePrefix, lim.ConcurrentPerPod)
		if lim.ConcurrencyWait <= 0 {
			return nil, reason, nil
		}
		if p.freed == nil {
			p.freed = make(chan struct{})
		}
		return nil, reason, p.freed
	}
	p.requests++
	p.inflight++
	p.reserved += reserve
	l.reserved += reserve
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		p.inflight--
		p.reserved -= reserve
		l.reserved -= reserve
		p.seen = l.now()
		if p.freed != nil {
			close(p.freed)
			p.freed = nil
		}
	}, "", nil
}

// over reports whether a request reserving reserve tokens is refused under
// limit given what is already committed.
func over(committed, reserve, limit int64) bool {
	return committed >= limit || committed+reserve > limit
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
