// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// fixedKeychain answers every host with one authenticator.
type fixedKeychain struct {
	user  string
	calls int
}

func (k *fixedKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	k.calls++
	return &authn.Basic{Username: k.user}, nil
}

func registryOf(t *testing.T, host string) name.Registry {
	t.Helper()
	r, err := name.NewRegistry(host)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHostKeychainSelectsByHost(t *testing.T) {
	tokens := 0
	ecrKC := &ecrKeychain{
		tokens: func(_ context.Context, region string) (string, time.Time, error) {
			tokens++
			return base64.StdEncoding.EncodeToString([]byte("AWS:" + region)), time.Now().Add(12 * time.Hour), nil
		},
		now: time.Now,
	}
	google := &fixedKeychain{user: "oauth2accesstoken"}
	docker := &fixedKeychain{user: "docker"}
	kc := &hostKeychain{ecr: ecrKC, google: google, docker: docker}

	cases := []struct {
		host string
		user string
	}{
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com", "AWS"},
		{"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com", "AWS"},
		{"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn", "AWS"},
		{"gcr.io", "oauth2accesstoken"},
		{"eu.gcr.io", "oauth2accesstoken"},
		{"europe-west1-docker.pkg.dev", "oauth2accesstoken"},
		{"ghcr.io", "docker"},
		{"public.ecr.aws", "docker"},
		{"evil-amazonaws.com", "docker"},
		{"index.docker.io", "docker"},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			a, err := kc.Resolve(registryOf(t, tc.host))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			cfg, err := a.Authorization()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Username != tc.user {
				t.Errorf("username = %q, want %q", cfg.Username, tc.user)
			}
		})
	}
	if tokens != 3 {
		t.Errorf("ECR tokens minted = %d, want one per region (3)", tokens)
	}
}

func TestECRKeychainCachesUntilExpiry(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	calls := 0
	kc := &ecrKeychain{
		tokens: func(context.Context, string) (string, time.Time, error) {
			calls++
			return base64.StdEncoding.EncodeToString([]byte("AWS:secret")), now.Add(time.Hour), nil
		},
		now: func() time.Time { return now },
	}
	host := registryOf(t, "123456789012.dkr.ecr.eu-west-2.amazonaws.com")
	for range 3 {
		a, err := kc.Resolve(host)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := a.Authorization()
		if cfg.Username != "AWS" || cfg.Password != "secret" {
			t.Errorf("auth = %+v", cfg)
		}
	}
	if calls != 1 {
		t.Errorf("token calls = %d, want 1 while the token is fresh", calls)
	}
	now = now.Add(time.Hour - ecrTokenSkew + time.Second) // inside the renewal skew
	if _, err := kc.Resolve(host); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("token calls = %d, want a renewal inside the expiry skew", calls)
	}
	if a, err := kc.Resolve(registryOf(t, "ghcr.io")); err != nil || a != authn.Anonymous {
		t.Errorf("non-ECR host = %v, %v; want Anonymous", a, err)
	}
}

func TestECRKeychainErrors(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		err    error
		wantIn string
	}{
		{"api error", "", errors.New("no credentials"), "no credentials"},
		{"not base64", "%%%", nil, "illegal base64"},
		{"no colon", base64.StdEncoding.EncodeToString([]byte("AWSsecret")), nil, "not user:password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := &ecrKeychain{
				tokens: func(context.Context, string) (string, time.Time, error) {
					return tc.token, time.Now().Add(time.Hour), tc.err
				},
				now: time.Now,
			}
			_, err := kc.Resolve(registryOf(t, "123456789012.dkr.ecr.us-east-1.amazonaws.com"))
			if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("Resolve = %v, want error containing %q", err, tc.wantIn)
			}
		})
	}
}

func TestNewKeychainBuilds(t *testing.T) {
	kc, ok := NewKeychain().(*hostKeychain)
	if !ok || kc.ecr == nil || kc.google == nil || kc.docker == nil {
		t.Fatalf("NewKeychain = %#v, want every branch wired", kc)
	}
}
