// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261009

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// TestParseCostMicroUSDRoundTrip: any whole-dollar amount and six-digit
// fraction rendered as a decimal parses back to exactly whole*1e6+frac.
func TestParseCostMicroUSDRoundTrip(t *testing.T) {
	prop := func(whole uint32, frac uint32) bool {
		f := int64(frac % 1_000_000)
		s := fmt.Sprintf("%d.%06d", whole, f)
		got, err := ParseCostMicroUSD(s)
		return err == nil && got == int64(whole)*1_000_000+f
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
}

// TestParseCostMicroUSDTruncates: digits past micro precision never change
// the value, and surrounding whitespace is ignored.
func TestParseCostMicroUSDTruncates(t *testing.T) {
	prop := func(whole uint16, frac uint32, extra uint32) bool {
		base := fmt.Sprintf("%d.%06d", whole, frac%1_000_000)
		want, err := ParseCostMicroUSD(base)
		if err != nil {
			return false
		}
		got, err := ParseCostMicroUSD("  " + base + fmt.Sprint(extra) + "\t")
		return err == nil && got == want
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
}

// TestParseCostMicroUSDRejectsNonDigits: a single non-digit anywhere in the
// whole or fraction part is an error, never a silently wrong number.
func TestParseCostMicroUSDRejectsNonDigits(t *testing.T) {
	prop := func(whole uint16, frac uint16, pos uint8, bad byte) bool {
		if (bad >= '0' && bad <= '9') || bad == '.' || bad == ' ' || bad == '\t' ||
			bad == '\n' || bad == '\r' || bad == '\v' || bad == '\f' || bad >= 0x80 {
			return true // not a corrupting byte for this property
		}
		s := []byte(fmt.Sprintf("%d.%d", whole, frac))
		i := int(pos) % len(s)
		if s[i] == '.' {
			i = 0
		}
		s[i] = bad
		_, err := ParseCostMicroUSD(string(s))
		return err != nil
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
}

// TestParseCostMicroUSDEdgeShapes pins the shapes around the separator.
func TestParseCostMicroUSDEdgeShapes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{".", 0, true},
		{".5", 500_000, false},
		{"7.", 7_000_000, false},
		{"   ", 0, false},
		{"1.x", 0, true},
	}
	for _, c := range cases {
		got, err := ParseCostMicroUSD(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ParseCostMicroUSD(%q) = %d, %v; want %d, wantErr %v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

// TestScopeObjectNameShape: every non-total, non-repository scope name is
// "<type>-" followed by a 1..40 character fragment of [a-z0-9-] that does
// not start with a dash, whatever the key holds.
func TestScopeObjectNameShape(t *testing.T) {
	prop := func(key string, model bool) bool {
		typ := v1alpha1.ScopeHarness
		if model {
			typ = v1alpha1.ScopeModel
		}
		name := ScopeObjectName(v1alpha1.RollupScope{Type: typ, Key: key})
		frag, ok := strings.CutPrefix(name, string(typ)+"-")
		if !ok || frag == "" || len(frag) > 40 || frag[0] == '-' {
			return false
		}
		for _, ch := range []byte(frag) {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(1000)); err != nil {
		t.Error(err)
	}
}

// TestScopeObjectNameRepositoryDeterministic: the repository scope name is a
// pure function of the key, fixed width, and distinct keys (in practice)
// never collide.
func TestScopeObjectNameRepositoryDeterministic(t *testing.T) {
	prop := func(a, b string) bool {
		na := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: a})
		again := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: a})
		nb := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: b})
		if na != again || len(na) != len("repo-")+10 || !strings.HasPrefix(na, "repo-") {
			return false
		}
		return (a == b) == (na == nb)
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
}

// TestScopeObjectNameEmptyAndLong pins the fallbacks: a key with no usable
// character becomes "x", and a long key is cut to 40 characters.
func TestScopeObjectNameEmptyAndLong(t *testing.T) {
	if got := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeHarness, Key: "///"}); got != "harness-x" {
		t.Errorf("all-separator key = %q, want harness-x", got)
	}
	if got := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeHarness}); got != "harness-x" {
		t.Errorf("empty key = %q, want harness-x", got)
	}
	long := strings.Repeat("a", 60)
	if got := ScopeObjectName(v1alpha1.RollupScope{Type: v1alpha1.ScopeModel, Key: long}); got != "model-"+long[:40] {
		t.Errorf("long key = %q, want 40-char fragment", got)
	}
}

// genDelta builds a bounded stage delta from random inputs.
func genDelta(r *rand.Rand, stage string) StageDelta {
	d := StageDelta{
		Stage:               stage,
		Outcome:             []string{"ok", "timeout", "aborted"}[r.Intn(3)],
		Succeeded:           r.Intn(2) == 0,
		InputTokens:         r.Int63n(1 << 20),
		OutputTokens:        r.Int63n(1 << 20),
		CacheReadTokens:     r.Int63n(1 << 20),
		CacheCreationTokens: r.Int63n(1 << 20),
		CostMicroUSD:        r.Int63n(1 << 30),
		ElapsedMilliseconds: r.Int63n(1 << 30),
		Turns:               r.Int63n(200),
	}
	if r.Intn(2) == 0 {
		d.Estimate = &EstimateDelta{
			PredictedTurns: r.Int63n(200), ActualTurns: d.Turns,
			PredictedOutputTokens: r.Int63n(1 << 20), ActualOutputTokens: d.OutputTokens,
		}
	}
	return d
}

// expectedStage is the plain sum of a set of stage deltas, computed apart
// from Apply.
type expectedStage struct {
	agg                         v1alpha1.StageAggregate
	estRuns, predTurns, predTok int64
	monthlyCost                 int64
}

