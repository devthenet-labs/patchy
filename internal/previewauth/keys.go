// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// MinMasterBytes is the shortest master secret a key ring accepts. The chart
// generates 64 random alphanumeric characters.
const MinMasterBytes = 32

// MaxGeneration is the largest key generation number (six digits).
const MaxGeneration = 999999

// purposeInfo is the HKDF info for one purpose key.
func purposeInfo(purpose string) string { return "patchy-preview-auth/v1/" + purpose }

// Generation is one key generation: its number and its master secret.
type Generation struct {
	Number int
	Master []byte
}

// genKeys are the keys derived from one generation.
type genKeys struct {
	number   int
	master   []byte
	seal     map[Kind][]byte
	pairwise []byte
}

// KeyRing holds the current key generation and, while a rotation overlaps,
// the previous one. Tokens are sealed under the current generation only and
// open under either; slot client secrets of both are accepted.
type KeyRing struct {
	current  genKeys
	previous *genKeys
}

// NewKeyRing derives every key of current and, when non-nil, previous. The
// generations must differ in number and in master secret.
func NewKeyRing(current Generation, previous *Generation) (*KeyRing, error) {
	cur, err := deriveGeneration(current)
	if err != nil {
		return nil, err
	}
	ring := &KeyRing{current: cur}
	if previous != nil {
		if previous.Number == current.Number {
			return nil, fmt.Errorf("previewauth: previous key generation is also %d", current.Number)
		}
		if bytes.Equal(previous.Master, current.Master) {
			return nil, errors.New("previewauth: previous key generation has the current master secret")
		}
		prev, err := deriveGeneration(*previous)
		if err != nil {
			return nil, err
		}
		ring.previous = &prev
	}
	return ring, nil
}

func deriveGeneration(g Generation) (genKeys, error) {
	if g.Number < 0 || g.Number > MaxGeneration {
		return genKeys{}, fmt.Errorf("previewauth: key generation %d is outside [0, %d]", g.Number, MaxGeneration)
	}
	if len(g.Master) < MinMasterBytes {
		return genKeys{}, fmt.Errorf("previewauth: generation %d master secret is %d bytes, want at least %d",
			g.Number, len(g.Master), MinMasterBytes)
	}
	keys := genKeys{number: g.Number, master: bytes.Clone(g.Master), seal: map[Kind][]byte{}}
	for _, k := range kinds {
		key, err := sealed.PurposeKey(g.Master, purposeInfo(k.purpose()))
		if err != nil {
			return genKeys{}, err
		}
		keys.seal[k] = key
	}
	pairwise, err := sealed.PurposeKey(g.Master, purposeInfo("pairwise"))
	if err != nil {
		return genKeys{}, err
	}
	keys.pairwise = pairwise
	return keys, nil
}

// Generation is the current generation's number.
func (r *KeyRing) Generation() int { return r.current.number }

// lookup returns the keys of generation n, current or previous.
func (r *KeyRing) lookup(n int) (genKeys, bool) {
	if n == r.current.number {
		return r.current, true
	}
	if r.previous != nil && n == r.previous.number {
		return *r.previous, true
	}
	return genKeys{}, false
}

// ClientSecret is slot's ALB client secret under the current generation:
// 64 hex characters, so form encoding cannot alter it.
func (r *KeyRing) ClientSecret(slot int) string {
	return clientSecret(r.current.master, r.current.number, slot)
}

// VerifyClientSecret reports whether presented is slot's client secret under
// the current or the previous generation. Both are compared, in constant time,
// whatever the first answer.
func (r *KeyRing) VerifyClientSecret(slot int, presented string) bool {
	if slot < 0 || slot >= MaxSlots {
		return false
	}
	ok := subtle.ConstantTimeCompare([]byte(presented), []byte(r.ClientSecret(slot)))
	if r.previous != nil {
		want := clientSecret(r.previous.master, r.previous.number, slot)
		ok |= subtle.ConstantTimeCompare([]byte(presented), []byte(want))
	}
	return ok == 1
}
