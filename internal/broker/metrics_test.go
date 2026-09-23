// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testMetrics installs, once per test binary, a ManualReader-backed global
// MeterProvider. The package's instruments are created lazily from the
// global provider, which delegates to the one installed here even for
// instruments created before it.
var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// counter sums the data points of the named int64 counter whose attributes
// include every pair in attrs.
func counter(t *testing.T, name string, attrs map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
		point:
			for _, dp := range sum.DataPoints {
				for k, v := range attrs {
					if got, ok := dp.Attributes.Value(attribute.Key(k)); !ok || got.AsString() != v {
						continue point
					}
				}
				total += dp.Value
			}
		}
	}
	return total
}

// TestMetrics: the request, pre-authentication and token counters record
// what the broker did, by route, outcome, reason and class.
func TestMetrics(t *testing.T) {
	testMetrics()
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(100, 5, true))
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{"foundry": {Target: mustTarget(t, up.URL)}}}, nil)
	h := s.Handler()

	type probe struct {
		name  string
		attrs map[string]string
		want  int64
	}
	probes := []probe{
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "proxied"}, 2},
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "preauth_token"}, 1},
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "surface"}, 1},
		{"patchy.broker.preauth.rejections", map[string]string{"reason": "token_shape"}, 1},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "input"}, 200},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "cache_creation"}, 20},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "cache_read"}, 40},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "output"}, 10},
	}
	before := make([]int64, len(probes))
	for i, p := range probes {
		before[i] = counter(t, p.name, p.attrs)
	}

	for range 2 {
		if rec := post(h, "/foundry/v1/messages", `{"model":"m","max_tokens":10}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	if rec := post(h, "/foundry/v1/files", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("off-surface: status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/foundry/v1/models", nil)
	req.Header.Set(TokenHeader, "junk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("junk token: status = %d", rec.Code)
	}

	for i, p := range probes {
		if got := counter(t, p.name, p.attrs) - before[i]; got != p.want {
			t.Errorf("%s%v grew by %d, want %d", p.name, p.attrs, got, p.want)
		}
	}
}
