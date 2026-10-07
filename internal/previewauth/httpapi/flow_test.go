// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/dex"
	"github.com/bitwise-media-group/patchy/internal/previewauth/fakedex"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// newFlowRelay is a relay whose upstream is the real Dex adapter against an
// in-memory Dex, with alice signed in there.
func newFlowRelay(t *testing.T) (*relay, *fakedex.Server) {
	t.Helper()
	d, err := fakedex.Start("patchy-preview-auth", "dex-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	d.SignIn(map[string]any{"sub": "dex-sub-alice", "preferred_username": "alice",
		"groups": []any{"devthenet-labs:viewers"}})
	up, err := dex.New(dex.Config{
		IssuerURL: d.URL, ClientID: "patchy-preview-auth", ClientSecret: "dex-secret",
		RedirectURL: issuer + dex.CallbackPath, AllowHTTP: true,
		Claims: auth.ClaimsConfig{Username: "preferred_username", Groups: "groups", UsernamePrefix: "github:",
			GroupsPrefix: "github:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRelay(t, func(c *Config) { c.Upstream = up })
	return r, d
}

// signIn drives a browser from the ALB's redirect, through Dex and back,
// to the code the relay sends to the preview host.
func signIn(t *testing.T, b *http.Client, authorize string) (string, string) {
	t.Helper()
	resp := get(t, b, authorize)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	dexAuth := resp.Header.Get("Location")
	if strings.HasPrefix(dexAuth, callback) {
		return codeFrom(t, resp) // already signed in at the relay
	}
	resp = get(t, b, dexAuth) // Dex signs the viewer in and sends them back
	_ = resp.Body.Close()
	back := resp.Header.Get("Location")
	if !strings.HasPrefix(back, issuer+dex.CallbackPath+"?") {
		t.Fatalf("Dex sent the browser to %q", back)
	}
	return codeFrom(t, get(t, b, back))
}

// verifyIDToken checks an ID token the way a strict relying party would.
func verifyIDToken(t *testing.T, r *relay, raw string, slot int, nonce string) *gooidc.IDToken {
	t.Helper()
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(r.signer.JWKS(), &set); err != nil {
		t.Fatal(err)
	}
	keys := make([]crypto.PublicKey, 0, len(set.Keys))
	for _, k := range set.Keys {
		keys = append(keys, k.Key.(*rsa.PublicKey))
	}
	v := gooidc.NewVerifier(issuer, &gooidc.StaticKeySet{PublicKeys: keys},
		&gooidc.Config{ClientID: previewauth.ClientID(slot), Now: r.clock.now})
	idt, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("ID token: %v", err)
	}
	if idt.Nonce != nonce {
		t.Fatalf("ID token nonce %q, want %q", idt.Nonce, nonce)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	for k := range claims {
		switch k {
		case "iss", "aud", "sub", "iat", "exp", "nonce", "at_hash":
		default:
			t.Errorf("ID token carries claim %q", k)
		}
	}
	return idt
}

func userinfo(t *testing.T, r *relay, method, accessToken string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, issuer+"/userinfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := r.backchannel().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body(t, resp)
}

// TestOAuthFlowAgainstFakeDex is the fake ALB's whole journey, in both
// client authentication methods, with and without a nonce and PKCE.
func TestOAuthFlowAgainstFakeDex(t *testing.T) {
	for _, how := range []string{"basic", "post"} {
		for _, extras := range []string{"none", "nonce", "pkce", "both"} {
			t.Run(how+"/"+extras, func(t *testing.T) { runFlow(t, how, extras) })
		}
	}
}

// runFlow is one variant of TestOAuthFlowAgainstFakeDex.
func runFlow(t *testing.T, how, extras string) {
	r, d := newFlowRelay(t)
	verifier := strings.Repeat("v", 43)
	nonce := ""
	authorize := authorizeURL(func(q url.Values) {
		if extras == "nonce" || extras == "both" {
			nonce = "alb-nonce"
			q.Set("nonce", nonce)
		}
		if extras == "pkce" || extras == "both" {
			q.Set("code_challenge", previewauth.S256(verifier))
			q.Set("code_challenge_method", "S256")
		}
	})
	b := r.browser()
	code, state := signIn(t, b, authorize)
	if state != "alb-state/with+odd=chars" {
		t.Fatalf("state came back as %q", state)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}}
	if extras == "pkce" || extras == "both" {
		form.Set("code_verifier", verifier)
	}
	tok, idt := redeem(t, r, how, form, nonce)
	userinfoAndReplay(t, r, how, form, tok, idt.Subject)

	// A second sign-in in the same browser does not go back to Dex.
	before := d.Authorizations()
	code2, _ := signIn(t, b, authorize)
	if d.Authorizations() != before || code2 == code {
		t.Fatalf("second sign-in went to Dex (%d -> %d)", before, d.Authorizations())
	}

	refreshInParallel(t, r, how, tok, idt.Subject)
}

// redeem exchanges a code at the token endpoint and verifies the answer.
func redeem(t *testing.T, r *relay, how string, form url.Values, nonce string) (tokenJSON, *gooidc.IDToken) {
	t.Helper()
	resp := r.tokenRequest(how, 1, r.ring.ClientSecret(1), form)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Pragma") != "no-cache" {
		t.Fatalf("token: %d %s", resp.StatusCode, body(t, resp))
	}
	tok := decodeToken(t, resp)
	idt := verifyIDToken(t, r, tok.IDToken, 1, nonce)
	if tok.TokenType != "Bearer" || tok.ExpiresIn != 600 || tok.RefreshToken == "" ||
		idt.Expiry.Unix()-r.clock.now().Unix() > 600 {
		t.Fatalf("token response %+v", tok)
	}
	if strings.Contains(idt.Subject, "alice") || strings.Contains(tok.AccessToken, "alice") {
		t.Fatal("the viewer's login reached the ALB's forwarded values")
	}
	return tok, idt
}

