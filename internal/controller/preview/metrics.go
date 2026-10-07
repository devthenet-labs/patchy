// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/bitwise-media-group/patchy/internal/controller/preview"

// Instruments are made once, on first use, from the global meter provider,
// which telemetry.Init has installed by then.
var (
	ingressRefusedCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.preview.ingress.refused",
			metric.WithDescription("preview Ingress writes refused by admission (or RBAC), by slot; "+
				"each waits and is retried at the next poll, never spending a deploy attempt"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	unauthenticatedGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("patchy.preview.ingress.unauthenticated",
			metric.WithDescription("slot Ingresses of the preview class without the pinned sign-in "+
				"annotations of the current or previous key generation, by slot, at the last sweep "+
				"(recorded only while preview auth is required)"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
	unauthenticatedDeletedCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.preview.ingress.unauthenticated.deleted",
			metric.WithDescription("slot Ingresses the sweeper deleted after they stayed without the "+
				"pinned sign-in annotations past the grace period, by slot"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
)

func slotAttr(slot int32) metric.MeasurementOption {
	return metric.WithAttributes(attribute.Int("slot", int(slot)))
}

func recordIngressRefused(ctx context.Context, slot int32) {
	if c := ingressRefusedCounter(); c != nil {
		c.Add(ctx, 1, slotAttr(slot))
	}
}

func recordUnauthenticated(ctx context.Context, slot int32, n int) {
	if g := unauthenticatedGauge(); g != nil {
		g.Record(ctx, int64(n), slotAttr(slot))
	}
}

func recordUnauthenticatedDeleted(ctx context.Context, slot int32) {
	if c := unauthenticatedDeletedCounter(); c != nil {
		c.Add(ctx, 1, slotAttr(slot))
	}
}
