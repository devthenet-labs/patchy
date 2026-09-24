// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// tokenExpiry is when every token the fake App server mints expires.
var tokenExpiry = time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)

// mint is one access-token request the fake App server answered.
type mint struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// appServer is a fake GitHub App API: installation lookup, access-token
// minting (recorded), and GET /app.
type appServer struct {
	mu    sync.Mutex
	mints []mint
}

func (a *appServer) minted() []mint {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]mint(nil), a.mints...)
}

// newAppServer starts the fake App API and returns it with its base URL.
func newAppServer(t *testing.T) (*appServer, string) {
	t.Helper()
	a := &appServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42}`))
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var m mint
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("decode access-token request: %v", err)
		}
		a.mu.Lock()
		a.mints = append(a.mints, m)
		n := len(a.mints)
		a.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, n, tokenExpiry.Format(time.RFC3339))
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":7,"slug":"patchy-devthenet"}`))
	})
	srv := httptest.NewServer(http.StripPrefix("/api/v3", mux))
	t.Cleanup(srv.Close)
	return a, srv.URL
}

// appSecretData is an App credential Secret's data with a throwaway key.
func appSecretData(t *testing.T) map[string][]byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return map[string][]byte{
		"appID":      []byte("7"),
		"privateKey": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}
}

// tokenStore builds a Store over a fake client holding the credential
// Secret, its clock pinned at 12:00 — an hour before tokenExpiry.
func tokenStore(t *testing.T, data map[string][]byte) (*Store, client.Client) {
	t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cred"}, Data: data}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(secret).Build()
	s := NewStore(c)
	s.now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	return s, c
}

// resolvedAt is o/<repo> on a Forge whose API lives at baseURL.
func resolvedAt(baseURL, repo string) *Resolved {
	f := proxyForge("")
	f.Spec.BaseURL = baseURL
	return &Resolved{Forge: f, Repo: ghclient.Repo{Owner: "o", Name: repo}, Host: "127.0.0.1"}
}

// TestTokenWithScopesAndCaches: each token is scoped to exactly one
// repository and exactly the permissions asked for, and is minted once per
// (repository, permissions) while it has time left.
func TestTokenWithScopesAndCaches(t *testing.T) {
	app, base := newAppServer(t)
	s, _ := tokenStore(t, appSecretData(t))
	ctx := context.Background()
	issuesWrite := ghclient.TokenPerms{Issues: ghclient.PermWrite}
	prWrite := ghclient.TokenPerms{Contents: ghclient.PermRead, PullRequests: ghclient.PermWrite}

	calls := []struct {
		repo      string
		perms     ghclient.TokenPerms
		wantToken string
	}{
		{"r", issuesWrite, "ghs_1"},
		{"r", issuesWrite, "ghs_1"}, // cached
		{"r", prWrite, "ghs_2"},     // other permissions: its own token
		{"other", issuesWrite, "ghs_3"},
		{"R", issuesWrite, "ghs_1"}, // repository names are case-insensitive
		{"r", prWrite, "ghs_2"},
	}
	for i, call := range calls {
		tok, exp, err := s.TokenWith(ctx, resolvedAt(base, call.repo), call.perms)
		if err != nil {
			t.Fatalf("call %d: TokenWith() error = %v", i, err)
		}
		if tok != call.wantToken || !exp.Equal(tokenExpiry) {
			t.Errorf("call %d: TokenWith() = %q, %v, want %q, %v", i, tok, exp, call.wantToken, tokenExpiry)
		}
	}

	want := []mint{
		{Repositories: []string{"r"}, Permissions: map[string]string{"issues": "write"}},
		{Repositories: []string{"r"}, Permissions: map[string]string{"contents": "read", "pull_requests": "write"}},
		{Repositories: []string{"other"}, Permissions: map[string]string{"issues": "write"}},
	}
	if got := app.minted(); !reflect.DeepEqual(got, want) {
		t.Errorf("minted %+v, want %+v", got, want)
	}
}

