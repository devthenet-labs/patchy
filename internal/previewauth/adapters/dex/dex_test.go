// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package dex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/fakedex"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

const redirect = "https://preview-auth.example.com/dex/callback"

func claims() auth.ClaimsConfig {
	return auth.ClaimsConfig{Username: "preferred_username", Groups: "groups", UsernamePrefix: "github:",
		GroupsPrefix: "github:"}
}

func startDex(t *testing.T) *fakedex.Server {
	t.Helper()
	d, err := fakedex.Start("relay", "relay-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func newUpstream(t *testing.T, d *fakedex.Server, edit func(*Config)) *Upstream {
	t.Helper()
	cfg := Config{IssuerURL: d.URL, ClientID: "relay", ClientSecret: "relay-secret", RedirectURL: redirect,
		Claims: claims(), AllowHTTP: true}
	if edit != nil {
		edit(&cfg)
	}
	u, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// signIn follows AuthURL at the fake Dex and returns the code it sends back.
func signIn(t *testing.T, u *Upstream, state, nonce, verifier string) string {
	t.Helper()
	loc, err := u.AuthURL(context.Background(), state, nonce, previewauth.S256(verifier))
	if err != nil {
		t.Fatal(err)
	}
	au, _ := url.Parse(loc)
	q := au.Query()
	if q.Get("redirect_uri") != redirect || q.Get("scope") != "openid profile email groups" ||
		q.Get("state") != state {
		t.Fatalf("auth URL %s", loc)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Query().Get("state") != state {
		t.Fatalf("callback %q", resp.Header.Get("Location"))
	}
	return back.Query().Get("code")
}

var verifier = strings.Repeat("v", 43)

func TestExchange(t *testing.T) {
	d := startDex(t)
	d.SignIn(map[string]any{"sub": "dex-sub", "preferred_username": "alice", "groups": []any{"org:viewers"}})
	u := newUpstream(t, d, nil)
	code := signIn(t, u, "st", "n1", verifier)
	id, err := u.Exchange(context.Background(), code, verifier, "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := previewauth.Identity{Username: "github:alice", Groups: []string{"github:org:viewers"}}
	if !reflect.DeepEqual(id, want) {
		t.Fatalf("identity %+v", id)
	}
}

func TestExchangeRefuses(t *testing.T) {
	alice := map[string]any{"sub": "s", "preferred_username": "alice"}
	tests := []struct {
		name     string
		claims   map[string]any
		edit     func(*Config)
		verifier string
		nonce    string
	}{
		{name: "wrong nonce", claims: alice, verifier: verifier, nonce: "other"},
		{name: "wrong verifier", claims: alice, verifier: strings.Repeat("w", 43), nonce: "n1"},
		{name: "no username", claims: map[string]any{"sub": "s"}, verifier: verifier, nonce: "n1"},
		{name: "unverified email", claims: map[string]any{"sub": "s", "email": "a@x"}, verifier: verifier,
			nonce: "n1", edit: func(c *Config) { c.Claims.Username, c.Claims.RequireVerifiedEmail = "email", true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := startDex(t)
			d.SignIn(tt.claims)
			u := newUpstream(t, d, tt.edit)
			code := signIn(t, u, "st", "n1", verifier)
			if _, err := u.Exchange(context.Background(), code, tt.verifier, tt.nonce); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestExchangeRefusesAnotherClientsToken: an ID token Dex issued to another
// client is not the relay's, even from the same issuer.
func TestExchangeRefusesAnotherClientsToken(t *testing.T) {
	d := startDex(t)
	d.SignIn(map[string]any{"sub": "s", "preferred_username": "alice", "aud": "status-server"})
	u := newUpstream(t, d, nil)
	code := signIn(t, u, "st", "n1", verifier)
	if _, err := u.Exchange(context.Background(), code, verifier, "n1"); err == nil {
		t.Fatal("accepted another client's ID token")
	}
}

func TestDiscoveryIsLazyAndRetried(t *testing.T) {
	d := startDex(t)
	// A handler that is down until the test says otherwise.
	var up atomic.Bool
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		d.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	u, err := New(Config{IssuerURL: d.URL, ClientID: "relay", ClientSecret: "s", RedirectURL: redirect,
		Claims: claims(), AllowHTTP: true, HTTPClient: &http.Client{Transport: redirectTo{gate.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	u.now = func() time.Time { return now }
	if _, err := u.AuthURL(context.Background(), "st", "n", previewauth.S256(verifier)); err == nil {
		t.Fatal("AuthURL with Dex down succeeded")
	}
	up.Store(true)
	// Within the backoff the failure is the answer; after it, discovery runs
	// again and succeeds.
	if err := u.Ready(context.Background()); err == nil {
		t.Fatal("Ready retried inside the backoff")
	}
	now = now.Add(discoveryBackoff)
	if err := u.Ready(context.Background()); err != nil {
		t.Fatalf("Ready after the backoff: %v", err)
	}
}

// redirectTo sends every request to base instead, keeping the path.
type redirectTo struct{ base string }

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.base)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(out)
}

func TestConfigValidate(t *testing.T) {
	good := Config{IssuerURL: "https://dex.example.com", ClientID: "relay", ClientSecret: "s",
		RedirectURL: redirect, Claims: claims()}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Config){
		"http issuer":         func(c *Config) { c.IssuerURL = "http://dex.example.com" },
		"no client":           func(c *Config) { c.ClientID = "" },
		"no secret":           func(c *Config) { c.ClientSecret = "" },
		"other redirect path": func(c *Config) { c.RedirectURL = "https://r.example.com/callback" },
		"http redirect":       func(c *Config) { c.RedirectURL = "http://r.example.com/dex/callback" },
		"no username prefix":  func(c *Config) { c.Claims.UsernamePrefix = "" },
		"no groups prefix":    func(c *Config) { c.Claims.GroupsPrefix = "" },
		"system prefix":       func(c *Config) { c.Claims.UsernamePrefix = "system:x" },
		"unverified email":    func(c *Config) { c.Claims.Username = "email" },
		"no groups claim":     func(c *Config) { c.Claims.Groups = "" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			c := good
			edit(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
