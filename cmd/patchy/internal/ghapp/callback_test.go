// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// get fetches u with a client that never follows redirects, answering the
// status and body.
func get(t *testing.T, u string, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestCallbackTakesOneCode: the start page is served; a request addressed to
// another host, a callback with the wrong state or without a code are
// refused and the wait goes on; the first right callback's code is the
// one Wait returns, a second is told it is too late, and then the server
// is gone.
func TestCallbackTakesOneCode(t *testing.T) {
	cb, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if !strings.HasPrefix(cb.StartURL(), "http://127.0.0.1:") || cb.RedirectURL() != cb.StartURL()+"callback" {
		t.Fatalf("StartURL %s, RedirectURL %s", cb.StartURL(), cb.RedirectURL())
	}
	cb.Serve("the-state", []byte("<p>start</p>"))

	if code, body := get(t, cb.StartURL(), ""); code != http.StatusOK || body != "<p>start</p>" {
		t.Errorf("start page = %d %q", code, body)
	}
	if code, _ := get(t, cb.StartURL(), "rebound.attacker.test"); code != http.StatusMisdirectedRequest {
		t.Errorf("a request for another host = %d, want 421", code)
	}
	if code, _ := get(t, cb.RedirectURL()+"?code=evil&state=other", ""); code != http.StatusBadRequest {
		t.Errorf("wrong state = %d, want 400", code)
	}
	if code, _ := get(t, cb.RedirectURL()+"?code=evil", ""); code != http.StatusBadRequest {
		t.Errorf("no state = %d, want 400", code)
	}
	if code, _ := get(t, cb.RedirectURL()+"?state=the-state", ""); code != http.StatusBadRequest {
		t.Errorf("no code = %d, want 400", code)
	}
	if code, _ := get(t, cb.RedirectURL()+"?state=the-state&code=a/b", ""); code != http.StatusBadRequest {
		t.Errorf("malformed code = %d, want 400", code)
	}
	if code, _ := get(t, cb.StartURL()+"other", ""); code != http.StatusNotFound {
		t.Errorf("other path = %d, want 404", code)
	}
	if code, body := get(t, cb.RedirectURL()+"?code=good123&state=the-state", ""); code != http.StatusOK ||
		!strings.Contains(body, "Return to your terminal") {
		t.Errorf("right callback = %d %q", code, body)
	}
	if code, _ := get(t, cb.RedirectURL()+"?code=second&state=the-state", ""); code != http.StatusGone {
		t.Errorf("second callback = %d, want 410", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := cb.Wait(ctx)
	if err != nil || code != "good123" {
		t.Fatalf("Wait() = %q, %v; want the first right code", code, err)
	}
	if _, err := http.Get(cb.StartURL()); err == nil {
		t.Error("the server still answers after it took its code")
	}
}

// TestCallbackWaitGivesUp: no callback before the deadline is ErrNoCode,
// and the server stops.
func TestCallbackWaitGivesUp(t *testing.T) {
	cb, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	cb.Serve("s", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := cb.Wait(ctx); !errors.Is(err, ErrNoCode) {
		t.Fatalf("Wait() error = %v, want ErrNoCode", err)
	}
	if _, err := http.Get(cb.StartURL()); err == nil {
		t.Error("the server still answers after the wait ended")
	}
}

var (
	formAction = regexp.MustCompile(`<form id="manifest" method="post" action="([^"]*)">`)
	formValue  = regexp.MustCompile(`<input type="hidden" name="manifest" value="([^"]*)">`)
)

// TestStartPage: the page posts exactly the manifest to the create URL, and
// a manifest that tries to break out of its attribute cannot.
func TestStartPage(t *testing.T) {
	m, err := Build(Config{Features: Features{Intents: true}, Name: `x"><script>alert(1)</script>`,
		HomepageURL: DefaultHomepageURL})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	create := CreateURL(DefaultWebURL, Owner{Org: "acme"}, "st&ate")
	page, err := StartPage(create, raw)
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	if n := strings.Count(s, "<script>"); n != 1 {
		t.Errorf("the page has %d script elements, want only its own:\n%s", n, s)
	}
	action := formAction.FindStringSubmatch(s)
	value := formValue.FindStringSubmatch(s)
	if action == nil || value == nil {
		t.Fatalf("no form in the page:\n%s", s)
	}
	if got := html.UnescapeString(action[1]); got != create {
		t.Errorf("form action = %s, want %s", got, create)
	}
	if got := html.UnescapeString(value[1]); got != string(raw) {
		t.Errorf("posted manifest = %s, want %s", got, raw)
	}
}

func TestParseCode(t *testing.T) {
	const state = "abc"
	landing := func(q url.Values) string {
		return "https://github.com/organizations/acme/settings/apps?" + q.Encode()
	}
	tests := []struct {
		name, input, want, err string
	}{
		{"bare code", "  a1b2c3\n", "a1b2c3", ""},
		{"landing address", landing(url.Values{"code": {"a1b2"}, "state": {state}}), "a1b2", ""},
		{"another attempt's address", landing(url.Values{"code": {"a1b2"}, "state": {"zzz"}}), "",
			"another attempt"},
		{"address without state", landing(url.Values{"code": {"a1b2"}}), "", "another attempt"},
		{"address without code", landing(url.Values{"state": {state}}), "", "no code"},
		{"junk", "not a code!", "", "neither"},
		{"empty", "", "", "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCode(tt.input, state)
			if tt.err == "" && (err != nil || got != tt.want) {
				t.Errorf("ParseCode() = %q, %v; want %q", got, err, tt.want)
			}
			if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("ParseCode() = %q, %v; want an error containing %q", got, err, tt.err)
			}
		})
	}
}

func TestNewState(t *testing.T) {
	a, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewState()
	if len(a) != 64 || a == b {
		t.Errorf("states %q and %q", a, b)
	}
}
