// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunBadAddress(t *testing.T) {
	s, err := NewServer(Config{
		Addr:      "not-a-valid-address",
		Endpoints: []Endpoint{githubEndpoint([]byte("s"), HandlerFunc(func(context.Context, Event) error { return nil }))},
	}, testLog)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run() = nil, want a listen error")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	s, err := NewServer(Config{
		Addr:      "127.0.0.1:0",
		Endpoints: []Endpoint{githubEndpoint([]byte("s"), HandlerFunc(func(context.Context, Event) error { return nil }))},
	}, testLog)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// A handler error is recorded and the worker keeps going: the next delivery
// is still handled.
func TestHandlerErrorDoesNotStopWorker(t *testing.T) {
	secret := []byte("s")
	handled := make(chan string, 4)
	h := HandlerFunc(func(_ context.Context, e Event) error {
		handled <- e.DeliveryID
		if e.DeliveryID == "bad" {
			return errors.New("boom")
		}
		return nil
	})
	url, stop := startServer(t, Config{Endpoints: []Endpoint{githubEndpoint(secret, h)}, Workers: 1})
	defer stop()

	body := []byte(`{}`)
	for _, id := range []string{"bad", "good"} {
		status := post(t, url+testPath, map[string]string{
			"X-Hub-Signature-256": sign(secret, body),
			"X-GitHub-Event":      "issues",
			"X-GitHub-Delivery":   id,
		}, body)
		if status != http.StatusAccepted {
			t.Fatalf("delivery %s: status %d, want 202", id, status)
		}
		select {
		case got := <-handled:
			if got != id {
				t.Fatalf("handled %q, want %q", got, id)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("delivery %s never handled", id)
		}
	}
}

// Server.ResetDedup lets a redelivery of an already-seen ID through again.
// One worker drains the queue in order, so a sentinel delivery proves what
// was (and was not) enqueued before it without any timing.
func TestServerResetDedup(t *testing.T) {
	secret := []byte("s")
	handled := make(chan string, 8)
	h := HandlerFunc(func(_ context.Context, e Event) error {
		handled <- e.DeliveryID
		return nil
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s, err := NewServer(Config{Endpoints: []Endpoint{githubEndpoint(secret, h)}, Workers: 1}, testLog)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() {
		cancel()
		<-done
	}()
	url := "http://" + ln.Addr().String() + testPath

	body := []byte(`{}`)
	deliver := func(id string) {
		t.Helper()
		status := post(t, url, map[string]string{
			"X-Hub-Signature-256": sign(secret, body),
			"X-GitHub-Event":      "issues",
			"X-GitHub-Delivery":   id,
		}, body)
		if status != http.StatusAccepted {
			t.Fatalf("delivery %s: status %d, want 202", id, status)
		}
	}
	next := func() string {
		t.Helper()
		select {
		case id := <-handled:
			return id
		case <-time.After(10 * time.Second):
			t.Fatal("no delivery handled")
			return ""
		}
	}

	deliver("same")
	deliver("same") // duplicate: answered 202, never enqueued
	deliver("sentinel-1")
	if got := []string{next(), next()}; got[0] != "same" || got[1] != "sentinel-1" {
		t.Fatalf("handled %v, want [same sentinel-1] (duplicate must be dropped)", got)
	}

	s.ResetDedup()
	deliver("same")
	deliver("sentinel-2")
	if got := []string{next(), next()}; got[0] != "same" || got[1] != "sentinel-2" {
		t.Errorf("handled %v after reset, want [same sentinel-2]", got)
	}
}
