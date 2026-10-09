// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFake starts a fake GitHub API server around a fresh mux. Pointing a
// client at a non-github host makes WithEnterpriseURLs append /api/v3/ to
// the base URL, so the server strips that prefix before dispatching and
// handlers register bare REST paths ("GET /repos/o/r/issues").
func newFake(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.StripPrefix("/api/v3", mux))
	t.Cleanup(srv.Close)
	return mux, srv.URL
}

// newFakeClient is newFake plus a PAT-authenticated Client pointed at it.
func newFakeClient(t *testing.T) (*http.ServeMux, *Client) {
	t.Helper()
	mux, base := newFake(t)
	c, err := NewToken("pat-token", base)
	if err != nil {
		t.Fatalf("NewToken() error = %v", err)
	}
	return mux, c
}

// writeJSON answers with a 200 and the given JSON body.
func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// decodeBody parses the request body into T for assertions. It runs on a
// server goroutine, so failures are t.Errorf, never t.Fatalf.
func decodeBody[T any](t *testing.T, r *http.Request) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Errorf("decode request body: %v", err)
	}
	return v
}

// wantAuth asserts the request's Authorization header value.
func wantAuth(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

// pagedHandler serves page1 with a rel="next" Link header pointing at
// ?page=2, and page2 as the terminal page.
func pagedHandler(t *testing.T, page1, page2 string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(t, w, page2)
			return
		}
		w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
		writeJSON(t, w, page1)
	}
}
