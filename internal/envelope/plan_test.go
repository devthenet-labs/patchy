// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package envelope

import (
	"reflect"
	"strings"
	"testing"
)

func testPlanEvent() Event {
	return Event{
		Type:    TypePlan,
		Repo:    "devthenet-labs/patchy-target",
		Finding: "target-1-plan-r1-a1",
		Plan: &Plan{
			Stage: Stage{
				Outcome: OutcomeOK, Harness: "claude", Model: "anthropic/claude-sonnet-5",
				SessionID: "a1b2c3d4-0000-0000-0000-000000000000", NumTurns: 12,
				Usage:          Usage{InputTokens: 900, OutputTokens: 4100, CostUSD: 0.31},
				ElapsedSeconds: 61.5,
			},
			ReportMarkdown:       "---\nsummary: \"Add GET /version\"\n---\n## Approach\n",
			Summary:              "Add GET /version",
			Repositories:         []string{"https://github.com/devthenet-labs/patchy-target"},
			NewDependencies:      []string{"golang.org/x/mod v0.20.0"},
			Questions:            []string{"RFC 3339 build time?"},
			Confidence:           0.8,
			EstimatedMaxTurns:    40,
			EstimatedTokenBudget: 200000,
		},
	}
}

// TestPlanEventRoundTrip: the intent plan event is an additive type at the
// current version — it encodes at Version 4 and decodes back whole.
func TestPlanEventRoundTrip(t *testing.T) {
	want := testPlanEvent()
	line, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !strings.HasPrefix(line, Prefix+`{"v":4,"type":"plan",`) {
		t.Errorf("line %q is not a version-4 plan event", line)
	}
	got, ok := Decode([]byte(line))
	if !ok {
		t.Fatal("Decode() ok = false")
	}
	want.V = Version
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

// TestDecodePlanWire pins the plan event's wire names, as a collector built
// from this package reads them off a pod log.
func TestDecodePlanWire(t *testing.T) {
	line := Prefix + `{"v":4,"type":"plan","repo":"devthenet-labs/patchy-target","finding":"target-1-plan-r1-a1",` +
		`"plan":{"outcome":"ok","harness":"claude","model":"anthropic/claude-sonnet-5","usage":{"input_tokens":1,` +
		`"output_tokens":2,"cache_read_tokens":0,"cache_creation_tokens":0,"cost_usd":0.5},"elapsed_seconds":3,` +
		`"report_markdown":"---\n...","summary":"Add GET /version",` +
		`"repositories":["https://github.com/devthenet-labs/patchy-target"],"new_dependencies":["dep"],` +
		`"questions":["q?"],"confidence":0.8,"estimated_max_turns":40,"estimated_token_budget":200000}}`
	got, ok := Decode([]byte(line))
	if !ok || got.Type != TypePlan || got.Plan == nil {
		t.Fatalf("Decode() = %+v, %v; want a plan event", got, ok)
	}
	p := got.Plan
	if p.Outcome != OutcomeOK || p.Summary != "Add GET /version" || len(p.Repositories) != 1 ||
		len(p.NewDependencies) != 1 || len(p.Questions) != 1 || p.Confidence != 0.8 ||
		p.EstimatedMaxTurns != 40 || p.EstimatedTokenBudget != 200000 || p.ReportMarkdown != "---\n..." ||
		p.Usage.CostUSD != 0.5 {
		t.Errorf("plan = %+v", p)
	}
	if got.Investigation != nil || got.Remediation != nil {
		t.Errorf("a plan event decoded with another stage's payload: %+v", got)
	}
}

// TestDecodePlanAtOtherVersionsRejected: the plan type rides version 4 and
// no other, like every type.
func TestDecodePlanAtOtherVersionsRejected(t *testing.T) {
	for _, v := range []string{"3", "5"} {
		if _, ok := Decode([]byte(Prefix + `{"v":` + v + `,"type":"plan","plan":{"outcome":"ok"}}`)); ok {
			t.Errorf("Decode(v%s plan) ok = true, want false", v)
		}
	}
}

// TestFindingEventWireUnchanged: adding the plan type left the Finding
// stages' events byte-for-byte what the job controllers already read.
func TestFindingEventWireUnchanged(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		{"investigation", testEvent(), Prefix + `{"v":4,"type":"investigation","repo":"acme/shop",` +
			`"finding":"finding-abc123def0-1","investigation":{"outcome":"ok","harness":"claude",` +
			`"model":"claude-sonnet-5","session_id":"a1b2c3d4-0000-0000-0000-000000000000","num_turns":9,` +
			`"usage":{"input_tokens":1200,"output_tokens":5600,"cache_read_tokens":0,"cache_creation_tokens":0,` +
			`"cost_usd":0.42},"elapsed_seconds":93.1,"report_markdown":"report","exploitability":{"rating":"high",` +
			`"summary":"reachable from the request path"},"likelihood":{"rating":"medium",` +
			`"summary":"requires authenticated caller"},"impact":{"rating":"critical","summary":"full data read"},` +
			`"recommendation":"remediate","priority":"high","severity":"high","confidence":0.85,` +
			`"remediation_model":"claude-sonnet-5","estimated_max_turns":40,"estimated_token_budget":200000,` +
			`"await_approval":false,"hold_reasons":["breakingChangeAvailable"]}}`},
		{"remediation", Event{
			Type: TypeRemediation, Repo: "acme/shop", Finding: "finding-abc123def0-1",
			Remediation: &Remediation{
				Stage:   Stage{Outcome: OutcomeOK, Harness: "claude", Model: "claude-sonnet-5"},
				Success: true, Confidence: 0.9, Branch: "patchy/finding-abc123def0-1",
				Changeset: &Changeset{BaseSHA: "0123", CommitMessage: "fix", Deletes: []string{"a.go"}},
			},
		}, Prefix + `{"v":4,"type":"remediation","repo":"acme/shop","finding":"finding-abc123def0-1",` +
			`"remediation":{"outcome":"ok","harness":"claude","model":"claude-sonnet-5","usage":{"input_tokens":0,` +
			`"output_tokens":0,"cache_read_tokens":0,"cache_creation_tokens":0,"cost_usd":0},"elapsed_seconds":0,` +
			`"success":true,"confidence":0.9,"branch":"patchy/finding-abc123def0-1","changeset":{"base_sha":"0123",` +
			`"commit_message":"fix","deletes":["a.go"]}}}`},
		{"fatal", Event{Type: TypeFatal, Repo: "acme/shop", Error: "workspace missing"},
			Prefix + `{"v":4,"type":"fatal","repo":"acme/shop","error":"workspace missing"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.ev.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Encode() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
