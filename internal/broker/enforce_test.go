// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// countingUpstream is a fake upstream that counts what reached it and
// answers with body.
func countingUpstream(t *testing.T, hits *atomic.Int64, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// errorMessage reads the message out of an error envelope.
func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Type != "error" {
		t.Fatalf("body is not the error envelope: %s", body)
	}
	return e.Error.Message
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(TokenHeader, tok("good"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSurface: every route answers 404 for anything off its positive
// method+path list before touching the upstream — Files and Batches among
// them — and admits what the CLI actually sends.
func TestSurface(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{
		"anthropic": {Target: mustTarget(t, up.URL)},
		"bedrock":   {Target: mustTarget(t, up.URL), BufferBody: true},
		"vertex":    {Target: mustTarget(t, up.URL), Project: "p", Location: "us-east5"},
		"foundry":   {Target: mustTarget(t, up.URL)},
	}}, nil)
	h := s.Handler()

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/anthropic/v1/messages", http.StatusOK},
		{http.MethodPost, "/anthropic/v1/messages/count_tokens", http.StatusOK},
		{http.MethodGet, "/anthropic/v1/models", http.StatusOK},
		{http.MethodPost, "/anthropic/v1/files", http.StatusNotFound},
		{http.MethodGet, "/anthropic/v1/files", http.StatusNotFound},
		{http.MethodPost, "/anthropic/v1/messages/batches", http.StatusNotFound},
		{http.MethodGet, "/anthropic/v1/messages", http.StatusNotFound},
		{http.MethodDelete, "/anthropic/v1/models", http.StatusNotFound},
		{http.MethodPost, "/anthropic/v1/messages/", http.StatusNotFound},
		{http.MethodPost, "/bedrock/model/us.anthropic.claude-sonnet-5-v1:0/invoke", http.StatusOK},
		{http.MethodPost, "/bedrock/model/us.anthropic.claude-sonnet-5-v1:0/invoke-with-response-stream",
			http.StatusOK},
		{http.MethodPost, "/bedrock/model/us.anthropic.claude-sonnet-5-v1:0/converse", http.StatusNotFound},
		{http.MethodGet, "/bedrock/foundation-models", http.StatusNotFound},
		{http.MethodPost,
			"/vertex/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-sonnet-5:streamRawPredict",
			http.StatusOK},
		{http.MethodPost, "/vertex/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-sonnet-5:rawPredict",
			http.StatusOK},
		{http.MethodPost, "/vertex/v1/projects/p/locations/us-east5/publishers/anthropic/models/count-tokens:rawPredict",
			http.StatusOK},
		{http.MethodPost, "/vertex/v1/projects/p/locations/us-east5/publishers/google/models/gemini:streamRawPredict",
			http.StatusNotFound},
		{http.MethodPost,
			"/vertex/v1/projects/attacker/locations/us-east5/publishers/anthropic/models/claude-sonnet-5:rawPredict",
			http.StatusNotFound},
		{http.MethodPost,
			"/vertex/v1/projects/p/locations/us-central1/publishers/anthropic/models/claude-sonnet-5:rawPredict",
			http.StatusNotFound},
		{http.MethodPost, "/vertex/v1/projects/p/locations/us-east5/publishers/anthropic/models/claude-sonnet-5:predict",
			http.StatusNotFound},
		{http.MethodPost, "/foundry/v1/messages", http.StatusOK},
		{http.MethodPost, "/foundry/anthropic/v1/messages", http.StatusOK},
		{http.MethodPost, "/foundry/openai/v1/chat/completions", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			before := hits.Load()
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"model":"claude-sonnet-5"}`))
			req.Header.Set(TokenHeader, tok("good"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if reached := hits.Load() - before; (tt.want == http.StatusOK) != (reached == 1) {
				t.Fatalf("upstream hits = %d for status %d", reached, rec.Code)
			}
		})
	}
}

// TestBodyChecks: a body that would have the upstream reach the internet,
// an MCP server, a container or the Files API is refused before any
// upstream call.
func TestBodyChecks(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{
		"anthropic": {Target: mustTarget(t, up.URL)},
		"bedrock":   {Target: mustTarget(t, up.URL), BufferBody: true},
	}}, nil)
	h := s.Handler()

	tests := []struct {
		name, path, body string
		want             int
		wantMsg          string
	}{
		{"plain", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"name":"bash","input_schema":{"type":"object"}}]}`, http.StatusOK, ""},
		{"web_fetch", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","tools":[{"type":"web_fetch_20260209","name":"web_fetch"}]}`,
			http.StatusForbidden, "web_fetch_20260209"},
		{"web_search", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","tools":[{"name":"x"},{"type":"web_search_20250305","name":"web_search"}]}`,
			http.StatusForbidden, "web_search_20250305"},
		{"code_execution", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","tools":[{"type":"code_execution_20260521","name":"code_execution"}]}`,
			http.StatusForbidden, "code_execution_20260521"},
		{"mcp_toolset", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","tools":[{"type":"mcp_toolset","mcp_server_name":"x"}]}`,
			http.StatusForbidden, "mcp_toolset"},
		{"mcp_servers", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","mcp_servers":[{"type":"url","url":"https://x","name":"x"}]}`,
			http.StatusForbidden, "mcp_servers"},
		{"container", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","container":{"skills":[]}}`, http.StatusForbidden, "container"},
		{"file_id deep", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"document",` +
				`"source":{"type":"file","file_id":"file_123"}}]}]}`, http.StatusForbidden, "file references"},
		{"source type file without id", "/anthropic/v1/messages",
			`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"image",` +
				`"source":{"type":"file"}}]}]}`, http.StatusForbidden, "file references"},
		{"count_tokens checked too", "/anthropic/v1/messages/count_tokens",
			`{"model":"claude-sonnet-5","tools":[{"type":"web_fetch_20260209","name":"web_fetch"}]}`,
			http.StatusForbidden, "web_fetch_20260209"},
		{"bedrock body checked", "/bedrock/model/us.anthropic.claude-sonnet-5-v1:0/invoke",
			`{"anthropic_version":"bedrock-2023-05-31","tools":[{"type":"web_search_20250305","name":"w"}]}`,
			http.StatusForbidden, "web_search_20250305"},
		{"not json", "/anthropic/v1/messages", `not json`, http.StatusForbidden, "not a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := hits.Load()
			rec := post(h, tt.path, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusOK {
				if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, tt.wantMsg) {
					t.Errorf("message = %q, want it to name %q", msg, tt.wantMsg)
				}
				if hits.Load() != before {
					t.Error("rejected request reached the upstream")
				}
			}
		})
	}
}

// TestModelAllowlist: the model id is read from the path on bedrock and
// vertex and from the body on anthropic and foundry, and checked before any
// upstream call; dated variants and the helper family pass.
func TestModelAllowlist(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{
		Limits: Limits{ModelAllowlist: []string{"anthropic/claude-sonnet-5", "claude-opus-5", "my-deployment"}},
		Upstreams: map[string]Upstream{
			"anthropic": {Target: mustTarget(t, up.URL)},
			"bedrock":   {Target: mustTarget(t, up.URL), BufferBody: true},
			"vertex":    {Target: mustTarget(t, up.URL), Project: "p", Location: "us-east5"},
			"foundry":   {Target: mustTarget(t, up.URL)},
		}}, nil)
	h := s.Handler()
	const vertex = "/vertex/v1/projects/p/locations/us-east5/publishers/anthropic/models/"

	tests := []struct {
		name, path, body string
		want             int
	}{
		{"anthropic allowed", "/anthropic/v1/messages", `{"model":"claude-sonnet-5"}`, http.StatusOK},
		{"anthropic dated", "/anthropic/v1/messages", `{"model":"claude-opus-5-20260401"}`, http.StatusOK},
		{"anthropic helper", "/anthropic/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`, http.StatusOK},
		{"anthropic denied", "/anthropic/v1/messages", `{"model":"claude-fable-5-1"}`, http.StatusForbidden},
		{"anthropic sibling", "/anthropic/v1/messages", `{"model":"claude-opus-5-5"}`, http.StatusForbidden},
		{"anthropic missing", "/anthropic/v1/messages", `{}`, http.StatusForbidden},
		{"anthropic count_tokens denied", "/anthropic/v1/messages/count_tokens", `{"model":"claude-fable-5-1"}`,
			http.StatusForbidden},
		{"bedrock allowed", "/bedrock/model/us.anthropic.claude-sonnet-5-20260514-v1:0/invoke",
			`{"model":"claude-fable-5-1"}`, http.StatusOK},
		{"bedrock arn",
			"/bedrock/model/arn:aws:bedrock:us-east-1:1:inference-profile%2Fus.anthropic.claude-opus-5-v1:0/invoke",
			`{}`, http.StatusOK},
		{"bedrock global profile", "/bedrock/model/global.anthropic.claude-sonnet-5-v1:0/invoke", `{}`, http.StatusOK},
		{"bedrock us-gov profile", "/bedrock/model/us-gov.anthropic.claude-sonnet-5-v1:0/invoke", `{}`, http.StatusOK},
		{"bedrock foundation-model arn",
			"/bedrock/model/arn:aws:bedrock:us-east-1::foundation-model%2Fanthropic.claude-sonnet-5-v1:0/invoke",
			`{}`, http.StatusOK},
		{"bedrock us-gov arn",
			"/bedrock/model/arn:aws-us-gov:bedrock:us-gov-west-1:1:inference-profile%2Fus-gov.anthropic.claude-opus-5-v1:0" +
				"/invoke", `{}`, http.StatusOK},
		{"bedrock application profile arn",
			"/bedrock/model/arn:aws:bedrock:us-east-1:1:application-inference-profile%2Fclaude-sonnet-5/invoke",
			`{}`, http.StatusForbidden},
		{"bedrock provisioned arn",
			"/bedrock/model/arn:aws:bedrock:us-east-1:1:provisioned-model%2Fclaude-sonnet-5/invoke",
			`{}`, http.StatusForbidden},
		{"bedrock nested arn resource",
			"/bedrock/model/arn:aws:bedrock:us-east-1:1:inference-profile%2Fx%2Fclaude-sonnet-5/invoke",
			`{}`, http.StatusForbidden},
		{"bedrock non-bedrock arn",
			"/bedrock/model/arn:aws:s3:us-east-1:1:inference-profile%2Fclaude-sonnet-5/invoke",
			`{}`, http.StatusForbidden},
		{"bedrock denied", "/bedrock/model/us.anthropic.claude-fable-5-1-v1:0/invoke",
			`{"model":"claude-sonnet-5"}`, http.StatusForbidden},
		{"vertex allowed", vertex + "claude-sonnet-5@20260514:streamRawPredict", `{}`, http.StatusOK},
		{"vertex denied", vertex + "claude-fable-5-1:streamRawPredict", `{}`, http.StatusForbidden},
		{"vertex count-tokens", vertex + "count-tokens:rawPredict", `{}`, http.StatusOK},
		{"foundry deployment", "/foundry/anthropic/v1/messages", `{"model":"my-deployment"}`, http.StatusOK},
		{"foundry denied", "/foundry/anthropic/v1/messages", `{"model":"other-deployment"}`, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := hits.Load()
			rec := post(h, tt.path, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusForbidden {
				if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, "not allowlisted") {
					t.Errorf("message = %q", msg)
				}
				if hits.Load() != before {
					t.Error("rejected request reached the upstream")
				}
			}
		})
	}
	// The listing names no model and needs none.
	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	req.Header.Set(TokenHeader, tok("good"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("models listing: status = %d: %s", rec.Code, rec.Body.String())
	}
}

