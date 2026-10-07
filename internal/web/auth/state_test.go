// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"strings"
	"testing"
	"time"
)

func TestSealUnsealRoundTrip(t *testing.T) {
	key, err := sealKey("client-secret", "test")
	if err != nil {
		t.Fatalf("sealKey: %v", err)
	}
	in := loginState{Verifier: "v", CSRF: "c", Nonce: "n", OriginalPath: "/finding/x"}
	blob, err := seal(key, in)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	var out loginState
	if err := unseal(key, blob, &out); err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestUnsealRejectsBadInput(t *testing.T) {
	key, _ := sealKey("client-secret", "test")
	otherKey, _ := sealKey("rotated-secret", "test")
	blob, err := seal(key, loginState{CSRF: "c"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	tampered := blob[:len(blob)-2] + "zz"

	cases := []struct {
		name string
		key  []byte
		blob string
	}{
		{"tampered ciphertext", key, tampered},
		{"rotated key", otherKey, blob},
		{"not base64", key, "!!!"},
		{"too short", key, "aaaa"},
		{"empty", key, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out loginState
			if err := unseal(tc.key, tc.blob, &out); err == nil {
				t.Error("unseal accepted bad input")
			}
		})
	}
}

func TestSealKeyDomainSeparation(t *testing.T) {
	a, _ := sealKey("secret", "session")
	b, _ := sealKey("secret", "other")
	if string(a) == string(b) {
		t.Error("different info strings derived the same key")
	}
}

func TestRandomToken(t *testing.T) {
	a, err1 := randomToken()
	b, err2 := randomToken()
	if err1 != nil || err2 != nil {
		t.Fatalf("randomToken: %v/%v", err1, err2)
	}
	if a == b || len(a) < 40 || strings.ContainsAny(a, "+/=") {
		t.Errorf("tokens %q/%q not distinct URL-safe strings", a, b)
	}
}

// TestUnsealBlobsSealedBeforeTheMove pins the wire format: blobs sealed by
// the code before the primitives moved to internal/sealed (HKDF-SHA256 key
// with no salt, base64url(nonce||ciphertext||tag), no additional data) must
// still open, or every signed-in dashboard user is signed out on upgrade.
// The blobs were made by that code with fixed nonces; never regenerate them
// with the current code.
func TestUnsealBlobsSealedBeforeTheMove(t *testing.T) {
	key, err := sealKey("client-secret", "patchy-status-session")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const sessionBlob = "MDEyMzQ1Njc4OWFiwKnFk1xA44uX5eh785UOxMZ-BR8K5WW_WomxHTVlKBXVORBdK-" +
		"nV363Pqia4urXLoARqs6BYo_buh_rOgV202B0-z-7qaiT1UapmNjQOeVr8sMFzepQkpPKQHX65WDRuL7kpZQ"
	var s session
	if err := unseal(key, sessionBlob, &s); err != nil {
		t.Fatalf("unseal session: %v", err)
	}
	if want := (session{IDToken: "eyJ.id.token", RefreshToken: "refresh-1", Start: start}); s != want {
		t.Errorf("session = %+v, want %+v", s, want)
	}
	const stateBlob = "YmE5ODc2NTQzMjEwxmnanQB69AXmvEpXFMsMk2sztYMohS-IO-5W00HZVK_DQ_2NXFSPKSeK0w8HVsZaLxuMo" +
		"AHRhvVCtX_coRWa7vR9jFxOvKWMK40XHV1lq04_DtnTrKBKonlKbo9UfleV4sC9YUPTGZSAb4O6tS77ESTxHiKu0-qj1oNEg2_0JQY1Yq4"
	var ls loginState
	if err := unseal(key, stateBlob, &ls); err != nil {
		t.Fatalf("unseal login state: %v", err)
	}
	want := loginState{Verifier: "verifier", CSRF: "csrf", Nonce: "nonce", OriginalPath: "/finding/x", IssuedAt: start}
	if ls != want {
		t.Errorf("login state = %+v, want %+v", ls, want)
	}
}
