// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestFlow drives the core the way the relay will: authorize without a
// session (login state out to Dex), the Dex callback (CSRF, session), the
// resumed authorize (code), the code redemption, userinfo and two refreshes.
func TestFlow(t *testing.T) {
	r := newRing(t, 1, masterA)
	lt := DefaultLifetimes()
	view := liveView("uid-7", "intent-7", 1)
	verifier := strings.Repeat("k", 43)

	code, resumed, now := flowSignIn(t, r, lt, view, verifier)

	// POST /token from the ALB's backchannel.
	client, err := r.AuthenticateClient(basicHeader(ClientID(1), r.ClientSecret(1)), url.Values{}, 2)
	must(t, err)
	treq, err := ParseTokenRequest(url.Values{"grant_type": {GrantAuthorizationCode}, "code": {code},
		"redirect_uri": {resumed.RedirectURI}, "code_verifier": {verifier}})
	must(t, err)
	g, err := r.OpenCode(treq.Code, now.Add(10*time.Second))
	must(t, err)
	must(t, RedeemCode(g, client, treq))
	must(t, g.Matches(view))
	toks, err := r.TokensForCode(now, lt, testIssuer, g)
	must(t, err)
	wantSub := r.Sub(ClientID(1), "uid-7", "intent-7", "github:alice")
	id := toks.IDToken
	if id.Issuer != testIssuer || id.Audience != ClientID(1) || id.Subject != wantSub || id.Nonce != "alb-nonce" ||
		id.AtHash != AtHash(toks.AccessToken) || id.Expires-id.IssuedAt != toks.ExpiresIn ||
		toks.ExpiresIn != int64(lt.Access/time.Second) {
		t.Errorf("ID token claims %+v, expires_in %d", id, toks.ExpiresIn)
	}

	// /userinfo with the forwarded access token.
	bearer, err := ParseBearer("GET", "Bearer "+toks.AccessToken, nil)
	must(t, err)
	a, err := r.OpenAccess(bearer, now.Add(time.Minute))
	must(t, err)
	if err := a.Matches(view); err != nil || a.Sub != wantSub || a.Expires != id.Expires {
		t.Errorf("access %+v: %v", a, err)
	}

	flowRefreshTwice(t, r, lt, client, view, toks, now, wantSub)

	// The refresh token is a refresh token, not a code; the code is not one.
	if _, err := r.OpenCode(toks.RefreshToken, now); err == nil {
		t.Error("the refresh token opened as a code")
	}
	if _, err := r.OpenRefresh(code, now); err == nil {
		t.Error("the code opened as a refresh token")
	}
}

// flowSignIn is TestFlow's first half: /authorize with no session, the Dex
// callback, and the resumed /authorize that issues the code.
func flowSignIn(t *testing.T, r *KeyRing, lt Lifetimes, view View, verifier string) (string, AuthorizeRequest,
	time.Time) {
	t.Helper()
	cb := newCallbacks(t)
	// /authorize from the ALB.
	q := authorizeQuery(func(q url.Values) {
		q.Set("nonce", "alb-nonce")
		q.Set("code_challenge", S256(verifier))
		q.Set("code_challenge_method", "S256")
	})
	req, err := ParseAuthorize(q, 2, cb)
	must(t, err)
	must(t, Admit(req.Label, req.Slot, view))
	st, csrf, err := NewLogin(t0, lt, req, view.UID)
	must(t, err)
	state, err := r.SealLogin(st)
	must(t, err)
	cookieName := LoginCookieName(state)

	// /dex/callback, a minute later.
	now := t0.Add(time.Minute)
	if LoginCookieName(state) != cookieName {
		t.Fatal("the callback cannot find the login cookie")
	}
	back, err := r.OpenLogin(state, now)
	must(t, err)
	must(t, back.CheckCSRF(csrf))
	sess, err := NewSession(now, lt, Identity{Username: "github:alice", Groups: []string{"github:org:viewers"}})
	must(t, err)
	if _, err := r.SealSession(sess); err != nil {
		t.Fatal(err)
	}
	resumed := back.Request()
	must(t, back.Resume(view))
	review, err := ReviewFor(sess.Identity, view)
	if err != nil || review.Project != "demo" || review.Subresource != "previews" {
		t.Fatalf("ReviewFor = %+v, %v", review, err)
	}
	code, err := r.IssueCode(now, lt, resumed, view, sess)
	must(t, err)
	loc := CodeRedirect(resumed.RedirectURI, code, resumed.State)
	if !strings.HasPrefix(loc, "https://intent-7."+testSuffix+CallbackPath+"?code=") {
		t.Fatalf("redirect %q", loc)
	}
	return code, resumed, now
}

