// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// signedIn returns a browser holding a relay session for alice, through
// the stub upstream.
func (r *relay) signedIn() *http.Client {
	r.t.Helper()
	b := r.browser()
	resp := get(r.t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	dexURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || dexURL.Host != "dex.example.com" {
		r.t.Fatalf("authorize sent the browser to %q", resp.Header.Get("Location"))
	}
	state := dexURL.Query().Get("state")
	codeFrom(r.t, get(r.t, b, issuer+"/dex/callback?"+url.Values{"state": {state}, "code": {"ok"}}.Encode()))
	return b
}

// TestAuthorizeNeverRedirectsAnError: every refused authorize request is a
// page on the relay host, with no Location and no cookie.
func TestAuthorizeNeverRedirectsAnError(t *testing.T) {
	type tc struct {
		name   string
		url    string
		setup  func(*relay)
		status int
	}
	q := func(edit func(url.Values)) string { return authorizeURL(edit) }
	tests := []tc{
		{"no response_type", q(func(v url.Values) { v.Del("response_type") }), nil, 400},
		{"token response_type", q(func(v url.Values) { v.Set("response_type", "token") }), nil, 400},
		{"unknown client", q(func(v url.Values) { v.Set("client_id", "patchy-preview-s9") }), nil, 400},
		{"client past the slot count", q(func(v url.Values) { v.Set("client_id", "patchy-preview-s2") }), nil, 400},
		{"foreign redirect", q(func(v url.Values) { v.Set("redirect_uri", "https://evil.example.com/cb") }), nil, 400},
		{"lookalike redirect", q(func(v url.Values) {
			v.Set("redirect_uri", "https://demo-7.preview.example.com.evil/oauth2/idpresponse")
		}), nil, 400},
		{"redirect with port", q(func(v url.Values) {
			v.Set("redirect_uri", "https://demo-7.preview.example.com:444/oauth2/idpresponse")
		}), nil, 400},
		{"no openid scope", q(func(v url.Values) { v.Set("scope", "profile") }), nil, 400},
		{"no state", q(func(v url.Values) { v.Del("state") }), nil, 400},
		{"plain PKCE", q(func(v url.Values) {
			v.Set("code_challenge", strings.Repeat("a", 43))
			v.Set("code_challenge_method", "plain")
		}), nil, 400},
		{"repeated state", authorizeURL(nil) + "&state=two", nil, 400},
		{"malformed query", authorizeURL(nil) + "&%zz", nil, 400},
		{"no preview", q(func(v url.Values) {
			v.Set("redirect_uri", "https://demo-8."+suffix+"/oauth2/idpresponse")
		}), nil, 404},
		{"placeholder host", q(func(v url.Values) {
			v.Set("redirect_uri", "https://placeholder."+suffix+"/oauth2/idpresponse")
		}), nil, 404},
		{"preview in another slot", q(func(v url.Values) { v.Set("client_id", previewauth.ClientID(0)) }), nil, 404},
		{"dead preview", authorizeURL(nil), func(r *relay) {
			r.previews.set(previewauth.View{UID: "uid-7", Label: label, Project: "demo", Slot: 1})
		}, 404},
		{"lookup fails", authorizeURL(nil), func(r *relay) { r.previews.fail(errors.New("down")) }, 503},
		{"Dex unreachable", authorizeURL(nil), func(r *relay) { r.stub.urlE = errors.New("down") }, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRelay(t, nil)
			if tt.setup != nil {
				tt.setup(r)
			}
			resp := get(t, r.browser(), tt.url)
			page := body(t, resp)
			if resp.StatusCode != tt.status || resp.Header.Get("Location") != "" || len(resp.Cookies()) != 0 ||
				!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("status %d (want %d), location %q, cookies %v", resp.StatusCode, tt.status,
					resp.Header.Get("Location"), resp.Cookies())
			}
			if strings.Contains(page, "demo-7") || strings.Contains(page, "evil") {
				t.Fatalf("the page echoes the request: %s", page)
			}
		})
	}
}

