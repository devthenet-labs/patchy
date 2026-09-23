// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), "http://artifacts.local")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestPutGetServe(t *testing.T) {
	s := newStore(t)
	body := "tarball-bytes"
	info, err := s.Put("patchy/finding-1-src", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	sum := sha256.Sum256([]byte(body))
	if info.Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("Put digest = %q, want sha256 of body", info.Digest)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Put size = %d, want %d", info.Size, len(body))
	}
	if !strings.HasPrefix(info.URL, "http://artifacts.local/artifacts/") || !strings.HasSuffix(info.URL, ".tar.gz") {
		t.Errorf("Put url = %q, want http://artifacts.local/artifacts/<id>.tar.gz", info.URL)
	}

	got, ok := s.Get("patchy/finding-1-src")
	if !ok || got.Digest != info.Digest || got.URL != info.URL {
		t.Errorf("Get = %+v (ok=%v), want %+v", got, ok, info)
	}

	path := strings.TrimPrefix(info.URL, "http://artifacts.local")
	req := httptest.NewRequest("GET", path, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != body {
		t.Errorf("GET %s = %d %q, want 200 %q", path, rr.Code, rr.Body.String(), body)
	}
}

func TestPutReplacesAndInvalidatesOldURL(t *testing.T) {
	s := newStore(t)
	first, err := s.Put("k", strings.NewReader("v1"))
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	second, err := s.Put("k", strings.NewReader("v2"))
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	if first.URL == second.URL {
		t.Errorf("replacement kept URL %q, want a fresh id", first.URL)
	}

	oldPath := strings.TrimPrefix(first.URL, "http://artifacts.local")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", oldPath, nil))
	if rr.Code != 404 {
		t.Errorf("GET replaced artifact = %d, want 404", rr.Code)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	info, err := s.Put("k", strings.NewReader("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Delete("k")
	if _, ok := s.Get("k"); ok {
		t.Error("Get after Delete reported present")
	}
	rr := httptest.NewRecorder()
	path := strings.TrimPrefix(info.URL, "http://artifacts.local")
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	if rr.Code != 404 {
		t.Errorf("GET deleted artifact = %d, want 404", rr.Code)
	}
}

func TestUnknownIDIs404(t *testing.T) {
	s := newStore(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/artifacts/deadbeef.tar.gz", nil))
	if rr.Code != 404 {
		t.Errorf("GET unknown artifact = %d, want 404", rr.Code)
	}
}

func TestOpenRoundTrip(t *testing.T) {
	s := newStore(t)
	body := "tarball-bytes-for-open"
	if _, err := s.Put("k", strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Open("k")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Errorf("Close: %v", cerr)
	}
	if err != nil || string(got) != body {
		t.Errorf("Open read = %q, %v; want %q", got, err, body)
	}

	if _, err := s.Open("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(missing) = %v, want fs.ErrNotExist", err)
	}
	s.Delete("k")
	if _, err := s.Open("k"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open after Delete = %v, want fs.ErrNotExist", err)
	}
}
