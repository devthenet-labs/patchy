// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/bitwise-media-group/patchy/internal/previewauth/httpapi"

func counter(name, desc string) func() metric.Int64Counter {
	return sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
}

var (
	authorizeCounter = counter("patchy.preview_auth.authorize",
		"authorize requests by result: issued, login, denied, no_preview, bad_request, error")
	tokenCounter    = counter("patchy.preview_auth.token", "token requests by grant type and result")
	userinfoCounter = counter("patchy.preview_auth.userinfo", "userinfo requests by result")
	sarCounter      = counter("patchy.preview_auth.sar", "decided preview access reviews by answer")
	ledgerCounter   = counter("patchy.preview_auth.ledger", "code ledger results: ok, replay, conflict, full, error")
	upstreamCounter = counter("patchy.preview_auth.upstream_login", "Dex sign-ins by result")
	limitedCounter  = counter("patchy.preview_auth.rate_limited", "requests refused by the per-address rate limit")
	durationHist    = sync.OnceValue(func() metric.Float64Histogram {
		h, err := otel.Meter(scopeName).Float64Histogram("patchy.preview_auth.request.duration",
			metric.WithDescription("relay request latency by endpoint"), metric.WithUnit("s"))
		if err != nil {
			otel.Handle(err)
		}
		return h
	})
)

func count(ctx context.Context, c func() metric.Int64Counter, attrs ...attribute.KeyValue) {
	c().Add(ctx, 1, metric.WithAttributes(attrs...))
}

// CountSAR records a decided access review; the access adapter's hook.
func CountSAR(ctx context.Context, allowed bool) {
	count(ctx, sarCounter, attribute.Bool("allowed", allowed))
}

// CountLedger records a ledger result; the ledger adapter's hook.
func CountLedger(ctx context.Context, result string) {
	count(ctx, ledgerCounter, attribute.String("result", result))
}

func observe(ctx context.Context, endpoint string, d time.Duration) {
	durationHist().Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("endpoint", endpoint)))
}