// TestSignedInAuthorizeRefusals: with a relay session, a refused or
// unanswerable access review is a page too.
func TestSignedInAuthorizeRefusals(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*relay)
		status int
	}{
		{"denied", func(r *relay) { r.viewers.revoke("github:alice") }, 403},
		{"no project", func(r *relay) {
			r.previews.set(previewauth.View{UID: "uid-7", Label: label, Slot: 1, Live: true})
		}, 403},
		{"review fails", func(r *relay) { r.viewers.setErr(errors.New("down")) }, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRelay(t, nil)
			b := r.signedIn()
			tt.setup(r)
			resp := get(t, b, authorizeURL(nil))
			_ = body(t, resp)
			if resp.StatusCode != tt.status || resp.Header.Get("Location") != "" {
				t.Fatalf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

// TestSessionCookie: the relay session is a browser-session cookie with the
// __Host- prefix's attributes, and an expired one sends the viewer to Dex.
func TestSessionCookie(t *testing.T) {
	r := newRelay(t, nil)
	b := r.browser()
	resp := get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	login := findCookie(resp, "__Host-patchy-pa-login-")
	if !hostOnly(login) || login.MaxAge != 300 {
		t.Fatalf("login cookie %+v", login)
	}
	state := mustQuery(t, resp.Header.Get("Location")).Get("state")
	resp = get(t, b, issuer+"/dex/callback?"+url.Values{"state": {state}, "code": {"ok"}}.Encode())
	_ = resp.Body.Close()
	if cleared := findCookie(resp, login.Name); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("login cookie not cleared: %+v", cleared)
	}
	sess := findCookie(resp, previewauth.RelaySessionCookie)
	if !hostOnly(sess) || sess.MaxAge != 0 || !sess.Expires.IsZero() {
		t.Fatalf("session cookie %+v", sess)
	}
	// After the session's absolute lifetime, authorize goes back to Dex.
	r.clock.advance(13 * time.Hour)
	resp = get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "https://dex.example.com/") {
		t.Fatalf("expired session: location %q", resp.Header.Get("Location"))
	}
}

// findCookie is the response's cookie called name, or, for a name ending
// in "-", the first whose name starts with it.
func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name || strings.HasSuffix(name, "-") && strings.HasPrefix(c.Name, name) {
			return c
		}
	}
	return nil
}

