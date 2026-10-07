// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// payload stands in for a caller's sealed struct.
type payload struct {
	Kind string   `json:"k"`
	N    int      `json:"n"`
	List []string `json:"l,omitempty"`
}

func mustKey(t *testing.T, secret, info string) []byte {
	t.Helper()
	k, err := PurposeKey([]byte(secret), info)
	if err != nil {
		t.Fatalf("PurposeKey: %v", err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	key := mustKey(t, "master", "patchy-test/v1/session")
	in := payload{Kind: "session", N: 7, List: []string{"github:acme:team"}}
	for _, aad := range [][]byte{nil, {}, []byte("pa1|s|3")} {
		blob, err := Seal(key, aad, in)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if strings.ContainsAny(blob, "+/=") {
			t.Errorf("blob %q is not unpadded base64url", blob)
		}
		var out payload
		if err := Open(key, aad, blob, &out); err != nil {
			t.Fatalf("Open(aad %q): %v", aad, err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("round trip = %+v, want %+v", out, in)
		}
	}
}

// Two seals of the same value differ: the nonce is fresh each time.
func TestSealIsRandomised(t *testing.T) {
	key := mustKey(t, "master", "p")
	a, _ := Seal(key, nil, payload{N: 1})
	b, _ := Seal(key, nil, payload{N: 1})
	if a == b {
		t.Error("two seals of one value are identical")
	}
}

func TestOpenRejects(t *testing.T) {
	key := mustKey(t, "master", "patchy-test/v1/access")
	other := mustKey(t, "master", "patchy-test/v1/refresh")
	rotated := mustKey(t, "rotated", "patchy-test/v1/access")
	aad := []byte("pa1|a|1")
	blob, err := Seal(key, aad, payload{Kind: "access"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(blob)
	flipped := bytes.Clone(raw)
	flipped[len(flipped)-1] ^= 1
	cases := []struct {
		name string
		key  []byte
		aad  []byte
		blob string
	}{
		{"another purpose's key", other, aad, blob},
		{"a rotated secret", rotated, aad, blob},
		{"another kind in the aad", key, []byte("pa1|r|1"), blob},
		{"another generation in the aad", key, []byte("pa1|a|2"), blob},
		{"no aad", key, nil, blob},
		{"a flipped tag bit", key, aad, base64.RawURLEncoding.EncodeToString(flipped)},
		{"truncated", key, aad, blob[:len(blob)-4]},
		{"a padding character", key, aad, blob + "="},
		{"standard base64 alphabet", key, aad, strings.NewReplacer("-", "+", "_", "/").Replace(blob) + "+/"},
		{"not base64", key, aad, "!!!"},
		{"shorter than a nonce", key, aad, "aaaa"},
		{"nonce and no tag", key, aad, base64.RawURLEncoding.EncodeToString(raw[:12])},
		{"empty", key, aad, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out payload
			if err := Open(tc.key, tc.aad, tc.blob, &out); !errors.Is(err, ErrOpen) {
				t.Errorf("Open = %v, want ErrOpen", err)
			}
		})
	}
}

// An authentic blob of another shape opens but does not decode, and says so
// with an error that is not ErrOpen.
func TestOpenShapeMismatch(t *testing.T) {
	key := mustKey(t, "master", "p")
	blob, err := Seal(key, nil, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	err = Open(key, nil, blob, &out)
	if err == nil || errors.Is(err, ErrOpen) {
		t.Errorf("Open = %v, want a JSON error that is not ErrOpen", err)
	}
}

func TestKeySizeEnforced(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33, 64} {
		key := make([]byte, n)
		if _, err := SealBytes(key, nil, []byte("x")); err == nil {
			t.Errorf("SealBytes accepted a %d-byte key", n)
		}
		if _, err := OpenBytes(key, nil, "aaaa"); err == nil || errors.Is(err, ErrOpen) {
			t.Errorf("OpenBytes with a %d-byte key = %v, want a key error", n, err)
		}
	}
}

func TestPurposeKey(t *testing.T) {
	a := mustKey(t, "secret", "session")
	b := mustKey(t, "secret", "other")
	c := mustKey(t, "other-secret", "session")
	if bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Error("distinct inputs derived the same key")
	}
	if len(a) != KeySize {
		t.Errorf("key is %d bytes, want %d", len(a), KeySize)
	}
	if !bytes.Equal(a, mustKey(t, "secret", "session")) {
		t.Error("derivation is not deterministic")
	}
	if _, err := PurposeKey(nil, "session"); err == nil {
		t.Error("an empty secret derived a key")
	}
	if _, err := PurposeKey([]byte("secret"), ""); err == nil {
		t.Error("an empty purpose derived a key")
	}
}

// The derivation is HKDF-SHA256 with no salt, pinned by vector so a refactor
// cannot silently change every key (and with it every live session). The
// vector is web/auth's existing derivation for its status-session key,
// computed independently from RFC 5869 (HMAC-SHA256, zero salt).
func TestPurposeKeyVector(t *testing.T) {
	got := hex.EncodeToString(mustKey(t, "client-secret", "patchy-status-session"))
	const want = "a3b176501d8f4950c51c9218247e83f906a270462c6086f46ccd5163ce2e9bbe"
	if got != want {
		t.Errorf("PurposeKey vector = %s, want %s", got, want)
	}
}

func TestRandomToken(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		tok, err := RandomToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 || strings.ContainsAny(tok, "+/=") {
			t.Errorf("token %q is not 43 unpadded base64url characters", tok)
		}
		if seen[tok] {
			t.Errorf("token %q repeated", tok)
		}
		seen[tok] = true
	}
}

// propertyConfig is seeded so the gate stays deterministic.
func propertyConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(20261007))}
}

