// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func authorizeQuery(mutate func(url.Values)) url.Values {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {"patchy-preview-s1"},
		"redirect_uri":  {"https://intent-7." + testSuffix + CallbackPath},
		"scope":         {"openid"},
		"state":         {"st"},
	}
	if mutate != nil {
		mutate(q)
	}
	return q
}

func TestParseAuthorize(t *testing.T) {
	cb := newCallbacks(t)
	challenge := S256(strings.Repeat("v", 43))
	for _, tc := range []struct {
		name   string
		mutate func(url.Values)
		code   string // "" means accepted
	}{
		{"minimal", nil, ""},
		{"extra scopes", func(q url.Values) { q.Set("scope", "profile openid email") }, ""},
		{"nonce and pkce", func(q url.Values) {
			q.Set("nonce", "n")
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "S256")
		}, ""},
		{"unknown parameters", func(q url.Values) { q.Set("prompt", "login"); q.Set("max_age", "0") }, ""},
		{"repeated state", func(q url.Values) { q.Add("state", "other") }, CodeInvalidRequest},
		{"repeated unknown", func(q url.Values) { q["prompt"] = []string{"a", "b"} }, CodeInvalidRequest},
		{"repeated redirect", func(q url.Values) { q.Add("redirect_uri", "https://evil.com/") }, CodeInvalidRequest},
		{"token response", func(q url.Values) { q.Set("response_type", "token") }, CodeUnsupportedResponseType},
		{"hybrid response", func(q url.Values) { q.Set("response_type", "code id_token") }, CodeUnsupportedResponseType},
		{"no client", func(q url.Values) { q.Del("client_id") }, CodeInvalidRequest},
		{"slot beyond count", func(q url.Values) { q.Set("client_id", "patchy-preview-s2") }, CodeInvalidRequest},
		{"foreign redirect", func(q url.Values) { q.Set("redirect_uri", "https://evil.com"+CallbackPath) },
			CodeInvalidRequest},
		{"no redirect", func(q url.Values) { q.Del("redirect_uri") }, CodeInvalidRequest},
		{"no openid", func(q url.Values) { q.Set("scope", "profile") }, CodeInvalidScope},
		{"openid as a substring", func(q url.Values) { q.Set("scope", "openidx") }, CodeInvalidScope},
		{"no state", func(q url.Values) { q.Del("state") }, CodeInvalidRequest},
		{"empty state", func(q url.Values) { q.Set("state", "") }, CodeInvalidRequest},
		{"long state", func(q url.Values) { q.Set("state", strings.Repeat("s", MaxStateBytes+1)) }, CodeInvalidRequest},
		{"long nonce", func(q url.Values) { q.Set("nonce", strings.Repeat("n", MaxNonceBytes+1)) }, CodeInvalidRequest},
		{"plain pkce", func(q url.Values) {
			q.Set("code_challenge", challenge)
			q.Set("code_challenge_method", "plain")
		}, CodeInvalidRequest},
		{"pkce without method", func(q url.Values) { q.Set("code_challenge", challenge) }, CodeInvalidRequest},
		{"method without pkce", func(q url.Values) { q.Set("code_challenge_method", "S256") }, CodeInvalidRequest},
		{"short challenge", func(q url.Values) {
			q.Set("code_challenge", challenge[:42])
			q.Set("code_challenge_method", "S256")
		}, CodeInvalidRequest},
	} {
		req, err := ParseAuthorize(authorizeQuery(tc.mutate), 2, cb)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != tc.code || e.Status != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want %s (400); req %+v", tc.name, err, tc.code, req)
		}
	}
	req, err := ParseAuthorize(authorizeQuery(nil), 2, cb)
	if err != nil {
		t.Fatal(err)
	}
	want := AuthorizeRequest{ClientID: "patchy-preview-s1", Slot: 1, Label: "intent-7",
		RedirectURI: "https://intent-7." + testSuffix + CallbackPath, State: "st"}
	if req != want {
		t.Errorf("ParseAuthorize = %+v, want %+v", req, want)
	}
}

