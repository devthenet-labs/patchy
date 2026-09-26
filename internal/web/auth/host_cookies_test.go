// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// assertHostCookies includes expiry cookies: browsers reject a __Host-
// deletion without Secure, Path=/ and no Domain just as they reject a write.
func assertHostCookies(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("no cookies to check")
	}
	for _, c := range rec.Result().Cookies() {
		if !strings.HasPrefix(c.Name, "__Host-patchy-") || !c.Secure || c.Path != "/" || c.Domain != "" {
			t.Errorf("cookie is not host-bound: %s", c.Name)
		}
		wantHTTPOnly := c.Name != "__Host-patchy-auth-provider" &&
			c.Name != "__Host-patchy-auth-error" && c.Name != "__Host-patchy-auth-logout"
		if c.MaxAge >= 0 && c.HttpOnly != wantHTTPOnly {
			t.Errorf("cookie %s HttpOnly = %v, want %v", c.Name, c.HttpOnly, wantHTTPOnly)
		}
	}
}

func TestCookieNamespacesProperty(t *testing.T) {
	property := func(size uint16, secure bool) bool {
		value := strings.Repeat("x", 1+int(size)%(chunkSize*maxChunks))
		rec := httptest.NewRecorder()
		if err := writeChunked(rec, value, time.Hour, secure); err != nil {
			return false
		}
		for _, c := range rec.Result().Cookies() {
			prefix := "__Host-patchy-auth"
			if !secure {
				prefix = "patchy-dev-auth"
			}
			if !strings.HasPrefix(c.Name, prefix) || c.Secure != secure ||
				c.Domain != "" || c.Path != "/" || !c.HttpOnly {
				return false
			}
		}
		req := carry(t, rec)
		return readChunked(req, secure) == value && readChunked(req, !secure) == ""
	}
	if err := quick.Check(property, &quick.Config{Rand: rand.New(rand.NewSource(42)), MaxCount: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCHostCookieLifecycle(t *testing.T) {
	fi := newFakeIssuer(t)
	a, mux := newTestOIDC(t, fi)
	a.cfg.Insecure = false
	loc, state := authorize(t, mux, "/")
	if state.Name != "__Host-patchy-oauth2-state" || state.Path != "/" || !state.Secure || state.Domain != "" {
		t.Errorf("OAuth state is not host-bound: name=%s path=%s secure=%v domain=%s",
			state.Name, state.Path, state.Secure, state.Domain)
	}
	fi.setNext(userClaims(loc.Query().Get("nonce")), "refresh")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	callbackURL, err := url.Parse("https://status.example.test/oauth2/callback")
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(callbackURL, []*http.Cookie{state})
	callbackRec := callback(t, mux, loc.Query().Get("state"), state)
	assertHostCookies(t, callbackRec)
	jar.SetCookies(callbackURL, callbackRec.Result().Cookies())
	for _, c := range jar.Cookies(callbackURL) {
		if c.Name == state.Name {
			t.Error("callback failed to remove the OAuth state cookie from its original path")
		}
	}
	id, err := a.Identify(httptest.NewRecorder(), sessionRequest(t, callbackRec, "/"))
	if err != nil || id == nil {
		t.Fatalf("host session did not authenticate: %v", err)
	}
	logout := httptest.NewRecorder()
	mux.ServeHTTP(logout, httptest.NewRequest(http.MethodPost, "/logout", nil))
	assertHostCookies(t, logout)
	jar.SetCookies(callbackURL, logout.Result().Cookies())
	for _, c := range jar.Cookies(callbackURL) {
		if c.Name == "__Host-patchy-auth" {
			t.Error("logout left the session cookie in the jar")
		}
	}
	failure := callback(t, mux, "invalid-state", nil)
	assertHostCookies(t, failure)
	// Exercise both live chunks and leftover-chunk expiry with literal names.
	chunks := httptest.NewRecorder()
	if err := writeChunked(chunks, strings.Repeat("x", chunkSize+1), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	assertHostCookies(t, chunks)
	if got := chunks.Result().Cookies()[1].Name; got != "__Host-patchy-auth-1" {
		t.Errorf("second chunk = %s", got)
	}
}

func TestOIDCLegacyInjectionCannotOverrideProtectedSession(t *testing.T) {
	fi := newFakeIssuer(t)
	a, _ := newTestOIDC(t, fi)
	sessionCookies := httptest.NewRecorder()
	err := a.writeSession(sessionCookies, session{IDToken: fi.mint(t, userClaims(""), time.Hour), Start: a.now()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://status.example.test/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	// Send legacy and dev cookies first, as a sibling's more-specific cookie
	// might arrive. Neither order nor forwarded headers may select them.
	req.AddCookie(&http.Cookie{Name: "patchy-auth", Value: "injected"})
	req.AddCookie(&http.Cookie{Name: "patchy-dev-auth", Value: "injected"})
	for _, c := range sessionRequest(t, sessionCookies, "/").Cookies() {
		req.AddCookie(c)
	}
	if id, err := a.Identify(httptest.NewRecorder(), req); err != nil || id == nil || id.Username != "dev@acme.test" {
		t.Fatalf("injected cookies displaced the protected session: id=%v err=%v", id, err)
	}
}

func TestLegacyCookieCleanupBounded(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/oauth2/callback", nil)
	names := [maxChunks + 4]string{
		"patchy-oauth2-state", "patchy-auth-provider", "patchy-auth-error", "patchy-auth-logout",
	}
	for i := range maxChunks {
		names[i+4] = strings.TrimPrefix(chunkName(i), "__Host-")
	}
	for _, name := range names {
		req.AddCookie(&http.Cookie{Name: name, Value: "injected"})
		req.AddCookie(&http.Cookie{Name: name, Value: "duplicate"})
	}
	req.AddCookie(&http.Cookie{Name: "patchy-auth-999", Value: "unrecognised"})
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: "protected"})
	for range 2 {
		rec := httptest.NewRecorder()
		clearLegacyCookies(rec, req, true)
		if got := len(rec.Result().Cookies()); got != maxChunks+4 {
			t.Fatalf("cleanup emitted %d cookies, want fixed bound %d", got, maxChunks+4)
		}
		for _, c := range rec.Result().Cookies() {
			if strings.HasPrefix(c.Name, "__Host-") || c.MaxAge >= 0 || c.Value != "" || c.Domain != "" {
				t.Errorf("cleanup changed a protected cookie or set a live cookie: %s", c.Name)
			}
		}
	}
}

func TestOIDCRejectsLegacyAndDevSessions(t *testing.T) {
	fi := newFakeIssuer(t)
	a, _ := newTestOIDC(t, fi)
	a.cfg.Insecure = false
	// Use a valid sealed session, not garbage that would fail decryption even
	// if the controller incorrectly read the attacker-injectable cookie name.
	blob, err := seal(a.key, session{IDToken: fi.mint(t, userClaims(""), time.Hour), Start: a.now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"patchy-auth", "patchy-dev-auth"} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: name, Value: blob})
			if id, err := a.Identify(httptest.NewRecorder(), req); err != nil || id != nil {
				t.Fatalf("unprotected cookie authenticated: id=%v err=%v", id, err)
			}
		})
	}
}

