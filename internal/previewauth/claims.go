// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxSessionBytes caps the relay's sealed session cookie, which carries
	// the viewer's identity, below a browser's 4 KiB cookie limit.
	MaxSessionBytes = 3072
	// maxName bounds a username or a group name.
	maxName = 256
	// maxUID bounds a Preview UID (Kubernetes UIDs are 36 characters).
	maxUID = 128
	// MaxStateBytes is the longest ALB state the relay accepts.
	MaxStateBytes = 4096
	// MaxNonceBytes is the longest ALB nonce the relay accepts.
	MaxNonceBytes = 512
)

// ErrIdentityTooLarge is returned when a viewer's identity (in practice, too
// many groups) does not fit the relay's session cookie.
var ErrIdentityTooLarge = errors.New("previewauth: identity too large for a session")

// Identity is the viewer as access reviews see them: the mapped (prefixed)
// username and groups. It is sealed into codes, refresh tokens and the relay
// session, which only the relay and the ALB's backchannel ever hold, and never
// into an access token.
type Identity struct {
	Username string   `json:"u"`
	Groups   []string `json:"g,omitempty"`
}

// Validate refuses an empty or oversized username or group and any system:
// name, which access reviews would treat as a Kubernetes identity. Claim
// mapping already guards this; the core checks it again.
func (id Identity) Validate() error {
	if id.Username == "" || len(id.Username) > maxName {
		return errors.New("previewauth: identity has no usable username")
	}
	if strings.HasPrefix(id.Username, "system:") {
		return errors.New("previewauth: identity username is a system: name")
	}
	for _, g := range id.Groups {
		if g == "" || len(g) > maxName || strings.HasPrefix(g, "system:") {
			return fmt.Errorf("previewauth: identity group %q is not usable", g)
		}
	}
	return nil
}

// Bound is what every code, refresh and access token is bound to: the ALB
// client it was issued to, that client's slot, and the Preview (UID and host
// label) that held the slot.
type Bound struct {
	Client string `json:"cid"`
	Slot   int    `json:"slot"`
	UID    string `json:"uid"`
	Label  string `json:"lbl"`
}

func (b Bound) validBound() error {
	if b.Slot < 0 || b.Slot >= MaxSlots || b.Client != ClientID(b.Slot) || b.UID == "" || len(b.UID) > maxUID ||
		!ValidLabel(b.Label) {
		return ErrInvalidToken
	}
	return nil
}

// Grant is the payload of an authorization code and of a refresh token.
// RedirectURI, Nonce and Challenge are set in codes only.
type Grant struct {
	V int `json:"v"`
	Bound
	JTI          string   `json:"jti"`
	IssuedAt     int64    `json:"iat"`
	Expires      int64    `json:"exp"`
	Sub          string   `json:"sub"`
	SessionStart int64    `json:"ss"`
	SessionEnd   int64    `json:"abs"`
	Identity     Identity `json:"id"`
	RedirectURI  string   `json:"ru,omitempty"`
	Nonce        string   `json:"n,omitempty"`
	Challenge    string   `json:"cc,omitempty"`
}

func (g Grant) validCommon() error {
	if g.V != payloadVersion || g.JTI == "" || g.Sub == "" || g.Expires <= g.IssuedAt ||
		g.SessionEnd <= g.SessionStart || g.Expires > g.SessionEnd {
		return ErrInvalidToken
	}
	if err := g.validBound(); err != nil {
		return err
	}
	if g.Identity.Validate() != nil {
		return ErrInvalidToken
	}
	return nil
}

func (g Grant) validCode() error {
	if err := g.validCommon(); err != nil {
		return err
	}
	if g.RedirectURI == "" || len(g.Nonce) > MaxNonceBytes || (g.Challenge != "" && !validChallenge(g.Challenge)) {
		return ErrInvalidToken
	}
	return nil
}

func (g Grant) validRefresh() error {
	if err := g.validCommon(); err != nil {
		return err
	}
	if g.RedirectURI != "" || g.Nonce != "" || g.Challenge != "" || g.Expires != g.SessionEnd {
		return ErrInvalidToken
	}
	return nil
}

// Access is the payload of an access token: the binding and the pairwise
// subject, and nothing that identifies the viewer. The ALB forwards it to
// preview code, so it works only at the relay's userinfo, only for its own
// Preview, and only until it expires.
type Access struct {
	V int `json:"v"`
	Bound
	JTI     string `json:"jti"`
	Expires int64  `json:"exp"`
	Sub     string `json:"sub"`
}

