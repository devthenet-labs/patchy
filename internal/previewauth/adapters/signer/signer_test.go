// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package signer

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

func pemKey(t *testing.T, bits int, pkcs8 bool) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func TestSignVerifiesAgainstJWKS(t *testing.T) {
	cur, prev := pemKey(t, 2048, false), pemKey(t, 2048, true)
	s, err := New(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(s.JWKS(), &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 2 || set.Keys[0].KeyID != s.KeyID() || set.Keys[1].KeyID == s.KeyID() {
		t.Fatalf("jwks = %s", s.JWKS())
	}
	for _, k := range set.Keys {
		if !k.IsPublic() || k.Algorithm != "RS256" || k.Use != "sig" {
			t.Errorf("jwks key %+v", k)
		}
	}
	claims := previewauth.IDTokenClaims{Issuer: "https://r.example.com", Audience: "patchy-preview-s0",
		Subject: "sub", IssuedAt: 1, Expires: 2, AtHash: "h"}
	tok, err := s.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := jose.ParseSigned(tok, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	if obj.Signatures[0].Header.KeyID != s.KeyID() {
		t.Errorf("kid %q", obj.Signatures[0].Header.KeyID)
	}
	payload, err := obj.Verify(set.Key(s.KeyID())[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	var back previewauth.IDTokenClaims
	if err := json.Unmarshal(payload, &back); err != nil || back != claims {
		t.Fatalf("claims %+v, %v", back, err)
	}
	// The previous key's kid is stable: the same key gives the same thumbprint.
	again, err := New(prev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.KeyID() != set.Keys[1].KeyID {
		t.Error("a key's kid changed between signers")
	}
}

func TestParseKeyRefuses(t *testing.T) {
	good := pemKey(t, 2048, false)
	small := pemKey(t, 1024, false)
	tests := map[string][]byte{
		"empty":           nil,
		"not pem":         []byte("hello"),
		"short key":       small,
		"trailing data":   append(append([]byte(nil), good...), []byte("junk")...),
		"certificate":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}),
		"broken pkcs1":    pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1}}),
		"two blocks":      append(append([]byte(nil), good...), good...),
		"garbage in pem8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKey(raw); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := ParseKey(append(good, '\n')); err != nil {
		t.Errorf("trailing newline refused: %v", err)
	}
}

func TestNewRefusesSameKeyTwice(t *testing.T) {
	k := pemKey(t, 2048, false)
	if _, err := New(k, k); err == nil || !strings.Contains(err.Error(), "previous") {
		t.Fatalf("err = %v", err)
	}
}
