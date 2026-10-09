// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// oidcIssuer is an in-process OpenID provider: a discovery document and a
// JWKS served from an httptest server, plus a signer for tokens it vouches
// for. It lets GoogleVerifier's real go-oidc path run without the network.
type oidcIssuer struct {
	srv         *httptest.Server
	key         *rsa.PrivateKey
	discoveries atomic.Int32
}

func newOIDCIssuer(t *testing.T) *oidcIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss := &oidcIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss.discoveries.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.srv.URL,
			"jwks_uri":                              iss.srv.URL + "/jwks",
			"authorization_endpoint":                iss.srv.URL + "/auth",
			"token_endpoint":                        iss.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
		}}})
	})
	iss.srv = httptest.NewServer(mux)
	t.Cleanup(iss.srv.Close)
	return iss
}

// sign mints an RS256 token over claims with the given key.
func (o *oidcIssuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

func (o *oidcIssuer) claims(aud, email string, verified bool, exp time.Time) map[string]any {
	return map[string]any{
		"iss":            o.srv.URL,
		"aud":            aud,
		"sub":            "1234",
		"email":          email,
		"email_verified": verified,
		"iat":            time.Now().Add(-time.Minute).Unix(),
		"exp":            exp.Unix(),
	}
}

func TestGoogleVerifier(t *testing.T) {
	const (
		audience = "https://patchy.example/google-cloud/webhooks"
		account  = "scc-push@acme-prod.iam.gserviceaccount.com"
	)
	iss := newOIDCIssuer(t)
	future := time.Now().Add(time.Hour)

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tests := []struct {
		name    string
		token   func() string
		wantErr bool
		want    IDTokenClaims
	}{
		{
			name:  "valid token yields its claims",
			token: func() string { return iss.sign(t, iss.key, iss.claims(audience, account, true, future)) },
			want:  IDTokenClaims{Audience: audience, Email: account, Verified: true},
		},
		{
			name:  "unverified email is reported, not decided, by the verifier",
			token: func() string { return iss.sign(t, iss.key, iss.claims(audience, account, false, future)) },
			want:  IDTokenClaims{Audience: audience, Email: account, Verified: false},
		},
		{
			name: "wrong audience is rejected",
			token: func() string {
				return iss.sign(t, iss.key, iss.claims("https://elsewhere.example", account, true, future))
			},
			wantErr: true,
		},
		{
			name: "expired token is rejected",
			token: func() string {
				return iss.sign(t, iss.key, iss.claims(audience, account, true, time.Now().Add(-time.Hour)))
			},
			wantErr: true,
		},
		{
			name:    "token signed by a key outside the JWKS is rejected",
			token:   func() string { return iss.sign(t, other, iss.claims(audience, account, true, future)) },
			wantErr: true,
		},
		{
			name: "token from another issuer is rejected",
			token: func() string {
				c := iss.claims(audience, account, true, future)
				c["iss"] = GoogleIssuer
				return iss.sign(t, iss.key, c)
			},
			wantErr: true,
		},
		{
			name:    "garbage is rejected",
			token:   func() string { return "not.a.jwt" },
			wantErr: true,
		},
	}

	v := NewGoogleVerifier(audience)
	v.Issuer = iss.srv.URL
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), tt.token())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Verify() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if *got != tt.want {
				t.Errorf("Verify() = %+v, want %+v", *got, tt.want)
			}
		})
	}

	// The discovery document is fetched once and the verifier cached, not
	// rebuilt per delivery.
	if n := iss.discoveries.Load(); n != 1 {
		t.Errorf("discovery fetched %d times, want 1", n)
	}
}

func TestGoogleVerifierThroughAuthenticator(t *testing.T) {
	const (
		audience = "https://patchy.example/google-cloud/webhooks"
		account  = "scc-push@acme-prod.iam.gserviceaccount.com"
	)
	iss := newOIDCIssuer(t)
	v := NewGoogleVerifier(audience)
	v.Issuer = iss.srv.URL
	a := &GoogleOIDCAuthenticator{Verify: v, Audience: audience, ServiceAccount: account}
	future := time.Now().Add(time.Hour)

	tests := []struct {
		name    string
		claims  map[string]any
		wantErr bool
	}{
		{name: "the configured service account passes", claims: iss.claims(audience, account, true, future)},
		{
			name:    "another Google identity is refused",
			claims:  iss.claims(audience, "attacker@gmail.com", true, future),
			wantErr: true,
		},
		{
			name:    "an unverified email is refused",
			claims:  iss.claims(audience, account, false, future),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := a.Authenticate(context.Background(), request("Bearer "+iss.sign(t, iss.key, tt.claims)), nil)
			if tt.wantErr != (err != nil) {
				t.Fatalf("Authenticate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate() = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestGoogleVerifierDiscoveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	// Zero-value verifier (no constructor) must still work: the cache map is
	// built lazily.
	v := &GoogleVerifier{Issuer: srv.URL, Audience: "aud"}
	_, err := v.Verify(context.Background(), "x.y.z")
	if err == nil || !strings.Contains(err.Error(), "discover "+srv.URL) {
		t.Fatalf("Verify() = %v, want a discovery error naming the issuer", err)
	}

	// The failure is not cached: once the issuer answers, the verifier
	// builds and is used.
	iss := newOIDCIssuer(t)
	v.Issuer = iss.srv.URL
	tok := iss.sign(t, iss.key, iss.claims("aud", "a@b.c", true, time.Now().Add(time.Hour)))
	got, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify() after recovery = %v", err)
	}
	if got.Audience != "aud" || got.Email != "a@b.c" {
		t.Errorf("Verify() = %+v, want aud/a@b.c", got)
	}
}