func (a Access) valid() error {
	if a.V != payloadVersion || a.JTI == "" || a.Sub == "" || a.Expires <= 0 {
		return ErrInvalidToken
	}
	return a.validBound()
}

// LoginState is the sealed state parameter of a sign-in through Dex: the
// ALB's authorize request to resume afterwards, the Preview UID it was for,
// the hash of the login cookie's CSRF value, and the relay's own PKCE
// verifier and nonce for Dex.
type LoginState struct {
	V                int    `json:"v"`
	IssuedAt         int64  `json:"iat"`
	Expires          int64  `json:"exp"`
	Client           string `json:"cid"`
	Slot             int    `json:"slot"`
	Label            string `json:"lbl"`
	UID              string `json:"uid"`
	RedirectURI      string `json:"ru"`
	State            string `json:"st"`
	Nonce            string `json:"n,omitempty"`
	Challenge        string `json:"cc,omitempty"`
	CSRFHash         string `json:"csrf"`
	UpstreamNonce    string `json:"un"`
	UpstreamVerifier string `json:"uv"`
}

func (s LoginState) valid() error {
	b := Bound{Client: s.Client, Slot: s.Slot, UID: s.UID, Label: s.Label}
	if s.V != payloadVersion || s.Expires <= s.IssuedAt || b.validBound() != nil || s.RedirectURI == "" ||
		s.State == "" || len(s.State) > MaxStateBytes || len(s.Nonce) > MaxNonceBytes ||
		(s.Challenge != "" && !validChallenge(s.Challenge)) || len(s.CSRFHash) != 64 ||
		s.UpstreamNonce == "" || !validVerifier(s.UpstreamVerifier) {
		return ErrInvalidToken
	}
	return nil
}

// Session is the relay's own sign-in session: who the viewer is and when the
// session started and ends. It is a browser-session cookie on the relay host,
// so a viewer signs in once per browser session, at most SessionMaxAge.
type Session struct {
	V        int      `json:"v"`
	Identity Identity `json:"id"`
	Start    int64    `json:"ss"`
	Expires  int64    `json:"abs"`
}

func (s Session) valid() error {
	if s.V != payloadVersion || s.Expires <= s.Start {
		return ErrInvalidToken
	}
	if s.Identity.Validate() != nil {
		return ErrInvalidToken
	}
	return nil
}

// NewSession starts a session for id at now.
func NewSession(now time.Time, lt Lifetimes, id Identity) (Session, error) {
	if err := id.Validate(); err != nil {
		return Session{}, err
	}
	return Session{V: payloadVersion, Identity: id, Start: now.Unix(), Expires: now.Add(lt.SessionMaxAge).Unix()}, nil
}

// Lifetimes are the relay's token lifetimes.
type Lifetimes struct {
	// Code is an authorization code's lifetime; codes are also single-use.
	Code time.Duration
	// Access is an access token's and an ID token's lifetime, and the
	// expires_in of every token response.
	Access time.Duration
	// Login is how long a sign-in through Dex may take.
	Login time.Duration
	// SessionMaxAge is the relay session's absolute lifetime, and so every
	// refresh token's.
	SessionMaxAge time.Duration
}

// DefaultLifetimes are the chart's defaults.
func DefaultLifetimes() Lifetimes {
	return Lifetimes{Code: time.Minute, Access: 10 * time.Minute, Login: 5 * time.Minute, SessionMaxAge: 12 * time.Hour}
}

// Validate keeps every lifetime whole seconds and within bounds that keep the
// worst-case revocation window small.
func (l Lifetimes) Validate() error {
	checks := []struct {
		name     string
		v        time.Duration
		min, max time.Duration
	}{
		{"code", l.Code, 10 * time.Second, 5 * time.Minute},
		{"access", l.Access, time.Minute, time.Hour},
		{"login", l.Login, time.Minute, 30 * time.Minute},
		{"session max age", l.SessionMaxAge, time.Hour, 7 * 24 * time.Hour},
	}
	for _, c := range checks {
		if c.v < c.min || c.v > c.max || c.v%time.Second != 0 {
			return fmt.Errorf("previewauth: %s lifetime %s is not whole seconds in [%s, %s]", c.name, c.v, c.min, c.max)
		}
	}
	if l.Access > l.SessionMaxAge {
		return errors.New("previewauth: access lifetime exceeds the session max age")
	}
	return nil
}
