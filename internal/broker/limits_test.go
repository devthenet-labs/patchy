// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

func TestLedgerHourlyCeilingAndEviction(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{TokensPerHour: 100, TokensPerPod: 1000}, func() time.Time { return now })

	release, reason := l.admit(t.Context(), "a", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	release()
	l.charge("a", 60)
	l.charge("b", 40)
	if _, reason := l.admit(t.Context(), "c", 0); !strings.Contains(reason, "hourly") ||
		!strings.HasPrefix(reason, provider.LimitMessagePrefix) {
		t.Fatalf("at the hourly ceiling: reason = %q", reason)
	}
	// The window slides: fifty-nine minutes on the tokens still count, an
	// hour on they do not.
	now = now.Add(59 * time.Minute)
	if _, reason := l.admit(t.Context(), "c", 0); reason == "" {
		t.Fatal("admitted inside the trailing hour")
	}
	now = now.Add(2 * time.Minute)
	release, reason = l.admit(t.Context(), "c", 0)
	if reason != "" {
		t.Fatalf("after the hour: %s", reason)
	}
	release()
	if got := l.totals("a"); got.tokens != 60 || got.requests != 1 {
		t.Fatalf("totals(a) = %+v", got)
	}
	// Pods idle past the TTL are evicted; one with a request in flight is
	// kept.
	holding, _ := l.admit(t.Context(), "held", 0)
	now = now.Add(ledgerTTL + time.Minute)
	if _, reason := l.admit(t.Context(), "x", 0); reason != "" {
		t.Fatal(reason)
	}
	if got := l.totals("a"); got != (podTotals{}) {
		t.Errorf("pod a survived the sweep: %+v", got)
	}
	if got := l.totals("held"); got.requests != 1 {
		t.Errorf("pod with an in-flight request was evicted: %+v", got)
	}
	holding()
}

func TestLedgerPerPod(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{RequestsPerPod: 2, ConcurrentPerPod: 1, TokensPerPod: 50}, func() time.Time { return now })
	r1, reason := l.admit(t.Context(), "p", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "concurrent") {
		t.Fatalf("second in flight: %q", reason)
	}
	r1()
	r2, reason := l.admit(t.Context(), "p", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	r2()
	if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "requests per pod") {
		t.Fatalf("third request: %q", reason)
	}
	l.charge("q", 50)
	if _, reason := l.admit(t.Context(), "q", 0); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("over tokens: %q", reason)
	}
	l.charge("q", 0)
	l.charge("q", -5)
	if got := l.totals("q").tokens; got != 50 {
		t.Fatalf("non-positive charges changed the total: %d", got)
	}
	// Unlimited: everything counts, nothing refuses.
	u := newLedger(Limits{}, func() time.Time { return now })
	for range 10 {
		release, reason := u.admit(t.Context(), "z", 0)
		if reason != "" {
			t.Fatal(reason)
		}
		defer release()
	}
	if got := u.totals("z").requests; got != 10 {
		t.Fatalf("requests = %d", got)
	}
}

func TestHourWindow(t *testing.T) {
	var h hourWindow
	base := time.Unix(1_800_000_000, 0).Truncate(time.Minute)
	for i := range 90 {
		h.add(base.Add(time.Duration(i)*time.Minute), 1)
	}
	if got := h.sum(base.Add(89 * time.Minute)); got != 60 {
		t.Fatalf("trailing hour sum = %d, want 60", got)
	}
	if got := h.sum(base.Add(200 * time.Minute)); got != 0 {
		t.Fatalf("stale sum = %d, want 0", got)
	}
}

// TestLedgerReservation: a reservation counts against both token limits
// until released, so parallel admissions see each other's worst case.
func TestLedgerReservation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{TokensPerPod: 1000, TokensPerHour: 1500}, func() time.Time { return now })
	if _, reason := l.admit(t.Context(), "fresh", 1001); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("a single reservation over the budget: reason = %q", reason)
	}
	r1, reason := l.admit(t.Context(), "p", 600)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit(t.Context(), "p", 600); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("second reservation over the pod budget: reason = %q", reason)
	}
	r2, reason := l.admit(t.Context(), "q", 800)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit(t.Context(), "z", 200); !strings.Contains(reason, "hourly") {
		t.Fatalf("reservations over the hourly ceiling: reason = %q", reason)
	}
	// Settled at its actual usage, the reservation is returned.
	l.charge("p", 10)
	r1()
	r2()
	release, reason := l.admit(t.Context(), "p", 600)
	if reason != "" {
		t.Fatalf("after release: %s", reason)
	}
	release()
	if got := l.totals("p").tokens; got != 10 {
		t.Fatalf("pod tokens = %d, want only the charged 10", got)
	}
	l.charge("p", 990)
	if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("at the budget with no reservation: reason = %q", reason)
	}
}

