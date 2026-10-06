// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func request(t *testing.T, method, url, site string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// With the intents views off, the server behaves exactly as before them: no
// intents routes, no CSP or HSTS, reads unchecked for Sec-Fetch-Site, a
// write without the header accepted, streams no-cache, and a stream's grant
// never re-checked.
func TestIntentsOffBehavesAsBefore(t *testing.T) {
	s := testServer(t, fullFinding(), testRollup("total", "", "total"))
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if s.hardened() || s.intents != nil {
		t.Fatal("a server built without WithIntents is hardened")
	}
	res := request(t, http.MethodGet, ts.URL+"/api/rollups", "")
	for _, h := range []string{"Content-Security-Policy", "Strict-Transport-Security", "Cross-Origin-Opener-Policy"} {
		if got := res.Header.Get(h); got != "" {
			t.Errorf("%s = %q with the intents views off, want none", h, got)
		}
	}
	// The intents routes do not exist: the paths fall through to the SPA
	// shell (or its stub), never a JSON payload.
	for _, path := range []string{"/api/me", "/api/intents", "/api/intents/alpha-7"} {
		res := request(t, http.MethodGet, ts.URL+path, "same-origin")
		if ct := res.Header.Get("Content-Type"); strings.Contains(ct, "json") {
			t.Errorf("GET %s answered %s with the intents views off", path, ct)
		}
	}
	// Reads carry no Sec-Fetch-Site check, as before.
	if res := request(t, http.MethodGet, ts.URL+"/api/findings", "cross-site"); res.StatusCode != http.StatusOK {
		t.Errorf("cross-site GET /api/findings = %d, want 200 (unchanged)", res.StatusCode)
	}
	// A write without the header is accepted, as before; one marked
	// cross-site is refused, as before.
	action := ts.URL + "/api/findings/gh-cs-orders-1/actions/resume"
	if res := request(t, http.MethodPost, action, ""); res.StatusCode != http.StatusOK {
		t.Errorf("POST without Sec-Fetch-Site = %d, want 200 (unchanged)", res.StatusCode)
	}
	if res := request(t, http.MethodPost, action, "cross-site"); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403 (unchanged)", res.StatusCode)
	}
	if got := s.streamCacheControl(); got != "no-cache" {
		t.Errorf("stream Cache-Control = %q, want no-cache (unchanged)", got)
	}
	if s.findingsRecheck(*operator) != nil {
		t.Error("a findings stream is re-checked with the intents views off")
	}
}

func TestHardenedEnvelope(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, viewerAlpha)

	// CSP and HSTS on API responses and on the page.
	for _, path := range []string{"/api/rollups", "/"} {
		res := request(t, http.MethodGet, ts.URL+path, "")
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "object-src 'none'") || strings.Contains(csp, "'unsafe-inline' 'unsafe-eval'") {
			t.Errorf("GET %s CSP = %q", path, csp)
		}
		if strings.Contains(strings.SplitN(strings.SplitN(csp, "script-src", 2)[1], ";", 2)[0], "unsafe-inline") {
			t.Errorf("script-src admits unsafe-inline: %q", csp)
		}
		if got := res.Header.Get("Strict-Transport-Security"); got != hstsValue {
			t.Errorf("GET %s HSTS = %q", path, got)
		}
	}

	// Sec-Fetch-Site on every API read and on /events.
	for _, path := range []string{"/api/intents", "/api/me", "/api/rollups", "/events",
		"/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream"} {
		for site, want := range map[string]int{
			"same-site": http.StatusForbidden, "cross-site": http.StatusForbidden,
		} {
			if res := request(t, http.MethodGet, ts.URL+path, site); res.StatusCode != want {
				t.Errorf("GET %s Sec-Fetch-Site %s = %d, want %d", path, site, res.StatusCode, want)
			}
		}
	}
	for _, site := range []string{"same-origin", "none", ""} {
		if res := request(t, http.MethodGet, ts.URL+"/api/intents", site); res.StatusCode != http.StatusOK {
			t.Errorf("GET /api/intents Sec-Fetch-Site %q = %d, want 200", site, res.StatusCode)
		}
	}
	// The sign-in redirect back from the provider is a cross-site
	// navigation, and must still reach the server.
	if res := request(t, http.MethodGet, ts.URL+"/", "cross-site"); res.StatusCode == http.StatusForbidden {
		t.Error("a cross-site page navigation was refused")
	}

	// Writes fail closed on a missing header.
	post := ts.URL + "/api/findings/none/actions/resume"
	if res := request(t, http.MethodPost, post, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST without Sec-Fetch-Site = %d, want 403", res.StatusCode)
	}
	if res := request(t, http.MethodPost, ts.URL+"/logout", ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("POST /logout without Sec-Fetch-Site = %d, want 403", res.StatusCode)
	}
	if res := request(t, http.MethodPost, post, "same-origin"); res.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "rejected") {
			t.Errorf("a same-origin POST was refused by the envelope: %s", body)
		}
	}

	if got := s.streamCacheControl(); got != "no-store" {
		t.Errorf("stream Cache-Control = %q, want no-store", got)
	}
	res := request(t, http.MethodGet, ts.URL+"/api/intents/alpha-7/runs/alpha-7-plan-r1-a1/stream", "same-origin")
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("run stream Cache-Control = %q, want no-store", got)
	}
}

// The public /events stream is capped while hardened.
func TestHardenedEventsCap(t *testing.T) {
	s, _ := intentsServer(t, nil)
	ts := as(t, s, nil)
	for range maxEventSubscribers {
		ch := s.broker.subscribe()
		defer s.broker.unsubscribe(ch)
	}
	res := request(t, http.MethodGet, ts.URL+"/events", "same-origin")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /events past the cap = %d, want 503", res.StatusCode)
	}
}

func TestBuildCSPHashesInlineScripts(t *testing.T) {
	index := `<!doctype html><html><head><script>(function(){})()</script>` +
		`<script type="module" crossorigin>console.log(1)</script>` +
		`<script src="/x.js"></script></head></html>`
	csp := buildCSP(fstest.MapFS{"index.html": {Data: []byte(index)}})
	// sha256 of "(function(){})()" and "console.log(1)", base64.
	for _, want := range []string{
		"'sha256-LG8MLlRLJqaeS84HDT6rhkfK9ckncOJZbBTNKOGqPwU='",
		"'sha256-CihokcEcBW4atb/CW/XWsvWwbTjqwQlE9nj9ii5ww5M='",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %s", csp, want)
		}
	}
	if got := len(inlineScripts(index)); got != 2 {
		t.Errorf("inline scripts = %d, want 2 (the src one skipped)", got)
	}
	if !strings.Contains(buildCSP(nil), "script-src 'self';") {
		t.Errorf("stub CSP = %q", buildCSP(nil))
	}
}
