// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// claudeStreamEdits is a run that reads, then edits on its third API
// message: msg_1 spans two events (thinking, then a Read call), msg_2 is a
// Bash call and msg_3 opens with text before its Edit block.
const claudeStreamEdits = `{"type":"system","subtype":"init","session_id":"` + claudeSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"look"}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1",` +
	`"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n" +
	`{"type":"user","message":{"content":[{"type":"tool_result","content":"package a"}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_2",` +
	`"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"sed -i s/a/b/ a.go"}}]}}` + "\n" +
	`{"type":"user","message":{"content":[{"type":"tool_result","content":""}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_3","content":[{"type":"text","text":"Fixing."}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_3",` +
	`"content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"a.go"}}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_4",` +
	`"content":[{"type":"tool_use","id":"t4","name":"Write","input":{"file_path":"b.go"}}]}}` + "\n" +
	`{"type":"result","subtype":"success","is_error":false,"result":"Done.","num_turns":4}`

func TestClaudeFirstEditTurn(t *testing.T) {
	edit := func(id, tool string) string {
		return `{"type":"assistant","message":{"id":"` + id + `",` +
			`"content":[{"type":"tool_use","id":"x","name":"` + tool + `","input":{}}]}}`
	}
	tests := []struct {
		name   string
		stdout string
		want   int
	}{
		{"turns counted by message id, a message's repeated events once", claudeStreamEdits, 3},
		{"Bash is not counted as an edit", claudeStreamSuccess, 0},
		{"Write", edit("m1", "Write"), 1},
		{"MultiEdit", edit("m1", "Read") + "\n" + edit("m2", "MultiEdit"), 2},
		{"NotebookEdit", edit("m1", "NotebookEdit"), 1},
		{"events without an id are each a turn",
			strings.ReplaceAll(edit("", "Read")+"\n"+edit("", "Read")+"\n"+edit("", "Edit"), `"id":"",`, ""), 3},
		{"a user event's tool name is not the agent's call",
			`{"type":"user","message":{"content":[{"type":"tool_use","name":"Edit"}]}}` + "\n" + edit("m1", "Read"), 0},
		{"a text block naming a tool is not a call",
			`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","name":"Edit"}]}}`, 0},
		{"an unparseable line is skipped", "not json\n" + edit("m1", "Edit"), 1},
		{"no output", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&Claude{}).FirstEditTurn([]byte(tt.stdout)); got != tt.want {
				t.Errorf("Claude.FirstEditTurn() = %d, want %d", got, tt.want)
			}
			if got := (&Fake{}).FirstEditTurn([]byte(tt.stdout)); got != tt.want {
				t.Errorf("Fake.FirstEditTurn() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFirstEditTurnProperty checks, over seeded random streams, that the
// first edit turn is the number of distinct messages up to and including the
// first one carrying an edit, whatever order and repetition the events of a
// message come in, and never more than the messages the stream holds.
func TestFirstEditTurnProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261008))
	tools := []string{"Read", "Bash", "Grep", "Edit", "Write", "MultiEdit", "NotebookEdit", "Glob"}
	for i := range 500 {
		var lines []string
		seen := map[int]bool{}
		want, distinct := 0, 0
		msg := 0
		for range rng.Intn(20) {
			if rng.Intn(3) > 0 || msg == 0 { // a new message, or another block of the current one
				msg++
			}
			tool := tools[rng.Intn(len(tools))]
			if !seen[msg] {
				seen[msg] = true
				distinct++
			}
			if want == 0 && claudeEditTools[tool] {
				want = distinct
			}
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%d",`+
				`"content":[{"type":"tool_use","name":%q,"input":{}}]}}`, msg, tool))
			if rng.Intn(2) == 0 {
				lines = append(lines, `{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`)
			}
		}
		stdout := strings.Join(lines, "\n")
		got := firstEditTurn([]byte(stdout))
		if got != want || got > distinct {
			t.Fatalf("case %d: firstEditTurn = %d, want %d (of %d messages)\n%s", i, got, want, distinct, stdout)
		}
	}
}
