// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func TestStageDeltaFrom(t *testing.T) {
	full := &v1alpha1.StageResult{
		Outcome: "ok", Harness: "claude", Model: "claude-sonnet-5", NumTurns: 12,
		ElapsedMilliseconds: 90_000,
		Usage: v1alpha1.UsageSummary{
			InputTokens: 100, OutputTokens: 40, CacheReadTokens: 7, CacheCreationTokens: 3,
			CostUSD: "0.42",
		},
	}
	cases := []struct {
		name     string
		st       *v1alpha1.StageResult
		est      *v1alpha1.AgentEstimate
		want     StageDelta
		wantErr  bool
		estimate *EstimateDelta
	}{
		{
			name: "no result is an aborted run",
			want: StageDelta{Stage: "remediation", Outcome: "aborted", Succeeded: true},
		},
		{
			name: "full result without estimate",
			st:   full,
			want: StageDelta{
				Stage: "remediation", Outcome: "ok", Succeeded: true, Harness: "claude", Model: "claude-sonnet-5",
				InputTokens: 100, OutputTokens: 40, CacheReadTokens: 7, CacheCreationTokens: 3,
				CostMicroUSD: 420_000, ElapsedMilliseconds: 90_000, Turns: 12,
			},
		},
		{
			name: "empty outcome stays aborted",
			st:   &v1alpha1.StageResult{Harness: "codex"},
			want: StageDelta{Stage: "remediation", Outcome: "aborted", Succeeded: true, Harness: "codex"},
		},
		{
			name: "zero estimate is no estimate",
			st:   full,
			est:  &v1alpha1.AgentEstimate{},
			want: StageDelta{
				Stage: "remediation", Outcome: "ok", Succeeded: true, Harness: "claude", Model: "claude-sonnet-5",
				InputTokens: 100, OutputTokens: 40, CacheReadTokens: 7, CacheCreationTokens: 3,
				CostMicroUSD: 420_000, ElapsedMilliseconds: 90_000, Turns: 12,
			},
		},
		{
			name: "estimate pairs prediction with the actual run",
			st:   full,
			est:  &v1alpha1.AgentEstimate{MaxTurns: 20, TokenBudget: 5000},
			want: StageDelta{
				Stage: "remediation", Outcome: "ok", Succeeded: true, Harness: "claude", Model: "claude-sonnet-5",
				InputTokens: 100, OutputTokens: 40, CacheReadTokens: 7, CacheCreationTokens: 3,
				CostMicroUSD: 420_000, ElapsedMilliseconds: 90_000, Turns: 12,
			},
			estimate: &EstimateDelta{PredictedTurns: 20, ActualTurns: 12, PredictedOutputTokens: 5000, ActualOutputTokens: 40},
		},
		{
			name:    "malformed cost is an error",
			st:      &v1alpha1.StageResult{Outcome: "ok", Usage: v1alpha1.UsageSummary{CostUSD: "1.2.3"}},
			want:    StageDelta{Stage: "remediation", Outcome: "aborted", Succeeded: true},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := StageDeltaFrom("remediation", c.st, true, c.est)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			gotEst := got.Estimate
			got.Estimate = nil
			if got != c.want {
				t.Errorf("delta = %+v, want %+v", got, c.want)
			}
			switch {
			case c.estimate == nil && gotEst != nil:
				t.Errorf("estimate = %+v, want nil", gotEst)
			case c.estimate != nil && (gotEst == nil || *gotEst != *c.estimate):
				t.Errorf("estimate = %+v, want %+v", gotEst, c.estimate)
			}
		})
	}
}

func TestCalibrationFrom(t *testing.T) {
	withEstimate := func(runs int64) *v1alpha1.FindingRollupStatus {
		return &v1alpha1.FindingRollupStatus{Bucket: v1alpha1.RollupBucket{
			Stages: map[string]v1alpha1.StageAggregate{"remediation": {Estimate: &v1alpha1.EstimateAggregate{
				Runs: runs, PredictedTurns: 30 * runs, ActualTurns: 20 * runs,
				PredictedOutputTokens: 9000 * runs, ActualOutputTokens: 6000 * runs,
			}}},
		}}
	}
	if CalibrationFrom(nil, "x") != nil {
		t.Error("nil status: want nil calibration")
	}
	if CalibrationFrom(&v1alpha1.FindingRollupStatus{}, "x") != nil {
		t.Error("no remediation stage: want nil calibration")
	}
	noEst := &v1alpha1.FindingRollupStatus{Bucket: v1alpha1.RollupBucket{
		Stages: map[string]v1alpha1.StageAggregate{"remediation": {Runs: 9}},
	}}
	if CalibrationFrom(noEst, "x") != nil {
		t.Error("remediation without estimates: want nil calibration")
	}
	if CalibrationFrom(withEstimate(MinCalibrationRuns-1), "x") != nil {
		t.Error("thin history: want nil calibration")
	}
	got := CalibrationFrom(withEstimate(MinCalibrationRuns+1), "acme/orders")
	if got == nil {
		t.Fatal("enough history: want a calibration")
	}
	if got.Scope != "acme/orders" || got.Runs != MinCalibrationRuns+1 ||
		got.AvgPredictedTurns != 30 || got.AvgActualTurns != 20 ||
		got.AvgPredictedOutputTokens != 9000 || got.AvgActualOutputTokens != 6000 {
		t.Errorf("calibration = %+v", got)
	}
}

