// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// auditServer builds a Server whose audit lines land in the returned buffer.
func auditServer(t *testing.T, cfg Config) (*Server, *syncBuffer) {
	t.Helper()
	cfg.AgentNamespace, cfg.AgentServiceAccount = testNamespace, testSA
	var buf syncBuffer
	s, err := New(fakeReviews(nil), cfg, slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, &buf
}

// lastAudit decodes the most recent audit line.
func lastAudit(t *testing.T, buf *syncBuffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err != nil {
		t.Fatalf("audit line is not JSON: %v: %s", err, buf.String())
	}
	return line
}

// auditTokens is what the most recent audit line says the request charged.
func auditTokens(t *testing.T, buf *syncBuffer) int64 {
	t.Helper()
	v, ok := lastAudit(t, buf)["tokens"].(float64)
	if !ok {
		t.Fatalf("audit line has no tokens: %s", buf.String())
	}
	return int64(v)
}

// TestURLSourceRefused: an image or document block whose source is a URL
// would have the upstream fetch it on the pod's behalf, so only inline
// source types are admitted — wherever the block sits.
func TestURLSourceRefused(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}}}, nil)
	h := s.Handler()
	block := func(b string) string {
		return `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":[` + b + `]}]}`
	}
	tests := []struct {
		name, body string
		want       int
	}{
		{"image url", block(`{"type":"image","source":{"type":"url","url":"https://attacker.example/a.png"}}`),
			http.StatusForbidden},
		{"document url", block(`{"type":"document","source":{"type":"url","url":"https://attacker.example/a.pdf"}}`),
			http.StatusForbidden},
		{"unknown source type", block(`{"type":"image","source":{"type":"s3","uri":"s3://b/k"}}`),
			http.StatusForbidden},
		{"url inside tool_result", block(`{"type":"tool_result","tool_use_id":"t","content":[` +
			`{"type":"image","source":{"type":"url","url":"https://attacker.example/a.png"}}]}`), http.StatusForbidden},
		{"url inside a content document", block(`{"type":"document","source":{"type":"content","content":[` +
			`{"type":"image","source":{"type":"url","url":"https://attacker.example/a.png"}}]}}`), http.StatusForbidden},
		{"base64", block(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`),
			http.StatusOK},
		{"text", block(`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}`),
			http.StatusOK},
		{"content", block(`{"type":"document","source":{"type":"content","content":[{"type":"text","text":"x"}]}}`),
			http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := hits.Load()
			rec := post(h, "/anthropic/v1/messages", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want != http.StatusOK && hits.Load() != before {
				t.Error("rejected request reached the upstream")
			}
		})
	}
}

// TestFileIDScopedToReferences: a file_id key inside caller-defined data —
// a tool's JSON Schema, a tool_use block's input, a structured-output
// schema — is not a Files API reference and passes.
func TestFileIDScopedToReferences(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}}}, nil)
	h := s.Handler()
	tests := []struct {
		name, body string
		want       int
	}{
		{"input_schema property", `{"model":"claude-sonnet-5","max_tokens":10,"tools":[{"name":"upload",` +
			`"input_schema":{"type":"object","properties":{"file_id":{"type":"string"},` +
			`"source":{"type":"object"}}}}]}`, http.StatusOK},
		{"tool_use input", `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"assistant","content":[` +
			`{"type":"tool_use","id":"t","name":"upload","input":{"file_id":"f","source":{"type":"url"}}}]}]}`,
			http.StatusOK},
		{"output schema", `{"model":"claude-sonnet-5","max_tokens":10,"output_format":{"type":"json_schema",` +
			`"schema":{"type":"object","properties":{"file_id":{"type":"string"}}}}}`, http.StatusOK},
		{"container_upload block", `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user",` +
			`"content":[{"type":"container_upload","file_id":"file_1"}]}]}`, http.StatusForbidden},
		{"file source", `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":[` +
			`{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := post(h, "/anthropic/v1/messages", tt.body); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

// TestMaxTokensMustBeInteger: a max_tokens the ceiling cannot read — a
// float, an exponent, a string, a negative — is refused rather than
// recorded as zero and waved past the ceiling.
func TestMaxTokensMustBeInteger(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{
		Limits:    Limits{MaxTokensCeiling: 8000},
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}, nil)
	h := s.Handler()
	for _, v := range []string{`8001.0`, `9e3`, `9E+3`, `"9000"`, `-1`, `null`, `true`, `1.5`, `99999999999999999999`} {
		t.Run(v, func(t *testing.T) {
			before := hits.Load()
			rec := post(h, "/anthropic/v1/messages", `{"model":"claude-sonnet-5","max_tokens":`+v+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if hits.Load() != before {
				t.Error("rejected request reached the upstream")
			}
		})
	}
	for _, v := range []string{`0`, `8000`} {
		rec := post(h, "/anthropic/v1/messages", `{"model":"claude-sonnet-5","max_tokens":`+v+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("max_tokens %s: status = %d: %s", v, rec.Code, rec.Body.String())
		}
	}
}

// TestBodyBetasFiltered: bedrock and vertex carry betas in the body's
// anthropic_beta array; the deny-list applies there exactly as it does to
// the header, and a body with nothing denied is forwarded byte for byte.
func TestBodyBetasFiltered(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{
		"bedrock": {Target: mustTarget(t, up.URL), BufferBody: true},
	}}, nil)
	h := s.Handler()
	const path = "/bedrock/model/us.anthropic.claude-sonnet-5-v1:0/invoke"

	rec := post(h, path, `{"anthropic_version":"bedrock-2023-05-31","max_tokens":10,`+
		`"anthropic_beta":["mcp-client-2025-11-20","prompt-caching-2024-07-31","Files-API-2025-04-14"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Beta []string `json:"anthropic_beta"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("forwarded body: %v: %s", err, got)
	}
	if len(body.Beta) != 1 || body.Beta[0] != "prompt-caching-2024-07-31" {
		t.Fatalf("forwarded anthropic_beta = %q, want only prompt-caching", body.Beta)
	}

	clean := `{"anthropic_version":"bedrock-2023-05-31", "max_tokens":10,"anthropic_beta":["prompt-caching-2024-07-31"]}`
	if rec := post(h, path, clean); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if string(got) != clean {
		t.Fatalf("an unfiltered body was rewritten: %s", got)
	}
	if rec := post(h, path, `{"anthropic_beta":"mcp-client-2025-11-20"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-array anthropic_beta: status = %d, want 400", rec.Code)
	}
}

// TestCutStreamChargesOutputBound: a metered 2xx that ends before usage is
// final charges output as the request's max_tokens, else the configured
// ceiling, else a bytes/4 estimate of what was streamed — so a pod cannot
// have the model generate and then cut before message_delta for free.
func TestCutStreamChargesOutputBound(t *testing.T) {
	stream := sseUsage(50, 3, false) // input 50 + cache 30, output 1 at message_start
	tests := []struct {
		name    string
		body    string
		ceiling int64
		want    func(est int64) int64
	}{
		{"max_tokens", `{"model":"claude-sonnet-5","max_tokens":4000}`, 0,
			func(est int64) int64 { return max(80, est) + 4000 }},
		{"ceiling when max_tokens absent", `{"model":"claude-sonnet-5"}`, 6000,
			func(est int64) int64 { return max(80, est) + 6000 }},
		{"streamed bytes otherwise", `{"model":"claude-sonnet-5"}`, 0,
			func(est int64) int64 { return max(80, est) + max(3, int64(len(stream))/4) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int64
			up := countingUpstream(t, &hits, "text/event-stream", stream)
			s, buf := auditServer(t, Config{
				Limits:    Limits{MaxTokensCeiling: tt.ceiling},
				Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
			})
			if rec := post(s.Handler(), "/anthropic/v1/messages", tt.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if got, want := auditTokens(t, buf), tt.want(int64(len(tt.body))/4); got != want {
				t.Fatalf("charged %d, want %d", got, want)
			}
		})
	}
}

// TestParallelRequestsReserveBudget: admission reserves the request's
// worst case against the pod's budget, so parallel requests cannot all pass
// the check before any usage lands.
func TestParallelRequestsReserveBudget(t *testing.T) {
	started, release := make(chan struct{}, 8), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":5,"output_tokens":5}}`)
	}))
	defer up.Close()
	s := newTestServer(t, Config{
		Limits:    Limits{TokensPerPod: 1000},
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}, nil)
	h := s.Handler()
	body := `{"model":"claude-sonnet-5","max_tokens":900}`

	first := make(chan int)
	go func() { first <- post(h, "/anthropic/v1/messages", body).Code }()
	<-started
	// Pre-fix the second request is admitted and parks at the upstream too,
	// so it runs aside and the upstream is released either way.
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- post(h, "/anthropic/v1/messages", body) }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-second:
		close(release)
	case <-time.After(2 * time.Second):
		close(release)
		rec = <-second
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("parallel request: status = %d, want 429 (the first holds a 900-token reservation)", rec.Code)
	}
	if got := <-first; got != http.StatusOK {
		t.Fatalf("first request: status = %d", got)
	}
	// Settled at the actual usage (10), the reservation is returned.
	if rec := post(h, "/anthropic/v1/messages", body); rec.Code != http.StatusOK {
		t.Fatalf("after settlement: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestEncodedResponseUsageSeen: a caller asking for a compressed response
// must not blind the usage scanner; the broker lets the transport negotiate
// and decode, so usage is read and the pod receives plain bytes.
func TestEncodedResponseUsageSeen(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, sseUsage(50, 3, true))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, sseUsage(50, 3, true))
		_ = gz.Close()
	}))
	defer up.Close()
	s, buf := auditServer(t, Config{Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}}})
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":4000}`))
	req.Header.Set(TokenHeader, tok("good"))
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := auditTokens(t, buf); got != 50+10+20+3 {
		t.Fatalf("charged %d, want the reported usage (83)", got)
	}
	if lastAudit(t, buf)["estimated"] != false {
		t.Fatal("usage of an encoded response fell back to the estimate")
	}
}

