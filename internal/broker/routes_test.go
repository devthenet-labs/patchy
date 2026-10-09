// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestFoundryKey(t *testing.T) {
	target := mustTarget(t, "https://foundry.example.com")
	up := FoundryKey(target, keyFile(t, "fk-123"))
	if up.Target != target {
		t.Errorf("target = %v", up.Target)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := up.Credential(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-api-key"); got != "fk-123" {
		t.Errorf("x-api-key = %q, want the trimmed key", got)
	}
	if err := up.Ready(context.Background()); err != nil {
		t.Errorf("Ready = %v", err)
	}

	empty := keyFile(t, "  ")
	missing := filepath.Join(t.TempDir(), "absent")
	for name, path := range map[string]string{"empty": empty, "missing": missing} {
		up := FoundryKey(target, path)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if err := up.Credential(context.Background(), req); err == nil {
			t.Errorf("%s key file: Credential succeeded", name)
		}
		if req.Header.Get("x-api-key") != "" {
			t.Errorf("%s key file: a key header was set", name)
		}
		if err := up.Ready(context.Background()); err == nil {
			t.Errorf("%s key file: Ready succeeded", name)
		}
	}
}

func TestAnthropicKeyFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	for _, bearer := range []bool{false, true} {
		up := Anthropic(mustTarget(t, "https://api.anthropic.com"), missing, bearer)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		err := up.Credential(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "read key file") {
			t.Errorf("bearer=%v: Credential = %v, want a read error", bearer, err)
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("x-api-key") != "" {
			t.Errorf("bearer=%v: credential header set despite the error", bearer)
		}
	}
}

func TestParseTargetShapes(t *testing.T) {
	cases := map[string]bool{
		"https://api.anthropic.com": true,
		"http://10.0.0.1:8080/base": true,
		"ftp://example.com":         false,
		"https://":                  false,
		"/relative":                 false,
		"%zz":                       false,
	}
	for raw, ok := range cases {
		u, err := ParseTarget(raw)
		if (err == nil) != ok {
			t.Errorf("ParseTarget(%q) = %v, %v; want ok %v", raw, u, err, ok)
		}
	}
}

// fakeTokenCredential hands out scripted Entra tokens and counts calls.
type fakeTokenCredential struct {
	mu     sync.Mutex
	calls  int
	scopes []string
	tokens []azcore.AccessToken
	err    error
}

func (f *fakeTokenCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.scopes = opts.Scopes
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	tok := f.tokens[0]
	if len(f.tokens) > 1 {
		f.tokens = f.tokens[1:]
	}
	return tok, nil
}

// TestEntraTokenSourceCachesAndRefreshes: a token well within its lifetime is
// reused; one inside the refresh margin is fetched again; the scope is the
// Cognitive Services one; a fetch error is returned and caches nothing.
func TestEntraTokenSourceCachesAndRefreshes(t *testing.T) {
	ctx := context.Background()
	fresh := &fakeTokenCredential{tokens: []azcore.AccessToken{
		{Token: "t1", ExpiresOn: time.Now().Add(time.Hour)},
		{Token: "t2", ExpiresOn: time.Now().Add(time.Hour)},
	}}
	ts := &entraTokenSource{cred: fresh}
	for range 3 {
		got, err := ts.token(ctx)
		if err != nil || got != "t1" {
			t.Fatalf("token = %q, %v; want cached t1", got, err)
		}
	}
	if fresh.calls != 1 {
		t.Errorf("GetToken calls = %d, want 1 (cached)", fresh.calls)
	}
	if len(fresh.scopes) != 1 || fresh.scopes[0] != foundryScope {
		t.Errorf("scopes = %v, want [%s]", fresh.scopes, foundryScope)
	}

	nearExpiry := &fakeTokenCredential{tokens: []azcore.AccessToken{
		{Token: "old", ExpiresOn: time.Now().Add(refreshMargin / 2)},
		{Token: "new", ExpiresOn: time.Now().Add(time.Hour)},
	}}
	ts = &entraTokenSource{cred: nearExpiry}
	if got, _ := ts.token(ctx); got != "old" {
		t.Fatalf("first token = %q", got)
	}
	if got, _ := ts.token(ctx); got != "new" {
		t.Errorf("token inside the refresh margin = %q, want a refreshed one", got)
	}

	failing := &fakeTokenCredential{err: errors.New("no identity")}
	ts = &entraTokenSource{cred: failing}
	if _, err := ts.token(ctx); err == nil || ts.cached != "" {
		t.Errorf("token with a failing credential = %v (cached %q)", err, ts.cached)
	}
}

