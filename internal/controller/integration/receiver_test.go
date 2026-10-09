// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghas"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/scc"
	"github.com/bitwise-media-group/patchy/internal/webhook"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// newGitHubAPI starts an in-process fake GitHub REST API. A non-github base
// URL makes the client append /api/v3 (the GHES convention), which the
// server strips so handlers register bare REST paths.
func newGitHubAPI(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.StripPrefix("/api/v3", mux))
	t.Cleanup(srv.Close)
	return mux, srv.URL
}

func respondJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// patSecret is a PAT credential Secret with a webhook HMAC secret.
func patSecret(name, token, hmacSecret string) *corev1.Secret {
	data := map[string][]byte{}
	if token != "" {
		data[ghsecret.KeyToken] = []byte(token)
	}
	if hmacSecret != "" {
		data[ghsecret.KeyWebhookSecret] = []byte(hmacSecret)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"}, Data: data}
}

// ghIntegrationAt is a github Integration with its own Secret and an API
// base URL (a fake server, or "" for github.com).
func ghIntegrationAt(name, secretName, baseURL string) *v1alpha1.Integration {
	integ := testIntegration()
	integ.Name = name
	integ.Spec.SecretRef = &v1alpha1.LocalSecretReference{Name: secretName}
	integ.Spec.GitHub.BaseURL = baseURL
	return integ
}

// receiverClient is a fake client carrying every index the receiver's
// downstream paths (ingest, signals) query.
func receiverClient(funcs *interceptor.Funcs, objs ...client.Object) client.Client {
	b := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Integration{}).
		WithIndex(&v1alpha1.Finding{}, KeyHashIndex, KeyHashIndexer).
		WithIndex(&v1alpha1.Finding{}, TrackingURLIndex, func(obj client.Object) []string {
			f := obj.(*v1alpha1.Finding)
			if f.Status.Tracking == nil {
				return nil
			}
			return []string{f.Status.Tracking.URL}
		})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func newTestReceiver(c client.Client) *Receiver {
	return &Receiver{
		Reader:    c,
		Creds:     NewCreds(c),
		Ingest:    &Ingestor{Client: c, Namespace: "patchy", Window: time.Hour, Now: func() time.Time { return testClock }},
		Signals:   &Signals{Client: c, Namespace: "patchy", Now: func() time.Time { return testClock }},
		Namespace: "patchy",
	}
}

// endpointFor returns the receiver's endpoint for path.
func endpointFor(t *testing.T, r *Receiver, path string) webhook.Endpoint {
	t.Helper()
	for _, e := range r.Endpoints() {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no endpoint for %s", path)
	return webhook.Endpoint{}
}

func hubSignature(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestReceiverEndpoints(t *testing.T) {
	r := newTestReceiver(receiverClient(nil))
	eps := r.Endpoints()
	paths := make([]string, 0, len(eps))
	for _, e := range eps {
		paths = append(paths, e.Path)
		if e.Auth == nil || e.Decode == nil || e.Handler == nil {
			t.Errorf("endpoint %s is incomplete: %+v", e.Path, e)
		}
	}
	want := []string{GitHubPath, SCCPath, WizPath, GenericPathPattern}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	// The webhook server refuses incomplete or duplicate endpoints; the
	// receiver's set must be servable as is.
	if _, err := webhook.NewServer(webhook.Config{Endpoints: eps}, slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("NewServer(Endpoints()) = %v", err)
	}
}

// The GitHub route accepts a delivery only when it is signed with the
// webhook secret of an enabled github Integration: suspended Integrations,
// other providers' credentials and unreadable Secrets contribute nothing.
func TestGitHubRouteAuthentication(t *testing.T) {
	suspended := ghIntegrationAt("gh-suspended", "creds-suspended", "")
	suspended.Spec.Suspend = true
	broken := ghIntegrationAt("gh-broken", "creds-missing", "")
	emptyKey := ghIntegrationAt("gh-nokey", "creds-nokey", "")
	wizInteg := testWizIntegration(true, false)

	c := receiverClient(nil,
		ghIntegrationAt("gh", "creds", ""), patSecret("creds", "pat", "live-secret"),
		suspended, patSecret("creds-suspended", "pat", "suspended-secret"),
		broken,
		emptyKey, patSecret("creds-nokey", "pat", ""),
		wizInteg, wizSecret("wiz-token"),
	)
	r := newTestReceiver(c)

	if got := r.Secrets(t.Context()); len(got) != 1 || string(got[0]) != "live-secret" {
		t.Fatalf("Secrets() = %q, want only the enabled integration's secret", got)
	}

	body := []byte(`{"action":"created"}`)
	auth := endpointFor(t, r, GitHubPath).Auth
	tests := []struct {
		name    string
		sig     string
		body    []byte
		wantErr bool
	}{
		{name: "signed with the live secret", sig: hubSignature([]byte("live-secret"), body), body: body},
		{
			name: "signed with a suspended integration's secret", sig: hubSignature([]byte("suspended-secret"), body),
			body: body, wantErr: true,
		},
		{
			name: "signed with the wiz bearer token", sig: hubSignature([]byte("wiz-token"), body), body: body,
			wantErr: true,
		},
		{name: "signed with an empty key", sig: hubSignature(nil, body), body: body, wantErr: true},
		{
			name: "body tampered after signing", sig: hubSignature([]byte("live-secret"), body),
			body: []byte(`{"action":"deleted"}`), wantErr: true,
		},
		{name: "no signature", body: body, wantErr: true},
		{
			name: "unprefixed digest", sig: strings.TrimPrefix(hubSignature([]byte("live-secret"), body), "sha256="),
			body: body, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, GitHubPath, bytes.NewReader(tt.body))
			if tt.sig != "" {
				req.Header.Set("X-Hub-Signature-256", tt.sig)
			}
			err := auth.Authenticate(t.Context(), req, tt.body)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Authenticate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, webhook.ErrUnauthenticated) {
				t.Errorf("Authenticate() = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// An Integration list failure yields no candidate secrets, so the route
// fails closed and the failure is logged.
func TestGitHubRouteFailsClosedOnListError(t *testing.T) {
	c := receiverClient(&interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache unavailable")
		},
	}, ghIntegrationAt("gh", "creds", ""), patSecret("creds", "pat", "live-secret"))
	var logs bytes.Buffer
	r := newTestReceiver(c)
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))

	if got := r.Secrets(t.Context()); got != nil {
		t.Errorf("Secrets() = %q, want nil on a list failure", got)
	}
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, GitHubPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", hubSignature([]byte("live-secret"), body))
	err := endpointFor(t, r, GitHubPath).Auth.Authenticate(t.Context(), req, body)
	if !errors.Is(err, webhook.ErrUnauthenticated) {
		t.Errorf("Authenticate() = %v, want ErrUnauthenticated", err)
	}
	if !strings.Contains(logs.String(), "list integrations for webhook credentials") {
		t.Errorf("logs = %q, want the list failure logged", logs.String())
	}
}

// stubVerifier stands in for Google's signature check: the authenticator
// decides on the claims it returns.
type stubVerifier struct {
	claims *webhook.IDTokenClaims
	err    error
}

func (s stubVerifier) Verify(context.Context, string) (*webhook.IDTokenClaims, error) {
	if s.err != nil {
		return nil, s.err
	}
	c := *s.claims
	return &c, nil
}

const (
	sccAudience = "https://patchy.example/google-cloud/webhooks"
	sccAccount  = "scc-push@acme-prod.iam.gserviceaccount.com"
)

func sccIntegration(name string) *v1alpha1.Integration {
	return &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec: v1alpha1.IntegrationSpec{
			Provider: v1alpha1.IntegrationProviderGoogleCloud,
			GoogleCloud: &v1alpha1.GoogleCloudIntegration{
				SecurityCommandCenter: &v1alpha1.GoogleCloudSCC{
					Enabled:        true,
					Audience:       sccAudience,
					ServiceAccount: sccAccount,
					MinSeverity:    v1alpha1.LevelLow,
				},
			},
		},
	}
}

func bearer(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, SCCPath, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// The Pub/Sub route authenticates against the google-cloud Integration's
// audience and service account, and fails closed with none configured.
func TestSCCRouteAuthentication(t *testing.T) {
	good := &webhook.IDTokenClaims{Audience: sccAudience, Email: sccAccount, Verified: true}
	suspended := sccIntegration("gcp")
	suspended.Spec.Suspend = true
	disabled := sccIntegration("gcp")
	disabled.Spec.GoogleCloud.SecurityCommandCenter.Enabled = false

	tests := []struct {
		name     string
		objs     []client.Object
		verifier stubVerifier
		req      *http.Request
		wantErr  bool
	}{
		{
			name: "the configured service account passes", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{claims: good}, req: bearer("tok"),
		},
		{
			name: "no integration fails closed", verifier: stubVerifier{claims: good}, req: bearer("tok"), wantErr: true,
		},
		{
			name: "a suspended integration fails closed", objs: []client.Object{suspended},
			verifier: stubVerifier{claims: good}, req: bearer("tok"), wantErr: true,
		},
		{
			name: "a disabled capability fails closed", objs: []client.Object{disabled},
			verifier: stubVerifier{claims: good}, req: bearer("tok"), wantErr: true,
		},
		{
			name:     "two integrations fail closed",
			objs:     []client.Object{sccIntegration("gcp-a"), sccIntegration("gcp-b")},
			verifier: stubVerifier{claims: good}, req: bearer("tok"), wantErr: true,
		},
		{
			name: "another identity is refused", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{claims: &webhook.IDTokenClaims{Audience: sccAudience, Email: "x@gmail.com", Verified: true}},
			req:      bearer("tok"), wantErr: true,
		},
		{
			name: "an unverified email is refused", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{claims: &webhook.IDTokenClaims{Audience: sccAudience, Email: sccAccount}},
			req:      bearer("tok"), wantErr: true,
		},
		{
			name: "another audience is refused", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{claims: &webhook.IDTokenClaims{Audience: "https://other", Email: sccAccount, Verified: true}},
			req:      bearer("tok"), wantErr: true,
		},
		{
			name: "a bad signature is refused", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{err: errors.New("signature invalid")}, req: bearer("tok"), wantErr: true,
		},
		{
			name: "no bearer token is refused", objs: []client.Object{sccIntegration("gcp")},
			verifier: stubVerifier{claims: good}, req: bearer(""), wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReceiver(receiverClient(nil, tt.objs...))
			r.NewVerifier = func(string) webhook.TokenVerifier { return tt.verifier }
			err := endpointFor(t, r, SCCPath).Auth.Authenticate(t.Context(), tt.req, nil)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Authenticate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, webhook.ErrUnauthenticated) {
				t.Errorf("Authenticate() = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// Verifiers are built once per audience (building one fetches Google's
// discovery document) and rebuilt when the operator changes the audience.
func TestSCCVerifierCachedPerAudience(t *testing.T) {
	integ := sccIntegration("gcp")
	c := receiverClient(nil, integ)
	r := newTestReceiver(c)
	var mu sync.Mutex
	var built []string
	r.NewVerifier = func(aud string) webhook.TokenVerifier {
		mu.Lock()
		built = append(built, aud)
		mu.Unlock()
		return stubVerifier{claims: &webhook.IDTokenClaims{Audience: aud, Email: sccAccount, Verified: true}}
	}
	auth := endpointFor(t, r, SCCPath).Auth
	for range 3 {
		if err := auth.Authenticate(t.Context(), bearer("tok"), nil); err != nil {
			t.Fatalf("Authenticate() = %v", err)
		}
	}
	if !slices.Equal(built, []string{sccAudience}) {
		t.Fatalf("verifiers built = %v, want one for %s", built, sccAudience)
	}

	const rotated = "https://patchy.example/rotated"
	var live v1alpha1.Integration
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(integ), &live); err != nil {
		t.Fatalf("get: %v", err)
	}
	live.Spec.GoogleCloud.SecurityCommandCenter.Audience = rotated
	if err := c.Update(t.Context(), &live); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := auth.Authenticate(t.Context(), bearer("tok"), nil); err != nil {
		t.Fatalf("Authenticate() after rotation = %v", err)
	}
	if !slices.Equal(built, []string{sccAudience, rotated}) {
		t.Errorf("verifiers built = %v, want a second for the rotated audience", built)
	}
}

// Without a NewVerifier seam the route verifies against Google discovery,
// honouring the issuer override and the Integration's audience.
func TestSCCDefaultVerifier(t *testing.T) {
	r := newTestReceiver(receiverClient(nil))
	r.OIDCIssuer = "https://issuer.test"
	a, ok := endpointFor(t, r, SCCPath).Auth.(*sccAuthenticator)
	if !ok {
		t.Fatalf("SCC auth = %T, want *sccAuthenticator", endpointFor(t, r, SCCPath).Auth)
	}
	v, ok := a.verifierFor(sccAudience).(*webhook.GoogleVerifier)
	if !ok {
		t.Fatalf("default verifier = %T, want *webhook.GoogleVerifier", a.verifierFor(sccAudience))
	}
	if v.Issuer != "https://issuer.test" || v.Audience != sccAudience {
		t.Errorf("verifier issuer/audience = %q/%q, want the override and the audience", v.Issuer, v.Audience)
	}
	if again := a.verifierFor(sccAudience); again != webhook.TokenVerifier(v) {
		t.Error("verifierFor rebuilt a cached verifier")
	}
}

// sccPush is a Pub/Sub push envelope carrying one ACTIVE, HIGH SCC
// notification.
func sccPush(t *testing.T, messageID string) []byte {
	t.Helper()
	notification := map[string]any{
		"finding": map[string]any{
			"name":         "organizations/1234567890/sources/555/findings/abc123",
			"parent":       "organizations/1234567890/sources/555",
			"resourceName": "//storage.googleapis.com/projects/acme-prod/buckets/acme-artifacts",
			"state":        "ACTIVE",
			"category":     "PUBLIC_BUCKET_ACL",
			"severity":     "HIGH",
			"mute":         "UNMUTED",
			"findingClass": "MISCONFIGURATION",
			"description":  "The bucket is publicly readable.",
		},
		"resource": map[string]any{
			"name":          "//storage.googleapis.com/projects/acme-prod/buckets/acme-artifacts",
			"displayName":   "acme-artifacts",
			"type":          "google.cloud.storage.Bucket",
			"project":       "projects/acme-prod",
			"cloudProvider": "GOOGLE_CLOUD_PLATFORM",
		},
	}
	raw, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"subscription": "projects/acme-prod/subscriptions/patchy-scc-push",
		"message": map[string]any{
			"data":      base64.StdEncoding.EncodeToString(raw),
			"messageId": messageID,
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func TestSCCDecoder(t *testing.T) {
	r := newTestReceiver(receiverClient(nil))
	decode := endpointFor(t, r, SCCPath).Decode
	typ, id, err := decode(httptest.NewRequest(http.MethodPost, SCCPath, nil), sccPush(t, "msg-77"))
	if err != nil || typ != scc.EventType || id != "msg-77" {
		t.Errorf("decode() = %q, %q, %v; want %q, msg-77, nil", typ, id, err, scc.EventType)
	}
	// An undecodable body still labels (the handler reports it), but cannot
	// be deduplicated.
	typ, id, err = decode(httptest.NewRequest(http.MethodPost, SCCPath, nil), []byte("not json"))
	if err != nil || typ != scc.EventType || id != "" {
		t.Errorf("decode(garbage) = %q, %q, %v; want %q, \"\", nil", typ, id, err, scc.EventType)
	}
}

func TestHandleSCC(t *testing.T) {
	t.Run("ingests a notification into a cloud finding", func(t *testing.T) {
		c := receiverClient(nil, sccIntegration("gcp"))
		r := newTestReceiver(c)
		e := webhook.Event{Type: scc.EventType, DeliveryID: "m1", Payload: sccPush(t, "m1"), Path: SCCPath}
		if err := endpointFor(t, r, SCCPath).Handler.Handle(t.Context(), e); err != nil {
			t.Fatalf("Handle() = %v", err)
		}
		got := listFindings(t, c)
		if len(got) != 1 {
			t.Fatalf("findings = %d, want 1", len(got))
		}
		f := got[0]
		if f.Spec.IntegrationRef.Name != "gcp" || f.Spec.CloudResource == nil ||
			f.Spec.CloudResource.Name != "//storage.googleapis.com/projects/acme-prod/buckets/acme-artifacts" {
			t.Errorf("finding spec = %+v, want the gcp integration's cloud finding", f.Spec)
		}
	})

	t.Run("no scc integration ingests nothing", func(t *testing.T) {
		c := receiverClient(nil)
		r := newTestReceiver(c)
		e := webhook.Event{Type: scc.EventType, Payload: sccPush(t, "m1")}
		if err := r.handleSCC(t.Context(), e); err != nil {
			t.Fatalf("handleSCC() = %v, want nil", err)
		}
		if got := listFindings(t, c); len(got) != 0 {
			t.Errorf("findings = %d, want 0", len(got))
		}
	})

	t.Run("ambiguous integrations are an error", func(t *testing.T) {
		c := receiverClient(nil, sccIntegration("gcp-a"), sccIntegration("gcp-b"))
		r := newTestReceiver(c)
		err := r.handleSCC(t.Context(), webhook.Event{Type: scc.EventType, Payload: sccPush(t, "m1")})
		if !errors.Is(err, ErrAmbiguousIntegration) {
			t.Errorf("handleSCC() = %v, want ErrAmbiguousIntegration", err)
		}
	})

	t.Run("an undecodable push is an error naming the source", func(t *testing.T) {
		c := receiverClient(nil, sccIntegration("gcp"))
		r := newTestReceiver(c)
		err := r.handleSCC(t.Context(), webhook.Event{Type: scc.EventType, Payload: []byte(`{"message":{}}`)})
		if err == nil || !strings.Contains(err.Error(), "decode "+scc.ID+" delivery") {
			t.Errorf("handleSCC() = %v, want a decode error naming %s", err, scc.ID)
		}
	})
}

// alertJSON is the REST shape of a code-scanning alert.
const alertJSON = `{
	"number": 9,
	"html_url": "https://github.com/acme/orders/security/code-scanning/9",
	"state": "open",
	"rule": {"id": "go/reflected-xss", "severity": "error", "security_severity_level": "high",
		"description": "Reflected cross-site scripting", "tags": ["security", "external/cwe/cwe-079"]},
	"tool": {"name": "CodeQL"},
	"most_recent_instance": {"ref": "refs/heads/main", "commit_sha": "45b1bec",
		"location": {"path": "echo.go", "start_line": 16, "end_line": 16}}
}`

func codeScanningPayload(action string) []byte {
	return []byte(`{"action":"` + action + `",` +
		`"alert":{"number":9,"most_recent_instance":{"ref":"refs/heads/main","commit_sha":"45b1bec"}},` +
		`"ref":"refs/heads/main","commit_oid":"45b1bec",` +
		`"repository":{"name":"orders","default_branch":"main","owner":{"login":"acme"}}}`)
}

func TestHandleGitHub(t *testing.T) {
	mux, base := newGitHubAPI(t)
	var alertCalls int
	var mu sync.Mutex
	mux.HandleFunc("GET /repos/acme/orders/code-scanning/alerts/9", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		alertCalls++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer pat" {
			respondJSON(w, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
			return
		}
		respondJSON(w, http.StatusOK, alertJSON)
	})

	t.Run("a code scanning alert is fetched and ingested", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", base), patSecret("creds", "pat", "hmac"))
		r := newTestReceiver(c)
		e := webhook.Event{Type: ghas.EventType, DeliveryID: "d1", Payload: codeScanningPayload("created"), Path: GitHubPath}
		if err := endpointFor(t, r, GitHubPath).Handler.Handle(t.Context(), e); err != nil {
			t.Fatalf("Handle() = %v", err)
		}
		got := listFindings(t, c)
		if len(got) != 1 {
			t.Fatalf("findings = %d, want 1", len(got))
		}
		spec := got[0].Spec
		if spec.Repository == nil || spec.Repository.Name != "acme/orders" || spec.RuleID != "go/reflected-xss" {
			t.Errorf("finding spec = %+v, want acme/orders go/reflected-xss", got[0].Spec)
		}
	})

	t.Run("an alert the API refuses is an error and ingests nothing", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", base), patSecret("creds", "wrong", "hmac"))
		r := newTestReceiver(c)
		err := r.handleGitHub(t.Context(), webhook.Event{Type: ghas.EventType, Payload: codeScanningPayload("created")})
		if err == nil || !strings.Contains(err.Error(), "fetch alert acme/orders#9") {
			t.Errorf("handleGitHub() = %v, want the fetch failure", err)
		}
		if got := listFindings(t, c); len(got) != 0 {
			t.Errorf("findings = %d, want 0", len(got))
		}
	})

	t.Run("a missing credential secret is an error", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", base))
		r := newTestReceiver(c)
		err := r.handleGitHub(t.Context(), webhook.Event{Type: ghas.EventType, Payload: codeScanningPayload("created")})
		if err == nil || !strings.Contains(err.Error(), "get integration secret") {
			t.Errorf("handleGitHub() = %v, want the secret read failure", err)
		}
	})

	t.Run("non-actionable actions make no API call", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", base), patSecret("creds", "pat", "hmac"))
		r := newTestReceiver(c)
		mu.Lock()
		before := alertCalls
		mu.Unlock()
		e := webhook.Event{Type: ghas.EventType, Payload: codeScanningPayload("fixed")}
		if err := r.handleGitHub(t.Context(), e); err != nil {
			t.Fatalf("handleGitHub() = %v", err)
		}
		mu.Lock()
		after := alertCalls
		mu.Unlock()
		if after != before || len(listFindings(t, c)) != 0 {
			t.Errorf("alert calls %d -> %d, findings %d; want no call and no finding", before, after, len(listFindings(t, c)))
		}
	})

	t.Run("without a code scanning integration nothing is ingested", func(t *testing.T) {
		integ := ghIntegrationAt("gh", "creds", base)
		integ.Spec.GitHub.CodeScanningAlerts.Enabled = false
		c := receiverClient(nil, integ, patSecret("creds", "pat", "hmac"))
		r := newTestReceiver(c)
		e := webhook.Event{Type: ghas.EventType, Payload: codeScanningPayload("created")}
		if err := r.handleGitHub(t.Context(), e); err != nil {
			t.Fatalf("handleGitHub() = %v, want nil", err)
		}
		if got := listFindings(t, c); len(got) != 0 {
			t.Errorf("findings = %d, want 0", len(got))
		}
	})

	t.Run("two code scanning integrations are an error", func(t *testing.T) {
		c := receiverClient(nil,
			ghIntegrationAt("gh-a", "creds", base), ghIntegrationAt("gh-b", "creds", base), patSecret("creds", "pat", "hmac"))
		r := newTestReceiver(c)
		err := r.handleGitHub(t.Context(), webhook.Event{Type: ghas.EventType, Payload: codeScanningPayload("created")})
		if !errors.Is(err, ErrAmbiguousIntegration) {
			t.Errorf("handleGitHub() = %v, want ErrAmbiguousIntegration", err)
		}
	})

	t.Run("an unknown event is ignored", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", base), patSecret("creds", "pat", "hmac"))
		r := newTestReceiver(c)
		if err := r.handleGitHub(t.Context(), webhook.Event{Type: "star", Payload: []byte(`{}`)}); err != nil {
			t.Errorf("handleGitHub() = %v, want nil", err)
		}
	})
}

// Issue, comment and pull-request events go to the human-signal handler for
// the issues-enabled Integration; with none configured they are dropped.
func TestHandleGitHubTrackingEvents(t *testing.T) {
	closed := `{"action":"closed","issue":{"number":7,"html_url":"https://github.com/acme/orders/issues/7"}}`

	t.Run("an issue close reaches the finding", func(t *testing.T) {
		fnd := trackedFinding(v1alpha1.PhaseQueued)
		c := receiverClient(nil, ghIntegrationAt("gh", "creds", ""), fnd)
		fnd.Status = trackedFinding(v1alpha1.PhaseQueued).Status
		if err := c.Status().Update(t.Context(), fnd); err != nil {
			t.Fatalf("seed status: %v", err)
		}
		r := newTestReceiver(c)
		if err := r.handleGitHub(t.Context(), webhook.Event{Type: "issues", Payload: []byte(closed)}); err != nil {
			t.Fatalf("handleGitHub() = %v", err)
		}
		if got := get(t, c, fnd.Name).Status.Phase; got != v1alpha1.PhaseHandedOff {
			t.Errorf("phase = %s, want HandedOff", got)
		}
	})

	t.Run("without an issues integration the event is dropped", func(t *testing.T) {
		integ := ghIntegrationAt("gh", "creds", "")
		integ.Spec.GitHub.Issues.Enabled = false
		fnd := trackedFinding(v1alpha1.PhaseQueued)
		c := receiverClient(nil, integ, fnd)
		fnd.Status = trackedFinding(v1alpha1.PhaseQueued).Status
		if err := c.Status().Update(t.Context(), fnd); err != nil {
			t.Fatalf("seed status: %v", err)
		}
		r := newTestReceiver(c)
		if err := r.handleGitHub(t.Context(), webhook.Event{Type: "issues", Payload: []byte(closed)}); err != nil {
			t.Fatalf("handleGitHub() = %v, want nil", err)
		}
		if got := get(t, c, fnd.Name).Status.Phase; got != v1alpha1.PhaseQueued {
			t.Errorf("phase = %s, want Queued (untouched)", got)
		}
	})

	t.Run("two issues integrations are an error", func(t *testing.T) {
		c := receiverClient(nil, ghIntegrationAt("gh-a", "creds", ""), ghIntegrationAt("gh-b", "creds", ""))
		r := newTestReceiver(c)
		err := r.handleGitHub(t.Context(), webhook.Event{Type: "pull_request", Payload: []byte(`{}`)})
		if !errors.Is(err, ErrAmbiguousIntegration) {
			t.Errorf("handleGitHub() = %v, want ErrAmbiguousIntegration", err)
		}
	})
}

// The commit graph asks GitHub's compare API, with the Integration's own
// credential, how two commits relate.
func TestCommitGraph(t *testing.T) {
	mux, base := newGitHubAPI(t)
	status := map[string]string{
		"head...base":  "behind",
		"base...head":  "ahead",
		"same...same":  "identical",
		"left...right": "diverged",
	}
	mux.HandleFunc("GET /repos/acme/orders/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := status[r.PathValue("basehead")]
		if !ok {
			respondJSON(w, http.StatusNotFound, `{"message":"Not Found"}`)
			return
		}
		respondJSON(w, http.StatusOK, `{"status":"`+s+`"}`)
	})
	integ := ghIntegrationAt("gh", "creds", base)
	c := receiverClient(nil, integ, patSecret("creds", "pat", "hmac"))
	g := NewCommitGraph(NewCreds(c))
	repo := source.Repo{Owner: "acme", Name: "orders"}

	precedes := []struct {
		commit, descendant string
		want               bool
	}{
		{commit: "base", descendant: "head", want: true}, // compare head...base = behind
		{commit: "head", descendant: "base", want: false},
		{commit: "same", descendant: "same", want: false},
	}
	for _, tt := range precedes {
		got, err := g.Precedes(t.Context(), integ, repo, tt.commit, tt.descendant)
		if err != nil || got != tt.want {
			t.Errorf("Precedes(%s, %s) = %v, %v; want %v", tt.commit, tt.descendant, got, err, tt.want)
		}
	}

	contains := []struct {
		branch, commit string
		want           bool
	}{
		{branch: "head", commit: "base", want: true},   // ahead
		{branch: "same", commit: "same", want: true},   // identical
		{branch: "base", commit: "head", want: false},  // behind
		{branch: "right", commit: "left", want: false}, // diverged
	}
	for _, tt := range contains {
		got, err := g.Contains(t.Context(), integ, repo, tt.branch, tt.commit)
		if err != nil || got != tt.want {
			t.Errorf("Contains(%s, %s) = %v, %v; want %v", tt.branch, tt.commit, got, err, tt.want)
		}
	}

	if _, err := g.Precedes(t.Context(), integ, repo, "x", "y"); err == nil {
		t.Error("Precedes() over an unknown pair = nil error, want the API failure")
	}
	noSecret := ghIntegrationAt("gh", "absent", base)
	if _, err := g.Contains(t.Context(), noSecret, repo, "head", "base"); err == nil {
		t.Error("Contains() without a credential = nil error, want the secret failure")
	}
}

func TestAlertLabel(t *testing.T) {
	tests := []struct {
		f    source.Finding
		want string
	}{
		{
			f:    source.Finding{AlertID: "organizations/1/sources/2/findings/3", AlertNumber: 4},
			want: "organizations/1/sources/2/findings/3",
		},
		{
			f:    source.Finding{Repo: source.Repo{Owner: "acme", Name: "orders"}, AlertNumber: 9},
			want: "acme/orders alert 9",
		},
	}
	for _, tt := range tests {
		if got := alertLabel(tt.f); got != tt.want {
			t.Errorf("alertLabel(%+v) = %q, want %q", tt.f, got, tt.want)
		}
	}
}
