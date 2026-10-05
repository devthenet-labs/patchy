// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

// streamCutOff is a claude stream that stops before its result event, as a
// timed-out, killed or crashed run's does. It holds two API messages, the
// first split over two content-block events that repeat its usage, which is
// how the CLI emits them.
const streamCutOff = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,` +
	`"cache_read_input_tokens":2000,"output_tokens":1},"content":[{"type":"thinking","thinking":""}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,` +
	`"cache_read_input_tokens":2000,"output_tokens":1},` +
	`"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"npm ci"}}]}}` + "\n" +
	`{"type":"user","message":{"content":[{"type":"tool_result","content":"added 912 packages"}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":8,"cache_creation_input_tokens":500,` +
	`"cache_read_input_tokens":3000,"output_tokens":3},` +
	`"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"npm run test:ci"}}]}}` + "\n"

// cutOffUsage is streamCutOff's usage, each message counted once.
var cutOffUsage = envelope.Usage{InputTokens: 18, CacheCreationTokens: 1500, CacheReadTokens: 5000, OutputTokens: 4}

// TestUsageSurvivesEveryOutcome is the regression guard for a stage that
// ended without the CLI's result event recording no usage at all. A build
// that hit its stage timeout reported usage {} although the egress broker
// had proxied 45 model calls for its pod, so its intent's cost ceiling,
// which sums what runs record, could not see that spend. Every stage, on
// every outcome that cuts the stream short, records what the stream
// reported instead.
func TestUsageSurvivesEveryOutcome(t *testing.T) {
	outcomes := []struct {
		name   string
		result runner.Result
		runErr error
		want   envelope.Outcome
	}{
		{
			name:   "stage timeout",
			result: runner.Result{TimedOut: true, ExitCode: -1, Elapsed: time.Hour},
			want:   envelope.OutcomeTimeout,
		},
		{
			name:   "budget kill switch",
			result: runner.Result{Aborted: true, AbortReason: "output token budget exceeded (9 > 8)", ExitCode: -1},
			want:   envelope.OutcomeBudgetExceeded,
		},
		{
			name:   "crash",
			result: runner.Result{ExitCode: -1, ExitStatus: "signal: segmentation fault"},
			want:   envelope.OutcomeRuntimeError,
		},
		{
			name:   "cancelled pod",
			result: runner.Result{ExitCode: -1, ExitStatus: "signal: killed"},
			runErr: context.Canceled,
			want:   envelope.OutcomeRuntimeError,
		},
	}
	stages := []struct {
		phase  Phase
		config func(t *testing.T, out *bytes.Buffer) (Config, string)
	}{
		{PhaseInvestigate, func(t *testing.T, out *bytes.Buffer) (Config, string) {
			ws := newWorkspace(t)
			return newConfig(t, ws, out), ws
		}},
		{PhaseRemediate, func(t *testing.T, out *bytes.Buffer) (Config, string) {
			cfg, ws := remediateConfig(t, goodInvestigation, out)
			cfg.Phase = PhaseRemediate
			return cfg, ws
		}},
		{PhasePlan, func(t *testing.T, out *bytes.Buffer) (Config, string) {
			return intentConfig(t, PhasePlan, out)
		}},
		{PhaseBuild, func(t *testing.T, out *bytes.Buffer) (Config, string) {
			return intentConfig(t, PhaseBuild, out)
		}},
	}
	for _, st := range stages {
		for _, oc := range outcomes {
			t.Run(string(st.phase)+"/"+oc.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := st.config(t, &out)
				fx := &fakeExec{steps: []step{{ws: ws, stdout: streamCutOff, result: oc.result, runErr: oc.runErr}}}
				stage := eventStage(t, onlyEvent(t, cfg, fx, &out))

				if stage.Outcome != oc.want {
					t.Fatalf("outcome = %q, want %q (detail: %q)", stage.Outcome, oc.want, stage.Detail)
				}
				if stage.Usage != cutOffUsage {
					t.Errorf("usage = %+v, want %+v: the stream's usage, each message once", stage.Usage, cutOffUsage)
				}
				if stage.ElapsedSeconds == 0 {
					t.Error("ElapsedSeconds = 0, want the stage's wall clock")
				}
			})
		}
	}
}

// eventStage is the stage an event's payload carries.
func eventStage(t *testing.T, ev envelope.Event) envelope.Stage {
	t.Helper()
	switch {
	case ev.Investigation != nil:
		return ev.Investigation.Stage
	case ev.Remediation != nil:
		return ev.Remediation.Stage
	case ev.Plan != nil:
		return ev.Plan.Stage
	}
	t.Fatalf("event %+v carries no stage", ev)
	return envelope.Stage{}
}
