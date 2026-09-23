// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTokenShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pod := map[string]any{"pod": map[string]any{"name": "agent-pod-1"}}
	tests := []struct {
		name  string
		token string
		want  string // "" for accepted, else a substring of the error
	}{
		{"pod-bound array aud", tokWith(map[string]any{
			"aud": []string{"x", DefaultAudience}, "exp": now.Unix() + 60, "kubernetes.io": pod}), ""},
		{"pod-bound string aud", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix() + 60, "kubernetes.io": pod}), ""},
		{"empty", "", "missing"},
		{"two segments", "a.b", "not a JWT"},
		{"empty segment", "a..c", "not a JWT"},
		{"not base64url", "a.!!!.c", "base64url"},
		{"payload not json", "a.bm90IGpzb24.c", "not JSON"},
		{"wrong audience", tokWith(map[string]any{
			"aud": "other", "exp": now.Unix() + 60, "kubernetes.io": pod}), "audience"},
		{"no audience", tokWith(map[string]any{"exp": now.Unix() + 60, "kubernetes.io": pod}), "audience"},
		{"expired", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix(), "kubernetes.io": pod}), "expired"},
		{"no exp", tokWith(map[string]any{"aud": DefaultAudience, "kubernetes.io": pod}), "expired"},
		{"not pod-bound", tokWith(map[string]any{"aud": DefaultAudience, "exp": now.Unix() + 60}), "pod-bound"},
		{"pod without name", tokWith(map[string]any{
			"aud": DefaultAudience, "exp": now.Unix() + 60, "kubernetes.io": map[string]any{"pod": map[string]any{}}}),
			"pod-bound"},
		{"too large", strings.Repeat("a", maxTokenBytes) + ".b.c", "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tokenShape(tt.token, DefaultAudience, now)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.want != "" && err == nil:
				t.Fatalf("accepted, want an error naming %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestIPLimiter(t *testing.T) {
	if l := newIPLimiter(0, 10, time.Now); l != nil {
		t.Fatal("zero rate did not disable the limiter")
	}
	if l := newIPLimiter(2.5, 0, time.Now); l.burst != 3 {
		t.Fatalf("unset burst = %d, want one second of the rate rounded up (3)", l.burst)
	}
	var disabled *ipLimiter
	if release, reason := disabled.acquire("1.2.3.4"); reason != "" {
		t.Fatalf("disabled limiter refused: %s", reason)
	} else {
		release()
	}
	disabled.penalize("1.2.3.4") // must not panic

	now := time.Unix(1_800_000_000, 0)
	l := newIPLimiter(1, 2, func() time.Time { return now })
	r1, reason := l.acquire("a")
	if reason != "" {
		t.Fatalf("first: %s", reason)
	}
	if _, reason := l.acquire("a"); reason != "" {
		t.Fatalf("second: %s", reason)
	}
	// Two in flight is the cap, and the bucket is empty either way.
	if _, reason := l.acquire("a"); reason == "" {
		t.Fatal("third acquire admitted over the burst")
	}
	r1()
	if _, reason := l.acquire("a"); !strings.Contains(reason, "rate") {
		t.Fatalf("with a slot free but no tokens: reason = %q, want a rate refusal", reason)
	}
	// Another IP has its own bucket.
	rb, reason := l.acquire("b")
	if reason != "" {
		t.Fatalf("other ip: %s", reason)
	}
	rb()
	// Time refills.
	now = now.Add(time.Second)
	if _, reason := l.acquire("a"); reason != "" {
		t.Fatalf("after refill: %s", reason)
	}
	// Idle entries are swept once idle past the TTL.
	now = now.Add(ipIdleTTL + 2*time.Minute)
	l.mu.Lock()
	l.sweepLocked(now)
	if _, ok := l.entries["b"]; ok {
		t.Error("idle entry survived the sweep")
	}
	if _, ok := l.entries["a"]; !ok {
		t.Error("entry with a request in flight was swept")
	}
	l.mu.Unlock()
}

func TestReviewLimiter(t *testing.T) {
	var disabled *reviewLimiter
	if err := disabled.admit(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := newReviewLimiter(1)
	if err := l.admit(t.Context()); err != nil {
		t.Fatalf("first review refused: %v", err)
	}
	// The next slot is a second away; a caller that cannot wait is refused
	// rather than queued indefinitely.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := l.admit(ctx); err == nil {
		t.Fatal("second review admitted without waiting for a slot")
	}
}