// sseUsage renders a minimal Messages event stream with the given usage.
func sseUsage(input, output int64, stop bool) string {
	var b strings.Builder
	b.WriteString(`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":` +
		itoa(input) + `,"cache_creation_input_tokens":10,"cache_read_input_tokens":20,"output_tokens":1}}}` + "\n\n")
	b.WriteString(`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n")
	b.WriteString(`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":` +
		itoa(output) + `}}` + "\n\n")
	if stop {
		b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
	return b.String()
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// syncBuffer is a bytes.Buffer safe to read while a handler goroutine's
// slog handler is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTokensPerPodTripsAfterMessageStart: the input tokens a streamed
// message_start reports count at once, so a pod over its budget after one
// response gets 429 on its next request — carrying the fixed prefix the
// in-pod runtime maps to budget_exceeded — before the upstream sees it.
func TestTokensPerPodTripsAfterMessageStart(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(5000, 7, true))
	s, buf := auditServer(t, Config{
		Limits:    Limits{TokensPerPod: 1000},
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	})
	h := s.Handler()

	rec := post(h, "/anthropic/v1/messages", `{"model":"claude-sonnet-5"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastAudit(t, buf)["pod_tokens"]; got != float64(5000+10+20+7) {
		t.Fatalf("audit pod_tokens = %v, want every usage field summed (5037)", got)
	}
	rec = post(h, "/anthropic/v1/messages", `{"model":"claude-sonnet-5"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec.Body.Bytes()); !strings.HasPrefix(msg, provider.LimitMessagePrefix) {
		t.Errorf("message = %q, want the %q prefix", msg, provider.LimitMessagePrefix)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (the second request never left the broker)", hits.Load())
	}
}

// TestCutStreamCharged: a stream that ends without message_stop, or a
// response that never reports usage, is charged its worst case — input at
// least the request-size estimate, output at least the output bound (here,
// with neither max_tokens nor a ceiling, a bytes/4 estimate of what
// streamed) — so aborting cannot dodge the counter. A response the upstream
// refused costs nothing.
func TestCutStreamCharged(t *testing.T) {
	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"` + strings.Repeat("x", 3900) + `"}]}`
	estimate := int64(len(body)) / 4
	cut, large := sseUsage(50, 3, false), sseUsage(50000, 3, false)
	streamed := func(s string) int64 { return int64(len(s)) / 4 }
	tests := []struct {
		name        string
		contentType string
		status      int
		upstream    string
		want        int64
	}{
		{"cut after message_start", "text/event-stream", http.StatusOK, cut, estimate + streamed(cut)},
		{"complete stream", "text/event-stream", http.StatusOK, sseUsage(50, 3, true), 50 + 10 + 20 + 3},
		{"json with usage", "application/json", http.StatusOK,
			`{"id":"m","usage":{"input_tokens":40,"output_tokens":2}}`, 42},
		{"json without usage", "application/json", http.StatusOK, `{"id":"m"}`, estimate + streamed(`{"id":"m"}`)},
		{"binary event stream", "application/vnd.amazon.eventstream", http.StatusOK, "\x00\x00\x01", estimate},
		{"large stream exceeds estimate", "text/event-stream", http.StatusOK, large, 50000 + 30 + streamed(large)},
		{"upstream 4xx", "application/json", http.StatusBadRequest, `{"type":"error"}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.upstream)
			}))
			defer up.Close()
			s, buf := auditServer(t, Config{
				Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
			})
			rec := post(s.Handler(), "/anthropic/v1/messages", body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if got := auditTokens(t, buf); got != tt.want {
				t.Errorf("charged %d, want %d", got, tt.want)
			}
		})
	}
}