// TestMalformedTokensSpendSourceBucket: junk tokens are refused without a
// TokenReview but still drain the source IP's bucket, so a junk flood is
// bounded like any other.
func TestMalformedTokensSpendSourceBucket(t *testing.T) {
	var calls atomic.Int64
	s := newTestServer(t, Config{
		PreauthRequestsPerSecond: 1, PreauthBurst: 4,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "http://127.0.0.1:1")}},
	}, &calls)
	frozen := time.Now()
	s.ips.now = func() time.Time { return frozen }
	h := s.Handler()
	send := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
		req.RemoteAddr = "10.0.0.9:4242"
		req.Header.Set(TokenHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := range 10 {
		if got := send("junk"); got != http.StatusUnauthorized && got != http.StatusTooManyRequests {
			t.Fatalf("junk %d: status = %d", i, got)
		}
	}
	if got := send(tok("good")); got != http.StatusTooManyRequests {
		t.Fatalf("after a junk flood: status = %d, want 429 (the source's bucket is spent)", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("token reviews = %d, want 0", calls.Load())
	}
}

// TestThrottledReviewIs429NotPenalized: a TokenReview the broker-wide
// limiter will not admit is the broker's problem, not the caller's: 429
// with Retry-After, no penalty on the source, and no cached verdict.
func TestThrottledReviewIs429NotPenalized(t *testing.T) {
	var calls atomic.Int64
	s := newTestServer(t, Config{
		TokenReviewsPerSecond:    0.01, // one review, then nothing for 100s
		PreauthRequestsPerSecond: 1, PreauthBurst: 3,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "http://127.0.0.1:1")}},
	}, &calls)
	frozen := time.Now()
	s.ips.now = func() time.Time { return frozen }
	s.auth.now = func() time.Time { return frozen }
	h := s.Handler()
	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
		req.RemoteAddr = "10.0.0.10:4242"
		req.Header.Set(TokenHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	second := tokWith(map[string]any{"sub": "good", "aud": DefaultAudience, "exp": tokenExp - 1,
		"kubernetes.io": map[string]any{"pod": map[string]any{"name": "agent-pod-1"}}})
	if rec := send(tok("good")); rec.Code != http.StatusBadGateway {
		t.Fatalf("first: status = %d, want 502 (admitted, upstream down)", rec.Code)
	}
	rec := send(second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled review: status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("throttled review answered without Retry-After")
	}
	// Two requests spent two of three tokens; a penalty would have spent
	// the third.
	s.auth.reviews = nil
	if rec := send(second); rec.Code != http.StatusBadGateway {
		t.Fatalf("after the throttle lifts: status = %d, want 502 (not penalized, verdict not cached): %s",
			rec.Code, rec.Body.String())
	}
}