// hostOnly reports the attributes a __Host- cookie must carry.
func hostOnly(c *http.Cookie) bool {
	return c != nil && c.Secure && c.HttpOnly && c.Path == "/" && c.Domain == "" &&
		c.SameSite == http.SameSiteLaxMode
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestCallbackRefusals(t *testing.T) {
	r := newRelay(t, nil)
	b := r.browser()
	resp := get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	state := mustQuery(t, resp.Header.Get("Location")).Get("state")
	tests := []struct {
		name   string
		query  string
		setup  func()
		status int
	}{
		{"no state", "code=ok", nil, 400},
		{"forged state", url.Values{"state": {"pa1.l.1.AAAA"}, "code": {"ok"}}.Encode(), nil, 400},
		{"repeated code", url.Values{"state": {state}, "code": {"ok", "ok"}}.Encode(), nil, 400},
		{"exchange fails", url.Values{"state": {state}, "code": {"bad"}}.Encode(), nil, 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := get(t, b, issuer+"/dex/callback?"+tt.query)
			_ = body(t, resp)
			if resp.StatusCode != tt.status || resp.Header.Get("Location") != "" {
				t.Fatalf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
	// Too many groups for a session cookie.
	r = newRelay(t, nil)
	groups := make([]string, 200)
	for i := range groups {
		groups[i] = fmt.Sprintf("github:org:team-with-a-long-name-%03d", i)
	}
	r.stub.id = previewauth.Identity{Username: "github:alice", Groups: groups}
	b = r.browser()
	resp = get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	state = mustQuery(t, resp.Header.Get("Location")).Get("state")
	resp = get(t, b, issuer+"/dex/callback?"+url.Values{"state": {state}, "code": {"ok"}}.Encode())
	if page := body(t, resp); resp.StatusCode != 400 || !strings.Contains(page, "more groups") {
		t.Fatalf("too many groups: %d %s", resp.StatusCode, page)
	}
	// A Preview replaced during the sign-in is not the one it was for.
	r = newRelay(t, nil)
	b = r.browser()
	resp = get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	state = mustQuery(t, resp.Header.Get("Location")).Get("state")
	r.previews.set(previewauth.View{UID: "uid-other", Label: label, Project: "demo", Slot: 1, Live: true})
	resp = get(t, b, issuer+"/dex/callback?"+url.Values{"state": {state}, "code": {"ok"}}.Encode())
	if _ = body(t, resp); resp.StatusCode != 404 || resp.Header.Get("Location") != "" {
		t.Fatalf("replaced preview: %d", resp.StatusCode)
	}
}

// codeFor signs alice in and returns a fresh code for the PKCE challenge.
func (r *relay) codeFor(challenge string) string {
	r.t.Helper()
	b := r.signedIn()
	code, _ := codeFrom(r.t, get(r.t, b, authorizeURL(func(q url.Values) {
		if challenge != "" {
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "S256")
		}
	})))
	return code
}

// TestTokenErrorMatrix is the token endpoint's RFC 6749 §5.2 matrix.
func TestTokenErrorMatrix(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	type tc struct {
		name   string
		how    string
		slot   int
		secret func(*relay) string
		form   func(r *relay) url.Values
		setup  func(*relay)
		status int
		code   string
	}
	good := func(r *relay) string { return r.ring.ClientSecret(1) }
	codeForm := func(challenge, v string) func(*relay) url.Values {
		return func(r *relay) url.Values {
			f := url.Values{"grant_type": {"authorization_code"}, "code": {r.codeFor(challenge)},
				"redirect_uri": {callback}}
			if v != "" {
				f.Set("code_verifier", v)
			}
			return f
		}
	}
	tests := []tc{
		{"ok basic", "basic", 1, good, codeForm("", ""), nil, 200, ""},
		{"ok post", "post", 1, good, codeForm("", ""), nil, 200, ""},
		{"both methods", "both", 1, good, codeForm("", ""), nil, 401, "invalid_client"},
		{"no method", "none", 1, good, codeForm("", ""), nil, 401, "invalid_client"},
		{"wrong secret", "post", 1, func(r *relay) string { return strings.Repeat("0", 64) }, codeForm("", ""),
			nil, 401, "invalid_client"},
		{"another slot's client", "basic", 0, func(r *relay) string { return r.ring.ClientSecret(0) },
			codeForm("", ""), nil, 400, "invalid_grant"},
		{"expired code", "basic", 1, good, codeForm("", ""), func(r *relay) { r.clock.advance(2 * time.Minute) },
			400, "invalid_grant"},
		{"redirect mismatch", "basic", 1, good, func(r *relay) url.Values {
			f := codeForm("", "")(r)
			f.Set("redirect_uri", "https://demo-8."+suffix+"/oauth2/idpresponse")
			return f
		}, nil, 400, "invalid_grant"},
		{"no redirect_uri", "basic", 1, good, func(r *relay) url.Values {
			f := codeForm("", "")(r)
			f.Del("redirect_uri")
			return f
		}, nil, 400, "invalid_request"},
		{"pkce ok", "basic", 1, good, codeForm(previewauth.S256(verifier), verifier), nil, 200, ""},
		{"pkce missing verifier", "basic", 1, good, codeForm(previewauth.S256(verifier), ""), nil, 400,
			"invalid_grant"},
		{"pkce wrong verifier", "basic", 1, good, codeForm(previewauth.S256(verifier), strings.Repeat("w", 43)),
			nil, 400, "invalid_grant"},
		{"verifier without challenge", "basic", 1, good, codeForm("", verifier), nil, 400, "invalid_grant"},
		{"forged code", "basic", 1, good, func(*relay) url.Values {
			return url.Values{"grant_type": {"authorization_code"}, "code": {"pa1.c.1.AAAA"},
				"redirect_uri": {callback}}
		}, nil, 400, "invalid_grant"},
		{"unsupported grant", "basic", 1, good, func(*relay) url.Values {
			return url.Values{"grant_type": {"password"}}
		}, nil, 400, "unsupported_grant_type"},
		{"repeated parameter", "basic", 1, good, func(r *relay) url.Values {
			f := codeForm("", "")(r)
			f.Add("redirect_uri", callback)
			return f
		}, nil, 400, "invalid_request"},
		{"ledger down", "basic", 1, good, codeForm("", ""), func(r *relay) { r.ledger.err = errors.New("down") },
			503, "temporarily_unavailable"},
		{"lookup down", "basic", 1, good, codeForm("", ""), func(r *relay) { r.previews.fail(errors.New("down")) },
			503, "temporarily_unavailable"},
		{"preview gone", "basic", 1, good, codeForm("", ""), func(r *relay) {
			r.previews.set(previewauth.View{UID: "uid-7", Label: label, Project: "demo", Slot: 1})
		}, 400, "invalid_grant"},
		{"viewer revoked", "basic", 1, good, codeForm("", ""), func(r *relay) { r.viewers.revoke("github:alice") },
			400, "invalid_grant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRelay(t, nil)
			form := tt.form(r)
			if tt.setup != nil {
				tt.setup(r)
			}
			resp := r.tokenRequest(tt.how, tt.slot, tt.secret(r), form)
			out := decodeToken(t, resp)
			if resp.StatusCode != tt.status || out.Error != tt.code ||
				resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d error %q, want %d %q", resp.StatusCode, out.Error, tt.status, tt.code)
			}
			if tt.code == "invalid_client" && tt.how == "basic" &&
				resp.Header.Get("WWW-Authenticate") != `Basic realm="preview-auth"` {
				t.Errorf("WWW-Authenticate %q", resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

// TestTokenBodyRules: the form comes from the body only.
func TestTokenBodyRules(t *testing.T) {
	r := newRelay(t, nil)
	post := func(target, ct, b string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(b))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := r.backchannel().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	creds := url.Values{"client_id": {previewauth.ClientID(1)}, "client_secret": {r.ring.ClientSecret(1)},
		"grant_type": {"refresh_token"}, "refresh_token": {"x"}}.Encode()
	// Credentials in the query string are not read.
	resp := post(issuer+"/token?"+creds, "application/x-www-form-urlencoded", "")
	if out := decodeToken(t, resp); resp.StatusCode != 401 || out.Error != "invalid_client" {
		t.Fatalf("query credentials: %d %+v", resp.StatusCode, out)
	}
	resp = post(issuer+"/token", "application/json", "{}")
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_request" {
		t.Fatalf("json body: %d %+v", resp.StatusCode, out)
	}
	resp = post(issuer+"/token", "application/x-www-form-urlencoded", "a="+strings.Repeat("x", MaxBodyBytes))
	if out := decodeToken(t, resp); resp.StatusCode != 413 || out.Error != "invalid_request" {
		t.Fatalf("large body: %d %+v", resp.StatusCode, out)
	}
}

// TestUserinfoTransports: header on GET or POST, form on POST, never the
// query string, never both.
func TestUserinfoTransports(t *testing.T) {
	r := newRelay(t, nil)
	resp := r.tokenRequest("basic", 1, r.ring.ClientSecret(1), url.Values{"grant_type": {"authorization_code"},
		"code": {r.codeFor("")}, "redirect_uri": {callback}})
	tok := decodeToken(t, resp)
	do := func(method, target, authz, form string) (*http.Response, string) {
		var rd io.Reader
		if form != "" {
			rd = strings.NewReader(form)
		}
		req, _ := http.NewRequest(method, target, rd)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		if form != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := r.backchannel().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, body(t, resp)
	}
	at := url.Values{"access_token": {tok.AccessToken}}.Encode()
	tests := []struct {
		name, method, target, authz, form string
		status                            int
	}{
		{"GET header", "GET", issuer + "/userinfo", "Bearer " + tok.AccessToken, "", 200},
		{"POST header", "POST", issuer + "/userinfo", "Bearer " + tok.AccessToken, "", 200},
		{"POST form", "POST", issuer + "/userinfo", "", at, 200},
		{"GET query", "GET", issuer + "/userinfo?" + at, "", "", 401},
		{"POST query", "POST", issuer + "/userinfo?" + at, "", "", 401},
		{"both", "POST", issuer + "/userinfo", "Bearer " + tok.AccessToken, at, 400},
		{"basic scheme", "GET", issuer + "/userinfo", "Basic " + tok.AccessToken, "", 401},
		{"a refresh token", "GET", issuer + "/userinfo", "Bearer " + tok.RefreshToken, "", 401},
		{"a code", "GET", issuer + "/userinfo", "Bearer " + r.codeFor(""), "", 401},
		{"PUT", "PUT", issuer + "/userinfo", "Bearer " + tok.AccessToken, "", 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, got := do(tt.method, tt.target, tt.authz, tt.form)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d: %s", resp.StatusCode, got)
			}
			if tt.status == 200 {
				var out map[string]string
				if err := json.Unmarshal([]byte(got), &out); err != nil || len(out) != 1 || out["sub"] == "" {
					t.Fatalf("userinfo %s", got)
				}
			}
			if tt.status == 401 && resp.Header.Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
				t.Errorf("WWW-Authenticate %q", resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

// TestEnvelope: every response's headers, methods and the other routes.
func TestEnvelope(t *testing.T) {
	r := newRelay(t, nil)
	tests := []struct {
		method, path string
		status       int
		cache        string
	}{
		{"GET", "/", 200, "no-store"},
		{"GET", "/.well-known/openid-configuration", 200, "public, max-age=300"},
		{"GET", "/jwks", 200, "public, max-age=300"},
		{"GET", "/nothing", 404, "no-store"},
		{"POST", "/jwks", 405, "no-store"},
		{"GET", "/token", 405, "no-store"},
		{"DELETE", "/", 405, "no-store"},
		{"HEAD", "/authorize", 405, "no-store"},
		{"POST", "/logout", 403, "no-store"},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, issuer+tt.path, nil)
			r.handler.ServeHTTP(rec, req)
			h := rec.Header()
			if rec.Code != tt.status || h.Get("Cache-Control") != tt.cache ||
				h.Get("Strict-Transport-Security") != "max-age=31536000" ||
				h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
				!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
				!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
				t.Fatalf("status %d, headers %v", rec.Code, h)
			}
		})
	}
}

func TestDiscovery(t *testing.T) {
	r := newRelay(t, nil)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, httptest.NewRequest("GET", issuer+"/.well-known/openid-configuration", nil))
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
		"userinfo_endpoint": issuer + "/userinfo", "jwks_uri": issuer + "/jwks",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %s", k, doc[k], v)
		}
	}
	if fmt.Sprint(doc["subject_types_supported"]) != "[pairwise]" ||
		fmt.Sprint(doc["code_challenge_methods_supported"]) != "[S256]" {
		t.Errorf("discovery %v", doc)
	}
}

func TestLogout(t *testing.T) {
	r := newRelay(t, nil)
	b := r.signedIn()
	req, _ := http.NewRequest(http.MethodPost, issuer+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	resp, _ := b.Do(req)
	_ = body(t, resp)
	if resp.StatusCode != 403 {
		t.Fatalf("same-site logout: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, issuer+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, _ = b.Do(req)
	_ = body(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	// Signed out: authorize goes to Dex again.
	resp = get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Location"), "https://dex.example.com/") {
		t.Fatalf("after logout: %q", resp.Header.Get("Location"))
	}
}

func TestRateLimit(t *testing.T) {
	r := newRelay(t, func(c *Config) { c.RateLimit = RateLimit{PerSecond: 1, Burst: 3, ForwardedHops: 1} })
	hit := func(xff, path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", issuer+path, nil)
		req.RemoteAddr = "10.0.0.5:1234" // the ALB node
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		r.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := range 3 {
		if code := hit("192.0.2.1", "/"); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code := hit("192.0.2.1", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("over the burst: %d", code)
	}
	// A client cannot pick a fresh bucket by prepending its own entry.
	if code := hit("198.51.100.1, 192.0.2.1", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed prefix: %d", code)
	}
	if code := hit("192.0.2.2", "/"); code != 200 {
		t.Fatalf("another address: %d", code)
	}
	// The backchannel gets a 503, not a sign-out.
	for range 3 {
		hit("192.0.2.3", "/userinfo")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", issuer+"/userinfo", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.3")
	r.handler.ServeHTTP(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "temporarily_unavailable") {
		t.Fatalf("limited userinfo: %d %s", rec.Code, rec.Body.String())
	}
}

// TestBackchannelRateLimitFollowsAuthentication: every /token and /userinfo
// call comes from the ALB's own address, so junk codes the ALB is driven to
// redeem (with its valid client secret) must never drain the bucket that
// real sign-ins share. Only requests that fail to authenticate are charged.
func TestBackchannelRateLimitFollowsAuthentication(t *testing.T) {
	r := newRelay(t, func(c *Config) { c.RateLimit = RateLimit{PerSecond: 0.001, Burst: 3} })
	junk := url.Values{"grant_type": {"authorization_code"}, "code": {"junk"}, "redirect_uri": {callback}}
	for i := range 50 {
		resp := r.tokenRequest("basic", 1, r.ring.ClientSecret(1), junk)
		if got := decodeToken(t, resp); resp.StatusCode != 400 || got.Error != "invalid_grant" {
			t.Fatalf("authenticated junk code %d: %d %q", i, resp.StatusCode, got.Error)
		}
	}
	// A real redemption from the same address still succeeds.
	good := url.Values{"grant_type": {"authorization_code"}, "code": {r.codeFor("")}, "redirect_uri": {callback}}
	resp := r.tokenRequest("post", 1, r.ring.ClientSecret(1), good)
	tok := decodeToken(t, resp)
	if resp.StatusCode != 200 || tok.AccessToken == "" {
		t.Fatalf("redeem after junk: %d %q", resp.StatusCode, tok.Error)
	}
	for i := range 20 {
		if resp, b := userinfo(t, r, http.MethodGet, tok.AccessToken); resp.StatusCode != 200 {
			t.Fatalf("userinfo %d: %d %s", i, resp.StatusCode, b)
		}
	}
	// Strangers are charged: a wrong client secret, then a token that does
	// not open, from the same address.
	wrong := strings.Repeat("0", 64)
	for i := range 3 {
		resp := r.tokenRequest("basic", 1, wrong, junk)
		if got := decodeToken(t, resp); resp.StatusCode != 401 || got.Error != "invalid_client" {
			t.Fatalf("wrong secret %d: %d %q", i, resp.StatusCode, got.Error)
		}
	}
	resp = r.tokenRequest("basic", 1, wrong, junk)
	if got := decodeToken(t, resp); resp.StatusCode != 503 || got.Error != "temporarily_unavailable" {
		t.Fatalf("wrong secret over the limit: %d %q", resp.StatusCode, got.Error)
	}
	if resp, b := userinfo(t, r, http.MethodGet, "junk"); resp.StatusCode != 503 {
		t.Fatalf("junk userinfo over the limit: %d %s", resp.StatusCode, b)
	}
	// The address is exhausted, yet the ALB's authenticated calls go on.
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(1), url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
	if got := decodeToken(t, resp); resp.StatusCode != 200 {
		t.Fatalf("refresh with the address exhausted: %d %q", resp.StatusCode, got.Error)
	}
	if resp, b := userinfo(t, r, http.MethodPost, tok.AccessToken); resp.StatusCode != 200 {
		t.Fatalf("userinfo with the address exhausted: %d %s", resp.StatusCode, b)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		xff  []string
		hops int
		want string
	}{
		{nil, 0, "10.0.0.5"},
		{[]string{"192.0.2.1"}, 0, "10.0.0.5"},
		{[]string{"192.0.2.1"}, 1, "192.0.2.1"},
		{[]string{"198.51.100.1, 192.0.2.1"}, 1, "192.0.2.1"},
		{[]string{"198.51.100.1", "192.0.2.1"}, 2, "198.51.100.1"},
		{[]string{"192.0.2.1"}, 2, "10.0.0.5"},
		{[]string{"not-an-ip"}, 1, "10.0.0.5"},
		{[]string{"::ffff:192.0.2.1"}, 1, "192.0.2.1"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.5:1234"
		for _, v := range tt.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(req, tt.hops); got != tt.want {
			t.Errorf("%v hops %d: %s, want %s", tt.xff, tt.hops, got, tt.want)
		}
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	tests := map[string]func(*Config){
		"http issuer":         func(c *Config) { c.Issuer = "http://" + relayHost },
		"issuer with path":    func(c *Config) { c.Issuer = issuer + "/x" },
		"relay under suffix":  func(c *Config) { c.Issuer = "https://auth." + suffix },
		"no slots":            func(c *Config) { c.SlotCount = 0 },
		"too many slots":      func(c *Config) { c.SlotCount = 5 },
		"no ledger":           func(c *Config) { c.Ledger = nil },
		"bad lifetimes":       func(c *Config) { c.Lifetimes.Access = time.Second },
		"no host suffix":      func(c *Config) { c.Callbacks = previewauth.Callbacks{} },
		"issuer is suffix":    func(c *Config) { c.Issuer = "https://" + suffix },
		"no signer":           func(c *Config) { c.Signer = nil },
		"no authorizer":       func(c *Config) { c.Authorizer = nil },
		"trailing slash":      func(c *Config) { c.Issuer = issuer + "/" },
		"issuer with port":    func(c *Config) { c.Issuer = issuer + ":8443" },
		"uppercase issuer":    func(c *Config) { c.Issuer = "https://Preview-Auth.example.com" },
		"issuer with a query": func(c *Config) { c.Issuer = issuer + "?a" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRelay(t, nil)
			cfg := r.srv.cfg
			edit(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
