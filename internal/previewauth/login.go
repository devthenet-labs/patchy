// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// RelaySessionCookie is the relay's own session cookie on the relay host.
// __Host- pins it to that host, Secure and Path=/.
const RelaySessionCookie = "__Host-patchy-pa"

// loginCookiePrefix starts every login cookie's name; LoginCookieName adds a
// hash of the sealed login state, so each sign-in in flight has its own.
const loginCookiePrefix = "__Host-patchy-pa-login-"

// ErrLoginCSRF means the browser that came back from Dex is not the one that
// started the sign-in: its login cookie is missing or holds another value.
var ErrLoginCSRF = errors.New("previewauth: sign-in was not started by this browser")

// LoginCookieName is the login cookie for one sign-in: the prefix plus the
// first 16 hex characters of the sealed state's SHA-256. The Dex callback
// recomputes it from the state Dex returns, so two sign-ins in two tabs set
// two cookies and neither breaks the other.
func LoginCookieName(sealedState string) string {
	sum := sha256.Sum256([]byte(sealedState))
	return loginCookiePrefix + hex.EncodeToString(sum[:8])
}

// NewLogin starts a sign-in through Dex for an admitted authorize request on
// the Preview with previewUID. It returns the state to seal and the random
// CSRF value for the login cookie; the state keeps only the value's hash.
func NewLogin(now time.Time, lt Lifetimes, req AuthorizeRequest, previewUID string) (LoginState, string, error) {
	csrf, err := sealed.RandomToken()
	if err != nil {
		return LoginState{}, "", err
	}
	nonce, err := sealed.RandomToken()
	if err != nil {
		return LoginState{}, "", err
	}
	verifier, err := sealed.RandomToken()
	if err != nil {
		return LoginState{}, "", err
	}
	s := LoginState{
		V: payloadVersion, IssuedAt: now.Unix(), Expires: now.Add(lt.Login).Unix(),
		Client: req.ClientID, Slot: req.Slot, Label: req.Label, UID: previewUID,
		RedirectURI: req.RedirectURI, State: req.State, Nonce: req.Nonce, Challenge: req.Challenge,
		CSRFHash: csrfHash(csrf), UpstreamNonce: nonce, UpstreamVerifier: verifier,
	}
	if err := s.valid(); err != nil {
		return LoginState{}, "", err
	}
	return s, csrf, nil
}

// CheckCSRF compares the login cookie's value with the state's hash of it,
// in constant time.
func (s LoginState) CheckCSRF(cookie string) error {
	if cookie == "" || len(cookie) > 128 {
		return ErrLoginCSRF
	}
	if subtle.ConstantTimeCompare([]byte(csrfHash(cookie)), []byte(s.CSRFHash)) != 1 {
		return ErrLoginCSRF
	}
	return nil
}

// Request is the authorize request the sign-in resumes.
func (s LoginState) Request() AuthorizeRequest {
	return AuthorizeRequest{
		ClientID: s.Client, Slot: s.Slot, RedirectURI: s.RedirectURI, Label: s.Label,
		State: s.State, Nonce: s.Nonce, Challenge: s.Challenge,
	}
}

// Resume is the Dex callback's check before it carries on with the
// authorize request: the Preview at the label is still live in the same slot
// and is the same Preview (UID) the sign-in started for. One that died or was
// replaced meanwhile is ErrNoPreview.
func (s LoginState) Resume(v View) error {
	if err := Admit(s.Label, s.Slot, v); err != nil {
		return err
	}
	if v.UID != s.UID {
		return ErrNoPreview
	}
	return nil
}

// UpstreamChallenge is the PKCE S256 challenge sent to Dex.
func (s LoginState) UpstreamChallenge() string { return S256(s.UpstreamVerifier) }

func csrfHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// S256 is the PKCE S256 challenge of verifier.
func S256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validChallenge accepts an S256 challenge: 43 base64url characters.
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if !base64URLChar(c[i]) {
			return false
		}
	}
	return true
}

// validVerifier accepts a PKCE verifier: 43 to 128 unreserved characters.
func validVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; !base64URLChar(c) && c != '.' && c != '~' {
			return false
		}
	}
	return true
}

func base64URLChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}
