// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// TestPairwiseSubProperty: the subject is stable for one viewer on one
// Preview, differs when the client (slot), Preview UID, label or viewer
// differs, is 43 base64url characters and never contains the username.
func TestPairwiseSubProperty(t *testing.T) {
	r := newRing(t, 1, masterA)
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		args[0] = reflect.ValueOf(rnd.Intn(MaxSlots))
		args[1] = reflect.ValueOf(randString(rnd, 1+rnd.Intn(36)))
		args[2] = reflect.ValueOf(randLabel(rnd))
		args[3] = reflect.ValueOf("github:" + randString(rnd, 1+rnd.Intn(20)))
		args[4] = reflect.ValueOf(rnd.Intn(4))
	}
	prop := func(slot int, uid, label, user string, change int) bool {
		sub := r.Sub(ClientID(slot), uid, label, user)
		if sub != r.Sub(ClientID(slot), uid, label, user) || len(sub) != 43 || !validChallenge(sub) {
			return false
		}
		if login := strings.TrimPrefix(user, "github:"); len(login) >= 8 && strings.Contains(sub, login) {
			return false
		}
		var other string
		switch change {
		case 0:
			other = r.Sub(ClientID((slot+1)%MaxSlots), uid, label, user)
		case 1:
			other = r.Sub(ClientID(slot), uid+"x", label, user)
		case 2:
			other = r.Sub(ClientID(slot), uid, label+"x", user)
		case 3:
			other = r.Sub(ClientID(slot), uid, label, user+"x")
		}
		return other != sub
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestPairwiseSubNoCollisions: one viewer across 10^4 distinct Previews gets
// 10^4 distinct subjects.
func TestPairwiseSubNoCollisions(t *testing.T) {
	r := newRing(t, 1, masterA)
	rnd := rand.New(rand.NewSource(seed))
	seen := map[string]bool{}
	for i := range 10000 {
		slot := i % MaxSlots
		sub := r.Sub(ClientID(slot), randString(rnd, 36), randLabel(rnd), "github:alice")
		if seen[sub] {
			t.Fatalf("subject collision after %d samples", i)
		}
		seen[sub] = true
	}
}

// TestPairwiseSubFieldsAreLengthPrefixed: moving bytes between adjacent
// fields changes the subject; a separator-joined encoding would not.
func TestPairwiseSubFieldsAreLengthPrefixed(t *testing.T) {
	r := newRing(t, 1, masterA)
	if r.Sub(ClientID(0), "uid", "ab", "c") == r.Sub(ClientID(0), "uid", "a", "bc") {
		t.Error("moving a byte from the label to the username kept the subject")
	}
	if r.Sub(ClientID(0), "uid\x00a", "b", "c") == r.Sub(ClientID(0), "uid", "a\x00b", "c") {
		t.Error("a NUL in a field let two inputs share a subject")
	}
}

// TestPairwiseSubDependsOnTheKey: another master gives another subject, so
// the subject reveals nothing without the relay's key.
func TestPairwiseSubDependsOnTheKey(t *testing.T) {
	a, b := newRing(t, 1, masterA), newRing(t, 1, masterB)
	if a.Sub(ClientID(0), "u", "l", "github:alice") == b.Sub(ClientID(0), "u", "l", "github:alice") {
		t.Error("two masters gave the same subject")
	}
}
