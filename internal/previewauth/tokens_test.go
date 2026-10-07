// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// sampleTokens seals one valid token of every kind under r at t0.
func sampleTokens(t testing.TB, r *KeyRing) map[Kind]string {
	t.Helper()
	lt := DefaultLifetimes()
	s := testSession(t)
	req := testRequest(t, "intent-7", 1)
	req.Nonce, req.Challenge = "n-1", S256(strings.Repeat("v", 43))
	code, err := r.IssueCode(t0, lt, req, liveView("uid-1", "intent-7", 1), s)
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
	st, _, err := NewLogin(t0, lt, req, "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	login, err := r.SealLogin(st)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.SealSession(s)
	if err != nil {
		t.Fatal(err)
	}
	return map[Kind]string{KindCode: code, KindAccess: toks.AccessToken, KindRefresh: toks.RefreshToken,
		KindLogin: login, KindSession: sess}
}

// openAs opens token as kind k at now and reports only the error.
func openAs(r *KeyRing, k Kind, token string, now time.Time) error {
	var err error
	switch k {
	case KindCode:
		_, err = r.OpenCode(token, now)
	case KindAccess:
		_, err = r.OpenAccess(token, now)
	case KindRefresh:
		_, err = r.OpenRefresh(token, now)
	case KindLogin:
		_, err = r.OpenLogin(token, now)
	case KindSession:
		_, err = r.OpenSession(token, now)
	}
	return err
}

func TestTokenFormat(t *testing.T) {
	r := newRing(t, 7, masterA)
	for k, tok := range sampleTokens(t, r) {
		want := "pa1." + string(rune(k)) + ".7."
		if !strings.HasPrefix(tok, want) {
			t.Errorf("kind %c token %q does not start %q", k, tok[:12], want)
		}
		if strings.Count(tok, ".") != 3 {
			t.Errorf("kind %c token has %d dots", k, strings.Count(tok, "."))
		}
		if err := openAs(r, k, tok, t0); err != nil {
			t.Errorf("kind %c does not open: %v", k, err)
		}
	}
}

// TestTokenKindsNeverCross: no token opens as any other kind.
func TestTokenKindsNeverCross(t *testing.T) {
	r := newRing(t, 1, masterA)
	toks := sampleTokens(t, r)
	for sealedAs, tok := range toks {
		for _, k := range kinds {
			if k == sealedAs {
				continue
			}
			if err := openAs(r, k, tok, t0); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("a %c token opened as %c: %v", sealedAs, k, err)
			}
			// Relabel the header too: the key and the AAD still refuse it.
			relabelled := "pa1." + string(rune(k)) + tok[5:]
			if err := openAs(r, k, relabelled, t0); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("a %c token relabelled %c opened: %v", sealedAs, k, err)
			}
		}
	}
}

// TestAADBindsKindAndGeneration: with the right key, a blob still does not
// open under another kind's or generation's authenticated data. This is the
// AAD's own contribution, beside the per-kind keys.
func TestAADBindsKindAndGeneration(t *testing.T) {
	r := newRing(t, 1, masterA)
	for k, tok := range sampleTokens(t, r) {
		blob := tok[strings.LastIndex(tok, ".")+1:]
		key := r.current.seal[k]
		if _, err := sealed.OpenBytes(key, aad(k, 1), blob); err != nil {
			t.Fatalf("kind %c does not open with its own AAD: %v", k, err)
		}
		for _, other := range kinds {
			if other != k {
				if _, err := sealed.OpenBytes(key, aad(other, 1), blob); err == nil {
					t.Errorf("kind %c opened under kind %c's AAD", k, other)
				}
			}
		}
		if _, err := sealed.OpenBytes(key, aad(k, 2), blob); err == nil {
			t.Errorf("kind %c opened under generation 2's AAD", k)
		}
	}
}

func TestTokenGenerations(t *testing.T) {
	old := newRing(t, 1, masterA)
	rotated := newRotatedRing(t)
	unrelated := newRing(t, 3, masterB)
	oldToks := sampleTokens(t, old)
	for k, tok := range oldToks {
		if err := openAs(rotated, k, tok, t0); err != nil {
			t.Errorf("a previous-generation %c token does not open during the overlap: %v", k, err)
		}
		if err := openAs(unrelated, k, tok, t0); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a generation-1 %c token opened in a ring without generation 1: %v", k, err)
		}
		// Claiming the current generation in the header does not help.
		forged := tok[:6] + "2" + tok[7:]
		if err := openAs(rotated, k, forged, t0); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a %c token relabelled to generation 2 opened: %v", k, err)
		}
	}
	for k, tok := range sampleTokens(t, rotated) {
		if !strings.HasPrefix(tok, "pa1."+string(rune(k))+".2.") {
			t.Errorf("a rotated ring sealed kind %c under %q, want generation 2", k, tok[:8])
		}
		if err := openAs(old, k, tok, t0); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a generation-2 %c token opened in the old ring: %v", k, err)
		}
	}
}

