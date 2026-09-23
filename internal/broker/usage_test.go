// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"sort"
	"strings"
	"testing"
	"testing/quick"
)

const sampleStream = "event: message_start\r\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":10,` +
	`"cache_read_input_tokens":20,"output_tokens":1}}}` + "\r\n\r\n" +
	": ping\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"data: {\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}," +
	"\"usage\":{\"output_tokens\":40}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":45,\"input_tokens\":100}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// sampleTotal is what sampleStream charges: every field at its final
// cumulative value, output 45 not 1+40+45.
const sampleTotal = 100 + 10 + 20 + 45

func scanAll(contentType string, chunks ...string) (usage, bool) {
	s := &usageScanner{}
	s.start(contentType)
	var total usage
	for _, c := range chunks {
		total = total.mergeAdd(s.write([]byte(c)))
	}
	total = total.mergeAdd(s.finish())
	return total, s.complete
}

func TestUsageScanner(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		chunks      []string
		want        int64
		complete    bool
	}{
		{"sse whole", "text/event-stream", []string{sampleStream}, sampleTotal, true},
		{"sse charset", "text/event-stream; charset=utf-8", []string{sampleStream}, sampleTotal, true},
		{"sse cut before stop", "text/event-stream",
			[]string{sampleStream[:strings.Index(sampleStream, "event: message_stop")]}, sampleTotal, false},
		{"sse cut after start", "text/event-stream",
			[]string{sampleStream[:strings.Index(sampleStream, ": ping")]}, 131, false},
		{"sse no usage", "text/event-stream", []string{"event: ping\ndata: {}\n\n"}, 0, false},
		{"sse over-long line skipped", "text/event-stream",
			[]string{"data: {\"type\":\"message_start\",\"pad\":\"" + strings.Repeat("x", maxSSELine) +
				"\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" + sampleStream}, sampleTotal, true},
		{"json usage", "application/json", []string{`{"id":"m","usage":{"input_tokens":7,"output_tokens":3}}`}, 10, true},
		{"json split", "application/json", []string{`{"id":"m","usa`, `ge":{"input_tokens":7,"output_tokens":3}}`},
			10, true},
		{"json no usage", "application/json", []string{`{"id":"m"}`}, 0, false},
		{"json too large", "application/json",
			[]string{`{"pad":"` + strings.Repeat("x", maxJSONBody) + `","usage":{"input_tokens":7}}`}, 0, false},
		{"opaque", "application/vnd.amazon.eventstream", []string{"\x00\x01"}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, complete := scanAll(tt.contentType, tt.chunks...)
			if got.total() != tt.want || complete != tt.complete {
				t.Fatalf("total = %d complete = %v, want %d/%v", got.total(), complete, tt.want, tt.complete)
			}
		})
	}
}

// TestUsageScannerChunkingProperty: however the stream is split into
// writes, the charge and completion are those of the whole stream.
func TestUsageScannerChunkingProperty(t *testing.T) {
	prop := func(cuts []uint16) bool {
		points := make([]int, 0, len(cuts))
		for _, c := range cuts {
			points = append(points, int(c)%len(sampleStream))
		}
		sort.Ints(points)
		chunks := make([]string, 0, len(points)+1)
		prev := 0
		for _, p := range points {
			chunks = append(chunks, sampleStream[prev:p])
			prev = p
		}
		chunks = append(chunks, sampleStream[prev:])
		got, complete := scanAll("text/event-stream", chunks...)
		return complete && got.total() == sampleTotal
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}
