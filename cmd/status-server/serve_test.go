// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

// The intents views come up only under an auth posture that can serve them:
// oidc with both prefixes, or mode none behind the explicit development flag
// on a loopback address. Every other posture refuses to start.
func TestIntentsAuthGate(t *testing.T) {
	oidc := func(cl auth.ClaimsConfig) *auth.Config {
		return &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: cl}}
	}
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:"}
	cases := []struct {
		name     string
		cfg      *auth.Config
		dev      bool
		listen   string
		wantErr  string
		wantFull bool
	}{
		{"unconfigured", nil, false, ":8080", "none is configured", false},
		{"mode none", &auth.Config{Mode: auth.ModeNone}, false, "127.0.0.1:8080", "refuses auth mode none", false},
		{"mode none with the dev flag on every interface", &auth.Config{Mode: auth.ModeNone}, true, ":8080",
			"loopback", false},
		{"mode none with the dev flag on a public address", &auth.Config{Mode: auth.ModeNone}, true,
			"10.0.0.5:8080", "loopback", false},
		{"mode none with the dev flag on loopback", &auth.Config{Mode: auth.ModeNone}, true, "127.0.0.1:8080", "", true},
		{"mode none with the dev flag on localhost", &auth.Config{Mode: auth.ModeNone}, true, "localhost:8080", "", true},
		{"mode anonymous", &auth.Config{Mode: auth.ModeAnonymous}, true, "127.0.0.1:8080", "anonymous", false},
		{"oidc without prefixes", oidc(auth.ClaimsConfig{Username: "preferred_username"}), false, ":8080",
			"usernamePrefix", false},
		{"oidc with prefixes", oidc(dex), false, ":8080", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			granter, err := intentsAuthGate(tc.cfg, tc.dev, tc.listen)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("intentsAuthGate = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("intentsAuthGate: %v", err)
			}
			_, full := granter.(authz.FullProjects)
			if full != tc.wantFull {
				t.Errorf("granter = %T, want FullProjects=%v (oidc gets the access-review granter)", granter, tc.wantFull)
			}
		})
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "127.1.2.3:80": true, "[::1]:8080": true, "localhost:8080": true,
		":8080": false, "0.0.0.0:8080": false, "10.0.0.5:8080": false, "[::]:8080": false, "bogus": false,
	} {
		if got := loopback(addr); got != want {
			t.Errorf("loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