// userinfoAndReplay checks userinfo in both methods (the same pairwise
// subject only) and that the code does not redeem twice.
func userinfoAndReplay(t *testing.T, r *relay, how string, form url.Values, tok tokenJSON, sub string) {
	t.Helper()
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		resp, got := userinfo(t, r, m, tok.AccessToken)
		if resp.StatusCode != http.StatusOK || got != fmt.Sprintf("{\"sub\":%q}\n", sub) {
			t.Fatalf("userinfo %s: %d %s", m, resp.StatusCode, got)
		}
	}
	resp := r.tokenRequest(how, 1, r.ring.ClientSecret(1), form)
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_grant" {
		t.Fatalf("replayed code: %d %+v", resp.StatusCode, out)
	}
}

// refreshInParallel refreshes with the same refresh token from several ALB
// nodes at once: all succeed, with the same subject and refresh token.
func refreshInParallel(t *testing.T, r *relay, how string, tok tokenJSON, sub string) {
	t.Helper()
	r.clock.advance(11 * time.Minute) // the access token has expired
	if resp, _ := userinfo(t, r, http.MethodGet, tok.AccessToken); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("expired access token: %d", resp.StatusCode)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := r.tokenRequest(how, 1, r.ring.ClientSecret(1),
				url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}})
			var out tokenJSON
			_ = json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || out.RefreshToken != tok.RefreshToken || out.IDToken == "" {
				errs <- fmt.Errorf("refresh: %d %+v", resp.StatusCode, out)
				return
			}
			if resp, got := userinfo(t, r, http.MethodGet, out.AccessToken); resp.StatusCode != 200 ||
				!strings.Contains(got, sub) {
				errs <- fmt.Errorf("userinfo after refresh: %d %s", resp.StatusCode, got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestRefreshBoundaries: a refresh token works only for its own client,
// only while its Preview lives, and only while the viewer may see it.
func TestRefreshBoundaries(t *testing.T) {
	r, _ := newFlowRelay(t)
	code, _ := signIn(t, r.browser(), authorizeURL(nil))
	resp := r.tokenRequest("basic", 1, r.ring.ClientSecret(1),
		url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}})
	tok := decodeToken(t, resp)
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}

	// Another slot's client, with its own valid secret.
	resp = r.tokenRequest("basic", 0, r.ring.ClientSecret(0), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_grant" {
		t.Fatalf("cross-client refresh: %d %+v", resp.StatusCode, out)
	}
	// Another slot's secret for this client.
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(0), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 401 || out.Error != "invalid_client" ||
		resp.Header.Get("WWW-Authenticate") != `Basic realm="preview-auth"` {
		t.Fatalf("wrong secret: %d %+v", resp.StatusCode, out)
	}
	// A transient access-review failure keeps the ALB session: 503.
	r.viewers.setErr(fmt.Errorf("api down"))
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(1), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 503 || out.Error != "temporarily_unavailable" {
		t.Fatalf("transient: %d %+v", resp.StatusCode, out)
	}
	r.viewers.setErr(nil)
	// A revoked viewer is signed out at the next refresh.
	r.viewers.revoke("github:alice")
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(1), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_grant" {
		t.Fatalf("revoked: %d %+v", resp.StatusCode, out)
	}
	r.viewers.users["github:alice"] = true
	// A Preview replaced at the same label (new UID) ends the session, and
	// its access tokens stop working at userinfo.
	r.previews.set(previewauth.View{UID: "uid-new", Label: label, Project: "demo", Slot: 1, Live: true})
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(1), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_grant" {
		t.Fatalf("replaced preview: %d %+v", resp.StatusCode, out)
	}
	if resp, _ := userinfo(t, r, http.MethodGet, tok.AccessToken); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("userinfo for a replaced preview: %d", resp.StatusCode)
	}
	// A Preview that is gone.
	r.previews.set(previewauth.View{UID: "uid-7", Label: label, Project: "demo", Slot: 1, Live: false})
	resp = r.tokenRequest("basic", 1, r.ring.ClientSecret(1), refresh)
	if out := decodeToken(t, resp); resp.StatusCode != 400 || out.Error != "invalid_grant" {
		t.Fatalf("expired preview: %d %+v", resp.StatusCode, out)
	}
}

