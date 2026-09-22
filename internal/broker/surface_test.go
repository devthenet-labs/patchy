// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"net/http"
	"testing"
)

func TestSurfaceMatch(t *testing.T) {
	const vertex = "/v1/projects/proj/locations/us-east5/publishers/anthropic/models/"
	tests := []struct {
		route, method, path string
		ok                  bool
		id                  string
		metered             bool
		model               modelSource
	}{
		{"anthropic", http.MethodPost, "/v1/messages", true, "", true, modelBody},
		{"anthropic", http.MethodPost, "/v1/messages/count_tokens", true, "", false, modelBody},
		{"anthropic", http.MethodGet, "/v1/models", true, "", false, modelNone},
		{"anthropic", http.MethodGet, "/v1/models/claude-sonnet-5", false, "", false, modelNone},
		{"anthropic", http.MethodPost, "/v1/files", false, "", false, modelNone},
		{"anthropic", http.MethodPost, "/v1/messages/batches", false, "", false, modelNone},
		{"anthropic", http.MethodPost, "//v1/messages", false, "", false, modelNone},
		{"anthropic", http.MethodPost, "/v1/messages/", false, "", false, modelNone},
		{"bedrock", http.MethodPost, "/model/us.anthropic.claude-sonnet-5-v1:0/invoke", true,
			"us.anthropic.claude-sonnet-5-v1:0", true, modelPath},
		{"bedrock", http.MethodPost, "/model/us.anthropic.claude-sonnet-5-v1:0/invoke-with-response-stream", true,
			"us.anthropic.claude-sonnet-5-v1:0", true, modelPath},
		{"bedrock", http.MethodPost, "/model/arn:aws:bedrock:us-east-1:1:inference-profile%2Fus.anthropic.x/invoke",
			true, "arn:aws:bedrock:us-east-1:1:inference-profile/us.anthropic.x", true, modelPath},
		{"bedrock", http.MethodPost, "/model//invoke", false, "", false, modelNone},
		{"bedrock", http.MethodPost, "/model/x/converse", false, "", false, modelNone},
		{"bedrock", http.MethodGet, "/model/x/invoke", false, "", false, modelNone},
		{"vertex", http.MethodPost, vertex + "claude-sonnet-5:streamRawPredict", true, "claude-sonnet-5", true, modelPath},
		{"vertex", http.MethodPost, vertex + "claude-sonnet-5@20260514:rawPredict", true, "claude-sonnet-5@20260514",
			true, modelPath},
		{"vertex", http.MethodPost, vertex + "count-tokens:rawPredict", true, "", false, modelNone},
		{"vertex", http.MethodPost, vertex + ":streamRawPredict", false, "", false, modelNone},
		{"vertex", http.MethodPost, vertex + "claude-sonnet-5:predict", false, "", false, modelNone},
		{"vertex", http.MethodPost, "/v1/projects/proj/locations/us-east5/publishers/google/models/g:streamRawPredict",
			false, "", false, modelNone},
		{"foundry", http.MethodPost, "/v1/messages", true, "", true, modelBody},
		{"foundry", http.MethodPost, "/anthropic/v1/messages", true, "", true, modelBody},
		{"foundry", http.MethodGet, "/anthropic/v1/models", true, "", false, modelNone},
		{"foundry", http.MethodPost, "/openai/v1/chat/completions", false, "", false, modelNone},
	}
	for _, tt := range tests {
		t.Run(tt.route+" "+tt.method+" "+tt.path, func(t *testing.T) {
			ep, id, ok := surfaceFor(tt.route).match(tt.method, tt.path)
			if ok != tt.ok {
				t.Fatalf("matched = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if id != tt.id || ep.metered != tt.metered || ep.model != tt.model {
				t.Fatalf("id=%q metered=%v model=%v, want id=%q metered=%v model=%v",
					id, ep.metered, ep.model, tt.id, tt.metered, tt.model)
			}
		})
	}
	if surfaceFor("unknown") != nil {
		t.Error("an unknown route has a surface")
	}
}
