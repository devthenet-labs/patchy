// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// tokenKnob shapes the knobbed issuer's next token answer.
type tokenKnob struct {
	status    int  // non-zero answers the token request with this error status
	noIDToken bool // omit the id_token
	foreign   bool // sign the ID token with a key the JWKS does not hold
}

// knobbedIssuer is fakeIssuer with a token endpoint the test can make fail.
type knobbedIssuer struct {
	*fakeIssuer
	foreignKey *rsa.PrivateKey

	knobMu sync.Mutex
	knob   tokenKnob
}

func (k *knobbedIssuer) set(knob tokenKnob) {
	k.knobMu.Lock()
	defer k.knobMu.Unlock()
	k.knob = knob
}

func newKnobbedIssuer(t *testing.T) *knobbedIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k := &knobbedIssuer{fakeIssuer: &fakeIssuer{key: key, expIn: time.Hour}, foreignKey: foreign}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss := k.ts.URL
		writeAny(w, map[string]any{
			"issuer": iss, "authorization_endpoint": iss + "/auth", "token_endpoint": iss + "/token",
			"jwks_uri": iss + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeAny(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		k.knobMu.Lock()
		knob := k.knob
		k.knobMu.Unlock()
		if knob.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(knob.status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.tokenCalls++
		resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
		if !knob.noIDToken {
			if knob.foreign {
				saved := k.key
				k.key = k.foreignKey
				resp["id_token"] = k.mintLocked(t, k.claims, k.expIn)
				k.key = saved
			} else {
				resp["id_token"] = k.mintLocked(t, k.claims, k.expIn)
			}
		}
		if k.refreshToken != "" {
			resp["refresh_token"] = k.refreshToken
		}
		writeAny(w, resp)
	})
	k.ts = httptest.NewServer(mux)
	t.Cleanup(k.ts.Close)
	return k
}

func oidcConfig(issuer string) *Config {
	cfg := &Config{Mode: ModeOIDC, OIDC: &OIDCConfig{IssuerURL: issuer, ClientID: "patchy", ClientSecret: "s3cret"}}
	cfg.applyDefaults()
	return cfg
}

func knobbedOIDC(t *testing.T, k *knobbedIssuer, mut func(*Config)) (*oidcAuthenticator, *http.ServeMux) {
	t.Helper()
	cfg := oidcConfig(k.ts.URL)
	if mut != nil {
		mut(cfg)
	}
	a, err := newOIDC(t.Context(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newOIDC: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux
}

func authErrorOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName(CookieAuthError, false) && c.Value != "" {
			var msg string
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(c)
			if readJSONCookie(req, CookieAuthError, &msg, false) {
				return msg
			}
		}
	}
	return ""
}

// TestCallbackFailureReasons: each way the code exchange can go wrong lands
// the browser home with its own reason and no session.
func TestCallbackFailureReasons(t *testing.T) {
	cases := []struct {
		name string
		knob tokenKnob
		want string
	}{
		{"token endpoint refuses", tokenKnob{status: http.StatusBadRequest}, "code exchange failed"},
		{"no ID token", tokenKnob{noIDToken: true}, "provider returned no ID token"},
		{"ID token signed by another key", tokenKnob{foreign: true}, "ID token failed verification"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKnobbedIssuer(t)
			a, mux := knobbedOIDC(t, k, func(cfg *Config) { cfg.Insecure = true })
			loc, stateCookie := authorize(t, mux, "/x")
			k.setNext(userClaims(loc.Query().Get("nonce")), "")
			k.set(c.knob)
			rec := callback(t, mux, loc.Query().Get("state"), stateCookie)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
				t.Fatalf("callback = %d → %q", rec.Code, rec.Header().Get("Location"))
			}
			if got := authErrorOf(rec); got != c.want {
				t.Errorf("auth error = %q, want %q", got, c.want)
			}
			if id, _ := a.Identify(httptest.NewRecorder(), sessionRequest(t, rec, "/")); id != nil {
				t.Errorf("failure produced a session: %+v", id)
			}
		})
	}
}

// TestCallbackStateExpired: a round trip older than the state lifetime is
// refused even with a valid state and cookie.
func TestCallbackStateExpired(t *testing.T) {
	k := newKnobbedIssuer(t)
	a, mux := knobbedOIDC(t, k, func(cfg *Config) { cfg.Insecure = true })
	loc, stateCookie := authorize(t, mux, "/")
	k.setNext(userClaims(loc.Query().Get("nonce")), "")
	a.now = func() time.Time { return time.Now().Add(stateTTL + time.Minute) }
	rec := callback(t, mux, loc.Query().Get("state"), stateCookie)
	if got := authErrorOf(rec); got != "sign-in took too long, try again" {
		t.Errorf("auth error = %q", got)
	}
}

