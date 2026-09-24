// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

func TestPreviousAttempt(t *testing.T) {
	long := "x" + strings.Repeat("é", 4000) // the cap lands inside a rune
	tests := []struct {
		name  string
		stage *v1alpha1.StageResult
		want  *v1alpha1.PreviousAttempt
	}{
		{"no stage, nothing to tell", nil, nil},
		{"outcome and detail carried",
			&v1alpha1.StageResult{Outcome: "commit_failed", Detail: "working tree not clean after commit.sh:\nM bin"},
			&v1alpha1.PreviousAttempt{Name: "f-rem-1", Attempt: 1, Outcome: "commit_failed",
				Detail: "working tree not clean after commit.sh:\nM bin"}},
		{"a missing outcome is named unknown",
			&v1alpha1.StageResult{Detail: "no event"},
			&v1alpha1.PreviousAttempt{Name: "f-rem-1", Attempt: 1, Outcome: "unknown", Detail: "no event"}},
		{"both are capped to the API bounds",
			&v1alpha1.StageResult{Outcome: strings.Repeat("o", 200), Detail: long},
			&v1alpha1.PreviousAttempt{Name: "f-rem-1", Attempt: 1, Outcome: strings.Repeat("o", maxOutcome),
				Detail: long[:maxDetail-1]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PreviousAttempt("f-rem-1", 1, tt.stage)
			if (got == nil) != (tt.want == nil) || got != nil && *got != *tt.want {
				t.Errorf("PreviousAttempt() = %+v, want %+v", got, tt.want)
			}
			if got != nil && !utf8.ValidString(got.Detail) {
				t.Error("detail cut inside a rune")
			}
		})
	}
}

func TestEncodePreviousAttempt(t *testing.T) {
	if got := EncodePreviousAttempt(nil); got != "" {
		t.Errorf("EncodePreviousAttempt(nil) = %q, want empty (no variable, no section)", got)
	}
	in := &v1alpha1.PreviousAttempt{Name: "f-rem-1", Attempt: 1, Outcome: "commit_failed",
		Detail: "working tree not clean after commit.sh:\nM <patchy-target> & \"x\""}
	var got templates.PreviousAttempt
	if err := json.Unmarshal([]byte(EncodePreviousAttempt(in)), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := templates.PreviousAttempt{Attempt: 1, Outcome: "commit_failed", Detail: in.Detail}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
