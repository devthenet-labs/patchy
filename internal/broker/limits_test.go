// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

func TestLedgerHourlyCeilingAndEviction(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{TokensPerHour: 100, TokensPerPod: 1000}, func() time.Time { return now })

	release, reason := l.admit("a", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	release()
	l.charge("a", 60)
	l.charge("b", 40)
	if _, reason := l.admit("c", 0); !strings.Contains(reason, "hourly") ||
		!strings.HasPrefix(reason, provider.LimitMessagePrefix) {
		t.Fatalf("at the hourly ceiling: reason = %q", reason)
	}
	// The window slides: fifty-nine minutes on the tokens still count, an
	// hour on they do not.
	now = now.Add(59 * time.Minute)
	if _, reason := l.admit("c", 0); reason == "" {
		t.Fatal("admitted inside the trailing hour")
	}
	now = now.Add(2 * time.Minute)
	release, reason = l.admit("c", 0)
	if reason != "" {
		t.Fatalf("after the hour: %s", reason)
	}
	release()
	if got := l.totals("a"); got.tokens != 60 || got.requests != 1 {
		t.Fatalf("totals(a) = %+v", got)
	}
	// Pods idle past the TTL are evicted; one with a request in flight is
	// kept.
	holding, _ := l.admit("held", 0)
	now = now.Add(ledgerTTL + time.Minute)
	if _, reason := l.admit("x", 0); reason != "" {
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
	r1, reason := l.admit("p", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit("p", 0); !strings.Contains(reason, "concurrent") {
		t.Fatalf("second in flight: %q", reason)
	}
	r1()
	r2, reason := l.admit("p", 0)
	if reason != "" {
		t.Fatal(reason)
	}
	r2()
	if _, reason := l.admit("p", 0); !strings.Contains(reason, "requests per pod") {
		t.Fatalf("third request: %q", reason)
	}
	l.charge("q", 50)
	if _, reason := l.admit("q", 0); !strings.Contains(reason, "tokens per pod") {
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
		release, reason := u.admit("z", 0)
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
	if _, reason := l.admit("fresh", 1001); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("a single reservation over the budget: reason = %q", reason)
	}
	r1, reason := l.admit("p", 600)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit("p", 600); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("second reservation over the pod budget: reason = %q", reason)
	}
	r2, reason := l.admit("q", 800)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit("z", 200); !strings.Contains(reason, "hourly") {
		t.Fatalf("reservations over the hourly ceiling: reason = %q", reason)
	}
	// Settled at its actual usage, the reservation is returned.
	l.charge("p", 10)
	r1()
	r2()
	release, reason := l.admit("p", 600)
	if reason != "" {
		t.Fatalf("after release: %s", reason)
	}
	release()
	if got := l.totals("p").tokens; got != 10 {
		t.Fatalf("pod tokens = %d, want only the charged 10", got)
	}
	l.charge("p", 990)
	if _, reason := l.admit("p", 0); !strings.Contains(reason, "tokens per pod") {
		t.Fatalf("at the budget with no reservation: reason = %q", reason)
	}
}