func TestPhaseKey(t *testing.T) {
	cases := map[v1alpha1.Phase]string{
		v1alpha1.Phase("Remediated"): "remediated",
		v1alpha1.Phase("HandedOff"):  "handedoff",
		v1alpha1.Phase(""):           "",
	}
	for in, want := range cases {
		if got := PhaseKey(in); got != want {
			t.Errorf("PhaseKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyUpgradesSchemaAndStamps: an older status is upgraded in place and
// stamped with first/last processed; a later apply keeps the first stamp.
func TestApplyUpgradesSchemaAndStamps(t *testing.T) {
	st := v1alpha1.FindingRollupStatus{SchemaVersion: 0}
	d := StageDelta{Stage: "investigation", Outcome: "ok"}
	Apply(&st, "a", &d, nil, applyClock, "")
	if st.SchemaVersion != v1alpha1.RollupSchemaVersion {
		t.Errorf("schema = %d, want %d", st.SchemaVersion, v1alpha1.RollupSchemaVersion)
	}
	if st.FirstProcessed == nil || !st.FirstProcessed.Equal(st.LastProcessed) {
		t.Fatalf("stamps = %v / %v", st.FirstProcessed, st.LastProcessed)
	}
	later := applyClock.Add(3600e9)
	Apply(&st, "b", &d, nil, later, "")
	if !st.FirstProcessed.Time.Equal(applyClock) || !st.LastProcessed.Time.Equal(later) {
		t.Errorf("stamps after second apply = %v / %v", st.FirstProcessed, st.LastProcessed)
	}
	if st.Monthly != nil {
		t.Errorf("monthly = %v, want untouched without a month", st.Monthly)
	}
}

var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// sumOf adds up the named counter's points whose attributes include attrs;
// it reads both int64 and float64 sums (cost is a float counter).
func sumOf(t *testing.T, name string, attrs map[string]string) float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	match := func(set attribute.Set) bool {
		for k, v := range attrs {
			if got, ok := set.Value(attribute.Key(k)); !ok || got.AsString() != v {
				return false
			}
		}
		return true
	}
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if match(dp.Attributes) {
						total += float64(dp.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, dp := range data.DataPoints {
					if match(dp.Attributes) {
						total += dp.Value
					}
				}
			default:
				t.Fatalf("%s is %T", name, m.Data)
			}
		}
	}
	return total
}

// TestRecordMetrics: the rollup-time instruments record runs, spend, token
// classes (zero classes skipped), completions and deletions.
func TestRecordMetrics(t *testing.T) {
	testMetrics()
	ctx := context.Background()
	repo := "acme/" + t.Name() // deltas below keep -count=N exact
	before := map[string]float64{
		"runs":    sumOf(t, "patchy.stage.runs", map[string]string{"repo": repo}),
		"cost":    sumOf(t, "patchy.stage.cost", map[string]string{"repo": repo}),
		"input":   sumOf(t, "patchy.stage.tokens", map[string]string{"stage": "metrics-test", "class": "input"}),
		"cache":   sumOf(t, "patchy.stage.tokens", map[string]string{"stage": "metrics-test", "class": "cache_read"}),
		"done":    sumOf(t, "patchy.finding.completed", map[string]string{"repo": repo, "phase": "remediated"}),
		"deleted": sumOf(t, "patchy.finding.deleted", map[string]string{"reason": "ttl"}),
	}
	RecordStage(ctx, StageDelta{
		Stage: "metrics-test", Harness: "claude", Model: "m", Outcome: "ok",
		InputTokens: 10, OutputTokens: 5, CostMicroUSD: 1_500_000,
	}, repo)
	RecordCompletion(ctx, "remediated", "remediate", repo)
	RecordDeleted(ctx, "ttl")

	checks := []struct {
		key, name string
		attrs     map[string]string
		delta     float64
	}{
		{"runs", "patchy.stage.runs", map[string]string{"repo": repo}, 1},
		{"cost", "patchy.stage.cost", map[string]string{"repo": repo}, 1.5},
		{"input", "patchy.stage.tokens", map[string]string{"stage": "metrics-test", "class": "input"}, 10},
		{"cache", "patchy.stage.tokens", map[string]string{"stage": "metrics-test", "class": "cache_read"}, 0},
		{"done", "patchy.finding.completed", map[string]string{"repo": repo, "phase": "remediated"}, 1},
		{"deleted", "patchy.finding.deleted", map[string]string{"reason": "ttl"}, 1},
	}
	for _, c := range checks {
		if got := sumOf(t, c.name, c.attrs) - before[c.key]; got != c.delta {
			t.Errorf("%s %v: delta = %v, want %v", c.name, c.attrs, got, c.delta)
		}
	}
}