// TestClientDisconnectCharged: the pod itself hanging up mid-stream (the
// proxy aborts the handler) still settles the worst case and the audit
// line.
func TestClientDisconnectCharged(t *testing.T) {
	upstreamDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `event: message_start`+"\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1}}}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer up.Close()

	var logBuf syncBuffer
	cfg := Config{
		AgentNamespace: testNamespace, AgentServiceAccount: testSA,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}
	s, err := New(fakeReviews(nil), cfg, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := `{"model":"claude-sonnet-5","max_tokens":500,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", 3900) + `"}]}`
	want := int64(len(body))/4 + 500
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TokenHeader, tok("good"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read the first event, then hang up.
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || !strings.Contains(line, "message_start") {
		t.Fatalf("first line = %q, %v", line, err)
	}
	cancel()
	_ = resp.Body.Close()
	<-upstreamDone

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logBuf.String(), `"estimated":true`) {
		if time.Now().After(deadline) {
			t.Fatalf("no audit line with the estimate after the disconnect: %s", logBuf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := auditTokens(t, &logBuf); got != want {
		t.Errorf("charged %d, want the estimate plus max_tokens %d (input already seen is topped up, not added)",
			got, want)
	}
}

// TestNoPodExtraRejectedWhenLimitsOn: with any per-pod limit configured, a
// token the API server reports without a bound pod is refused rather than
// counted in an anonymous bucket; without limits it is served as before.
func TestNoPodExtraRejectedWhenLimitsOn(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	for _, tt := range []struct {
		name   string
		limits Limits
		want   int
	}{
		{"no limits", Limits{}, http.StatusOK},
		{"requests per pod", Limits{RequestsPerPod: 10}, http.StatusUnauthorized},
		{"tokens per pod", Limits{TokensPerPod: 10}, http.StatusUnauthorized},
		{"concurrent per pod", Limits{ConcurrentPerPod: 10}, http.StatusUnauthorized},
		{"global only", Limits{TokensPerHour: 10}, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{
				Limits:    tt.limits,
				Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
			}, nil)
			req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
			req.Header.Set(TokenHeader, tok("nopod"))
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// TestMalformedTokenFloodNoReview: a flood of tokens that cannot be
// pod-bound projected tokens for this broker never reaches the API server.
func TestMalformedTokenFloodNoReview(t *testing.T) {
	var calls atomic.Int64
	s := newTestServer(t, Config{
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "http://127.0.0.1:1")}},
	}, &calls)
	h := s.Handler()
	junk := []string{
		"", "x", "a.b", "a.b.c", strings.Repeat("a", maxTokenBytes+1) + ".b.c",
		tokWith(map[string]any{"aud": "someone-else", "exp": tokenExp,
			"kubernetes.io": map[string]any{"pod": map[string]any{"name": "p"}}}),
		tokWith(map[string]any{"aud": DefaultAudience, "exp": 1,
			"kubernetes.io": map[string]any{"pod": map[string]any{"name": "p"}}}),
		tokWith(map[string]any{"aud": DefaultAudience, "exp": tokenExp}),
	}
	for i := range 200 {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
		req.Header.Set(TokenHeader, junk[i%len(junk)])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401", i, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("token reviews = %d, want 0 for a malformed-token flood", calls.Load())
	}
}

// TestPreauthSourceRate: the per-source-IP bucket refuses beyond its burst
// and a failed authentication costs an extra token.
func TestPreauthSourceRate(t *testing.T) {
	var calls atomic.Int64
	s := newTestServer(t, Config{
		PreauthRequestsPerSecond: 1, PreauthBurst: 3,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "http://127.0.0.1:1")}},
	}, &calls)
	frozen := time.Now()
	s.ips.now = func() time.Time { return frozen }
	h := s.Handler()

	send := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
		req.RemoteAddr = "10.0.0.7:4242"
		req.Header.Set(TokenHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// One bad token costs two of the three: the request plus the penalty.
	if got := send(tok("expired")); got != http.StatusUnauthorized {
		t.Fatalf("bad token: status = %d", got)
	}
	if got := send(tok("good")); got != http.StatusBadGateway {
		t.Fatalf("second request: status = %d, want 502 (admitted, upstream down)", got)
	}
	if got := send(tok("good")); got != http.StatusTooManyRequests {
		t.Fatalf("third request: status = %d, want 429 (bucket empty)", got)
	}
	// Another source is unaffected.
	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	req.RemoteAddr = "10.0.0.8:4242"
	req.Header.Set(TokenHeader, tok("good"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("other source: status = %d, want 502", rec.Code)
	}
}

// TestBetaDenylist: denied anthropic-beta entries are stripped and the rest
// forwarded; the operator can replace or disable the list.
func TestBetaDenylist(t *testing.T) {
	tests := []struct {
		name string
		deny []string
		sent []string
		want string
	}{
		{"default strips the server-side betas", nil,
			[]string{"mcp-client-2025-11-20, prompt-caching-2024-07-31", "context-1m-2025-08-07,files-api-2025-04-14"},
			"prompt-caching-2024-07-31"},
		{"default strips everything", nil, []string{"web-fetch-2025-09-10", "code-execution-2025-08-25"}, ""},
		{"operator list", []string{"fast-mode-*"}, []string{"fast-mode-2026-02-01,mcp-client-2025-11-20"},
			"mcp-client-2025-11-20"},
		{"disabled", []string{}, []string{"mcp-client-2025-11-20"}, "mcp-client-2025-11-20"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got http.Header
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}))
			defer up.Close()
			s := newTestServer(t, Config{
				BetaDenylist: tt.deny,
				Upstreams:    map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
			}, nil)
			req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
			req.Header.Set(TokenHeader, tok("good"))
			for _, v := range tt.sent {
				req.Header.Add("anthropic-beta", v)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if v := strings.Join(got.Values("anthropic-beta"), "|"); v != tt.want {
				t.Errorf("upstream anthropic-beta = %q, want %q", v, tt.want)
			}
		})
	}
}

