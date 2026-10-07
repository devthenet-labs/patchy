// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"fmt"
	"strings"
	"testing"
)

func TestSlotOf(t *testing.T) {
	for _, tc := range []struct {
		id    string
		count int
		slot  int
		ok    bool
	}{
		{"patchy-preview-s0", 1, 0, true},
		{"patchy-preview-s3", 4, 3, true},
		{"patchy-preview-s3", 3, 0, false},
		{"patchy-preview-s4", 4, 0, false},
		{"patchy-preview-s0", 0, 0, false},
		{"patchy-preview-s0", 5, 0, false},
		{"patchy-preview-s01", 4, 0, false},
		{"patchy-preview-s+1", 4, 0, false},
		{"patchy-preview-s-1", 4, 0, false},
		{"patchy-preview-s 1", 4, 0, false},
		{"patchy-preview-s1 ", 4, 0, false},
		{"patchy-preview-s", 4, 0, false},
		{"Patchy-preview-s1", 4, 0, false},
		{"patchy-preview-auth", 4, 0, false},
		{"", 4, 0, false},
	} {
		slot, ok := SlotOf(tc.id, tc.count)
		if ok != tc.ok || slot != tc.slot {
			t.Errorf("SlotOf(%q, %d) = %d, %v; want %d, %v", tc.id, tc.count, slot, ok, tc.slot, tc.ok)
		}
	}
	for s := range MaxSlots {
		if got, ok := SlotOf(ClientID(s), MaxSlots); !ok || got != s {
			t.Errorf("SlotOf(ClientID(%d)) = %d, %v", s, got, ok)
		}
		if ALBCookieName(s) != "patchy-preview-s"+string(rune('0'+s)) {
			t.Errorf("ALBCookieName(%d) = %q", s, ALBCookieName(s))
		}
	}
}

// TestClientSecretVector pins the construction the chart reproduces with
// sha256sum. The value was computed outside Go:
//
//	printf '%s\0patchy-preview-auth/v1/client-secret\0%d\0%d' "$master" 3 1 | shasum -a 256
func TestClientSecretVector(t *testing.T) {
	r := newRing(t, 3, masterA)
	const want = "7b22d9ebe2b0aab50bad79a1d6781f9d48bcb2ab1c44a5e38afa1675dc2d4eb9"
	if got := r.ClientSecret(1); got != want {
		t.Errorf("ClientSecret(1) = %s, want %s", got, want)
	}
}

func TestClientSecretsAreDistinct(t *testing.T) {
	a, b := newRing(t, 1, masterA), newRing(t, 2, masterA)
	seen := map[string]string{}
	for _, r := range []*KeyRing{a, b, newRing(t, 1, masterB)} {
		for s := range MaxSlots {
			sec := r.ClientSecret(s)
			if len(sec) != 64 || strings.Trim(sec, "0123456789abcdef") != "" {
				t.Fatalf("secret %q is not 64 lowercase hex characters", sec)
			}
			name := fmt.Sprintf("%.4s/g%d/s%d", r.current.master, r.Generation(), s)
			if prev, dup := seen[sec]; dup {
				t.Errorf("%s and %s share a client secret", prev, name)
			}
			seen[sec] = name
		}
	}
}

func TestVerifyClientSecret(t *testing.T) {
	old := newRing(t, 1, masterA)
	cur := newRing(t, 2, masterB)
	rotated := newRotatedRing(t)
	for _, tc := range []struct {
		name string
		ring *KeyRing
		slot int
		pres string
		want bool
	}{
		{"current", cur, 1, cur.ClientSecret(1), true},
		{"another slot's secret", cur, 0, cur.ClientSecret(1), false},
		{"old generation without overlap", cur, 1, old.ClientSecret(1), false},
		{"current during overlap", rotated, 1, cur.ClientSecret(1), true},
		{"previous during overlap", rotated, 1, old.ClientSecret(1), true},
		{"previous, another slot", rotated, 2, old.ClientSecret(1), false},
		{"empty", cur, 1, "", false},
		{"upper case", cur, 1, strings.ToUpper(cur.ClientSecret(1)), false},
		{"prefix", cur, 1, cur.ClientSecret(1)[:63], false},
		{"slot out of range", cur, MaxSlots, cur.ClientSecret(MaxSlots), false},
		{"negative slot", cur, -1, cur.ClientSecret(-1), false},
	} {
		if got := tc.ring.VerifyClientSecret(tc.slot, tc.pres); got != tc.want {
			t.Errorf("%s: VerifyClientSecret = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNewKeyRing(t *testing.T) {
	short := strings.Repeat("x", MinMasterBytes-1)
	for _, tc := range []struct {
		name string
		cur  Generation
		prev *Generation
		ok   bool
	}{
		{"current only", Generation{1, []byte(masterA)}, nil, true},
		{"generation zero", Generation{0, []byte(masterA)}, nil, true},
		{"with previous", Generation{2, []byte(masterB)}, &Generation{1, []byte(masterA)}, true},
		{"short master", Generation{1, []byte(short)}, nil, false},
		{"negative generation", Generation{-1, []byte(masterA)}, nil, false},
		{"generation too large", Generation{MaxGeneration + 1, []byte(masterA)}, nil, false},
		{"same number", Generation{1, []byte(masterB)}, &Generation{1, []byte(masterA)}, false},
		{"same master", Generation{2, []byte(masterA)}, &Generation{1, []byte(masterA)}, false},
		{"short previous", Generation{2, []byte(masterB)}, &Generation{1, []byte(short)}, false},
	} {
		_, err := NewKeyRing(tc.cur, tc.prev)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

func TestKeyRingCopiesTheMaster(t *testing.T) {
	m := []byte(masterA)
	r, err := NewKeyRing(Generation{Number: 1, Master: m}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := r.ClientSecret(0)
	m[0] ^= 1
	if r.ClientSecret(0) != before {
		t.Error("changing the caller's master slice changed the ring's client secret")
	}
}