// TestUnboundViewerIsRefused: Dex signs bob in, but no binding names him.
func TestUnboundViewerIsRefused(t *testing.T) {
	r, d := newFlowRelay(t)
	d.SignIn(map[string]any{"sub": "dex-bob", "preferred_username": "bob"})
	b := r.browser()
	resp := get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	resp = get(t, b, resp.Header.Get("Location"))
	_ = resp.Body.Close()
	resp = get(t, b, resp.Header.Get("Location"))
	page := body(t, resp)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" ||
		!strings.Contains(page, "not a preview viewer") {
		t.Fatalf("unbound viewer: %d %s", resp.StatusCode, page)
	}
	last := r.viewers.asked[len(r.viewers.asked)-1]
	if last.Username != "github:bob" || last.Project != "demo" || last.Subresource != "previews" {
		t.Fatalf("access review %+v", last)
	}
}

// TestDexRefusalIsAPage: Dex's own error comes back as a relay page.
func TestDexRefusalIsAPage(t *testing.T) {
	r, d := newFlowRelay(t)
	d.SignIn(nil)
	b := r.browser()
	resp := get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	resp = get(t, b, resp.Header.Get("Location"))
	_ = resp.Body.Close()
	resp = get(t, b, resp.Header.Get("Location"))
	_ = body(t, resp)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Fatalf("Dex refusal: %d", resp.StatusCode)
	}
}

// TestCallbackNeedsTheStartingBrowser: a Dex callback replayed in another
// browser (no login cookie) does not sign that browser in.
func TestCallbackNeedsTheStartingBrowser(t *testing.T) {
	r, _ := newFlowRelay(t)
	b := r.browser()
	resp := get(t, b, authorizeURL(nil))
	_ = resp.Body.Close()
	resp = get(t, b, resp.Header.Get("Location"))
	_ = resp.Body.Close()
	back := resp.Header.Get("Location")
	resp = get(t, r.browser(), back)
	_ = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" ||
		len(resp.Cookies()) != 0 {
		t.Fatalf("callback in another browser: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	// Two sign-ins in flight in one browser do not break each other.
	first := get(t, b, authorizeURL(nil))
	_ = first.Body.Close()
	second := get(t, b, authorizeURL(nil))
	_ = second.Body.Close()
	for _, start := range []*http.Response{second, first} {
		resp := get(t, b, start.Header.Get("Location"))
		_ = resp.Body.Close()
		codeFrom(t, get(t, b, resp.Header.Get("Location")))
	}
}
