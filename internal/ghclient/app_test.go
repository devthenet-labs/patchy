// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPrivateKeyPEM builds a throwaway RSA private key PEM for app auth.
func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// newFakeApp is newFake plus an App pointed at it.
func newFakeApp(t *testing.T) (*http.ServeMux, *App) {
	t.Helper()
	mux, base := newFake(t)
	app, err := NewApp(AppConfig{AppID: 7, PrivateKey: testPrivateKeyPEM(t), BaseURL: base})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	return mux, app
}

func TestNewAppErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  AppConfig
	}{
		{"missing app id", AppConfig{PrivateKey: testPrivateKeyPEM(t)}},
		{"bad private key", AppConfig{AppID: 1, PrivateKey: []byte("not a key")}},
		{"bad base url", AppConfig{AppID: 1, PrivateKey: testPrivateKeyPEM(t), BaseURL: "://bad"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewApp(tt.cfg); err == nil {
				t.Error("NewApp() error = nil, want non-nil")
			}
		})
	}
}

func TestInstallation(t *testing.T) {
	mux, app := newFakeApp(t)
	var lookups atomic.Int32
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("installation lookup Authorization = %q, want app JWT bearer", auth)
		}
		writeJSON(t, w, `{"id": 42}`)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"token":"inst-tok","expires_at":"2100-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		wantAuth(t, r, "token inst-tok")
		writeJSON(t, w, `{"default_branch":"main"}`)
	})

	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}
	c1, err := app.Installation(ctx, repo)
	if err != nil {
		t.Fatalf("Installation() error = %v", err)
	}
	branch, err := c1.DefaultBranch(ctx, repo)
	if err != nil {
		t.Fatalf("DefaultBranch() error = %v", err)
	}
	if branch != "main" {
		t.Errorf("DefaultBranch() = %q, want %q", branch, "main")
	}

	c2, err := app.Installation(ctx, repo)
	if err != nil {
		t.Fatalf("second Installation() error = %v", err)
	}
	if c2 != c1 {
		t.Error("second Installation() returned a different client, want the cached one")
	}
	if got := lookups.Load(); got != 1 {
		t.Errorf("installation endpoint hit %d times, want 1", got)
	}
}

func TestScopedToken(t *testing.T) {
	mux, app := newFakeApp(t)
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"id": 9}`)
	})
	mux.HandleFunc("POST /app/installations/9/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody[map[string]any](t, r)
		if repos, ok := body["repositories"].([]any); !ok || !reflect.DeepEqual(repos, []any{"r"}) {
			t.Errorf("repositories = %v, want [r]", body["repositories"])
		}
		if perms, ok := body["permissions"].(map[string]any); !ok || perms["contents"] != "write" {
			t.Errorf("permissions = %v, want map[contents:write]", body["permissions"])
		}
		writeJSON(t, w, `{"token":"scoped-tok","expires_at":"2026-07-13T12:00:00Z"}`)
	})

	tok, exp, err := app.ScopedToken(context.Background(), Repo{Owner: "o", Name: "r"},
		TokenPerms{Contents: "write"})
	if err != nil {
		t.Fatalf("ScopedToken() error = %v", err)
	}
	if tok != "scoped-tok" {
		t.Errorf("ScopedToken() token = %q, want %q", tok, "scoped-tok")
	}
	if want := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC); !exp.Equal(want) {
		t.Errorf("ScopedToken() expiry = %v, want %v", exp, want)
	}
}

func TestInstallations(t *testing.T) {
	mux, app := newFakeApp(t)
	var lists atomic.Int32
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		writeJSON(t, w, `[{"id":11},{"id":22}]`)
	})

	got, err := app.Installations(context.Background())
	if err != nil {
		t.Fatalf("Installations() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Installations() returned %d clients, want 2", len(got))
	}
	if lists.Load() != 1 {
		t.Errorf("list endpoint hit %d times, want 1", lists.Load())
	}

	// The per-installation clients share the App's cache: resolving one of
	// the same installations by repo must not build a second client.
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"id":11}`)
	})
	byRepo, err := app.Installation(context.Background(), Repo{Owner: "o", Name: "r"})
	if err != nil {
		t.Fatalf("Installation() error = %v", err)
	}
	if byRepo != got[0] {
		t.Error("Installation() built a new client for a cached installation ID")
	}
}