func TestOIDCRejectsLegacyAndDevOAuthState(t *testing.T) {
	fi := newFakeIssuer(t)
	a, mux := newTestOIDC(t, fi)
	a.cfg.Insecure = false
	for _, name := range []string{"patchy-oauth2-state", "patchy-dev-oauth2-state"} {
		t.Run(name, func(t *testing.T) {
			loc, state := authorize(t, mux, "/")
			fi.setNext(userClaims(loc.Query().Get("nonce")), "")
			state.Name = name
			rec := callback(t, mux, loc.Query().Get("state"), state)
			if id, _ := a.Identify(httptest.NewRecorder(), sessionRequest(t, rec, "/")); id != nil {
				t.Fatal("unprotected OAuth state accepted")
			}
		})
	}
	if fi.tokenCalls != 0 {
		t.Errorf("exchanged %d codes using unprotected OAuth state", fi.tokenCalls)
	}
}

func TestOIDCInsecureDevCookieNamespace(t *testing.T) {
	fi := newFakeIssuer(t)
	a, mux := newTestOIDC(t, fi)
	a.cfg.Insecure = true
	loc, state := authorize(t, mux, "/")
	if state.Name != "patchy-dev-oauth2-state" || state.Secure || state.Path != "/" {
		t.Errorf("dev state attributes: name=%s secure=%v path=%s", state.Name, state.Secure, state.Path)
	}
	fi.setNext(userClaims(loc.Query().Get("nonce")), "")
	rec := callback(t, mux, loc.Query().Get("state"), state)
	for _, c := range rec.Result().Cookies() {
		if !strings.HasPrefix(c.Name, "patchy-dev-") || c.Secure || c.Domain != "" || c.Path != "/" {
			t.Errorf("invalid dev cookie: %s", c.Name)
		}
	}
	if id, err := a.Identify(httptest.NewRecorder(), sessionRequest(t, rec, "/")); err != nil || id == nil {
		t.Fatalf("dev flow failed: id=%v err=%v", id, err)
	}
}

func TestOIDCLegacyCookiesDoNotSuppressProviderUpdate(t *testing.T) {
	fi := newFakeIssuer(t)
	a, _ := newTestOIDC(t, fi)
	a.cfg.Insecure = false
	old := httptest.NewRecorder()
	if err := setJSONCookie(old, "patchy-auth-provider", providerState{Provider: "oidc"}, 0, true); err != nil {
		t.Fatal(err)
	}
	req := carry(t, old)
	req.AddCookie(&http.Cookie{Name: "patchy-auth", Value: "old-session"})
	req.AddCookie(&http.Cookie{Name: "patchy-auth-1", Value: "old-chunk"})
	req.AddCookie(&http.Cookie{Name: "patchy-oauth2-state", Value: "old-state"})
	out := httptest.NewRecorder()
	_, _ = a.Identify(out, req)
	cleared := map[string]bool{}
	provider := false
	for _, c := range out.Result().Cookies() {
		if c.Name == "__Host-patchy-auth-provider" && c.Value != "" {
			provider = true
		}
		if c.MaxAge < 0 && c.Domain == "" {
			cleared[c.Name+":"+c.Path] = true
		}
	}
	if !provider {
		t.Error("legacy provider cookie suppressed the protected provider cookie")
	}
	for _, key := range []string{
		"patchy-auth:/", "patchy-auth-1:/", "patchy-auth-provider:/", "patchy-oauth2-state:/oauth2/",
	} {
		if !cleared[key] {
			t.Errorf("legacy host-only cookie not expired: %s", key)
		}
	}
}
