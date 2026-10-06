// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SSE event names the status page listens for; each triggers a refetch of
// its own dataset.
const (
	// eventFindingsChanged: the findings/rollups projection changed.
	eventFindingsChanged = "findings-changed"
	// eventConfigChanged: the Forge/Integration configuration changed.
	eventConfigChanged = "config-changed"
)

// broker fans a notification out to every connected SSE client. Sends are
// non-blocking: a client whose buffer is full simply misses an intermediate
// notification, which is harmless because every notification carries the
// same meaning ("refetch") — the next one catches it up.
type broker struct {
	mu sync.Mutex
	// clients maps each client's channel to its subscription order.
	clients map[chan string]uint64
	next    uint64
}

func newBroker() *broker {
	return &broker{clients: make(map[chan string]uint64)}
}

// subscribe registers a new client and returns its event channel.
func (b *broker) subscribe() chan string { return b.subscribeCapped(0) }

// subscribeCapped registers a new client, first dropping the oldest ones
// while limit (when positive) are already subscribed: a dropped client's
// channel is closed, which ends its stream. Dropping the oldest rather than
// refusing the newest keeps the cap from locking viewers out: anyone can
// fill an unauthenticated stream's subscribers, and a browser's EventSource
// gives up for good on a refused stream but reconnects on its own after one
// that ends.
func (b *broker) subscribeCapped(limit int) chan string {
	ch := make(chan string, 8)
	b.mu.Lock()
	defer b.mu.Unlock()
	for limit > 0 && len(b.clients) >= limit {
		var oldest chan string
		for c, order := range b.clients {
			if oldest == nil || order < b.clients[oldest] {
				oldest = c
			}
		}
		delete(b.clients, oldest)
		close(oldest)
	}
	b.next++
	b.clients[ch] = b.next
	return ch
}

// unsubscribe removes and closes a client's channel. Safe to call once.
func (b *broker) unsubscribe(ch chan string) {
	b.mu.Lock()
	if _, ok := b.clients[ch]; ok {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// publish delivers one named notification to every subscriber, dropping it
// for any client whose buffer is full rather than blocking the watcher —
// harmless, because every notification of one name carries the same
// meaning ("refetch") and the next one catches the client up.
func (b *broker) publish(event string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

// count reports the number of connected clients.
func (b *broker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// keepalivePeriod bounds how long an idle SSE connection waits before a
// comment ping, so proxies and load balancers do not reap it.
const keepalivePeriod = 25 * time.Second

// handleEvents is the Server-Sent Events stream. It is public like the
// rollups endpoint: the only information it carries is that something
// changed. It emits a named event per published notification plus periodic
// comment pings to hold the connection open.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", s.streamCacheControl())
	h.Set("Connection", "keep-alive")

	limit := 0
	if s.hardened() {
		limit = maxEventSubscribers
	}
	ch := s.broker.subscribeCapped(limit)
	defer s.broker.unsubscribe(ch)

	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(keepalivePeriod)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: {}\n\n", event)
			flusher.Flush()
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
