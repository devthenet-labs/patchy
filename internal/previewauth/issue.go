// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"time"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// IssueCode issues an authorization code for an authorize request that the
// Preview v admits and whose viewer's access review passed, from the relay
// session s. The code expires after lt.Code, or when the session ends if
// that is sooner, and binds the client, slot, Preview UID, label, redirect
// URI, nonce, challenge and the viewer's pairwise subject.
func (r *KeyRing) IssueCode(now time.Time, lt Lifetimes, req AuthorizeRequest, v View, s Session) (string, error) {
	if err := Admit(req.Label, req.Slot, v); err != nil {
		return "", err
	}
	if s.valid() != nil {
		return "", ErrInvalidToken
	}
	if err := live(now, s.Expires); err != nil {
		return "", err
	}
	jti, err := sealed.RandomToken()
	if err != nil {
		return "", err
	}
	b := Bound{Client: req.ClientID, Slot: req.Slot, UID: v.UID, Label: req.Label}
	g := Grant{
		V: payloadVersion, Bound: b, JTI: jti, IssuedAt: now.Unix(),
		Expires: min(now.Add(lt.Code).Unix(), s.Expires),
		Sub:     r.Sub(b.Client, b.UID, b.Label, s.Identity.Username), SessionStart: s.Start, SessionEnd: s.Expires,
		Identity: s.Identity, RedirectURI: req.RedirectURI, Nonce: req.Nonce, Challenge: req.Challenge,
	}
	return r.SealCode(g)
}

// Tokens is a token response before its ID token is signed.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	// ExpiresIn equals the ID token's and the access token's remaining
	// lifetime, so whichever the ALB reads gives the same refresh cadence.
	ExpiresIn int64
	IDToken   IDTokenClaims
}

// TokensForCode answers a redeemed code: a new access token, an ID token
// claim set echoing the code's nonce, and a refresh token that lasts until
// the relay session ends.
func (r *KeyRing) TokensForCode(now time.Time, lt Lifetimes, issuer string, code Grant) (Tokens, error) {
	if code.validCode() != nil {
		return Tokens{}, ErrInvalidToken
	}
	jti, err := sealed.RandomToken()
	if err != nil {
		return Tokens{}, err
	}
	refresh := code
	refresh.JTI, refresh.IssuedAt, refresh.Expires = jti, now.Unix(), code.SessionEnd
	refresh.RedirectURI, refresh.Nonce, refresh.Challenge = "", "", ""
	rt, err := r.SealRefresh(refresh)
	if err != nil {
		return Tokens{}, err
	}
	return r.tokens(now, lt, issuer, code, code.Nonce, rt)
}

// TokensForRefresh answers a refresh: a new access token and ID token claim
// set (no nonce), and the same refresh token. Refresh tokens do not rotate, so
// parallel refreshes from several ALB nodes, or an ALB that keeps the old
// token, never sign the viewer out.
func (r *KeyRing) TokensForRefresh(now time.Time, lt Lifetimes, issuer string, refresh Grant,
	refreshToken string) (Tokens, error) {
	if refresh.validRefresh() != nil {
		return Tokens{}, ErrInvalidToken
	}
	return r.tokens(now, lt, issuer, refresh, "", refreshToken)
}

func (r *KeyRing) tokens(now time.Time, lt Lifetimes, issuer string, g Grant,
	nonce, refreshToken string) (Tokens, error) {
	exp := min(now.Add(lt.Access).Unix(), g.SessionEnd)
	if err := live(now, exp); err != nil {
		return Tokens{}, err
	}
	jti, err := sealed.RandomToken()
	if err != nil {
		return Tokens{}, err
	}
	at, err := r.SealAccess(Access{V: payloadVersion, Bound: g.Bound, JTI: jti, Expires: exp, Sub: g.Sub})
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		AccessToken: at, RefreshToken: refreshToken, ExpiresIn: exp - now.Unix(),
		IDToken: IDTokenClaims{
			Issuer: issuer, Audience: g.Client, Subject: g.Sub, IssuedAt: now.Unix(), Expires: exp,
			Nonce: nonce, AtHash: AtHash(at),
		},
	}, nil
}
