// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// Kind is a sealed token's kind. Each kind has its own purpose key and its own
// authenticated data, so a token of one kind never opens as another.
type Kind byte

// The token kinds, by the letter that names them in a token.
const (
	KindCode    Kind = 'c'
	KindAccess  Kind = 'a'
	KindRefresh Kind = 'r'
	KindLogin   Kind = 'l'
	KindSession Kind = 's'
)

// kinds lists every kind, in the order keys are derived.
var kinds = []Kind{KindCode, KindAccess, KindRefresh, KindLogin, KindSession}

func (k Kind) purpose() string {
	switch k {
	case KindCode:
		return "code"
	case KindAccess:
		return "access"
	case KindRefresh:
		return "refresh"
	case KindLogin:
		return "login"
	case KindSession:
		return "session"
	}
	return ""
}

// tokenPrefix is the format version every token starts with.
const tokenPrefix = "pa1"

// payloadVersion is the version every sealed payload carries in "v".
const payloadVersion = 1

// MaxTokenBytes bounds every token before it is parsed or opened. The largest
// is a login state, which carries the ALB's state of up to MaxStateBytes.
const MaxTokenBytes = 8192

// ErrInvalidToken is returned for every token that is malformed, of another
// kind, of an unknown generation, tampered with, or of the wrong shape. It
// never says which.
var ErrInvalidToken = errors.New("previewauth: invalid token")

// ErrExpired is returned for an authentic token whose lifetime has passed.
var ErrExpired = errors.New("previewauth: token expired")

// aad binds a token's kind and generation into its authenticated data.
func aad(k Kind, gen int) []byte {
	return []byte(tokenPrefix + "|" + string(rune(k)) + "|" + strconv.Itoa(gen))
}

// seal seals v as a token of kind k under the current generation.
func (r *KeyRing) seal(k Kind, v any) (string, error) {
	blob, err := sealed.Seal(r.current.seal[k], aad(k, r.current.number), v)
	if err != nil {
		return "", err
	}
	return tokenPrefix + "." + string(rune(k)) + "." + strconv.Itoa(r.current.number) + "." + blob, nil
}

// open opens token as kind k under the generation it names, which must be
// the current or the previous one, into v.
func (r *KeyRing) open(k Kind, token string, v any) error {
	if len(token) > MaxTokenBytes {
		return ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != tokenPrefix || len(parts[1]) != 1 || Kind(parts[1][0]) != k {
		return ErrInvalidToken
	}
	gen, ok := canonicalUint(parts[2])
	if !ok {
		return ErrInvalidToken
	}
	keys, ok := r.lookup(gen)
	if !ok {
		return ErrInvalidToken
	}
	// Unpadded base64 leaves spare bits in its last character, which a
	// lenient decoder ignores. Refusing them keeps one token one string, so
	// no second spelling of a code or token ever opens.
	if _, err := base64.RawURLEncoding.Strict().DecodeString(parts[3]); err != nil {
		return ErrInvalidToken
	}
	if err := sealed.Open(keys.seal[k], aad(k, gen), parts[3], v); err != nil {
		return ErrInvalidToken
	}
	return nil
}

// live reports an authentic payload's expiry against now: a token is valid
// strictly before its expiry second.
func live(now time.Time, exp int64) error {
	if now.Unix() >= exp {
		return ErrExpired
	}
	return nil
}

// SealCode seals an authorization code.
func (r *KeyRing) SealCode(g Grant) (string, error) {
	if err := g.validCode(); err != nil {
		return "", err
	}
	return r.seal(KindCode, g)
}

// OpenCode opens an authorization code that has not expired at now.
func (r *KeyRing) OpenCode(token string, now time.Time) (Grant, error) {
	var g Grant
	if err := r.open(KindCode, token, &g); err != nil {
		return Grant{}, err
	}
	if g.validCode() != nil {
		return Grant{}, ErrInvalidToken
	}
	return g, live(now, g.Expires)
}

// SealRefresh seals a refresh token.
func (r *KeyRing) SealRefresh(g Grant) (string, error) {
	if err := g.validRefresh(); err != nil {
		return "", err
	}
	return r.seal(KindRefresh, g)
}

// OpenRefresh opens a refresh token whose session has not ended at now.
func (r *KeyRing) OpenRefresh(token string, now time.Time) (Grant, error) {
	var g Grant
	if err := r.open(KindRefresh, token, &g); err != nil {
		return Grant{}, err
	}
	if g.validRefresh() != nil {
		return Grant{}, ErrInvalidToken
	}
	return g, live(now, g.Expires)
}

// SealAccess seals an access token.
func (r *KeyRing) SealAccess(a Access) (string, error) {
	if err := a.valid(); err != nil {
		return "", err
	}
	return r.seal(KindAccess, a)
}

// OpenAccess opens an access token that has not expired at now.
func (r *KeyRing) OpenAccess(token string, now time.Time) (Access, error) {
	var a Access
	if err := r.open(KindAccess, token, &a); err != nil {
		return Access{}, err
	}
	if a.valid() != nil {
		return Access{}, ErrInvalidToken
	}
	return a, live(now, a.Expires)
}

// SealLogin seals a sign-in's login state, the state parameter sent to Dex.
func (r *KeyRing) SealLogin(s LoginState) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	return r.seal(KindLogin, s)
}

// OpenLogin opens a login state that has not expired at now.
func (r *KeyRing) OpenLogin(token string, now time.Time) (LoginState, error) {
	var s LoginState
	if err := r.open(KindLogin, token, &s); err != nil {
		return LoginState{}, err
	}
	if s.valid() != nil {
		return LoginState{}, ErrInvalidToken
	}
	return s, live(now, s.Expires)
}

// SealSession seals the relay's own session cookie. A session whose sealed
// form exceeds MaxSessionBytes (an identity with too many groups) is
// ErrIdentityTooLarge: a browser drops a cookie that large.
func (r *KeyRing) SealSession(s Session) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	tok, err := r.seal(KindSession, s)
	if err != nil {
		return "", err
	}
	if len(tok) > MaxSessionBytes {
		return "", ErrIdentityTooLarge
	}
	return tok, nil
}

// OpenSession opens a relay session that has not ended at now.
func (r *KeyRing) OpenSession(token string, now time.Time) (Session, error) {
	if len(token) > MaxSessionBytes {
		return Session{}, ErrInvalidToken
	}
	var s Session
	if err := r.open(KindSession, token, &s); err != nil {
		return Session{}, err
	}
	if s.valid() != nil {
		return Session{}, ErrInvalidToken
	}
	return s, live(now, s.Expires)
}