func TestTokenMalformed(t *testing.T) {
	r := newRing(t, 1, masterA)
	code := sampleTokens(t, r)[KindCode]
	blob := code[strings.LastIndex(code, ".")+1:]
	for _, tok := range []string{
		"", "pa1", "pa1.c.1", "pa1.c.1.", "pa2.c.1." + blob, "pa1.cc.1." + blob, "pa1.c.01." + blob,
		"pa1.c.+1." + blob, "pa1.c.1.." + blob, "pa1.c.1." + blob + ".x", "PA1.c.1." + blob,
		"pa1.c.1." + blob + "=", "pa1.c.1." + strings.Repeat("A", MaxTokenBytes),
		" " + code, code + " ",
	} {
		if _, err := r.OpenCode(tok, t0); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("OpenCode(%.30q...) = %v, want ErrInvalidToken", tok, err)
		}
	}
}

// TestTokenTamperProperty: flipping any single bit of any token makes it not
// open.
func TestTokenTamperProperty(t *testing.T) {
	r := newRing(t, 1, masterA)
	toks := sampleTokens(t, r)
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		k := kinds[rnd.Intn(len(kinds))]
		tok := []byte(toks[k])
		i := rnd.Intn(len(tok))
		tok[i] ^= byte(1 << rnd.Intn(8))
		args[0] = reflect.ValueOf(k)
		args[1] = reflect.ValueOf(string(tok))
	}
	prop := func(k Kind, tok string) bool { return openAs(r, k, tok, t0) != nil }
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestTokenRoundTripProperty: random codes and access tokens round-trip
// exactly, and open as no other kind.
func TestTokenRoundTripProperty(t *testing.T) {
	r := newRotatedRing(t)
	cfg := quickConfig(1000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		slot := rnd.Intn(MaxSlots)
		label := randLabel(rnd)
		b := Bound{Client: ClientID(slot), Slot: slot, UID: randString(rnd, 1+rnd.Intn(40)), Label: label}
		iat := t0.Unix() + rnd.Int63n(1000)
		g := Grant{V: 1, Bound: b, JTI: randString(rnd, 43), IssuedAt: iat, Expires: iat + 60, Sub: randString(rnd, 43),
			SessionStart: iat - 10, SessionEnd: iat + 3600,
			Identity:    Identity{Username: "github:" + randString(rnd, 1+rnd.Intn(30)), Groups: randGroups(rnd)},
			RedirectURI: "https://" + label + "." + testSuffix + CallbackPath, Nonce: randString(rnd, rnd.Intn(64))}
		args[0] = reflect.ValueOf(g)
	}
	prop := func(g Grant) bool {
		tok, err := r.SealCode(g)
		if err != nil {
			return false
		}
		back, err := r.OpenCode(tok, time.Unix(g.IssuedAt, 0))
		if err != nil || !reflect.DeepEqual(normalize(back), normalize(g)) {
			return false
		}
		a := Access{V: 1, Bound: g.Bound, JTI: g.JTI, Expires: g.Expires, Sub: g.Sub}
		at, err := r.SealAccess(a)
		if err != nil {
			return false
		}
		aBack, err := r.OpenAccess(at, time.Unix(g.IssuedAt, 0))
		if err != nil || aBack != a {
			return false
		}
		for _, k := range []Kind{KindAccess, KindRefresh, KindLogin, KindSession} {
			if openAs(r, k, tok, time.Unix(g.IssuedAt, 0)) == nil {
				return false
			}
		}
		return openAs(r, KindCode, at, time.Unix(g.IssuedAt, 0)) != nil
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func normalize(g Grant) Grant {
	if len(g.Identity.Groups) == 0 {
		g.Identity.Groups = nil
	}
	return g
}

func randString(r *rand.Rand, n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_:é\"\\<>"
	b := make([]rune, n)
	runes := []rune(alpha)
	for i := range b {
		b[i] = runes[r.Intn(len(runes))]
	}
	return string(b)
}

func randGroups(r *rand.Rand) []string {
	n := r.Intn(4)
	out := make([]string, 0, n)
	for range n {
		out = append(out, "github:"+randString(r, 1+r.Intn(20)))
	}
	return out
}

// TestTokenExpiryBoundary: a token is valid strictly before its expiry second
// and expired from it on.
func TestTokenExpiryBoundary(t *testing.T) {
	r := newRing(t, 1, masterA)
	lt := DefaultLifetimes()
	toks := sampleTokens(t, r)
	exp := map[Kind]time.Duration{KindCode: lt.Code, KindAccess: lt.Access, KindRefresh: lt.SessionMaxAge,
		KindLogin: lt.Login, KindSession: lt.SessionMaxAge}
	for k, d := range exp {
		if err := openAs(r, k, toks[k], t0.Add(d-time.Second)); err != nil {
			t.Errorf("kind %c one second before expiry: %v", k, err)
		}
		if err := openAs(r, k, toks[k], t0.Add(d)); !errors.Is(err, ErrExpired) {
			t.Errorf("kind %c at expiry: %v, want ErrExpired", k, err)
		}
	}
}

// TestAccessTokenCarriesNoIdentity: the one token preview code sees holds the
// binding and the pairwise subject, and nothing else.
func TestAccessTokenCarriesNoIdentity(t *testing.T) {
	r := newRing(t, 1, masterA)
	at := sampleTokens(t, r)[KindAccess]
	plain, err := sealed.OpenBytes(r.current.seal[KindAccess], aad(KindAccess, 1), at[strings.LastIndex(at, ".")+1:])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"cid", "exp", "jti", "lbl", "slot", "sub", "uid", "v"}; !slices.Equal(keys, want) {
		t.Errorf("access token fields = %v, want %v", keys, want)
	}
	if strings.Contains(string(plain), "alice") || strings.Contains(string(plain), "github:") {
		t.Errorf("access token holds the viewer: %s", plain)
	}
	if len(at) > 400 {
		t.Errorf("access token is %d bytes; it should stay small", len(at))
	}
}

func TestSessionSizeCap(t *testing.T) {
	r := newRing(t, 1, masterA)
	groups := make([]string, 0, 200)
	for i := range 200 {
		groups = append(groups, "github:org:team-with-a-long-name-"+strings.Repeat("x", i%10))
	}
	s, err := NewSession(t0, DefaultLifetimes(), Identity{Username: "github:alice", Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SealSession(s); !errors.Is(err, ErrIdentityTooLarge) {
		t.Errorf("SealSession with 200 groups = %v, want ErrIdentityTooLarge", err)
	}
}

func TestSealRefusesInvalidPayloads(t *testing.T) {
	r := newRing(t, 1, masterA)
	good := Access{V: 1, Bound: Bound{Client: ClientID(1), Slot: 1, UID: "u", Label: "l"}, JTI: "j", Expires: 1, Sub: "s"}
	if _, err := r.SealAccess(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Access){
		"client of another slot": func(a *Access) { a.Client = ClientID(2) },
		"slot out of range":      func(a *Access) { a.Slot, a.Client = MaxSlots, ClientID(MaxSlots) },
		"no uid":                 func(a *Access) { a.UID = "" },
		"bad label":              func(a *Access) { a.Label = "A.b" },
		"no sub":                 func(a *Access) { a.Sub = "" },
		"no jti":                 func(a *Access) { a.JTI = "" },
		"version":                func(a *Access) { a.V = 2 },
	} {
		a := good
		mutate(&a)
		if _, err := r.SealAccess(a); err == nil {
			t.Errorf("%s: SealAccess accepted %+v", name, a)
		}
	}
	sys := Session{V: 1, Identity: Identity{Username: "system:admin"}, Start: 1, Expires: 2}
	if _, err := r.SealSession(sys); err == nil {
		t.Error("SealSession accepted a system: identity")
	}
}

// TestTokenNonCanonicalEncodingRefused pins the counterexample
// TestTokenTamperProperty shrank to: a bit flipped in the spare low bits of
// the last base64url character. A lenient decoder reads the same bytes, so
// without the strict check a second spelling of a code would open, and a
// ledger keyed on the spelling would let it redeem twice.
func TestTokenNonCanonicalEncodingRefused(t *testing.T) {
	r := newRing(t, 1, masterA)
	tried := 0
	for k, tok := range sampleTokens(t, r) {
		blob := tok[strings.LastIndex(tok, ".")+1:]
		raw, err := base64.RawURLEncoding.DecodeString(blob)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw)%3 == 0 {
			continue // no spare bits to set
		}
		last := blob[len(blob)-1]
		idx := strings.IndexByte(b64url, last)
		for bit := 0; bit < 4; bit++ {
			alt := b64url[idx^(1<<bit)]
			variant := tok[:len(tok)-1] + string(alt)
			again, err := base64.RawURLEncoding.DecodeString(variant[strings.LastIndex(variant, ".")+1:])
			if err != nil || string(again) != string(raw) {
				continue // that bit is not a spare one
			}
			tried++
			if err := openAs(r, k, variant, t0); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("kind %c: a non-canonical spelling opened: %v", k, err)
			}
		}
	}
	if tried == 0 {
		t.Error("no sample token had spare bits, so nothing was checked")
	}
}

const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
