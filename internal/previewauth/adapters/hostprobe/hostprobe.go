// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package hostprobe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

const scopeName = "github.com/bitwise-media-group/patchy/internal/previewauth/adapters/hostprobe"

// DefaultTimeout bounds one host's probe.
const DefaultTimeout = 10 * time.Second

// maxConcurrent bounds the probes in flight.
const maxConcurrent = 4

var (
	unprotectedGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("patchy.preview_auth.unprotected_hosts",
			metric.WithDescription("Ready preview hosts that answered an unauthenticated request without "+
				"redirecting to the relay for their own slot's client"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
	unreachableGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("patchy.preview_auth.probe.unreachable_hosts",
			metric.WithDescription("Ready preview hosts the relay's probe could not reach"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
)

// Lister lists the Ready Previews to probe.
type Lister interface {
	Ready(ctx context.Context) ([]previewauth.View, error)
}

// Result is one pass's outcome.
type Result struct {
	Probed, Unprotected, Unreachable int
	// UnprotectedLabels lists the unprotected hosts' labels.
	UnprotectedLabels []string
}

// Prober probes preview hosts.
type Prober struct {
	Lister    Lister
	Callbacks previewauth.Callbacks
	Issuer    string
	Interval  time.Duration
	Timeout   time.Duration
	// Transport, when set, carries the probes (tests); otherwise a clone of
	// http.DefaultTransport.
	Transport http.RoundTripper
	Log       *slog.Logger
}

// NeedLeaderElection is false: every replica probes; nothing is written.
func (p *Prober) NeedLeaderElection() bool { return false }

// Start probes every Interval until ctx ends. A non-positive Interval
// disables probing.
func (p *Prober) Start(ctx context.Context) error {
	if p.Interval <= 0 {
		return nil
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *Prober) client() *http.Client {
	rt := p.Transport
	if rt == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableKeepAlives = true
		rt = tr
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Transport: rt, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Once runs one pass and records its gauges. A failed listing records
// nothing (the gauges keep their last values) and is logged.
func (p *Prober) Once(ctx context.Context) Result {
	views, err := p.Lister.Ready(ctx)
	if err != nil {
		p.log().LogAttrs(ctx, slog.LevelWarn, "preview host probe: list previews", slog.Any("error", err))
		return Result{}
	}
	c := p.client()
	var (
		mu  sync.Mutex
		res Result
		wg  sync.WaitGroup
		sem = make(chan struct{}, maxConcurrent)
	)
	for _, v := range views {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			verdict := p.probe(ctx, c, v)
			mu.Lock()
			defer mu.Unlock()
			res.Probed++
			switch verdict {
			case errUnreachable:
				res.Unreachable++
			case nil:
			default:
				res.Unprotected++
				res.UnprotectedLabels = append(res.UnprotectedLabels, v.Label)
			}
		}()
	}
	wg.Wait()
	unprotectedGauge().Record(ctx, int64(res.Unprotected))
	unreachableGauge().Record(ctx, int64(res.Unreachable))
	return res
}

var errUnreachable = errors.New("unreachable")

func (p *Prober) probe(ctx context.Context, c *http.Client, v previewauth.View) error {
	host := v.Label + "." + p.Callbacks.Suffix()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		return errUnreachable
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "patchy-preview-auth-probe")
	resp, err := c.Do(req)
	if err != nil {
		p.log().LogAttrs(ctx, slog.LevelDebug, "preview host probe: unreachable",
			slog.String("label", v.Label), slog.Any("error", err))
		return errUnreachable
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if err := previewauth.JudgeProbe(resp.StatusCode, resp.Header.Get("Location"), p.Issuer, v.Slot, v.Label,
		p.Callbacks); err != nil {
		p.log().LogAttrs(ctx, slog.LevelWarn, "preview host is not protected by the sign-in relay",
			slog.String("label", v.Label), slog.String("slot", strconv.Itoa(v.Slot)),
			slog.Int("status", resp.StatusCode))
		return err
	}
	return nil
}

func (p *Prober) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.DiscardHandler)
}
