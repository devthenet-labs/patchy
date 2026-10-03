// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import "testing"

// TestPortPoolNeverRepeats: a probe that finds the same free port twice —
// as the kernel did in CI, handing integration-controller 127.0.0.1:40023
// as both its webhook and its health address — does not hand it out twice;
// the pool probes again until it finds a port it has not given anyone.
func TestPortPoolNeverRepeats(t *testing.T) {
	found := []int{40023, 40023, 40023, 40024, 40023, 40025}
	pool := &portPool{probe: func() (int, error) {
		port := found[0]
		found = found[1:]
		return port, nil
	}}
	for _, want := range []int{40023, 40024, 40025} {
		got, err := pool.next()
		if err != nil || got != want {
			t.Fatalf("next() = %d, %v; want %d", got, err, want)
		}
	}
}

// TestPortPoolGivesUp: a probe that only ever finds ports already handed
// out fails rather than looping forever.
func TestPortPoolGivesUp(t *testing.T) {
	pool := &portPool{probe: func() (int, error) { return 40023, nil }}
	if _, err := pool.next(); err != nil {
		t.Fatalf("first next() = %v", err)
	}
	if port, err := pool.next(); err == nil {
		t.Fatalf("next() = %d, want an error once every probe repeats", port)
	}
}
