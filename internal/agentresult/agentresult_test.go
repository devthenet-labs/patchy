// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"testing"
	"testing/quick"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
)

func TestFromStageCost(t *testing.T) {
	// A harness-reported cost is used verbatim.
	st := &envelope.Stage{Model: "anthropic/claude-sonnet-5", Usage: envelope.Usage{CostUSD: 1.25}}
	if got := FromStage(st).Usage.CostUSD; got != "1.250000" {
		t.Errorf("reported cost = %q, want 1.250000", got)
	}

	// No reported cost but a priced model: fall back to token pricing.
	// claude-sonnet-5 is 3/15 per MTok: 1_000_000/1e6*3 + 200_000/1e6*15 = 6.0.
	st = &envelope.Stage{
		Model: "anthropic/claude-sonnet-5",
		Usage: envelope.Usage{InputTokens: 1_000_000, OutputTokens: 200_000},
	}
	if got := FromStage(st).Usage.CostUSD; got != "6.000000" {
		t.Errorf("priced fallback = %q, want 6.000000", got)
	}

	// Codex reports tokens but never a cost, so the fallback is the only thing
	// that prices it. gpt-5.3-codex is 1.75/14 per MTok:
	// 1000/1e6*1.75 + 500/1e6*14 = 0.00175 + 0.007 = 0.00875.
	st = &envelope.Stage{
		Model: "openai/gpt-5.3-codex",
		Usage: envelope.Usage{InputTokens: 1000, OutputTokens: 500},
	}
	if got := FromStage(st).Usage.CostUSD; got != "0.008750" {
		t.Errorf("codex priced fallback = %q, want 0.008750", got)
	}

	// A model outside the registry cannot be priced: cost stays empty.
	st = &envelope.Stage{
		Model: "openai/not-in-the-registry",
		Usage: envelope.Usage{InputTokens: 1000, OutputTokens: 500},
	}
	if got := FromStage(st).Usage.CostUSD; got != "" {
		t.Errorf("unknown model cost = %q, want empty", got)
	}
}

func TestFormatConfidence(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero", 0, "0.0000"},
		{"one", 1, "1.0000"},
		{"interior", 0.85, "0.8500"},
		{"rounds up to one", 0.99996, "1.0000"},
		// A negative zero passes the range check; rendered as-is it would be
		// "-0.0000", which the CRD pattern rejects.
		{"negative zero", math.Copysign(0, -1), "0.0000"},
		{"below range", -0.1, ""},
		{"above range", 1.5, ""},
		// NaN compares false against both bounds, so the range check alone
		// rendered it as "NaN" and the status write would be rejected.
		{"NaN", math.NaN(), ""},
		{"+Inf", math.Inf(1), ""},
		{"-Inf", math.Inf(-1), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatConfidence(tt.in); got != tt.want {
				t.Errorf("FormatConfidence(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFormatConfidenceProperty states FormatConfidence's contract as an
// invariant: it renders exactly the values in [0, 1], and whatever it
// renders is a string the CRD's confidence pattern admits, so a status write
// never fails on it. The generator mixes uniform values straddling the
// interval with the IEEE edge cases.
func TestFormatConfidenceProperty(t *testing.T) {
	pattern := regexp.MustCompile(v1alpha1.ConfidencePattern)
	edges := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), 0, math.Copysign(0, -1), 1,
		math.Nextafter(1, 2), math.Nextafter(0, -1), math.Nextafter(1, 0), math.SmallestNonzeroFloat64,
	}
	cfg := &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(20260924)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			f := r.Float64()*2 - 0.5 // [-0.5, 1.5)
			if r.Intn(4) == 0 {
				f = edges[r.Intn(len(edges))]
			}
			args[0] = reflect.ValueOf(f)
		},
	}
	rendersValidDecimals := func(f float64) bool {
		got := FormatConfidence(f)
		if !(f >= 0 && f <= 1) {
			return got == ""
		}
		return pattern.MatchString(got)
	}
	if err := quick.Check(rendersValidDecimals, cfg); err != nil {
		t.Error(err)
	}
}

func TestFormatCost(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"reported", 1.25, "1.250000"},
		{"unreported", 0, ""},
		{"negative", -1, ""},
		// Neither fails the positive-cost check on its own: NaN compares
		// false, +Inf is above zero. Both would render as strings the CRD's
		// decimal pattern rejects.
		{"NaN", math.NaN(), ""},
		{"+Inf", math.Inf(1), ""},
		{"-Inf", math.Inf(-1), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatCost(tt.in); got != tt.want {
				t.Errorf("FormatCost(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
