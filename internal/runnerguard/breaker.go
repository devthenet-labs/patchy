// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/bitwise-media-group/patchy/internal/runnerguard"

// breakerMetrics is the gauge reporting 1 while a component's breaker is
// tripped, and the meter it came from: a callback must be registered on the
// meter that made its instrument, which after the global provider is swapped
// is no longer what otel.Meter returns.
type breakerMetrics struct {
	meter metric.Meter
	gauge metric.Int64ObservableGauge
}

var sandboxMetrics = sync.OnceValue(func() breakerMetrics {
	m := otel.Meter(scopeName)
	g, err := m.Int64ObservableGauge("patchy.sandbox.breaker",
		metric.WithDescription("1 while the sandbox circuit breaker refuses repository-declared "+
			"runner images (until the controller restarts), by component"))
	if err != nil {
		otel.Handle(err)
	}
	return breakerMetrics{meter: m, gauge: g}
})

// Breaker is the in-memory sandbox circuit breaker of one job controller.
// A repository-image Job whose sandbox probe finds NetworkPolicy unenforced
// trips it, and from then until the process restarts no launch copies a
// repository image: every later Job runs its harness's trusted image, which
// needs no sandbox guarantee beyond what it has always had. It never resets
// by itself on purpose — whether the CNI enforces policy is a property of the
// cluster, and a restart is the operator saying it has been fixed. The nil
// Breaker never trips.
type Breaker struct {
	component string
	log       *slog.Logger

	mu      sync.Mutex
	tripped bool
}

// NewBreaker returns an untripped breaker for component (the controller's
// name, the gauge's attribute), logging its trip to log (nil discards).
func NewBreaker(component string, log *slog.Logger) *Breaker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := &Breaker{component: component, log: log}
	bm := sandboxMetrics()
	attrs := metric.WithAttributes(attribute.String("component", component))
	if _, err := bm.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(bm.gauge, b.value(), attrs)
		return nil
	}, bm.gauge); err != nil {
		otel.Handle(err)
	}
	return b
}

// Trip opens the breaker, logging once, on the first trip, which Job did it.
func (b *Breaker) Trip(ctx context.Context, job, finding string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	first := !b.tripped
	b.tripped = true
	b.mu.Unlock()
	if first {
		b.log.LogAttrs(ctx, slog.LevelError,
			"sandbox breaker tripped: NetworkPolicy is not enforced; refusing repository-declared "+
				"runner images until restart",
			slog.String("component", b.component),
			slog.String("job", job),
			slog.String("finding", finding))
	}
}

// Tripped reports whether the breaker is open.
func (b *Breaker) Tripped() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

func (b *Breaker) value() int64 {
	if b.Tripped() {
		return 1
	}
	return 0
}