// TestRequestLimits: the request count, in-flight cap and max_tokens
// ceiling each answer 429 with the fixed prefix before forwarding.
func TestRequestLimits(t *testing.T) {
	t.Run("requests per pod", func(t *testing.T) {
		var hits atomic.Int64
		up := countingUpstream(t, &hits, "application/json", `{}`)
		s := newTestServer(t, Config{
			Limits:    Limits{RequestsPerPod: 2},
			Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
		}, nil)
		h := s.Handler()
		for i := range 2 {
			if rec := post(h, "/anthropic/v1/messages", `{}`); rec.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d", i, rec.Code)
			}
		}
		rec := post(h, "/anthropic/v1/messages", `{}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("third request: status = %d, want 429", rec.Code)
		}
		if msg := errorMessage(t, rec.Body.Bytes()); !strings.HasPrefix(msg, provider.LimitMessagePrefix) {
			t.Errorf("message = %q", msg)
		}
		if hits.Load() != 2 {
			t.Errorf("upstream hits = %d, want 2", hits.Load())
		}
	})
	t.Run("max_tokens ceiling", func(t *testing.T) {
		var hits atomic.Int64
		up := countingUpstream(t, &hits, "application/json", `{}`)
		s := newTestServer(t, Config{
			Limits:    Limits{MaxTokensCeiling: 8000},
			Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
		}, nil)
		h := s.Handler()
		if rec := post(h, "/anthropic/v1/messages", `{"max_tokens":8000}`); rec.Code != http.StatusOK {
			t.Fatalf("at the ceiling: status = %d", rec.Code)
		}
		rec := post(h, "/anthropic/v1/messages", `{"max_tokens":8001}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("over the ceiling: status = %d, want 429: %s", rec.Code, rec.Body.String())
		}
		if msg := errorMessage(t, rec.Body.Bytes()); !strings.HasPrefix(msg, provider.LimitMessagePrefix) {
			t.Errorf("message = %q", msg)
		}
		if hits.Load() != 1 {
			t.Errorf("upstream hits = %d, want 1", hits.Load())
		}
	})
	t.Run("concurrent per pod", func(t *testing.T) {
		// started is buffered: only the first request is waited for, later
		// ones must not block the upstream on an unread signal.
		started, release := make(chan struct{}, 8), make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			w.WriteHeader(http.StatusOK)
		}))
		defer up.Close()
		s := newTestServer(t, Config{
			Limits:    Limits{ConcurrentPerPod: 1},
			Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
		}, nil)
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()

		first := make(chan int)
		go func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/anthropic/v1/messages",
				strings.NewReader(`{}`))
			req.Header.Set(TokenHeader, tok("good"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				first <- 0
				return
			}
			_ = resp.Body.Close()
			first <- resp.StatusCode
		}()
		<-started
		rec := post(s.Handler(), "/anthropic/v1/messages", `{}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("concurrent request: status = %d, want 429", rec.Code)
		}
		close(release)
		if got := <-first; got != http.StatusOK {
			t.Fatalf("first request: status = %d", got)
		}
		// The slot is released with the response.
		if rec := post(s.Handler(), "/anthropic/v1/messages", `{}`); rec.Code == http.StatusTooManyRequests {
			t.Fatal("slot not released after the first response")
		}
	})
}

// TestAuditTotals: the audit line carries the model, this request's charge
// and the pod's running totals.
func TestAuditTotals(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(100, 5, true))
	var buf bytes.Buffer
	cfg := Config{
		AgentNamespace: testNamespace, AgentServiceAccount: testSA,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}
	s, err := New(fakeReviews(nil), cfg, slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for range 2 {
		if rec := post(h, "/anthropic/v1/messages", `{"model":"claude-sonnet-5"}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": "claude-sonnet-5", "tokens": 135.0, "pod_requests": 2.0, "pod_tokens": 270.0,
		"estimated": false}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("audit %s = %v, want %v", k, last[k], v)
		}
	}
}