// flowRefreshTwice refreshes with the same refresh token twice, as parallel
// ALB nodes would.
func flowRefreshTwice(t *testing.T, r *KeyRing, lt Lifetimes, client ClientAuth, view View, toks Tokens,
	now time.Time, wantSub string) {
	t.Helper()
	for i := range 2 {
		later := now.Add(time.Duration(11+i) * time.Minute)
		rg, err := r.OpenRefresh(toks.RefreshToken, later)
		must(t, err)
		must(t, CheckRefresh(rg, client))
		must(t, rg.Matches(view))
		rt, err := r.TokensForRefresh(later, lt, testIssuer, rg, toks.RefreshToken)
		must(t, err)
		if rt.RefreshToken != toks.RefreshToken || rt.IDToken.Subject != wantSub || rt.IDToken.Nonce != "" ||
			rt.AccessToken == toks.AccessToken {
			t.Errorf("refresh %d: %+v", i, rt.IDToken)
		}
	}
}

// TestCrossClientAndRecycledPreview: a refresh token stolen from one slot's
// ALB is refused by another slot's client, and a token for a Preview that has
// been replaced at the same label and slot is refused.
func TestCrossClientAndRecycledPreview(t *testing.T) {
	r := newRing(t, 1, masterA)
	lt := DefaultLifetimes()
	sess := testSession(t)
	view := liveView("uid-old", "intent-7", 0)
	code, err := r.IssueCode(t0, lt, testRequest(t, "intent-7", 0), view, sess)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.OpenCode(code, t0)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := r.TokensForCode(t0, lt, testIssuer, g)
	if err != nil {
		t.Fatal(err)
	}
	rg, err := r.OpenRefresh(toks.RefreshToken, t0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.AuthenticateClient("", url.Values{"client_id": {ClientID(1)}, "client_secret": {r.ClientSecret(1)}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if TokenError(CheckRefresh(rg, other)).Code != CodeInvalidGrant {
		t.Error("slot 1's client refreshed slot 0's token")
	}
	recycled := liveView("uid-new", "intent-7", 0)
	if err := rg.Matches(recycled); !errors.Is(err, ErrNotBound) {
		t.Errorf("a token for the replaced Preview: %v, want ErrNotBound", err)
	}
	a, err := r.OpenAccess(toks.AccessToken, t0)
	if err != nil {
		t.Fatal(err)
	}
	if UserinfoError(a.Matches(recycled)).Status != 401 {
		t.Error("userinfo accepted an access token for the replaced Preview")
	}
	gone := View{UID: "uid-old", Label: "intent-7", Slot: 0}
	if TokenError(rg.Matches(gone)).Code != CodeInvalidGrant {
		t.Error("a refresh for a Preview that is gone is not invalid_grant")
	}
	moved := liveView("uid-old", "intent-7", 1)
	if err := rg.Matches(moved); err == nil {
		t.Error("a token for slot 0 matched the Preview in slot 1")
	}
}

func TestIssueCodeRefusals(t *testing.T) {
	r := newRing(t, 1, masterA)
	lt := DefaultLifetimes()
	sess := testSession(t)
	req := testRequest(t, "intent-7", 0)
	if _, err := r.IssueCode(t0, lt, req, liveView("u", "intent-8", 0), sess); !errors.Is(err, ErrNoPreview) {
		t.Errorf("another label: %v", err)
	}
	if _, err := r.IssueCode(t0, lt, req, liveView("u", "intent-7", 1), sess); !errors.Is(err, ErrNoPreview) {
		t.Errorf("another slot: %v", err)
	}
	if _, err := r.IssueCode(t0.Add(lt.SessionMaxAge), lt, req, liveView("u", "intent-7", 0), sess); !errors.Is(err,
		ErrExpired) {
		t.Errorf("an ended session: %v", err)
	}
	if _, err := r.IssueCode(t0, lt, req, liveView("u", "intent-7", 0), Session{}); err == nil {
		t.Error("an empty session issued a code")
	}
}

// TestLifetimesCapAtSessionEnd: neither a code nor an access token outlives
// the relay session it came from, and none is issued once it has ended.
func TestLifetimesCapAtSessionEnd(t *testing.T) {
	r := newRing(t, 1, masterA)
	lt := DefaultLifetimes()
	sess := testSession(t)
	end := time.Unix(sess.Expires, 0)
	view := liveView("u", "intent-7", 0)
	code, err := r.IssueCode(end.Add(-30*time.Second), lt, testRequest(t, "intent-7", 0), view, sess)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.OpenCode(code, end.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if g.Expires != sess.Expires {
		t.Errorf("code expires %d, want the session end %d", g.Expires, sess.Expires)
	}
	toks, err := r.TokensForCode(end.Add(-20*time.Second), lt, testIssuer, g)
	if err != nil {
		t.Fatal(err)
	}
	if toks.ExpiresIn != 20 || toks.IDToken.Expires != sess.Expires {
		t.Errorf("expires_in %d, ID exp %d; want 20 and the session end", toks.ExpiresIn, toks.IDToken.Expires)
	}
	rg, err := r.OpenRefresh(toks.RefreshToken, end.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenRefresh(toks.RefreshToken, end); !errors.Is(err, ErrExpired) {
		t.Errorf("refresh at the session end: %v", err)
	}
	if _, err := r.TokensForRefresh(end, lt, testIssuer, rg, toks.RefreshToken); !errors.Is(err, ErrExpired) {
		t.Errorf("tokens at the session end: %v", err)
	}
}

// TestRotationKeepsSessions: tokens sealed before a rotation keep working
// during the overlap, and the subject a session started with is kept.
func TestRotationKeepsSessions(t *testing.T) {
	old := newRing(t, 1, masterA)
	rotated := newRotatedRing(t)
	lt := DefaultLifetimes()
	view := liveView("u", "intent-7", 0)
	code, err := old.IssueCode(t0, lt, testRequest(t, "intent-7", 0), view, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	g, err := rotated.OpenCode(code, t0)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := rotated.TokensForCode(t0, lt, testIssuer, g)
	if err != nil {
		t.Fatal(err)
	}
	if toks.IDToken.Subject != old.Sub(ClientID(0), "u", "intent-7", "github:alice") {
		t.Error("the subject changed across the rotation")
	}
	if !strings.HasPrefix(toks.AccessToken, "pa1.a.2.") || !strings.HasPrefix(toks.RefreshToken, "pa1.r.2.") {
		t.Error("new tokens are not sealed under the current generation")
	}
}

func TestLifetimesValidate(t *testing.T) {
	if err := DefaultLifetimes().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, mutate := range map[string]func(*Lifetimes){
		"zero code":         func(l *Lifetimes) { l.Code = 0 },
		"long code":         func(l *Lifetimes) { l.Code = time.Hour },
		"sub-second access": func(l *Lifetimes) { l.Access = 90*time.Second + time.Millisecond },
		"long access":       func(l *Lifetimes) { l.Access = 2 * time.Hour },
		"short login":       func(l *Lifetimes) { l.Login = time.Second },
		"short session":     func(l *Lifetimes) { l.SessionMaxAge = time.Minute },
		"long session":      func(l *Lifetimes) { l.SessionMaxAge = 30 * 24 * time.Hour },
		"negative access":   func(l *Lifetimes) { l.Access = -time.Minute },
		"access beyond the session": func(l *Lifetimes) {
			l.Access, l.SessionMaxAge = time.Hour, time.Hour-time.Second
		},
	} {
		l := DefaultLifetimes()
		mutate(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, l)
		}
	}
}

func TestAtHash(t *testing.T) {
	// Computed outside Go:
	// printf 'token' | shasum -a 256 | head -c 32 | xxd -r -p | base64 | tr '+/' '-_' | tr -d =
	if got, want := AtHash("token"), "PEaenWxYddN6Q_NT1PiOYQ"; got != want {
		t.Errorf("AtHash = %s, want %s", got, want)
	}
}

// must fails the test at once on err.
func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
