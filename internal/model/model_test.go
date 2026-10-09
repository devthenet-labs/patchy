// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package model

import (
	"math/rand"
	"slices"
	"testing"
	"testing/quick"
)

func TestBareIDWithoutProvider(t *testing.T) {
	tests := []struct {
		id, want string
	}{
		{"anthropic/claude-opus-5", "claude-opus-5"},
		{"claude-opus-5", "claude-opus-5"},
		{"a/b/c", "b/c"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := (Model{ID: tt.id}).BareID(); got != tt.want {
			t.Errorf("BareID(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestSupportsAndSupportedHarnessIDs(t *testing.T) {
	m, ok := ModelByID(Builtins(), "anthropic/claude-haiku-4-5")
	if !ok {
		t.Fatal("anthropic/claude-haiku-4-5 missing")
	}
	for _, tt := range []struct {
		harness string
		want    bool
	}{
		{HarnessClaude, true},
		{HarnessCopilot, true},
		{HarnessCodex, false},
		{HarnessFake, false},
		{"", false},
	} {
		if got := m.Supports(tt.harness); got != tt.want {
			t.Errorf("Supports(%q) = %v, want %v", tt.harness, got, tt.want)
		}
	}
	if got, want := m.SupportedHarnessIDs(), []string{HarnessClaude, HarnessCopilot}; !slices.Equal(got, want) {
		t.Errorf("SupportedHarnessIDs = %v, want %v", got, want)
	}
	if got := (Model{}).SupportedHarnessIDs(); len(got) != 0 {
		t.Errorf("SupportedHarnessIDs(empty) = %v, want none", got)
	}
}

// TestSupportedHarnessIDsProperty: the listing is sorted, has exactly the
// Supported keys, and agrees with Supports for every entry.
func TestSupportedHarnessIDsProperty(t *testing.T) {
	prop := func(keys []string) bool {
		m := Model{Supported: map[string]string{}}
		for _, k := range keys {
			m.Supported[k] = "id-" + k
		}
		got := m.SupportedHarnessIDs()
		if !slices.IsSorted(got) || len(got) != len(m.Supported) {
			return false
		}
		for _, id := range got {
			if !m.Supports(id) {
				return false
			}
		}
		return true
	}
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(1)), MaxCount: 200}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestBuiltinsMatchesAllModelsWithoutOverrides(t *testing.T) {
	b := Builtins()
	a := AllModels(nil)
	if len(b) != len(a) || len(b) == 0 {
		t.Fatalf("Builtins has %d models, AllModels(nil) %d", len(b), len(a))
	}
	for i := range b {
		if b[i].ID != a[i].ID {
			t.Errorf("model %d: Builtins %q, AllModels %q", i, b[i].ID, a[i].ID)
		}
	}
	// Builtins hands out a fresh slice each call: a caller mutating its copy
	// cannot corrupt the registry.
	b[0].ID = "mutated"
	if Builtins()[0].ID == "mutated" {
		t.Error("Builtins returned shared backing storage")
	}
}

func TestProviderByID(t *testing.T) {
	for _, tt := range []struct {
		id       string
		wantName string
		wantOK   bool
	}{
		{ProviderAnthropic, "Anthropic", true},
		{ProviderOpenAI, "OpenAI", true},
		{"google", "", false},
		{"", "", false},
	} {
		p, ok := ProviderByID(tt.id)
		if ok != tt.wantOK || p.Name != tt.wantName {
			t.Errorf("ProviderByID(%q) = (%+v, %v), want name %q ok %v", tt.id, p, ok, tt.wantName, tt.wantOK)
		}
		if ok && p.ID != tt.id {
			t.Errorf("ProviderByID(%q).ID = %q", tt.id, p.ID)
		}
		if IsProviderID(tt.id) != tt.wantOK {
			t.Errorf("IsProviderID(%q) = %v, want %v", tt.id, !tt.wantOK, tt.wantOK)
		}
	}
}

func TestModelByIDMissing(t *testing.T) {
	if m, ok := ModelByID(Builtins(), "anthropic/nope"); ok || m.ID != "" {
		t.Errorf("ModelByID(missing) = (%+v, %v), want zero, false", m, ok)
	}
}

// TestAllModelsOverrideUnknownProvider: an override keyed by a provider the
// registry does not know replaces nothing and adds nothing, since overrides are
// applied in Providers() order.
func TestAllModelsOverrideUnknownProvider(t *testing.T) {
	got := AllModels(map[string][]Model{"google": {{ID: "google/gemini", ProviderID: "google"}}})
	if len(got) != len(Builtins()) {
		t.Errorf("AllModels(unknown override) has %d models, want %d", len(got), len(Builtins()))
	}
	if _, ok := ModelByID(got, "google/gemini"); ok {
		t.Error("model for an unknown provider was added")
	}
}

// TestAllModelsOverrideEmptyListRemovesProvider: replace, not merge — an empty
// list for a provider removes all its builtin models.
func TestAllModelsOverrideEmptyListRemovesProvider(t *testing.T) {
	got := AllModels(map[string][]Model{ProviderAnthropic: {}})
	for _, m := range got {
		if m.ProviderID == ProviderAnthropic {
			t.Errorf("anthropic model %q survived an empty override", m.ID)
		}
	}
	if len(got) == 0 {
		t.Error("openai models were dropped too")
	}
}