// TestLedgerConcurrencyWait: a request refused only by the in-flight cap
// waits for a slot to free and is then admitted; every limit is checked
// again after the wait, and the other limits never wait.
func TestLedgerConcurrencyWait(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }

	t.Run("admitted when a slot frees", func(t *testing.T) {
		l := newLedger(Limits{ConcurrentPerPod: 1, ConcurrencyWait: time.Minute}, clock)
		holder, reason := l.admit(t.Context(), "p", 0)
		if reason != "" {
			t.Fatal(reason)
		}
		time.AfterFunc(20*time.Millisecond, holder)
		release, reason := l.admit(t.Context(), "p", 0)
		if reason != "" {
			t.Fatalf("waiting request refused: %s", reason)
		}
		release()
		if got := l.totals("p").requests; got != 2 {
			t.Errorf("requests = %d, want 2", got)
		}
	})
	t.Run("limits checked again after the wait", func(t *testing.T) {
		l := newLedger(Limits{ConcurrentPerPod: 1, TokensPerPod: 100, ConcurrencyWait: time.Minute}, clock)
		holder, reason := l.admit(t.Context(), "p", 0)
		if reason != "" {
			t.Fatal(reason)
		}
		// The in-flight request is charged past the budget as it settles.
		time.AfterFunc(20*time.Millisecond, func() {
			l.charge("p", 100)
			holder()
		})
		if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "tokens per pod") {
			t.Fatalf("after the wait: reason = %q, want the token limit", reason)
		}
	})
	t.Run("other limits never wait", func(t *testing.T) {
		l := newLedger(Limits{ConcurrentPerPod: 1, RequestsPerPod: 1, ConcurrencyWait: time.Minute}, clock)
		holder, reason := l.admit(t.Context(), "p", 0)
		if reason != "" {
			t.Fatal(reason)
		}
		defer holder()
		start := time.Now()
		if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "requests per pod") {
			t.Fatalf("reason = %q", reason)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("a request-count refusal waited %v", waited)
		}
	})
	t.Run("negative wait refuses at once", func(t *testing.T) {
		l := newLedger(Limits{ConcurrentPerPod: 1, ConcurrencyWait: -1}, clock)
		holder, _ := l.admit(t.Context(), "p", 0)
		defer holder()
		if _, reason := l.admit(t.Context(), "p", 0); !strings.Contains(reason, "concurrent") {
			t.Fatalf("reason = %q", reason)
		}
	})
	t.Run("context ends the wait without taking a slot", func(t *testing.T) {
		l := newLedger(Limits{ConcurrentPerPod: 1, ConcurrencyWait: time.Minute}, clock)
		holder, reason := l.admit(t.Context(), "p", 0)
		if reason != "" {
			t.Fatal(reason)
		}
		ctx, cancel := context.WithCancel(t.Context())
		const after = 20 * time.Millisecond
		start := time.Now()
		time.AfterFunc(after, cancel)
		if _, reason := l.admit(ctx, "p", 0); !strings.Contains(reason, "concurrent") {
			t.Fatalf("cancelled wait: reason = %q", reason)
		}
		if waited := time.Since(start); waited < after || waited > 5*time.Second {
			t.Errorf("cancelled wait took %v, want it to wait for the cancel and return promptly", waited)
		}
		holder()
		// The slot is free again: even a caller that has already given up
		// is admitted without waiting.
		done, cancelDone := context.WithCancel(t.Context())
		cancelDone()
		release, reason := l.admit(done, "p", 0)
		if reason != "" {
			t.Fatalf("slot leaked by the cancelled waiter: %s", reason)
		}
		release()
		if got := l.totals("p").requests; got != 2 {
			t.Errorf("requests = %d, want 2 (the cancelled request is not counted)", got)
		}
	})
}

// TestLedgerConcurrencyWaitRace: many requests contending for a pod's slots
// never hold more than the cap at once, and every one is eventually
// admitted when each releases promptly.
func TestLedgerConcurrencyWaitRace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const limit, workers = 2, 16
	l := newLedger(Limits{ConcurrentPerPod: limit, ConcurrencyWait: time.Minute}, func() time.Time { return now })
	var inflight, peak atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for range workers {
		wg.Go(func() {
			release, reason := l.admit(t.Context(), "p", 0)
			if reason != "" {
				errs <- reason
				return
			}
			n := inflight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inflight.Add(-1)
			release()
		})
	}
	wg.Wait()
	close(errs)
	for reason := range errs {
		t.Errorf("refused: %s", reason)
	}
	if p := peak.Load(); p > limit {
		t.Errorf("peak in flight = %d, over the cap %d", p, limit)
	}
	if got := l.totals("p").requests; got != workers {
		t.Errorf("requests = %d, want %d", got, workers)
	}
}
