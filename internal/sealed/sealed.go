// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// KeySize is the length of every key this package seals with: AES-256.
const KeySize = 32

// ErrOpen is the one error Open and OpenBytes return for a blob that is not
// base64url, too short, sealed under another key or AAD, or tampered with.
var ErrOpen = errors.New("sealed: blob does not open")

// PurposeKey derives a KeySize key for one purpose from secret with
// HKDF-SHA256 (no salt, info as the purpose). Distinct info strings give
// independent keys from the same secret. An empty secret or info is refused:
// both would be a configuration mistake that still produced a working key.
func PurposeKey(secret []byte, info string) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("derive %s key: empty secret", info)
	}
	if info == "" {
		return nil, errors.New("derive key: empty purpose")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, info, KeySize)
	if err != nil {
		return nil, fmt.Errorf("derive %s key: %w", info, err)
	}
	return key, nil
}

// SealBytes encrypts plaintext under key, authenticating aad with it, and
// returns base64url(nonce || ciphertext || tag).
func SealBytes(key, aad, plaintext []byte) (string, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal nonce: %w", err)
	}
	out := aead.Seal(nonce, nonce, plaintext, aad)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// OpenBytes reverses SealBytes. It returns ErrOpen unless blob was sealed
// under key with exactly aad and is unmodified.
func OpenBytes(key, aad []byte, blob string) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrOpen
	}
	nonce, ciphertext := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// Seal encrypts v's JSON encoding under key with aad; see SealBytes.
func Seal(key, aad []byte, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	return SealBytes(key, aad, plain)
}

// Open decrypts blob with key and aad and decodes the JSON into v. A blob
// that does not open is ErrOpen; JSON that does not decode into v (an
// authentic blob of another shape) is a distinct error.
func Open(key, aad []byte, blob string, v any) error {
	plain, err := OpenBytes(key, aad, blob)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("open decode json: %w", err)
	}
	return nil
}

// RandomToken returns 32 random bytes as unpadded base64url: CSRF values,
// nonces, PKCE verifiers, token ids.
func RandomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// newAEAD builds AES-256-GCM for key, refusing any key that is not KeySize
// bytes so a short key never silently selects AES-128.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("sealed: key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("seal aead: %w", err)
	}
	return aead, nil
}
