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

	release, reason := l.admit("a")
	if reason != "" {
		t.Fatal(reason)
	}
	release()
	l.charge("a", 60)
	l.charge("b", 40)
	if _, reason := l.admit("c"); !strings.Contains(reason, "hourly") ||
		!strings.HasPrefix(reason, provider.LimitMessagePrefix) {
		t.Fatalf("at the hourly ceiling: reason = %q", reason)
	}
	// The window slides: fifty-nine minutes on the tokens still count, an
	// hour on they do not.
	now = now.Add(59 * time.Minute)
	if _, reason := l.admit("c"); reason == "" {
		t.Fatal("admitted inside the trailing hour")
	}
	now = now.Add(2 * time.Minute)
	release, reason = l.admit("c")
	if reason != "" {
		t.Fatalf("after the hour: %s", reason)
	}
	release()
	if got := l.totals("a"); got.tokens != 60 || got.requests != 1 {
		t.Fatalf("totals(a) = %+v", got)
	}
	// Pods idle past the TTL are evicted; one with a request in flight is
	// kept.
	holding, _ := l.admit("held")
	now = now.Add(ledgerTTL + time.Minute)
	if _, reason := l.admit("x"); reason != "" {
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
	r1, reason := l.admit("p")
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.admit("p"); !strings.Contains(reason, "concurrent") {
		t.Fatalf("second in flight: %q", reason)
	}
	r1()
	r2, reason := l.admit("p")
	if reason != "" {
		t.Fatal(reason)
	}
	r2()
	if _, reason := l.admit("p"); !strings.Contains(reason, "requests per pod") {
		t.Fatalf("third request: %q", reason)
	}
	l.charge("q", 50)
	if _, reason := l.admit("q"); !strings.Contains(reason, "tokens per pod") {
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
		release, reason := u.admit("z")
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
