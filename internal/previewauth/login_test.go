// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestLoginRoundTrip(t *testing.T) {
	r := newRing(t, 1, masterA)
	req := testRequest(t, "intent-7", 2)
	req.State = "ALB state with spaces, +plus, %25 and ünïcode"
	req.Nonce = "alb-nonce"
	req.Challenge = S256(strings.Repeat("q", 50))
	st, csrf, err := NewLogin(t0, DefaultLifetimes(), req, "uid-7")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.CSRFHash, csrf) || len(csrf) != 43 {
		t.Errorf("the state holds the CSRF value itself, or the value is %d characters", len(csrf))
	}
	tok, err := r.SealLogin(st)
	if err != nil {
		t.Fatal(err)
	}
	back, err := r.OpenLogin(tok, t0)
	if err != nil {
		t.Fatal(err)
	}
	if back != st {
		t.Errorf("login state did not round-trip:\n got %+v\nwant %+v", back, st)
	}
	if back.Request() != req {
		t.Errorf("Request() = %+v, want %+v", back.Request(), req)
	}
	if err := back.CheckCSRF(csrf); err != nil {
		t.Errorf("the browser's own CSRF value: %v", err)
	}
	if back.UpstreamChallenge() != S256(back.UpstreamVerifier) || !validVerifier(back.UpstreamVerifier) {
		t.Error("the upstream PKCE pair is not S256")
	}
}

func TestLoginCSRF(t *testing.T) {
	st, csrf, err := NewLogin(t0, DefaultLifetimes(), testRequest(t, "a", 0), "uid")
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := NewLogin(t0, DefaultLifetimes(), testRequest(t, "a", 0), "uid")
	if err != nil {
		t.Fatal(err)
	}
	for name, cookie := range map[string]string{
		"missing": "", "another sign-in's": other, "the hash itself": st.CSRFHash,
		"a prefix": csrf[:42], "too long": strings.Repeat("a", 129),
	} {
		if err := st.CheckCSRF(cookie); !errors.Is(err, ErrLoginCSRF) {
			t.Errorf("%s cookie: %v, want ErrLoginCSRF", name, err)
		}
	}
}

// TestLoginCookieNameProperty: the login cookie's name is a valid __Host-
// cookie name, the same for one sealed state and different for two, so two
// sign-ins in flight in one browser keep separate cookies.
func TestLoginCookieNameProperty(t *testing.T) {
	r := newRing(t, 1, masterA)
	cfg := quickConfig(500)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		req := testRequest(t, randLabel(rnd), rnd.Intn(MaxSlots))
		req.State = randString(rnd, 1+rnd.Intn(100))
		args[0] = reflect.ValueOf(req)
	}
	prop := func(req AuthorizeRequest) bool {
		var names []string
		for range 2 {
			st, _, err := NewLogin(t0, DefaultLifetimes(), req, "uid")
			if err != nil {
				return false
			}
			tok, err := r.SealLogin(st)
			if err != nil {
				return false
			}
			name := LoginCookieName(tok)
			if name != LoginCookieName(tok) || !strings.HasPrefix(name, "__Host-patchy-pa-login-") ||
				len(name) != len("__Host-patchy-pa-login-")+16 || name == RelaySessionCookie {
				return false
			}
			names = append(names, name)
		}
		return names[0] != names[1]
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestNewLoginRefusesBadRequests(t *testing.T) {
	good := testRequest(t, "a", 0)
	for name, mutate := range map[string]func(*AuthorizeRequest){
		"no state":        func(r *AuthorizeRequest) { r.State = "" },
		"long state":      func(r *AuthorizeRequest) { r.State = strings.Repeat("s", MaxStateBytes+1) },
		"long nonce":      func(r *AuthorizeRequest) { r.Nonce = strings.Repeat("n", MaxNonceBytes+1) },
		"bad challenge":   func(r *AuthorizeRequest) { r.Challenge = "short" },
		"client mismatch": func(r *AuthorizeRequest) { r.ClientID = ClientID(1) },
		"no redirect":     func(r *AuthorizeRequest) { r.RedirectURI = "" },
	} {
		req := good
		mutate(&req)
		if _, _, err := NewLogin(t0, DefaultLifetimes(), req, "uid"); err == nil {
			t.Errorf("%s: NewLogin accepted %+v", name, req)
		}
	}
	if _, _, err := NewLogin(t0, DefaultLifetimes(), good, ""); err == nil {
		t.Error("NewLogin accepted an empty Preview UID")
	}
}

func TestLoginResume(t *testing.T) {
	st, _, err := NewLogin(t0, DefaultLifetimes(), testRequest(t, "intent-7", 1), "uid-7")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		v    View
		ok   bool
	}{
		{"same Preview", liveView("uid-7", "intent-7", 1), true},
		{"replaced meanwhile", liveView("uid-8", "intent-7", 1), false},
		{"moved slot", liveView("uid-7", "intent-7", 0), false},
		{"gone", View{UID: "uid-7", Label: "intent-7", Slot: 1}, false},
	} {
		err := st.Resume(tc.v)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrNoPreview)) {
			t.Errorf("%s: Resume = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}
