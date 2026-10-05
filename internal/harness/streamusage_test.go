// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

// claudeStreamCutOff is a claude stream that stops before its result event,
// shaped as CLI 2.1 emits one: each content block of an API message is its
// own assistant event repeating that message's usage, whose output_tokens is
// the count at message start rather than the final one.
const claudeStreamCutOff = `{"type":"system","subtype":"init","session_id":"` + claudeSessionID + `"}` + "\n" +
	`{"type":"system","subtype":"thinking_tokens","estimated_tokens":50}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":11441,` +
	`"cache_read_input_tokens":13796,"output_tokens":1},"content":[{"type":"thinking","thinking":""}]}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":11441,` +
	`"cache_read_input_tokens":13796,"output_tokens":1},` +
	`"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.txt"}}]}}` + "\n" +
	`{"type":"user","message":{"content":[{"type":"tool_result","content":"hello"}]}}` + "\n" +
	`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_2","usage":{"input_tokens":8,"cache_creation_input_tokens":5364,` +
	`"cache_read_input_tokens":25237,"output_tokens":3},` +
	`"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"npm run test:ci"}}]}}` + "\n"

// usageOf flattens a Usage for comparison; nil fields read as -1 so an
// absent figure never compares equal to a reported zero.
func usageOf(u *Usage) [4]int {
	if u == nil {
		return [4]int{-1, -1, -1, -1}
	}
	or := func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	}
	return [4]int{or(u.InputTokens), or(u.CacheReadTokens), or(u.CacheCreationTokens), or(u.OutputTokens)}
}

func TestClaudeStreamUsage(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   [4]int // input, cache read, cache write, output; -1 for nil
	}{
		{
			name:   "cut off: each message once, its repeated events merged",
			stdout: claudeStreamCutOff,
			want:   [4]int{18, 39033, 16805, 4},
		},
		{
			name: "a later event of one message carrying more is the one counted",
			stdout: `{"type":"assistant","message":{"id":"m","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n" +
				`{"type":"assistant","message":{"id":"m","usage":{"input_tokens":5,"output_tokens":40}}}`,
			want: [4]int{5, 0, 0, 40},
		},
		{
			name: "events without an id each count",
			stdout: `{"type":"assistant","message":{"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n" +
				`{"type":"assistant","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
			want: [4]int{10, 0, 0, 2},
		},
		{
			// The result event's totals are ParseResult's; StreamUsage reads
			// the per-call events alone, so it never counts a run twice.
			name:   "a result event is not tallied",
			stdout: claudeStreamSuccess,
			want:   [4]int{0, 0, 0, 21},
		},
		{
			name:   "no assistant usage is nil, not a fabricated zero",
			stdout: claudeStreamExecError,
			want:   [4]int{-1, -1, -1, -1},
		},
		{
			name:   "plain text is nil",
			stdout: "Error: not logged in\n",
			want:   [4]int{-1, -1, -1, -1},
		},
	}
	for _, h := range []StreamUsageReporter{NewClaude(), NewFake()} {
		for _, tt := range tests {
			got := h.StreamUsage([]byte(tt.stdout))
			if usageOf(got) != tt.want {
				t.Errorf("%T: %s: StreamUsage = %v, want %v", h, tt.name, usageOf(got), tt.want)
			}
			if got != nil && got.CostUSD != nil {
				t.Errorf("%T: %s: CostUSD = %v, want nil: no stream reports a cost before its result", h, tt.name,
					*got.CostUSD)
			}
		}
	}
}

// TestClaudeStreamUsageProperty: however many events the CLI splits a message
// over, and however its messages interleave with other events, the tally is
// the sum over distinct messages.
func TestClaudeStreamUsageProperty(t *testing.T) {
	type message struct{ In, Read, Write, Out, Events uint16 }
	property := func(msgs []message, seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var lines []string
		var want [4]int
		for i, m := range msgs {
			for range m.Events%3 + 1 {
				lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%d","usage":`+
					`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,`+
					`"output_tokens":%d}}}`, i, m.In, m.Read, m.Write, m.Out))
			}
			lines = append(lines, `{"type":"user","message":{"content":[]}}`)
			want[0] += int(m.In)
			want[1] += int(m.Read)
			want[2] += int(m.Write)
			want[3] += int(m.Out)
		}
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		got := streamUsage([]byte(strings.Join(lines, "\n")))
		if len(msgs) == 0 {
			return got == nil
		}
		return usageOf(got) == want
	}
	cfg := &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261004))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}

func TestCopilotStreamUsage(t *testing.T) {
	c := NewCopilot()
	// The success stream cut before its result event: ParseResult has no
	// terminal event to report, but every call's usage already streamed.
	cut := copilotStreamSuccess[:strings.LastIndex(copilotStreamSuccess, "\n")]
	if _, ok := c.ParseResult([]byte(cut)); ok {
		t.Fatal("ParseResult(cut) ok = true, want no terminal result")
	}
	if got, want := usageOf(c.StreamUsage([]byte(cut))), [4]int{9000, 34000, 2000, 184}; got != want {
		t.Errorf("StreamUsage(cut) = %v, want %v", got, want)
	}
	if got := c.StreamUsage([]byte(copilotStreamTruncated)); got != nil {
		t.Errorf("StreamUsage(truncated) = %v, want nil: no call reported usage", usageOf(got))
	}
}

func TestCodexStreamUsage(t *testing.T) {
	c := NewCodex()
	if got, want := usageOf(c.StreamUsage([]byte(codexStreamSuccess))), [4]int{10067, 51328, -1, 184}; got != want {
		t.Errorf("StreamUsage(success) = %v, want %v", got, want)
	}
	// Codex reports usage only when a turn completes: a run cut off inside
	// its turn has nothing to report.
	if got := c.StreamUsage([]byte(codexStreamTruncated)); got != nil {
		t.Errorf("StreamUsage(truncated) = %v, want nil", usageOf(got))
	}
}

func TestHarnessesImplementStreamUsageReporter(t *testing.T) {
	for _, h := range All() {
		if _, ok := h.(StreamUsageReporter); !ok {
			t.Errorf("harness %q does not implement StreamUsageReporter", h.ID())
		}
	}
}
