// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261007

const (
	testSuffix = "preview.example.com"
	testIssuer = "https://preview-auth.example.com"
	masterA    = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ01"
	masterB    = "ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210zyxwvutsrqponmlkjihgfedcba10"
)

// t0 is a fixed instant every example starts from.
var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func newRing(t testing.TB, gen int, master string) *KeyRing {
	t.Helper()
	r, err := NewKeyRing(Generation{Number: gen, Master: []byte(master)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newRotatedRing(t testing.TB) *KeyRing {
	t.Helper()
	r, err := NewKeyRing(Generation{Number: 2, Master: []byte(masterB)}, &Generation{Number: 1, Master: []byte(masterA)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newCallbacks(t testing.TB) Callbacks {
	t.Helper()
	cb, err := NewCallbacks(testSuffix)
	if err != nil {
		t.Fatal(err)
	}
	return cb
}

// randLabel generates a valid host label of 1 to 63 characters.
func randLabel(r *rand.Rand) string {
	const edge = "abcdefghijklmnopqrstuvwxyz0123456789"
	const inner = edge + "-"
	n := 1 + r.Intn(63)
	if r.Intn(4) == 0 {
		n = 1 + r.Intn(12)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		set := inner
		if i == 0 || i == n-1 {
			set = edge
		}
		c := set[r.Intn(len(set))]
		if i == 3 && b.String()[2] == '-' && c == '-' {
			c = 'a' // never an IDN-reserved "--" in positions 3 and 4
		}
		b.WriteByte(c)
	}
	return b.String()
}

func liveView(uid, label string, slot int) View {
	return View{UID: uid, Label: label, Project: "demo", Slot: slot, Live: true}
}

func testSession(t testing.TB) Session {
	t.Helper()
	s, err := NewSession(t0, DefaultLifetimes(), Identity{Username: "github:alice", Groups: []string{"github:org:team"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testRequest(t testing.TB, label string, slot int) AuthorizeRequest {
	t.Helper()
	ru, err := newCallbacks(t).URL(label)
	if err != nil {
		t.Fatal(err)
	}
	return AuthorizeRequest{ClientID: ClientID(slot), Slot: slot, RedirectURI: ru, Label: label, State: "alb-state"}
}
