// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
)

func TestFailedStage(t *testing.T) {
	t.Run("keeps reported accounting", func(t *testing.T) {
		reported := &envelope.Stage{
			Outcome: envelope.OutcomeOK, Harness: "claude", Model: "anthropic/claude-opus-5",
			SessionID: "s-1", NumTurns: 12, ElapsedSeconds: 3.5,
			Usage:  envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.5},
			Detail: "agent detail",
		}
		got := FailedStage(reported, "aborted", "pod vanished")
		if got.Outcome != "aborted" || got.Detail != "pod vanished" {
			t.Errorf("verdict = %q/%q, want aborted/pod vanished", got.Outcome, got.Detail)
		}
		if got.Harness != "claude" || got.Model != "anthropic/claude-opus-5" || got.SessionID != "s-1" ||
			got.NumTurns != 12 || got.ElapsedSeconds != 3.5 || got.Usage != reported.Usage {
			t.Errorf("accounting lost: %+v", got)
		}
		// The caller's stage is not mutated.
		if reported.Outcome != envelope.OutcomeOK || reported.Detail != "agent detail" {
			t.Errorf("reported stage mutated: %+v", reported)
		}
	})
	t.Run("nil reported is verdict only", func(t *testing.T) {
		got := FailedStage(nil, string(envelope.OutcomeTimeout), "deadline")
		want := envelope.Stage{Outcome: envelope.OutcomeTimeout, Detail: "deadline"}
		if got != want {
			t.Errorf("FailedStage(nil) = %+v, want %+v", got, want)
		}
	})
}

func TestFromStageMapsEveryField(t *testing.T) {
	st := &envelope.Stage{
		Outcome: envelope.OutcomeReportInvalid, Harness: "codex", Model: "openai/gpt-5.6-sol",
		SessionID: "sess", NumTurns: 7, ElapsedSeconds: 1.2345,
		Usage: envelope.Usage{
			InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.25,
		},
		Detail: strings.Repeat("d", maxDetail+10),
	}
	got := FromStage(st)
	want := v1alpha1.StageResult{
		Outcome: string(envelope.OutcomeReportInvalid), Harness: "codex", Model: "openai/gpt-5.6-sol",
		SessionID: "sess", NumTurns: 7,
		Usage: v1alpha1.UsageSummary{
			InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: "0.250000",
		},
		ElapsedMilliseconds: 1234,
		Detail:              strings.Repeat("d", maxDetail),
	}
	if *got != want {
		t.Errorf("FromStage = %+v\nwant %+v", *got, want)
	}
}

func TestAnalysis(t *testing.T) {
	if got := Analysis(envelope.AnalysisResult{Summary: "ignored"}); got != nil {
		t.Errorf("Analysis(unrated) = %+v, want nil", got)
	}
	got := Analysis(envelope.AnalysisResult{Rating: "high", Summary: strings.Repeat("s", maxDetail+1)})
	if got == nil || got.Rating != v1alpha1.Rating("high") || len(got.Summary) != maxDetail {
		t.Errorf("Analysis(high) = %+v, want rating high and summary capped at %d", got, maxDetail)
	}
}

func TestTruncateReport(t *testing.T) {
	short := "# Report\n"
	if got := TruncateReport(short); got != short {
		t.Errorf("TruncateReport(short) = %q", got)
	}
	exact := strings.Repeat("r", maxReport)
	if got := TruncateReport(exact); got != exact {
		t.Errorf("TruncateReport(exact cap) changed length to %d", len(got))
	}
	// A multi-byte rune straddling the cap is dropped whole, never split.
	straddle := strings.Repeat("r", maxReport-1) + "é"
	got := TruncateReport(straddle)
	if got != strings.Repeat("r", maxReport-1) {
		t.Errorf("TruncateReport(straddle) len %d, want %d", len(got), maxReport-1)
	}
}

// TestTruncateProperty: the result is valid UTF-8 within the cap, is a prefix
// of the input, and is the input unchanged when it already fits; it never
// drops more than one rune's worth (utf8.UTFMax-1 bytes) below the cap.
func TestTruncateProperty(t *testing.T) {
	prop := func(s string, limit uint16) bool {
		l := int(limit%64) + 1
		got := truncate(s, l)
		if !strings.HasPrefix(s, got) || len(got) > l {
			return false
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			return false
		}
		if len(s) <= l {
			return got == s
		}
		return !utf8.ValidString(s) || len(got) > l-utf8.UTFMax
	}
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(3)), MaxCount: 500}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