// TestUnavailableReviewIs503: an unreachable API server is answered 503 and
// never charged to the source.
func TestUnavailableReviewIs503(t *testing.T) {
	cs := fakeReviews(nil)
	cs.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	s, err := New(cs, Config{
		AgentNamespace: testNamespace, AgentServiceAccount: testSA,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, "http://127.0.0.1:1")}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := post(s.Handler(), "/anthropic/v1/messages", `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestAttackerStringsBounded: the model id and path a caller names reach the
// 403 message and the audit line truncated.
func TestAttackerStringsBounded(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s, buf := auditServer(t, Config{
		Limits:    Limits{ModelAllowlist: []string{"claude-sonnet-5"}},
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	})
	h := s.Handler()
	long := strings.Repeat("m", 100_000)
	rec := post(h, "/anthropic/v1/messages", `{"model":"`+long+`","max_tokens":1}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if msg := errorMessage(t, rec.Body.Bytes()); len(msg) > 512 {
		t.Errorf("403 message is %d bytes", len(msg))
	}
	if m, _ := lastAudit(t, buf)["model"].(string); len(m) > 512 {
		t.Errorf("audit model is %d bytes", len(m))
	}
	rec = post(h, "/anthropic/v1/"+strings.Repeat("p", 100_000), `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if p, _ := lastAudit(t, buf)["path"].(string); len(p) > 512 {
		t.Errorf("audit path is %d bytes", len(p))
	}
}

// TestPreauthConcurrencyCap: the per-source-IP in-flight cap refuses a
// source's extra request while its bucket still has tokens — the refusal
// is the cap, not the rate — and leaves other sources alone.
func TestPreauthConcurrencyCap(t *testing.T) {
	started, release := make(chan struct{}, 8), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s := newTestServer(t, Config{
		PreauthRequestsPerSecond: 1000, PreauthBurst: 1,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}, nil)
	h := s.Handler()
	send := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
		req.RemoteAddr = ip + ":4242"
		req.Header.Set(TokenHeader, tok("good"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	go send("10.0.0.20")
	<-started
	time.Sleep(20 * time.Millisecond) // the bucket refills; only the cap can refuse now
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- send("10.0.0.20") }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-second:
	case <-started:
		t.Fatal("second in-flight request from the same source reached the upstream")
	case <-time.After(5 * time.Second):
		t.Fatal("second in-flight request neither refused nor forwarded")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second in-flight request: status = %d, want 429", rec.Code)
	}
	if msg := errorMessage(t, rec.Body.Bytes()); !strings.Contains(msg, "concurrent") {
		t.Fatalf("message = %q, want the concurrency cap", msg)
	}
	other := make(chan int, 1)
	go func() { other <- send("10.0.0.21").Code }()
	<-started // the other source reached the upstream
	unblock()
	if got := <-other; got != http.StatusOK {
		t.Fatalf("other source: status = %d", got)
	}
}

// TestMaxAnthropicRequestBytes: a non-signing route buffers under its own
// cap and refuses a larger body before any upstream contact.
func TestMaxAnthropicRequestBytes(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "application/json", `{}`)
	s := newTestServer(t, Config{
		MaxAnthropicRequestBytes: 16,
		Upstreams:                map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}},
	}, nil)
	rec := post(s.Handler(), "/anthropic/v1/messages", `{"model":"`+strings.Repeat("x", 52)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hits = %d, want 0", hits.Load())
	}
	if rec := post(s.Handler(), "/anthropic/v1/messages", `{"max_tokens":1}`); rec.Code != http.StatusOK {
		t.Fatalf("a body within the cap: status = %d", rec.Code)
	}
}
