// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"connected", nil, "REACHABLE"},
		{"timeout", os.ErrDeadlineExceeded, "blocked"},
		{"no-route", syscall.ENETUNREACH, "blocked"},
		{"permission", syscall.EPERM, "blocked"},
		{"refused-is-not-proof", syscall.ECONNREFUSED, "inconclusive"},
		{"wrapped-refusal", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestTargets(t *testing.T) {
	required := map[string]string{
		"PROBE_KUBERNETES_API": "172.20.0.1", "PROBE_EGRESS_BROKER": "172.20.10.1",
		"PROBE_INTEGRATION_CONTROLLER": "172.20.10.2", "PROBE_SOURCE_CONTROLLER": "172.20.10.3",
		"PROBE_STATUS_SERVER": "172.20.10.4",
	}
	siblings := map[string]string{
		"PROBE_SIBLING_POD": "10.40.32.7", "PROBE_SIBLING_SERVICE": "172.20.99.9", "PROBE_OTHER_SLOT_POD": "10.40.33.8",
	}
	env := func(sets ...map[string]string) func(string) string {
		merged := map[string]string{}
		for _, s := range sets {
			for k, v := range s {
				merged[k] = v
			}
		}
		return func(k string) string { return merged[k] }
	}
	without := func(m map[string]string, key string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		want   []string // target addresses, or nil when inconclusive
	}{
		{"the eight slice-2 targets", env(required), []string{
			"169.254.169.254:80", "169.254.170.23:80", "1.1.1.1:443", "172.20.0.1:443", "172.20.10.1:8080",
			"172.20.10.2:8080", "172.20.10.3:9790", "172.20.10.4:8080",
		}},
		{"with the sibling targets", env(required, siblings), []string{
			"169.254.169.254:80", "169.254.170.23:80", "1.1.1.1:443", "172.20.0.1:443", "172.20.10.1:8080",
			"172.20.10.2:8080", "172.20.10.3:9790", "172.20.10.4:8080",
			"10.40.32.7:8080", "172.20.99.9:80", "10.40.33.8:8080",
		}},
		{"a required target missing", env(without(required, "PROBE_STATUS_SERVER"), siblings), nil},
		{"one sibling target missing", env(required, without(siblings, "PROBE_OTHER_SLOT_POD")), nil},
		{"a sibling target that is not an IPv4 address", env(required, siblings,
			map[string]string{"PROBE_SIBLING_POD": "sibling.patchy-preview-0"}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := targetsFrom(tc.getenv)
			if ok != (tc.want != nil) {
				t.Fatalf("ok = %v, want %v", ok, tc.want != nil)
			}
			var addrs []string
			for _, target := range got {
				addrs = append(addrs, target.addr)
			}
			if fmt.Sprint(addrs) != fmt.Sprint(tc.want) {
				t.Errorf("targets = %v, want %v", addrs, tc.want)
			}
		})
	}
}

func TestReadStatusCodeDoesNotReadTokenBody(t *testing.T) {
	const statusLine = "HTTP/1.1 200 OK\r\n"
	const rest = "Content-Length: 12\r\n\r\nsecret-token"
	r := bytes.NewReader([]byte(statusLine + rest))
	code, err := readStatusCode(r)
	if err != nil || code != 200 || r.Len() != len(rest) {
		t.Fatalf("readStatusCode = %d, %v; %d unread bytes, want %d", code, err, r.Len(), len(rest))
	}
}

func TestReadStatusCodeRejectsMalformed(t *testing.T) {
	for _, line := range []string{"oops\n", "HTTP/1.1 nope\n", strings.Repeat("x", 129)} {
		if _, err := readStatusCode(strings.NewReader(line)); err == nil {
			t.Fatalf("accepted malformed status %q", line)
		}
	}
}