func (e *expectedStage) add(d StageDelta) {
	e.agg.Runs++
	if d.Succeeded {
		e.agg.Succeeded++
	}
	if e.agg.Outcomes == nil {
		e.agg.Outcomes = map[string]int64{}
	}
	e.agg.Outcomes[d.Outcome]++
	e.agg.InputTokens += d.InputTokens
	e.agg.OutputTokens += d.OutputTokens
	e.agg.CacheReadTokens += d.CacheReadTokens
	e.agg.CacheCreationTokens += d.CacheCreationTokens
	e.agg.CostMicroUSD += d.CostMicroUSD
	e.agg.ElapsedMilliseconds += d.ElapsedMilliseconds
	e.agg.Turns += d.Turns
	e.monthlyCost += d.CostMicroUSD
	if d.Estimate != nil {
		e.estRuns++
		e.predTurns += d.Estimate.PredictedTurns
		e.predTok += d.Estimate.PredictedOutputTokens
	}
}

// check reports how got differs from the expected sums, or "".
func (e *expectedStage) check(got v1alpha1.StageAggregate) string {
	w := e.agg
	if got.Runs != w.Runs || got.Succeeded != w.Succeeded ||
		got.InputTokens != w.InputTokens || got.OutputTokens != w.OutputTokens ||
		got.CacheReadTokens != w.CacheReadTokens || got.CacheCreationTokens != w.CacheCreationTokens ||
		got.CostMicroUSD != w.CostMicroUSD || got.ElapsedMilliseconds != w.ElapsedMilliseconds ||
		got.Turns != w.Turns {
		return fmt.Sprintf("aggregate = %+v, want %+v", got, w)
	}
	for k, v := range w.Outcomes {
		if got.Outcomes[k] != v {
			return fmt.Sprintf("outcomes[%s] = %d, want %d", k, got.Outcomes[k], v)
		}
	}
	switch {
	case e.estRuns == 0 && got.Estimate != nil:
		return fmt.Sprintf("estimate = %+v with no estimated runs", got.Estimate)
	case e.estRuns > 0 && (got.Estimate == nil || got.Estimate.Runs != e.estRuns ||
		got.Estimate.PredictedTurns != e.predTurns || got.Estimate.PredictedOutputTokens != e.predTok):
		return fmt.Sprintf("estimate = %+v, want runs %d turns %d tokens %d",
			got.Estimate, e.estRuns, e.predTurns, e.predTok)
	}
	return ""
}

// TestApplyStageAdditiveAndExactlyOnce: applying N distinct-key deltas, each
// one replayed, yields aggregates equal to the plain sums of the N deltas —
// the ledger absorbs every replay, and the estimate aggregate counts exactly
// the runs that carried an estimate.
func TestApplyStageAdditiveAndExactlyOnce(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	for iter := range 50 {
		var st v1alpha1.FindingRollupStatus
		n := 1 + r.Intn(40)
		var want expectedStage
		for i := range n {
			d := genDelta(r, "remediation")
			key := fmt.Sprintf("r:%d:%d", iter, i)
			if !Apply(&st, key, &d, nil, applyClock, "2026-10") {
				t.Fatalf("iter %d: first Apply(%s) = false", iter, key)
			}
			if Apply(&st, key, &d, nil, applyClock, "2026-10") {
				t.Fatalf("iter %d: replayed Apply(%s) = true", iter, key)
			}
			want.add(d)
		}
		if msg := want.check(st.Bucket.Stages["remediation"]); msg != "" {
			t.Fatalf("iter %d: %s", iter, msg)
		}
		if m := st.Monthly["2026-10"]; m.Runs != int64(n) || m.CostMicroUSD != want.monthlyCost {
			t.Fatalf("iter %d: monthly = %+v, want runs %d cost %d", iter, m, n, want.monthlyCost)
		}
		if len(st.Recent) != n {
			t.Fatalf("iter %d: ledger = %d entries, want %d", iter, len(st.Recent), n)
		}
	}
}

// TestApplyFindingCountersNeverNegative: whatever sequence of finding deltas
// arrives (including reversals of phases never counted), no counter in the
// bucket ever goes below zero.
func TestApplyFindingCountersNeverNegative(t *testing.T) {
	phases := []string{"", "remediated", "failed", "dismissed", "handedoff"}
	recs := []string{"", "remediate", "ignore", "manual"}
	r := rand.New(rand.NewSource(seed))
	for iter := range 100 {
		var st v1alpha1.FindingRollupStatus
		for i := range 30 {
			d := FindingDelta{
				Phase:              phases[r.Intn(len(phases))],
				PrevPhase:          phases[r.Intn(len(phases))],
				Recommendation:     recs[r.Intn(len(recs))],
				PrevRecommendation: recs[r.Intn(len(recs))],
				Count:              r.Intn(2) == 0,
				Uncount:            r.Intn(2) == 0,
				First:              r.Intn(4) == 0,
				Attempts:           r.Int63n(3),
			}
			Apply(&st, fmt.Sprintf("f:%d:%d", iter, i), nil, &d, applyClock, "2026-10")
			b := st.Bucket
			if b.Findings < 0 || b.Attempts < 0 {
				t.Fatalf("iter %d step %d: findings %d attempts %d", iter, i, b.Findings, b.Attempts)
			}
			for k, v := range b.Phases {
				if v < 0 {
					t.Fatalf("iter %d step %d: phases[%s] = %d", iter, i, k, v)
				}
			}
			for k, v := range b.Recommendations {
				if v < 0 {
					t.Fatalf("iter %d step %d: recommendations[%s] = %d", iter, i, k, v)
				}
			}
		}
	}
}