func TestFoundryEntraBuildsRoute(t *testing.T) {
	target := mustTarget(t, "https://foundry.example.com")
	up, err := FoundryEntra(target)
	if err != nil {
		t.Fatalf("FoundryEntra = %v", err)
	}
	if up.Target != target || up.Credential == nil || up.Ready == nil || up.BufferBody {
		t.Errorf("route = %+v, want a credentialed, unbuffered route to the target", up)
	}
}

// isolateAWS points the AWS SDK at empty shared files and turns off the
// instance metadata service, so only the environment can supply credentials.
func isolateAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
}

func TestBedrockReadyAndMissingCredentials(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	up, err := Bedrock(context.Background(), mustTarget(t, "https://bedrock.example.com"), "eu-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if !up.BufferBody {
		t.Error("bedrock route must buffer bodies to sign them")
	}
	if err := up.Ready(context.Background()); err != nil {
		t.Errorf("Ready with env credentials = %v", err)
	}

	isolateAWS(t)
	up, err = Bedrock(context.Background(), mustTarget(t, "https://bedrock.example.com"), "eu-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Ready(context.Background()); err == nil {
		t.Error("Ready with no credentials succeeded")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock.example.com/model/x/invoke", strings.NewReader("{}"))
	err = up.Credential(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "resolve AWS credentials") {
		t.Errorf("Credential with no credentials = %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("unsigned request carries an Authorization header")
	}
}

func TestPayloadHash(t *testing.T) {
	bodyless, _ := http.NewRequest(http.MethodGet, "https://x", nil)
	if got, err := payloadHash(bodyless); err != nil || got != emptyPayloadHash {
		t.Errorf("bodyless hash = %q, %v", got, err)
	}
	sum := sha256.Sum256([]byte(""))
	if emptyPayloadHash != hex.EncodeToString(sum[:]) {
		t.Fatal("emptyPayloadHash is not the SHA-256 of nothing")
	}

	body := `{"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, "https://x", strings.NewReader(body))
	want := sha256.Sum256([]byte(body))
	for range 2 { // re-readable: hashing twice gives the same answer
		got, err := payloadHash(req)
		if err != nil || got != hex.EncodeToString(want[:]) {
			t.Errorf("hash = %q, %v; want sha256 of the body", got, err)
		}
	}

	broken, _ := http.NewRequest(http.MethodPost, "https://x", nil)
	broken.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("gone") }
	if _, err := payloadHash(broken); err == nil || !strings.Contains(err.Error(), "reread request body") {
		t.Errorf("hash with a failing GetBody = %v", err)
	}
	unreadable, _ := http.NewRequest(http.MethodPost, "https://x", nil)
	unreadable.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(errReader{}), nil }
	if _, err := payloadHash(unreadable); err == nil || !strings.Contains(err.Error(), "hash request body") {
		t.Errorf("hash with an unreadable body = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestBufferBody(t *testing.T) {
	nilBody := httptest.NewRequest(http.MethodPost, "/", nil)
	nilBody.Body = nil
	if b, fit, err := bufferBody(nilBody, 10); b != nil || !fit || err != nil {
		t.Errorf("nil body = %q, %v, %v", b, fit, err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	b, fit, err := bufferBody(req, 5)
	if string(b) != "hello" || !fit || err != nil || req.ContentLength != 5 {
		t.Fatalf("exact-limit body = %q, %v, %v (len %d)", b, fit, err, req.ContentLength)
	}
	again, _ := io.ReadAll(req.Body)
	reread, _ := req.GetBody()
	twice, _ := io.ReadAll(reread)
	if string(again) != "hello" || string(twice) != "hello" {
		t.Errorf("body not re-readable: %q / %q", again, twice)
	}

	over := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello!"))
	if b, fit, err := bufferBody(over, 5); b != nil || fit || err != nil {
		t.Errorf("over-limit body = %q, %v, %v", b, fit, err)
	}

	bad := httptest.NewRequest(http.MethodPost, "/", errReader{})
	if _, _, err := bufferBody(bad, 5); err == nil {
		t.Error("unreadable body: no error")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCredentialTransport(t *testing.T) {
	var sent *http.Request
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "https://x/v1/messages", nil)

	failing := credentialTransport{next: next, cred: func(context.Context, *http.Request) error {
		return errors.New("token expired")
	}}
	if _, err := failing.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "attach credential") {
		t.Fatalf("RoundTrip with a failing credential = %v", err)
	}
	if sent != nil {
		t.Fatal("request left without its credential")
	}

	plain := credentialTransport{next: next}
	resp, err := plain.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || sent != req {
		t.Fatalf("RoundTrip without a credential = %v, %v", resp, err)
	}
}

func TestSourceIP(t *testing.T) {
	cases := map[string]string{
		"10.0.0.7:41234":  "10.0.0.7",
		"[fd00::1]:8080":  "fd00::1",
		"10.0.0.7":        "10.0.0.7",
		"@unix-socket-id": "@unix-socket-id",
	}
	for remote, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if got := sourceIP(r); got != want {
			t.Errorf("sourceIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestReviewLimiterRetryAfter(t *testing.T) {
	var disabled *reviewLimiter
	if got := disabled.retryAfter(); got != 1 {
		t.Errorf("nil limiter retryAfter = %d, want 1", got)
	}
	if newReviewLimiter(0) != nil {
		t.Error("a zero rate must disable the limiter")
	}
	cases := map[float64]int{50: 1, 1: 1, 0.5: 2, 0.25: 4, 0.3: 4}
	for rate, want := range cases {
		if got := newReviewLimiter(rate).retryAfter(); got != want {
			t.Errorf("retryAfter at %v/s = %d, want %d", rate, got, want)
		}
	}
}

// writeServiceAccount writes a Google service-account key file whose token
// endpoint is tokenURL, so the route's token exchange stays in-process.
func writeServiceAccount(t *testing.T, tokenURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "demo",
		"private_key_id": "kid-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "broker@demo.iam.gserviceaccount.com",
		"client_id":      "1",
		"token_uri":      tokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVertexAttachesCachedBearer: the route exchanges its service-account
// assertion once for a bearer, attaches it to every request, and carries the
// project and location it admits.
func TestVertexAttachesCachedBearer(t *testing.T) {
	var mu sync.Mutex
	exchanges := 0
	var grant string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		exchanges++
		grant = r.PostForm.Get("grant_type")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.test","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccount(t, tokenSrv.URL))

	target := mustTarget(t, "https://us-east5-aiplatform.googleapis.com")
	up, err := Vertex(context.Background(), target, "demo", "us-east5")
	if err != nil {
		t.Fatal(err)
	}
	if up.Target != target || up.Project != "demo" || up.Location != "us-east5" {
		t.Errorf("route = %+v", up)
	}
	if err := up.Ready(context.Background()); err != nil {
		t.Fatalf("Ready = %v", err)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/demo/locations/us-east5/x", nil)
		if err := up.Credential(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ya29.test" {
			t.Errorf("Authorization = %q", got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if exchanges != 1 {
		t.Errorf("token exchanges = %d, want 1 (cached)", exchanges)
	}
	if grant != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("grant_type = %q", grant)
	}
}

func TestVertexTokenFailure(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer tokenSrv.Close()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccount(t, tokenSrv.URL))
	up, err := Vertex(context.Background(), mustTarget(t, "https://x.googleapis.com"), "demo", "us-east5")
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Ready(context.Background()); err == nil {
		t.Error("Ready with a refused exchange succeeded")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	if err := up.Credential(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "resolve Google token") {
		t.Errorf("Credential = %v", err)
	}
}

func TestVertexMissingCredentialsFile(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))
	if _, err := Vertex(context.Background(), mustTarget(t, "https://x.googleapis.com"), "p", "l"); err == nil {
		t.Error("Vertex with a missing credentials file succeeded")
	}
}
