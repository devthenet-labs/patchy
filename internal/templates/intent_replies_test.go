// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestIntentReplyGoldens pins the command replies, the estimate notice and
// the summary comment.
func TestIntentReplyGoldens(t *testing.T) {
	tests := []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_notice_done_approve.md", func() (string, error) {
			return RenderCommandDoneNotice(CommandDoneNotice{
				Namespace: "patchy", Intent: "target-1", Key: "command-4415",
				Actor: "peter", Verb: "approve", PlanRevision: 2,
			})
		}},
		{"intent_notice_done_replan.md", func() (string, error) {
			return RenderCommandDoneNotice(CommandDoneNotice{
				Namespace: "patchy", Intent: "target-1", Key: "command-4416", Actor: "peter", Verb: "replan",
			})
		}},
		{"intent_notice_done_cancel.md", func() (string, error) {
			return RenderCommandDoneNotice(CommandDoneNotice{
				Namespace: "patchy", Intent: "target-1", Key: "command-4417", Actor: "peter", Verb: "cancel",
			})
		}},
		{"intent_notice_unknown.md", func() (string, error) {
			return RenderUnknownCommandNotice(UnknownCommandNotice{
				Namespace: "patchy", Intent: "target-1", Key: "command-4418", Verb: "ship",
			})
		}},
		{"intent_notice_unknown_empty.md", func() (string, error) {
			return RenderUnknownCommandNotice(UnknownCommandNotice{
				Namespace: "patchy", Intent: "target-1", Key: "command-4419",
			})
		}},
		{"intent_notice_edited.md", func() (string, error) {
			return RenderEditedCommandNotice(EditedCommandNotice{
				Namespace: "patchy", Intent: "target-1", Key: "comment-4420", Verb: "approve",
			})
		}},
		{"intent_notice_ambiguous_approval.md", func() (string, error) {
			return RenderAmbiguousApprovalNotice("patchy", "target-1", "event-4421", "other-1")
		}},
		{"intent_notice_estimate.md", func() (string, error) {
			return RenderEstimateNotice(EstimateNotice{
				Namespace: "patchy", Intent: "target-1", Key: "estimate-r2", PlanRevision: 2,
				EstimatedMaxTurns: 200, EstimatedTokenBudget: 500000,
				GrantMaxTurns: 150, GrantTokenBudget: 800000, TriggerLabel: "patchy:target",
			})
		}},
		{"intent_summary.md", func() (string, error) {
			return RenderIntentSummaryComment(IntentSummaryComment{
				Namespace: "patchy", Intent: "target-1",
				PullRequests: []IntentPullRequest{{
					Repository: "devthenet-labs/patchy-target", Number: 12,
					URL: "https://github.com/devthenet-labs/patchy-target/pull/12", State: "merged",
				}},
				CostMicroUSD: 1_234_567,
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestCommandDoneRefusesUnknownVerbs: a done reply exists only for the
// verbs patchy applies on an intent issue, so a caller cannot claim a verb
// was done that has no outcome to state.
func TestCommandDoneRefusesUnknownVerbs(t *testing.T) {
	for _, verb := range []string{"", "revise", "retry", "expedite"} {
		if _, err := RenderCommandDoneNotice(CommandDoneNotice{Verb: verb}); err == nil {
			t.Errorf("RenderCommandDoneNotice(%q) succeeded, want an error", verb)
		}
	}
}

// TestIntentRepliesHeadedByMarker: every reply opens with its notice marker,
// the handle the controller finds its own replies by, and mentions nobody:
// the actor is code.
func TestIntentRepliesHeadedByMarker(t *testing.T) {
	done, err := RenderCommandDoneNotice(CommandDoneNotice{
		Namespace: "patchy", Intent: "target-1", Key: "command-1", Actor: "peter", Verb: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := RenderIntentSummaryComment(IntentSummaryComment{Namespace: "patchy", Intent: "target-1"})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := RenderEditedCommandNotice(EditedCommandNotice{
		Namespace: "patchy", Intent: "target-1", Key: "comment-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for body, marker := range map[string]string{
		done:    NoticeMarker("patchy", "target-1", "command-1"),
		summary: NoticeMarker("patchy", "target-1", SummaryKey),
		edited:  NoticeMarker("patchy", "target-1", "comment-2"),
	} {
		if !strings.HasPrefix(body, marker+"\n") {
			t.Errorf("body does not open with %q:\n%s", marker, body)
		}
		if strings.Contains(body, "@peter") {
			t.Errorf("body mentions the actor:\n%s", body)
		}
	}
}
