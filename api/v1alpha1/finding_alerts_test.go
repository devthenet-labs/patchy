// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"reflect"
	"testing"
)

// TestFindingAlertsBySource pins how the verdict write-back splits a
// finding's alerts between sources: an alert's own source wins, spec.source
// stands in for alerts predating Alert.Source, and an alert whose source is
// unknowable is dropped rather than sent to a guessed tool.
func TestFindingAlertsBySource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		alerts []Alert
		want   map[string][]Alert
	}{
		{
			name:   "no alerts",
			source: "github",
			want:   nil,
		},
		{
			name:   "own source wins over spec source",
			source: "github",
			alerts: []Alert{
				{ID: "1", Source: "wiz"},
				{ID: "2", Source: "github"},
				{ID: "3", Source: "wiz"},
			},
			want: map[string][]Alert{
				"wiz":    {{ID: "1", Source: "wiz"}, {ID: "3", Source: "wiz"}},
				"github": {{ID: "2", Source: "github"}},
			},
		},
		{
			name:   "legacy alerts fall back to spec source",
			source: "github",
			alerts: []Alert{{ID: "1"}, {ID: "2", Source: "scanner"}},
			want: map[string][]Alert{
				"github":  {{ID: "1"}},
				"scanner": {{ID: "2", Source: "scanner"}},
			},
		},
		{
			name:   "unknowable source is dropped",
			alerts: []Alert{{ID: "1"}, {ID: "2", Source: "wiz"}},
			want:   map[string][]Alert{"wiz": {{ID: "2", Source: "wiz"}}},
		},
		{
			name:   "every source unknowable",
			alerts: []Alert{{ID: "1"}},
			want:   map[string][]Alert{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Finding{Spec: FindingSpec{Source: tt.source, Alerts: tt.alerts}}
			if got := f.AlertsBySource(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AlertsBySource() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