// TestStateRoundTripProperty: whatever state the ALB sends comes back on the
// code redirect byte for byte once the query is decoded, and the redirect
// stays on the validated callback.
func TestStateRoundTripProperty(t *testing.T) {
	cb := newCallbacks(t)
	cfg := quickConfig(2000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		b := make([]byte, 1+r.Intn(300))
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		args[0] = reflect.ValueOf(string(b))
	}
	prop := func(state string) bool {
		q := authorizeQuery(func(q url.Values) { q.Set("state", state) })
		req, err := ParseAuthorize(q, 2, cb)
		if err != nil || req.State != state {
			return false
		}
		loc, err := url.Parse(CodeRedirect(req.RedirectURI, "pa1.c.1.x", req.State))
		if err != nil {
			return false
		}
		got := loc.Query()
		return loc.Scheme+"://"+loc.Host+loc.Path == req.RedirectURI && got.Get("state") == state &&
			got.Get("code") == "pa1.c.1.x" && len(got) == 2
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func basicHeader(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
}

func TestAuthenticateClient(t *testing.T) {
	r := newRotatedRing(t)
	old := newRing(t, 1, masterA)
	sec := r.ClientSecret(1)
	for _, tc := range []struct {
		name      string
		authz     string
		form      url.Values
		slot      int  // when accepted
		wantBasic bool // when refused
		ok        bool
	}{
		{"basic", basicHeader(ClientID(1), sec), url.Values{}, 1, false, true},
		{"basic, lower-case scheme", strings.Replace(basicHeader(ClientID(1), sec), "Basic", "basic", 1),
			url.Values{}, 1, false, true},
		{"basic with a matching form client_id", basicHeader(ClientID(1), sec),
			url.Values{"client_id": {ClientID(1)}}, 1, false, true},
		{"post", "", url.Values{"client_id": {ClientID(1)}, "client_secret": {sec}}, 1, false, true},
		{"post, previous generation", "",
			url.Values{"client_id": {ClientID(1)}, "client_secret": {old.ClientSecret(1)}}, 1, false, true},
		{"both", basicHeader(ClientID(1), sec), url.Values{"client_id": {ClientID(1)}, "client_secret": {sec}},
			0, true, false},
		{"neither", "", url.Values{"client_id": {ClientID(1)}}, 0, false, false},
		{"bearer", "Bearer x", url.Values{"client_id": {ClientID(1)}, "client_secret": {sec}}, 0, false, false},
		{"basic, wrong secret", basicHeader(ClientID(1), r.ClientSecret(0)), url.Values{}, 0, true, false},
		{"basic, another slot", basicHeader(ClientID(0), sec), url.Values{}, 0, true, false},
		{"basic, slot beyond count", basicHeader(ClientID(3), r.ClientSecret(3)), url.Values{}, 0, true, false},
		{"basic, mismatched form client_id", basicHeader(ClientID(1), sec),
			url.Values{"client_id": {ClientID(0)}}, 0, true, false},
		{"basic, not base64", "Basic !!!", url.Values{}, 0, true, false},
		{"basic, no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte(ClientID(1))), url.Values{}, 0, true,
			false},
		{"basic, bad escape", "Basic " + base64.StdEncoding.EncodeToString([]byte(ClientID(1)+":%zz")), url.Values{},
			0, true, false},
		{"post, no client_id", "", url.Values{"client_secret": {sec}}, 0, false, false},
		{"post, repeated secret", "", url.Values{"client_id": {ClientID(1)}, "client_secret": {sec, sec}}, 0, false,
			false},
		{"post, wrong secret", "", url.Values{"client_id": {ClientID(1)}, "client_secret": {"x"}}, 0, false, false},
	} {
		ca, err := r.AuthenticateClient(tc.authz, tc.form, 3)
		if tc.ok {
			if err != nil || ca != (ClientAuth{ClientID: ClientID(tc.slot), Slot: tc.slot}) {
				t.Errorf("%s: %+v, %v", tc.name, ca, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%s: err = %v, want an *Error", tc.name, err)
			continue
		}
		if e.Code != CodeInvalidClient && e.Code != CodeInvalidRequest {
			t.Errorf("%s: code %s", tc.name, e.Code)
		}
		if e.Code == CodeInvalidClient && (e.Status != http.StatusUnauthorized || e.Basic != tc.wantBasic) {
			t.Errorf("%s: status %d basic %v, want 401 basic %v", tc.name, e.Status, e.Basic, tc.wantBasic)
		}
	}
}

// TestBasicFormDecoding: RFC 6749 §2.3.1 form-encodes the id and secret
// before Basic, so '+' is a space and %xx an escape.
func TestBasicFormDecoding(t *testing.T) {
	id, secret, ok := decodeBasic(base64.StdEncoding.EncodeToString([]byte("a+b%3Ac:s%2Bt+u")))
	if !ok || id != "a b:c" || secret != "s+t u" {
		t.Errorf("decodeBasic = %q, %q, %v", id, secret, ok)
	}
}

func TestParseTokenRequest(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	for _, tc := range []struct {
		name string
		form url.Values
		code string
	}{
		{"code", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "redirect_uri": {"r"}}, ""},
		{"code with verifier", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "redirect_uri": {"r"},
			"code_verifier": {verifier}}, ""},
		{"refresh with scope", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"r"},
			"scope": {"openid admin"}}, ""},
		{"no grant type", url.Values{"code": {"c"}}, CodeInvalidRequest},
		{"password grant", url.Values{"grant_type": {"password"}}, CodeUnsupportedGrantType},
		{"client credentials", url.Values{"grant_type": {"client_credentials"}}, CodeUnsupportedGrantType},
		{"code without code", url.Values{"grant_type": {"authorization_code"}, "redirect_uri": {"r"}},
			CodeInvalidRequest},
		{"code without redirect", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}},
			CodeInvalidRequest},
		{"short verifier", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "redirect_uri": {"r"},
			"code_verifier": {verifier[:42]}}, CodeInvalidRequest},
		{"empty verifier", url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "redirect_uri": {"r"},
			"code_verifier": {""}}, CodeInvalidRequest},
		{"verifier with a space", url.Values{"grant_type": {"authorization_code"}, "code": {"c"},
			"redirect_uri": {"r"}, "code_verifier": {verifier + " x"}}, CodeInvalidRequest},
		{"refresh without token", url.Values{"grant_type": {"refresh_token"}}, CodeInvalidRequest},
		{"huge code", url.Values{"grant_type": {"authorization_code"}, "code": {strings.Repeat("c", MaxTokenBytes+1)},
			"redirect_uri": {"r"}}, CodeInvalidRequest},
		{"repeated code", url.Values{"grant_type": {"authorization_code"}, "code": {"c", "d"}, "redirect_uri": {"r"}},
			CodeInvalidRequest},
		{"repeated grant type", url.Values{"grant_type": {"refresh_token", "authorization_code"},
			"refresh_token": {"r"}}, CodeInvalidRequest},
	} {
		_, err := ParseTokenRequest(tc.form)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != tc.code || e.Status != http.StatusBadRequest {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
}

func TestRedeemCode(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	ru := "https://a." + testSuffix + CallbackPath
	base := Grant{Bound: Bound{Client: ClientID(1), Slot: 1, UID: "u", Label: "a"}, RedirectURI: ru}
	withPKCE := base
	withPKCE.Challenge = S256(verifier)
	client := ClientAuth{ClientID: ClientID(1), Slot: 1}
	for _, tc := range []struct {
		name   string
		g      Grant
		client ClientAuth
		req    TokenRequest
		ok     bool
	}{
		{"no pkce", base, client, TokenRequest{RedirectURI: ru}, true},
		{"pkce", withPKCE, client, TokenRequest{RedirectURI: ru, Verifier: verifier}, true},
		{"pkce, no verifier", withPKCE, client, TokenRequest{RedirectURI: ru}, false},
		{"pkce, wrong verifier", withPKCE, client, TokenRequest{RedirectURI: ru, Verifier: verifier + "x"}, false},
		{"verifier without a challenge", base, client, TokenRequest{RedirectURI: ru, Verifier: verifier}, false},
		{"another client", base, ClientAuth{ClientID: ClientID(0), Slot: 0}, TokenRequest{RedirectURI: ru}, false},
		{"slot disagrees with the client", base, ClientAuth{ClientID: ClientID(1), Slot: 0},
			TokenRequest{RedirectURI: ru}, false},
		{"another redirect", base, client, TokenRequest{RedirectURI: "https://b." + testSuffix + CallbackPath}, false},
		{"redirect with a slash", base, client, TokenRequest{RedirectURI: ru + "/"}, false},
	} {
		err := RedeemCode(tc.g, tc.client, tc.req)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
		var e *Error
		if err != nil && (!errors.As(err, &e) || e.Code != CodeInvalidGrant) {
			t.Errorf("%s: err = %v, want invalid_grant", tc.name, err)
		}
	}
}

func TestCheckRefresh(t *testing.T) {
	g := Grant{Bound: Bound{Client: ClientID(1), Slot: 1, UID: "u", Label: "a"}}
	if err := CheckRefresh(g, ClientAuth{ClientID: ClientID(1), Slot: 1}); err != nil {
		t.Errorf("own client: %v", err)
	}
	if err := CheckRefresh(g, ClientAuth{ClientID: ClientID(2), Slot: 2}); err == nil {
		t.Error("another slot's client refreshed the token")
	}
}

func TestCodeKey(t *testing.T) {
	a := Grant{JTI: "j1"}
	if CodeKey(a) != CodeKey(Grant{JTI: "j1", Sub: "other"}) {
		t.Error("CodeKey depends on more than the JTI")
	}
	if CodeKey(a) == CodeKey(Grant{JTI: "j2"}) {
		t.Error("two JTIs share a ledger key")
	}
}

func TestParseBearer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		authz  string
		form   url.Values
		tok    string
		status int
	}{
		{"header GET", http.MethodGet, "Bearer t", nil, "t", 0},
		{"header POST", http.MethodPost, "Bearer t", url.Values{}, "t", 0},
		{"lower-case scheme", http.MethodGet, "bearer t", nil, "t", 0},
		{"form POST", http.MethodPost, "", url.Values{"access_token": {"t"}}, "t", 0},
		{"form on GET is not read", http.MethodGet, "", url.Values{"access_token": {"t"}}, "", 401},
		{"both", http.MethodPost, "Bearer t", url.Values{"access_token": {"t"}}, "", 400},
		{"neither", http.MethodGet, "", nil, "", 401},
		{"basic", http.MethodGet, "Basic dDp0", nil, "", 401},
		{"empty bearer", http.MethodGet, "Bearer ", nil, "", 401},
		{"two spaces", http.MethodGet, "Bearer  t", nil, "", 401},
		{"token with a space", http.MethodGet, "Bearer t u", nil, "", 401},
		{"empty form token", http.MethodPost, "", url.Values{"access_token": {""}}, "", 401},
		{"repeated form token", http.MethodPost, "", url.Values{"access_token": {"t", "u"}}, "", 400},
		{"PUT", http.MethodPut, "Bearer t", nil, "", 405},
	} {
		tok, err := ParseBearer(tc.method, tc.authz, tc.form)
		if tc.status == 0 {
			if err != nil || tok != tc.tok {
				t.Errorf("%s: %q, %v", tc.name, tok, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Status != tc.status {
			t.Errorf("%s: err = %v, want status %d", tc.name, err, tc.status)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	transient := errors.New("api server unreachable")
	for _, tc := range []struct {
		err             error
		token, userinfo int
	}{
		{ErrInvalidToken, 400, 401},
		{ErrExpired, 400, 401},
		{ErrNotBound, 400, 401},
		{ErrNoPreview, 400, 401},
		{ErrReplayed, 400, 503},
		{ErrDenied, 400, 503},
		{ErrNoProject, 400, 503},
		{fmt.Errorf("lookup: %w", ErrNoPreview), 400, 401},
		{transient, 503, 503},
		{&Error{Status: 401, Code: CodeInvalidClient}, 401, 401},
	} {
		if got := TokenError(tc.err); got.Status != tc.token {
			t.Errorf("TokenError(%v) = %d %s, want %d", tc.err, got.Status, got.Code, tc.token)
		}
		if got := UserinfoError(tc.err); got.Status != tc.userinfo {
			t.Errorf("UserinfoError(%v) = %d %s, want %d", tc.err, got.Status, got.Code, tc.userinfo)
		}
	}
	if TokenError(errors.New("x")).Code != CodeTemporarilyUnavailable {
		t.Error("a transient error is not temporarily_unavailable")
	}
}

func TestValidIssuer(t *testing.T) {
	for _, tc := range []struct {
		iss string
		ok  bool
	}{
		{"https://preview-auth.example.com", true},
		{"https://preview-auth.example.com/", false},
		{"http://preview-auth.example.com", false},
		{"https://preview-auth.example.com:443", false},
		{"https://preview-auth.example.com/x", false},
		{"https://preview-auth.example.com?x", false},
		{"https://Preview-auth.example.com", false},
		{"https://u@preview-auth.example.com", false},
		{"https://localhost", false},
		{"", false},
	} {
		if got := ValidIssuer(tc.iss); got != tc.ok {
			t.Errorf("ValidIssuer(%q) = %v, want %v", tc.iss, got, tc.ok)
		}
	}
}
