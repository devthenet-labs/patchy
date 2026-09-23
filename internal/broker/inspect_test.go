// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// quickConfig is the seeded, deterministic configuration every property in
// this package runs under.
func quickConfig() *quick.Config {
	return &quick.Config{Rand: rand.New(rand.NewSource(20260922)), MaxCount: 300}
}

func TestInspectBody(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantErr   string
		model     string
		maxTokens int64
	}{
		{"empty", "", "", "", 0},
		{"whitespace", "  \n", "", "", 0},
		{"plain", `{"model":"claude-sonnet-5","max_tokens":4096,"tools":[{"name":"bash"}]}`, "", "claude-sonnet-5", 4096},
		{"tools not an array", `{"tools":{"type":"web_fetch_20260209"}}`, "", "", 0},
		{"array body", `[1,2]`, "not a JSON object", "", 0},
		{"invalid", `{`, "not a JSON object", "", 0},
		{"web_fetch", `{"tools":[{"type":"web_fetch_20260209"}]}`, "web_fetch_20260209", "", 0},
		{"mcp_toolset", `{"tools":[{"type":"mcp_toolset"}]}`, "mcp_toolset", "", 0},
		{"mcp_servers null is absent", `{"mcp_servers":null}`, "", "", 0},
		{"mcp_servers", `{"mcp_servers":[]}`, "mcp_servers", "", 0},
		{"container", `{"container":"c"}`, "container", "", 0},
		{"file_id nested", `{"a":[{"b":{"c":[{"file_id":"f"}]}}]}`, "file references", "", 0},
		{"source file", `{"messages":[{"content":[{"source":{"type":"file"}}]}]}`, "file references", "", 0},
		{"source base64 fine", `{"messages":[{"content":[{"source":{"type":"base64","data":"x"}}]}]}`, "", "", 0},
		{"source url", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"u"}}]}]}`,
			`source type "url"`, "", 0},
		{"source without type", `{"messages":[{"content":[{"source":{"url":"u"}}]}]}`, "source type", "", 0},
		{"file_id in input_schema", `{"tools":[{"name":"t","input_schema":{"properties":{"file_id":{}}}}]}`, "", "", 0},
		{"file_id in tool_use input", `{"messages":[{"content":[{"type":"tool_use","input":{"file_id":"f"}}]}]}`,
			"", "", 0},
		{"file_id in server_tool_use input",
			`{"messages":[{"content":[{"type":"server_tool_use","input":{"file_id":"f"}}]}]}`, "", "", 0},
		{"input of a non-tool block is walked", `{"messages":[{"content":[{"type":"text","input":{"file_id":"f"}}]}]}`,
			"file references", "", 0},
		{"schema outside json_schema is walked", `{"x":{"type":"other","schema":{"file_id":"f"}}}`,
			"file references", "", 0},
		{"max_tokens float", `{"max_tokens":1.0}`, "max_tokens", "", 0},
		{"max_tokens exponent", `{"max_tokens":1e3}`, "max_tokens", "", 0},
		{"max_tokens string", `{"max_tokens":"10"}`, "max_tokens", "", 0},
		{"max_tokens negative", `{"max_tokens":-1}`, "max_tokens", "", 0},
		{"max_tokens overflow", `{"max_tokens":9223372036854775808}`, "max_tokens", "", 0},
		{"max_tokens zero", `{"max_tokens":0}`, "", "", 0},
		{"anthropic_beta not an array", `{"anthropic_beta":"x"}`, "anthropic_beta", "", 0},
		{"anthropic_beta non-string entry", `{"anthropic_beta":[1]}`, "anthropic_beta", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			insp, err := inspectBody([]byte(tt.body), DefaultBetaDenylist)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("accepted, want a rejection naming %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error = %q, want it to name %q", err, tt.wantErr)
			case err == nil && (insp.model != tt.model || insp.maxTokens != tt.maxTokens):
				t.Fatalf("model=%q max_tokens=%d, want %q/%d", insp.model, insp.maxTokens, tt.model, tt.maxTokens)
			}
			if insp.estimate != int64(len(tt.body))/4 {
				t.Errorf("estimate = %d, want %d", insp.estimate, len(tt.body)/4)
			}
		})
	}
}

// TestWalkForFilesProperty: a file reference is found at any depth and
// inside any mix of arrays and objects.
func TestWalkForFilesProperty(t *testing.T) {
	prop := func(depth uint8, shape []bool, useFileID bool) bool {
		var leaf any
		if useFileID {
			leaf = map[string]any{"file_id": "f"}
		} else {
			leaf = map[string]any{"source": map[string]any{"type": "file"}}
		}
		v := leaf
		for i := range int(depth % 40) {
			if i < len(shape) && shape[i] {
				v = []any{"pad", v}
			} else {
				v = map[string]any{"k": 1, "nested": v}
			}
		}
		raw, _ := json.Marshal(map[string]any{"model": "m", "messages": v})
		_, err := inspectBody(raw, DefaultBetaDenylist)
		return err != nil && strings.Contains(err.Error(), "file references")
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestModelPolicy(t *testing.T) {
	var none *modelPolicy
	if !none.allows("anything") {
		t.Fatal("nil policy must admit everything")
	}
	if newModelPolicy(nil) != nil || newModelPolicy([]string{" ", ""}) == nil {
		t.Fatal("empty list must yield a nil policy; blank entries must not")
	}
	p := newModelPolicy([]string{"anthropic/claude-sonnet-5", "Claude-Opus-5", "my-deployment",
		"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-fable-5-1-v1:0"})
	tests := []struct {
		wire string
		want bool
	}{
		{"claude-sonnet-5", true},
		{"Claude-Sonnet-5", true},
		{"claude-sonnet-5-20260514", true},
		{"claude-sonnet-5@20260514", true},
		{"us.anthropic.claude-sonnet-5-20260514-v1:0", true},
		{"eu.anthropic.claude-sonnet-5-v1:0", true},
		{"anthropic.claude-sonnet-5-v1:0", true},
		{"apac.anthropic.claude-opus-5-20260401-v1:0", true},
		{"arn:aws:bedrock:us-east-1:1:inference-profile/us.anthropic.claude-opus-5-v1:0", true},
		{"my-deployment", true},
		{"claude-haiku-4-5", true},
		{"claude-haiku-4-5-20251001", true},
		{"claude-haiku", true},
		{"", false},
		{"claude-fable-5-1", false},
		{"claude-opus-5-5", false},
		{"claude-sonnet-5-turbo", false},
		{"claude-sonnet-5-2026", false},
		{"claude-sonnet-5-20260514-extra", false},
		{"claude-sonnet-55", false},
		{"my-deployment-2", false},
		{"claude-haikus", false},
		{"other/claude-sonnet-5", true},
		{"global.anthropic.claude-sonnet-5-v1:0", true},
		{"us-gov.anthropic.claude-sonnet-5-v1:0", true},
		{"jp.anthropic.claude-sonnet-5-v1:0", true},
		{"zz.anthropic.claude-sonnet-5-v1:0", false},
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-sonnet-5-v1:0", true},
		{"arn:aws-us-gov:bedrock:us-gov-west-1:1:inference-profile/us-gov.anthropic.claude-opus-5-v1:0", true},
		{"ARN:AWS:BEDROCK:US-EAST-1:1:INFERENCE-PROFILE/US.ANTHROPIC.CLAUDE-OPUS-5-V1:0", true},
		{"arn:aws:bedrock:us-east-1:1:application-inference-profile/claude-sonnet-5", false},
		{"arn:aws:bedrock:us-east-1:1:provisioned-model/claude-sonnet-5", false},
		{"arn:aws:bedrock:us-east-1:1:custom-model/claude-sonnet-5", false},
		{"arn:aws:bedrock:us-east-1:1:inference-profile/x/claude-sonnet-5", false},
		{"arn:aws:bedrock:us-east-1:1:inference-profile/", false},
		{"arn:aws:sagemaker:us-east-1:1:inference-profile/claude-sonnet-5", false},
		{"arn:aws:bedrock:us-east-1", false},
		{"claude-fable-5-1-v1:0", true}, // admitted by the ARN entry
	}
	for _, tt := range tests {
		if got := p.allows(tt.wire); got != tt.want {
			t.Errorf("allows(%q) = %v, want %v", tt.wire, got, tt.want)
		}
	}
}

// TestModelPolicyProperty: for any entry, the entry with a dated snapshot
// suffix is admitted while any other "-<digits>" continuation is not, and
// a wire id never matches an entry it does not begin with.
func TestModelPolicyProperty(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"
	prop := func(seed uint32, date uint32, other uint32) bool {
		r := rand.New(rand.NewSource(int64(seed)))
		n := 3 + r.Intn(12)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		entry := "m" + string(b) // never the helper family
		p := newModelPolicy([]string{entry})
		dated := fmt.Sprintf("%s-%08d", entry, date%100_000_000)
		if !p.allows(dated) || !p.allows(entry) || !p.allows("us.anthropic."+dated+"-v1:0") {
			return false
		}
		digits := fmt.Sprint(other)
		if len(digits) == 8 {
			digits += "9"
		}
		if p.allows(entry + "-" + digits) {
			return false
		}
		return !p.allows("x"+entry) && !p.allows(entry[:len(entry)-1])
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestFilterBetas(t *testing.T) {
	tests := []struct {
		name string
		sent []string
		deny []string
		want []string
	}{
		{"absent", nil, DefaultBetaDenylist, nil},
		{"kept", []string{"prompt-caching-2024-07-31"}, DefaultBetaDenylist, []string{"prompt-caching-2024-07-31"}},
		{"split and joined", []string{"a, mcp-client-2025-11-20 ,b", "context-1m-2025-08-07"}, DefaultBetaDenylist,
			[]string{"a,b"}},
		{"case-insensitive", []string{"MCP-Client-2025-11-20,x"}, DefaultBetaDenylist, []string{"x"}},
		{"all stripped", []string{"web-fetch-2025-09-10,files-api-2025-04-14,code-execution-2025-08-25"},
			DefaultBetaDenylist, nil},
		{"empty entries dropped", []string{",,"}, DefaultBetaDenylist, nil},
		{"no deny", []string{"mcp-client-2025-11-20"}, nil, []string{"mcp-client-2025-11-20"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.sent {
				h.Add(betaHeader, v)
			}
			filterBetas(h, tt.deny)
			if got := h.Values(betaHeader); !slices.Equal(got, tt.want) {
				t.Errorf("anthropic-beta = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFilterBetasProperty: the surviving entries are exactly the sent
// entries that match no deny pattern, in order, and filtering is
// idempotent.
func TestFilterBetasProperty(t *testing.T) {
	// The oracle is hand-labelled against DefaultBetaDenylist, never
	// computed with the matcher under test.
	pool := []struct {
		entry  string
		denied bool
	}{
		{"mcp-client-2025-11-20", true}, {"web-fetch-2025-09-10", true}, {"code-execution-2025-08-25", true},
		{"files-api-2025-04-14", true}, {"context-1m-2025-08-07", true}, {"MCP-CLIENT-X", true},
		{"prompt-caching-2024-07-31", false}, {"fast-mode-2026-02-01", false}, {"compact-2026-01-12", false},
		{"mcp-client", false}, {"context-1m", false}, {"x-web-fetch-1", false},
	}
	prop := func(picks []uint8, splits []bool) bool {
		sent := make([]string, 0, len(picks))
		var expect []string
		for _, p := range picks {
			e := pool[int(p)%len(pool)]
			sent = append(sent, e.entry)
			if !e.denied {
				expect = append(expect, e.entry)
			}
		}
		// Spread the entries over one or more header values.
		h := http.Header{}
		cur := make([]string, 0, len(sent))
		for i, e := range sent {
			cur = append(cur, e)
			if i < len(splits) && splits[i] {
				h.Add(betaHeader, strings.Join(cur, ", "))
				cur = nil
			}
		}
		if len(cur) > 0 {
			h.Add(betaHeader, strings.Join(cur, ","))
		}
		filterBetas(h, DefaultBetaDenylist)
		var got []string
		if v := h.Get(betaHeader); v != "" {
			got = strings.Split(v, ",")
		}
		if !slices.Equal(got, expect) {
			return false
		}
		before := h.Values(betaHeader)
		filterBetas(h, DefaultBetaDenylist)
		return slices.Equal(before, h.Values(betaHeader))
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

// TestInspectBodyStripsBodyBetas: denied anthropic_beta entries are dropped
// from the body and the field removed when nothing survives; a body with
// nothing denied is not rewritten.
func TestInspectBodyStripsBodyBetas(t *testing.T) {
	tests := []struct {
		name, body string
		want       string // the rewritten body, "" when forwarded unchanged
	}{
		{"absent", `{"model":"m"}`, ""},
		{"nothing denied", `{"anthropic_beta":["prompt-caching-2024-07-31"]}`, ""},
		{"some denied", `{"anthropic_beta":["mcp-client-2025-11-20","a<b"],"max_tokens":5}`,
			`{"anthropic_beta":["a<b"],"max_tokens":5}`},
		{"all denied", `{"anthropic_beta":["context-1m-2025-08-07"],"x":1.50}`, `{"x":1.50}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			insp, err := inspectBody([]byte(tt.body), DefaultBetaDenylist)
			if err != nil {
				t.Fatal(err)
			}
			if string(insp.rewritten) != tt.want {
				t.Fatalf("rewritten = %s, want %s", insp.rewritten, tt.want)
			}
		})
	}
}
