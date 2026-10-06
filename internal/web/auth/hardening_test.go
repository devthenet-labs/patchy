// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// The prefixes and the verified-email rule are the identity hardening the
// intents views depend on: a group named system:masters at the provider must
// never reach an access review as system:masters, and an unverified email
// must name no one.
func TestMapClaimsHardening(t *testing.T) {
	dex := ClaimsConfig{
		Username: "preferred_username", Groups: "groups", DisplayName: "name",
		UsernamePrefix: "github:", GroupsPrefix: "github:",
	}
	email := ClaimsConfig{
		Username: "email", Groups: "groups", DisplayName: "name",
		UsernamePrefix: "oidc:", GroupsPrefix: "oidc:", RequireVerifiedEmail: true,
	}
	cases := []struct {
		name    string
		claims  map[string]any
		cfg     ClaimsConfig
		want    *Identity
		wantErr bool
	}{
		{
			name: "dex github logins and org:team groups are prefixed",
			claims: map[string]any{
				"preferred_username": "octocat", "name": "The Octocat",
				"groups": []any{"acme:security", "acme:platform"},
			},
			cfg: dex,
			want: &Identity{
				Username: "github:octocat", DisplayName: "The Octocat",
				Groups: []string{"github:acme:security", "github:acme:platform"}, Session: true,
			},
		},
		{
			name:   "a provider system group arrives prefixed",
			claims: map[string]any{"preferred_username": "mallory", "groups": []any{"system:masters"}},
			cfg:    dex,
			want: &Identity{
				Username: "github:mallory", Groups: []string{"github:system:masters"}, Session: true,
			},
		},
		{
			name:   "a single-string group claim is prefixed too",
			claims: map[string]any{"preferred_username": "octocat", "groups": "acme:security"},
			cfg:    dex,
			want:   &Identity{Username: "github:octocat", Groups: []string{"github:acme:security"}, Session: true},
		},
		{
			name:   "verified email",
			claims: map[string]any{"email": "dev@acme.test", "email_verified": true},
			cfg:    email,
			want:   &Identity{Username: "oidc:dev@acme.test", Session: true},
		},
		{
			name:   "verified email as the string some providers send",
			claims: map[string]any{"email": "dev@acme.test", "email_verified": "true"},
			cfg:    email,
			want:   &Identity{Username: "oidc:dev@acme.test", Session: true},
		},
		{
			name:    "unverified email is refused",
			claims:  map[string]any{"email": "dev@acme.test", "email_verified": false},
			cfg:     email,
			wantErr: true,
		},
		{
			name:    "missing email_verified is refused",
			claims:  map[string]any{"email": "dev@acme.test"},
			cfg:     email,
			wantErr: true,
		},
		{
			name:    "email_verified of another type is refused",
			claims:  map[string]any{"email": "dev@acme.test", "email_verified": 1},
			cfg:     email,
			wantErr: true,
		},
		{
			name:   "no prefixes configured leaves identities as they were",
			claims: map[string]any{"email": "dev@acme.test", "groups": []any{"system:masters"}},
			cfg:    ClaimsConfig{Username: "email", Groups: "groups", DisplayName: "name"},
			want:   &Identity{Username: "dev@acme.test", Groups: []string{"system:masters"}, Session: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MapClaims(tc.claims, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("MapClaims accepted %v, want refusal", tc.claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("MapClaims: %v", err)
			}
			if got.Username != tc.want.Username || got.DisplayName != tc.want.DisplayName ||
				!slices.Equal(got.Groups, tc.want.Groups) || !got.Session {
				t.Errorf("identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// For any username and groups: every mapped name carries its prefix exactly
// once more than the input did (prefixing is applied once, never skipped and
// never doubled by a value that already looks prefixed), groups keep their
// order and count less the empty ones, and with a non-system prefix no
// mapped identity is a system: one.
func TestMapClaimsPrefixProperty(t *testing.T) {
	alphabet := []string{"a", "b", ":", "system:", "github:", "-", ""}
	word := func(r *rand.Rand) string {
		var b strings.Builder
		for range r.Intn(5) {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261006)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			user := "u" + word(r)
			groups := make([]any, r.Intn(5))
			for i := range groups {
				groups[i] = word(r)
			}
			args[0] = reflect.ValueOf(user)
			args[1] = reflect.ValueOf(groups)
		},
	}
	claims := ClaimsConfig{
		Username: "preferred_username", Groups: "groups", DisplayName: "name",
		UsernamePrefix: "github:", GroupsPrefix: "github:",
	}
	prop := func(user string, groups []any) bool {
		id, err := MapClaims(map[string]any{"preferred_username": user, "groups": groups}, claims)
		if err != nil {
			return false
		}
		if id.Username != "github:"+user || strings.HasPrefix(id.Username, "system:") {
			return false
		}
		var want []string
		for _, g := range groups {
			if s := g.(string); s != "" {
				want = append(want, "github:"+s)
			}
		}
		if !slices.Equal(id.Groups, want) {
			return false
		}
		for _, g := range id.Groups {
			if strings.HasPrefix(g, "system:") {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestClaimsValidateRejectsSystemPrefixes(t *testing.T) {
	for _, cfg := range []ClaimsConfig{
		{UsernamePrefix: "system:"},
		{GroupsPrefix: "system:serviceaccounts:"},
		{UsernamePrefix: "SYSTEM:x"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted a system: prefix", cfg)
		}
	}
	for _, cfg := range []ClaimsConfig{{}, {UsernamePrefix: "github:", GroupsPrefix: "github:"}} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", cfg, err)
		}
	}
	path := writeConfig(t, "mode: oidc\noidc:\n  issuerURL: https://sso.acme.test\n  clientID: c\n"+
		"  clientSecret: s\n  claims:\n    groupsPrefix: \"system:\"\n")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "groupsPrefix") {
		t.Errorf("LoadConfig with a system: groupsPrefix = %v, want a groupsPrefix error", err)
	}
}

// The intents views refuse every posture that is not oidc with both
// prefixes, and an email username without verification.
func TestRequireForIntents(t *testing.T) {
	const base = "mode: oidc\noidc:\n  issuerURL: https://sso.acme.test\n  clientID: c\n  clientSecret: s\n"
	cases := []struct {
		name    string
		yaml    string // "" means no auth config at all
		wantErr string
	}{
		{"unconfigured", "", "none is configured"},
		{"mode none", "mode: none\n", `not "none"`},
		{"mode anonymous", "mode: anonymous\nanonymous:\n  username: viewer\n", `not "anonymous"`},
		{"oidc without prefixes", base + "  claims:\n    username: preferred_username\n", "usernamePrefix"},
		{"oidc with only the username prefix", base +
			"  claims:\n    username: preferred_username\n    usernamePrefix: \"github:\"\n", "groupsPrefix"},
		{"oidc email username unverified", base +
			"  claims:\n    usernamePrefix: \"oidc:\"\n    groupsPrefix: \"oidc:\"\n", "requireVerifiedEmail"},
		{"oidc email username verified", base +
			"  claims:\n    usernamePrefix: \"oidc:\"\n    groupsPrefix: \"oidc:\"\n    requireVerifiedEmail: true\n", ""},
		{"dex github logins", base + "  claims:\n    username: preferred_username\n" +
			"    usernamePrefix: \"github:\"\n    groupsPrefix: \"github:\"\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *Config
			if tc.yaml != "" {
				var err error
				if cfg, err = LoadConfig(writeConfig(t, tc.yaml)); err != nil {
					t.Fatalf("LoadConfig: %v", err)
				}
			}
			err := cfg.RequireForIntents()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("RequireForIntents = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("RequireForIntents = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
