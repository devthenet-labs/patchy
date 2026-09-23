// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// verdict is one cached check result: the accepted image or its rejection.
type verdict struct {
	resolved runnerimage.Resolved
	err      error
	expires  time.Time
}

// cache remembers check verdicts by pinned reference. Only deterministic
// outcomes are stored (an acceptance or a *runnerimage.Rejection); transient
// failures are retried. The key already carries the digest, so a moved tag
// can never hit a stale entry.
type cache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]verdict
}

func newCache(ttl time.Duration, now func() time.Time) *cache {
	return &cache{ttl: ttl, now: now, entries: make(map[string]verdict)}
}

// get returns the live verdict for key, if any.
func (c *cache) get(key string) (verdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if !ok {
		return verdict{}, false
	}
	if !c.now().Before(v.expires) {
		delete(c.entries, key)
		return verdict{}, false
	}
	return v, true
}

// put stores a verdict for key until the TTL elapses.
func (c *cache) put(key string, resolved runnerimage.Resolved, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = verdict{resolved: resolved, err: err, expires: c.now().Add(c.ttl)}
}
