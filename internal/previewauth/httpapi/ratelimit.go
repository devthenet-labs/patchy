// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimit is the per-source-address limit on every relay endpoint.
type RateLimit struct {
	// PerSecond is each address's sustained rate; 0 disables the limit.
	PerSecond float64
	// Burst is each address's bucket size; 0 is one second's worth.
	Burst int
	// ForwardedHops is how many trusted proxies append to X-Forwarded-For
	// in front of the relay (1 behind an ALB): the address is the entry
	// that many from the right. 0 uses the connection's peer address.
	ForwardedHops int
}

const (
	// limiterIdle is how long an idle address's bucket is kept.
	limiterIdle = 10 * time.Minute
	// limiterMaxEntries bounds the table; past it, it is reset.
	limiterMaxEntries = 100_000
)

type limiter struct {
	cfg RateLimit
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newLimiter(cfg RateLimit, now func() time.Time) *limiter {
	if cfg.PerSecond <= 0 {
		return nil
	}
	if cfg.Burst < 1 {
		cfg.Burst = max(1, int(math.Ceil(cfg.PerSecond)))
	}
	return &limiter{cfg: cfg, now: now, buckets: map[string]*bucket{}}
}

// allow takes one token for r's source address. A nil limiter allows all.
func (l *limiter) allow(r *http.Request) bool {
	if l == nil {
		return true
	}
	ip := clientIP(r, l.cfg.ForwardedHops)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) > time.Minute {
		l.lastSweep = now
		for k, b := range l.buckets {
			if now.Sub(b.seen) > limiterIdle {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= limiterMaxEntries {
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{lim: rate.NewLimiter(rate.Limit(l.cfg.PerSecond), l.cfg.Burst)}
		l.buckets[ip] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// clientIP is the request's source address: with hops > 0, the
// X-Forwarded-For entry that many from the right (the one the nearest
// trusted proxy appended), else the peer. A header too short or not an
// address falls back to the peer, so a client can never pick its own bucket
// by sending the header.
func clientIP(r *http.Request, hops int) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if hops <= 0 {
		return peer
	}
	var entries []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for e := range strings.SplitSeq(h, ",") {
			entries = append(entries, strings.TrimSpace(e))
		}
	}
	if len(entries) < hops {
		return peer
	}
	addr, err := netip.ParseAddr(entries[len(entries)-hops])
	if err != nil {
		return peer
	}
	return addr.Unmap().String()
}
