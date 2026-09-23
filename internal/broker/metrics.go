// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/bitwise-media-group/patchy/internal/broker"

// The broker's counters: every request by route and outcome (proxied or the
// rejection reason), the pre-authentication rejections by reason (a flood
// shows here first), and the tokens charged by route and class, "estimated"
// being the request-size charge for a cut or usage-less response.
var (
	requestsCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.broker.requests",
			metric.WithDescription("brokered requests by route and outcome"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	preauthCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.broker.preauth.rejections",
			metric.WithDescription("requests refused before any TokenReview, by reason"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	tokensCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.broker.tokens",
			metric.WithDescription("tokens charged to pods by route and class"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
)

func countRequest(ctx context.Context, route, outcome string) {
	requestsCounter().Add(ctx, 1, metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("outcome", outcome)))
}

func countPreauth(ctx context.Context, reason string) {
	preauthCounter().Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func countTokens(ctx context.Context, route string, u usage, estimated int64) {
	for class, n := range map[string]int64{
		"input":          u.input,
		"cache_creation": u.cacheCreation,
		"cache_read":     u.cacheRead,
		"output":         u.output,
		"estimated":      estimated,
	} {
		if n > 0 {
			tokensCounter().Add(ctx, n, metric.WithAttributes(
				attribute.String("route", route),
				attribute.String("class", class)))
		}
	}
}
