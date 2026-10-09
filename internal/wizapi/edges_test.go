// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package wizapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scripted answers the token and GraphQL endpoints with fixed responses.
type scripted struct {
	tokenStatus int
	tokenBody   string
	gqlStatus   int
	gqlBody     string

	mu         sync.Mutex
	tokenCalls int
}

func (s *scripted) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			s.mu.Lock()
			s.tokenCalls++
			s.mu.Unlock()
			w.WriteHeader(orOK(s.tokenStatus))
			_, _ = io.WriteString(w, s.tokenBody)
		case "/graphql":
			w.WriteHeader(orOK(s.gqlStatus))
			_, _ = io.WriteString(w, s.gqlBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func orOK(code int) int {
	if code == 0 {
		return http.StatusOK
	}
	return code
}

func clientFor(t *testing.T, s *scripted) *Client {
	t.Helper()
	srv := s.server(t)
	c, err := New(Options{Endpoint: srv.URL + "/graphql", TokenURL: srv.URL + "/oauth/token",
		ClientID: "id", ClientSecret: "secret", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTokenFailures(t *testing.T) {
	cases := []struct {
		name string
		s    *scripted
		want string
	}{
		{"token endpoint refuses", &scripted{tokenStatus: http.StatusUnauthorized, tokenBody: `{"error":"denied"}`},
			"token endpoint returned 401"},
		{"token answer not JSON", &scripted{tokenBody: "<html>"}, "decode token response"},
		{"token answer without a token", &scripted{tokenBody: `{"expires_in":60}`}, "no access_token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := clientFor(t, c.s)
			err := cl.RejectIssue(context.Background(), "i-1", "FALSE_POSITIVE", "n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("RejectIssue = %v, want %q", err, c.want)
			}
		})
	}
}

func TestMutationFailures(t *testing.T) {
	const tok = `{"access_token":"tok","expires_in":3600}`
	cases := []struct {
		name string
		s    *scripted
		want string
	}{
		{"answer not JSON", &scripted{tokenBody: tok, gqlBody: "nope"}, "decode response"},
		{"several GraphQL errors", &scripted{tokenBody: tok,
			gqlBody: `{"errors":[{"message":"first"},{"message":"second"}]}`}, "first; second"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := clientFor(t, c.s)
			err := cl.RejectIssue(context.Background(), "i-1", "FALSE_POSITIVE", "n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("RejectIssue = %v, want %q", err, c.want)
			}
		})
	}
}

// TestShortLivedTokenIsRefetched: a token inside the expiry slack is never
// reused.
func TestShortLivedTokenIsRefetched(t *testing.T) {
	s := &scripted{tokenBody: `{"access_token":"tok","expires_in":1}`, gqlBody: `{"data":{}}`}
	cl := clientFor(t, s)
	for range 2 {
		if err := cl.RejectIssue(context.Background(), "i-1", "FALSE_POSITIVE", ""); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenCalls != 2 {
		t.Errorf("token calls = %d, want 2 (a nearly expired token is refetched)", s.tokenCalls)
	}
}

type failingTransport struct{ fail func(*http.Request) bool }

func (f failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.fail(r) {
		return nil, errors.New("connection reset")
	}
	body := `{"access_token":"tok","expires_in":3600}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{}}, nil
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("body cut") }
func (brokenBody) Close() error             { return nil }

func TestTransportFailures(t *testing.T) {
	newClient := func(rt http.RoundTripper, endpoint, tokenURL string) *Client {
		c, err := New(Options{Endpoint: endpoint, TokenURL: tokenURL, ClientID: "id", ClientSecret: "s",
			HTTPClient: &http.Client{Transport: rt}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := []struct {
		name string
		c    *Client
		want string
	}{
		{"token request fails", newClient(failingTransport{fail: func(*http.Request) bool { return true }},
			"https://wiz.test/graphql", "https://auth.wiz.test/oauth/token"), "fetch token"},
		{"mutation request fails", newClient(failingTransport{fail: func(r *http.Request) bool {
			return r.URL.Path == "/graphql"
		}}, "https://wiz.test/graphql", "https://auth.wiz.test/oauth/token"), "post mutation"},
		{"token body cut", newClient(roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: brokenBody{}, Header: http.Header{}}, nil
		}), "https://wiz.test/graphql", "https://auth.wiz.test/oauth/token"), "read token response"},
		{"mutation body cut", newClient(roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/graphql" {
				return &http.Response{StatusCode: http.StatusOK, Body: brokenBody{}, Header: http.Header{}}, nil
			}
			return failingTransport{fail: func(*http.Request) bool { return false }}.RoundTrip(r)
		}), "https://wiz.test/graphql", "https://auth.wiz.test/oauth/token"), "read response"},
		{"unbuildable token URL", newClient(failingTransport{fail: func(*http.Request) bool { return false }},
			"https://wiz.test/graphql", "https://auth.wiz.test/\x7f"), "build token request"},
		{"unbuildable endpoint", newClient(failingTransport{fail: func(*http.Request) bool { return false }},
			"https://wiz.test/\x7f", "https://auth.wiz.test/oauth/token"), "build request"},
	}
	for _, c := range cases {
		err := c.c.RejectIssue(context.Background(), "i-1", "FALSE_POSITIVE", "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: RejectIssue = %v, want %q", c.name, err, c.want)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewDefaults(t *testing.T) {
	c, err := New(Options{Endpoint: "https://wiz.test/graphql", ClientID: "id", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if c.opts.TokenURL != DefaultTokenURL {
		t.Errorf("token URL = %q, want the default", c.opts.TokenURL)
	}
	if c.http == nil || c.http.Timeout == 0 {
		t.Error("default HTTP client has no timeout")
	}
}

func TestSummarize(t *testing.T) {
	if got := summarize([]byte("  short  ")); got != "short" {
		t.Errorf("summarize(short) = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := summarize([]byte(long)); len(got) != 256+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("summarize(long) = %d chars", len(got))
	}
}
