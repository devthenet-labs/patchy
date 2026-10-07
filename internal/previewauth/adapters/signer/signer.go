// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package signer

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// MinRSABits is the smallest RSA key the signer accepts.
const MinRSABits = 2048

// Signer signs ID tokens with the current key.
type Signer struct {
	signer jose.Signer
	kid    string
	jwks   []byte
}

var _ previewauth.Signer = (*Signer)(nil)

// New builds a Signer from the current key and, when non-nil, the previous
// one, both PEM (PKCS#1 or PKCS#8 RSA private keys of at least MinRSABits).
func New(currentPEM, previousPEM []byte) (*Signer, error) {
	cur, err := ParseKey(currentPEM)
	if err != nil {
		return nil, fmt.Errorf("current signing key: %w", err)
	}
	keys := []*rsa.PrivateKey{cur}
	if previousPEM != nil {
		prev, err := ParseKey(previousPEM)
		if err != nil {
			return nil, fmt.Errorf("previous signing key: %w", err)
		}
		if prev.Equal(cur) {
			return nil, errors.New("previous signing key is the current one")
		}
		keys = append(keys, prev)
	}
	set := jose.JSONWebKeySet{}
	for _, k := range keys {
		kid, err := Thumbprint(&k.PublicKey)
		if err != nil {
			return nil, err
		}
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key: &k.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig",
		})
	}
	jwks, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("encode jwks: %w", err)
	}
	kid := set.Keys[0].KeyID
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: cur},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), kid))
	if err != nil {
		return nil, fmt.Errorf("rs256 signer: %w", err)
	}
	return &Signer{signer: s, kid: kid, jwks: jwks}, nil
}

// ParseKey decodes one PEM RSA private key, PKCS#1 or PKCS#8, and refuses
// a key shorter than MinRSABits or trailing data after the block.
func ParseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if len(trimSpace(rest)) != 0 {
		return nil, errors.New("data after the PEM block")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pkcs1: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("pkcs8: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("pkcs8 key is not RSA")
		}
		key = rk
	default:
		return nil, fmt.Errorf("PEM block %q is not an RSA private key", block.Type)
	}
	if key.N.BitLen() < MinRSABits {
		return nil, fmt.Errorf("RSA key is %d bits, want at least %d", key.N.BitLen(), MinRSABits)
	}
	return key, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of key, base64url.
func Thumbprint(key *rsa.PublicKey) (string, error) {
	sum, err := (&jose.JSONWebKey{Key: key}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("jwk thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// KeyID is the current key's id.
func (s *Signer) KeyID() string { return s.kid }

// Sign signs claims as a compact RS256 JWT.
func (s *Signer) Sign(claims previewauth.IDTokenClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode claims: %w", err)
	}
	obj, err := s.signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("sign id token: %w", err)
	}
	return obj.CompactSerialize()
}

// JWKS is the public key set, current key first.
func (s *Signer) JWKS() []byte { return append([]byte(nil), s.jwks...) }