// TestTokenWithRefreshesBeforeExpiry: a cached token is served until the
// refresh margin before its expiry, then minted again.
func TestTokenWithRefreshesBeforeExpiry(t *testing.T) {
	app, base := newAppServer(t)
	s, _ := tokenStore(t, appSecretData(t))
	ctx := context.Background()
	res := resolvedAt(base, "r")
	perms := ghclient.TokenPerms{Issues: ghclient.PermRead}

	steps := []struct {
		at        time.Time
		wantToken string
	}{
		{tokenExpiry.Add(-time.Hour), "ghs_1"},
		{tokenExpiry.Add(-tokenRefreshMargin - time.Second), "ghs_1"},
		{tokenExpiry.Add(-tokenRefreshMargin), "ghs_2"},
	}
	for i, step := range steps {
		s.now = func() time.Time { return step.at }
		tok, _, err := s.TokenWith(ctx, res, perms)
		if err != nil {
			t.Fatalf("step %d: TokenWith() error = %v", i, err)
		}
		if tok != step.wantToken {
			t.Errorf("step %d at %v: token = %q, want %q", i, step.at, tok, step.wantToken)
		}
	}
	if got := len(app.minted()); got != 2 {
		t.Errorf("minted %d tokens, want 2", got)
	}
	// The expired entry was evicted when its replacement was stored.
	if got := len(s.tokens); got != 1 {
		t.Errorf("cache holds %d tokens, want 1", got)
	}
}

// TestTokenWithSecretRotation: a rotated Secret builds a new App, whose
// tokens never come from the old App's cache entries.
func TestTokenWithSecretRotation(t *testing.T) {
	app, base := newAppServer(t)
	s, c := tokenStore(t, appSecretData(t))
	ctx := context.Background()
	res := resolvedAt(base, "r")
	perms := ghclient.TokenPerms{Issues: ghclient.PermWrite}

	if _, _, err := s.TokenWith(ctx, res, perms); err != nil {
		t.Fatalf("TokenWith() error = %v", err)
	}
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cred"}, &secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	secret.Data = appSecretData(t)
	if err := c.Update(ctx, &secret); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	tok, _, err := s.TokenWith(ctx, res, perms)
	if err != nil {
		t.Fatalf("TokenWith() after rotation error = %v", err)
	}
	if tok != "ghs_2" || len(app.minted()) != 2 {
		t.Errorf("after rotation token = %q with %d mints, want a fresh ghs_2", tok, len(app.minted()))
	}
}

// TestTokenWithRefusesUnscopedPermissions: a permission set that requests
// nothing (which GitHub would answer with the installation's full set) or an
// unknown level never reaches GitHub, even on a PAT Forge.
func TestTokenWithRefusesUnscopedPermissions(t *testing.T) {
	app, base := newAppServer(t)
	s, _ := tokenStore(t, appSecretData(t))
	pat, _ := tokenStore(t, map[string][]byte{"token": []byte("pat")})
	for _, perms := range []ghclient.TokenPerms{{}, {Issues: "admin"}, {Contents: "Write"}} {
		for name, store := range map[string]*Store{"app": s, "pat": pat} {
			if _, _, err := store.TokenWith(context.Background(), resolvedAt(base, "r"), perms); err == nil {
				t.Errorf("%s TokenWith(%+v) error = nil, want refusal", name, perms)
			}
		}
	}
	if got := len(app.minted()); got != 0 {
		t.Errorf("minted %d tokens, want none", got)
	}
}

// TestTokenWithPAT: a PAT cannot be narrowed; it is returned as-is with no
// expiry and nothing is minted or cached.
func TestTokenWithPAT(t *testing.T) {
	s, _ := tokenStore(t, map[string][]byte{"token": []byte("pat-token\n")})
	tok, exp, err := s.TokenWith(context.Background(), resolvedAt("", "r"),
		ghclient.TokenPerms{Issues: ghclient.PermWrite})
	if err != nil {
		t.Fatalf("TokenWith() error = %v", err)
	}
	if tok != "pat-token" || !exp.IsZero() {
		t.Errorf("TokenWith() = %q, %v, want the PAT and a zero expiry", tok, exp)
	}
	if len(s.tokens) != 0 {
		t.Errorf("cache holds %d tokens, want none for a PAT", len(s.tokens))
	}
}

func TestBotLogin(t *testing.T) {
	_, base := newAppServer(t)
	s, _ := tokenStore(t, appSecretData(t))
	login, err := s.BotLogin(context.Background(), resolvedAt(base, "r"))
	if err != nil {
		t.Fatalf("BotLogin() error = %v", err)
	}
	if login != "patchy-devthenet[bot]" {
		t.Errorf("BotLogin() = %q, want patchy-devthenet[bot]", login)
	}

	pat, _ := tokenStore(t, map[string][]byte{"token": []byte("pat")})
	if _, err := pat.BotLogin(context.Background(), resolvedAt(base, "r")); !errors.Is(err, ErrNoBotIdentity) {
		t.Errorf("PAT BotLogin() error = %v, want ErrNoBotIdentity", err)
	}
}