// Any plaintext under any AAD round-trips.
func TestPropertyRoundTrip(t *testing.T) {
	key := make([]byte, KeySize)
	prop := func(seed [KeySize]byte, aad, plain []byte) bool {
		copy(key, seed[:])
		blob, err := SealBytes(key, aad, plain)
		if err != nil {
			return false
		}
		got, err := OpenBytes(key, aad, blob)
		return err == nil && bytes.Equal(got, plain)
	}
	if err := quick.Check(prop, propertyConfig(2000)); err != nil {
		t.Error(err)
	}
}

// Flipping any single bit of the sealed bytes, or changing the AAD in any
// way, makes the blob fail to open.
func TestPropertyTamper(t *testing.T) {
	key := mustKey(t, "master", "tamper")
	prop := func(aad, plain []byte, pos uint16, bit uint8, extra byte) bool {
		blob, err := SealBytes(key, aad, plain)
		if err != nil {
			return false
		}
		raw, _ := base64.RawURLEncoding.DecodeString(blob)
		raw[int(pos)%len(raw)] ^= 1 << (bit % 8)
		if _, err := OpenBytes(key, aad, base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrOpen) {
			return false
		}
		// The AAD with one more byte, and (when it has one) with its first
		// byte changed, are both different AADs.
		if _, err := OpenBytes(key, append(bytes.Clone(aad), extra), blob); !errors.Is(err, ErrOpen) {
			return false
		}
		if len(aad) > 0 {
			changed := bytes.Clone(aad)
			changed[0] ^= 0x80
			if _, err := OpenBytes(key, changed, blob); !errors.Is(err, ErrOpen) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, propertyConfig(2000)); err != nil {
		t.Error(err)
	}
}

// Keys for two different purposes never open each other's blobs.
func TestPropertyPurposeSeparation(t *testing.T) {
	prop := func(secret []byte, a, b string, plain []byte) bool {
		if len(secret) == 0 || a == "" || b == "" || a == b {
			return true
		}
		ka, err1 := PurposeKey(secret, a)
		kb, err2 := PurposeKey(secret, b)
		if err1 != nil || err2 != nil {
			return false
		}
		blob, err := SealBytes(ka, nil, plain)
		if err != nil {
			return false
		}
		_, err = OpenBytes(kb, nil, blob)
		return errors.Is(err, ErrOpen)
	}
	if err := quick.Check(prop, propertyConfig(1000)); err != nil {
		t.Error(err)
	}
}
