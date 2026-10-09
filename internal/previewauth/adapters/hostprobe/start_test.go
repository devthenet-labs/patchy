// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package hostprobe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// countingHosts answers every probe with the app (unprotected) and counts.
type countingHosts struct{ n atomic.Int64 }

func (c *countingHosts) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

// TestStartProbesOnceThenStops: with probing on, Start runs a pass at once
// and returns when its context ends; every replica probes, so it needs no
// leader.
func TestStartProbesOnceThenStops(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	rt := &countingHosts{}
	p := &Prober{
		Lister:    views{{UID: "u", Label: "a-1", Project: "p", Slot: 1, Live: true}},
		Callbacks: cb, Issuer: issuer, Interval: time.Hour, Transport: rt,
	}
	if p.NeedLeaderElection() {
		t.Error("the probe must run on every replica")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start = %v", err)
	}
	if got := rt.n.Load(); got != 1 {
		t.Errorf("probes = %d, want one pass before stopping", got)
	}
}

// TestClientNeverFollowsRedirects: the default client is a fresh,
// keep-alive-free transport with the default timeout, and reports a
// redirect rather than following it (the redirect is the evidence).
func TestClientNeverFollowsRedirects(t *testing.T) {
	c := (&Prober{}).client()
	if c.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || !tr.DisableKeepAlives || tr == http.DefaultTransport {
		t.Errorf("transport = %T (keep-alives off %v), want a private clone", c.Transport, ok && tr.DisableKeepAlives)
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
	if got := (&Prober{Timeout: 7}).client().Timeout; got != 7 {
		t.Errorf("configured timeout = %v", got)
	}
}

// TestUnbuildableHostIsUnreachable: a label that cannot form a URL is
// counted unreachable, never unprotected, and no request is sent.
func TestUnbuildableHostIsUnreachable(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	rt := &countingHosts{}
	p := &Prober{Lister: views{{UID: "u", Label: "bad label", Slot: 1, Live: true}}, Callbacks: cb, Issuer: issuer,
		Transport: rt}
	res := p.Once(context.Background())
	if res.Probed != 1 || res.Unreachable != 1 || res.Unprotected != 0 || rt.n.Load() != 0 {
		t.Errorf("result = %+v (requests %d)", res, rt.n.Load())
	}
}

// TestFailuresAreLogged: a failed listing and an unprotected host each leave
// a log line on the configured logger.
func TestFailuresAreLogged(t *testing.T) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &Prober{Lister: failing{}, Callbacks: cb, Issuer: issuer, Log: log}
	p.Once(context.Background())
	p.Lister = views{{UID: "u", Label: "b-2", Slot: 1, Live: true}}
	p.Transport = hosts{"b-2": "app"}
	p.Once(context.Background())
	for _, want := range []string{"list previews", "not protected by the sign-in relay", "label=b-2"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}