// TestAuthorizeCarriesExtraParams: configured authorization parameters reach
// the provider's URL beside PKCE and the nonce.
func TestAuthorizeCarriesExtraParams(t *testing.T) {
	k := newKnobbedIssuer(t)
	_, mux := knobbedOIDC(t, k, func(cfg *Config) {
		cfg.Insecure = true
		cfg.OIDC.AuthURLParams = map[string]string{"prompt": "select_account", "hd": "acme.test"}
	})
	loc, _ := authorize(t, mux, "/")
	q := loc.Query()
	if q.Get("prompt") != "select_account" || q.Get("hd") != "acme.test" {
		t.Errorf("authorize URL = %s, want the extra params", loc)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Errorf("authorize URL = %s, want PKCE, nonce and state", loc)
	}
}

func TestRedirectURL(t *testing.T) {
	k := newKnobbedIssuer(t)
	a, _ := knobbedOIDC(t, k, nil)
	plain := httptest.NewRequest(http.MethodGet, "http://status.local/oauth2/authorize", nil)
	if got := a.redirectURL(plain); got != "http://status.local/oauth2/callback" {
		t.Errorf("plain = %q", got)
	}
	overTLS := httptest.NewRequest(http.MethodGet, "https://status.local/", nil)
	overTLS.TLS = &tls.ConnectionState{}
	if got := a.redirectURL(overTLS); got != "https://status.local/oauth2/callback" {
		t.Errorf("tls = %q", got)
	}
	forwarded := httptest.NewRequest(http.MethodGet, "http://10.0.0.1/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	forwarded.Header.Set("X-Forwarded-Host", "status.acme.test")
	if got := a.redirectURL(forwarded); got != "https://status.acme.test/oauth2/callback" {
		t.Errorf("forwarded = %q", got)
	}
	a.oc.RedirectURL = "https://fixed.acme.test/oauth2/callback"
	if got := a.redirectURL(forwarded); got != "https://fixed.acme.test/oauth2/callback" {
		t.Errorf("override = %q", got)
	}
}

// expiredSession writes a session whose ID token has expired and which holds
// refreshToken, returning the request carrying it.
func expiredSession(t *testing.T, a *oidcAuthenticator, k *knobbedIssuer, refreshToken string,
	claims map[string]any) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := a.writeSession(rec, session{
		IDToken: k.mint(t, claims, -time.Hour), RefreshToken: refreshToken, Start: a.now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return sessionRequest(t, rec, "/api/findings")
}

// TestRefreshFailuresEndTheSession: a refresh that fails, returns no ID
// token or returns one that does not verify clears the session, and a
// refresh without a new refresh token keeps the old one.
func TestRefreshFailuresEndTheSession(t *testing.T) {
	for _, c := range []struct {
		name string
		knob tokenKnob
	}{
		{"refresh refused", tokenKnob{status: http.StatusBadRequest}},
		{"no ID token", tokenKnob{noIDToken: true}},
		{"foreign ID token", tokenKnob{foreign: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newKnobbedIssuer(t)
			a, _ := knobbedOIDC(t, k, nil)
			req := expiredSession(t, a, k, "refresh-1", userClaims(""))
			k.setNext(userClaims(""), "")
			k.set(c.knob)
			out := httptest.NewRecorder()
			id, err := a.Identify(out, req)
			if id != nil || err != nil {
				t.Fatalf("Identify = %+v, %v; want no session", id, err)
			}
			if readChunked(sessionRequest(t, out, "/"), a.secure()) != "" {
				t.Error("failed refresh left a session cookie")
			}
		})
	}

	t.Run("refresh keeps the old refresh token", func(t *testing.T) {
		k := newKnobbedIssuer(t)
		a, _ := knobbedOIDC(t, k, nil)
		req := expiredSession(t, a, k, "refresh-1", userClaims(""))
		k.setNext(userClaims(""), "") // no new refresh token
		out := httptest.NewRecorder()
		id, err := a.Identify(out, req)
		if err != nil || id == nil {
			t.Fatalf("Identify = %+v, %v", id, err)
		}
		s, ok := a.readSession(sessionRequest(t, out, "/"))
		if !ok || s.RefreshToken != "refresh-1" {
			t.Errorf("rewritten session = %+v (ok %v), want refresh-1 kept", s, ok)
		}
	})
}

// TestIdentifyRejectsUnusableSessions: a session whose token fails for a
// reason other than expiry, whose claims lack the username, or whose cookie
// is not a sealed session is no session.
func TestIdentifyRejectsUnusableSessions(t *testing.T) {
	k := newKnobbedIssuer(t)
	a, _ := knobbedOIDC(t, k, nil)

	write := func(s session) *http.Request {
		rec := httptest.NewRecorder()
		if err := a.writeSession(rec, s); err != nil {
			t.Fatal(err)
		}
		return sessionRequest(t, rec, "/")
	}
	cases := map[string]*http.Request{
		"expired without refresh": write(session{IDToken: k.mint(t, userClaims(""), -time.Hour), Start: a.now()}),
		"not a JWT":               write(session{IDToken: "not.a.jwt", Start: a.now()}),
		"no username claim":       write(session{IDToken: k.mint(t, map[string]any{"name": "x"}, time.Hour), Start: a.now()}),
	}
	garbage := httptest.NewRecorder()
	if err := writeChunked(garbage, "not-sealed", time.Hour, a.secure()); err != nil {
		t.Fatal(err)
	}
	cases["unsealable cookie"] = sessionRequest(t, garbage, "/")
	for name, req := range cases {
		out := httptest.NewRecorder()
		if id, err := a.Identify(out, req); id != nil || err != nil {
			t.Errorf("%s: Identify = %+v, %v; want no session", name, id, err)
		}
	}
}

// TestWriteSessionPastLifetimeClears: a session already past its absolute
// lifetime is never written; any session cookie is cleared instead.
func TestWriteSessionPastLifetimeClears(t *testing.T) {
	k := newKnobbedIssuer(t)
	a, _ := knobbedOIDC(t, k, nil)
	rec := httptest.NewRecorder()
	err := a.writeSession(rec, session{IDToken: "x", Start: time.Now().Add(-a.cfg.SessionDuration.Duration - time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if readChunked(sessionRequest(t, rec, "/"), a.secure()) != "" {
		t.Error("an over-lifetime session was written")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" && c.MaxAge > 0 {
			t.Errorf("cookie %s set with a value", c.Name)
		}
	}
}

func TestNewOIDCFailures(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if _, err := newOIDC(context.Background(), oidcConfig(deadURL), nil); err == nil ||
		!strings.Contains(err.Error(), "oidc discovery") {
		t.Errorf("newOIDC with an unreachable issuer = %v", err)
	}

	cfg := oidcConfig("https://issuer.invalid")
	cfg.OIDC.ClientSecret = ""
	cfg.OIDC.ClientSecretFile = filepath.Join(t.TempDir(), "absent")
	if _, err := newOIDC(context.Background(), cfg, nil); err == nil ||
		!strings.Contains(err.Error(), "clientSecretFile") {
		t.Errorf("newOIDC with a missing secret file = %v", err)
	}

	empty := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.OIDC.ClientSecretFile = empty
	if _, err := cfg.OIDC.clientSecret(); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("clientSecret of an empty file = %v", err)
	}
}

// TestNonOIDCModesRegisterNothing: the modes without a sign-in surface mount
// no routes, so the sign-in paths are not found.
func TestNonOIDCModesRegisterNothing(t *testing.T) {
	cfgs := map[string]*Config{
		"unconfigured": nil,
		"none":         {Mode: ModeNone},
		"anonymous":    {Mode: ModeAnonymous, Anonymous: &AnonymousConfig{Username: "viewer"}},
	}
	for name, cfg := range cfgs {
		a, err := New(context.Background(), cfg, nil)
		if err != nil {
			t.Fatalf("%s: New = %v", name, err)
		}
		mux := http.NewServeMux()
		a.Register(mux)
		for _, path := range []string{"/oauth2/authorize", "/oauth2/callback"} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: GET %s = %d, want 404", name, path, rec.Code)
			}
		}
	}
	if _, err := New(context.Background(), &Config{Mode: "ldap"}, nil); err == nil {
		t.Error("New accepted an unknown mode")
	}
}

func TestDurationUnmarshalErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadConfig(write("bad-duration.yaml", "mode: none\nsessionDuration: forever\n")); err == nil ||
		!strings.Contains(err.Error(), `duration "forever"`) {
		t.Errorf("bad duration = %v", err)
	}
	if _, err := LoadConfig(write("non-scalar.yaml", "mode: none\nsessionDuration: [1h]\n")); err == nil {
		t.Error("a non-scalar duration was accepted")
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("LoadConfig of a directory succeeded")
	}
	cfg, err := LoadConfig(write("ok.yaml", "mode: none\nsessionDuration: 30m\n"))
	if err != nil || cfg.SessionDuration.Duration != 30*time.Minute {
		t.Errorf("LoadConfig = %+v, %v", cfg, err)
	}
}

func TestReadJSONCookieRejectsGarbage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieProvider, Value: "!!!not-base64"})
	var v providerState
	if readJSONCookie(req, CookieProvider, &v, false) {
		t.Error("undecodable cookie read as present")
	}
	if err := setJSONCookie(httptest.NewRecorder(), CookieProvider, func() {}, time.Minute, false); err == nil {
		t.Error("setJSONCookie accepted an unmarshalable value")
	}
}
