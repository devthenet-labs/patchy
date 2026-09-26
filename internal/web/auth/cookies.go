// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cookie names. The session and state cookies are HttpOnly and carry sealed
// data; the provider/error/logout cookies are SPA-visible and carry no
// secrets — they only tell the client what the sign-in surface looks like.
// __Host- requires Secure, Path=/ and no Domain, so a sibling preview host
// cannot inject parent-domain cookies under these names.
const (
	// cookieSession is the chunked session cookie; chunks after the first
	// are cookieSession-1, cookieSession-2, ….
	cookieSession = "__Host-patchy-auth"
	// cookieOAuthState carries the CSRF half of the state double-submit
	// during one authorization round trip.
	cookieOAuthState = "__Host-patchy-oauth2-state"
	// CookieProvider tells the SPA whether and how sign-in works:
	// {"provider","authenticated","autoLogin"}, base64url JSON.
	CookieProvider = "__Host-patchy-auth-provider"
	// CookieAuthError carries a human-readable sign-in failure to the SPA.
	CookieAuthError = "__Host-patchy-auth-error"
	// CookieLogout marks an explicit sign-out so autoLogin pauses for it.
	CookieLogout = "__Host-patchy-auth-logout"
)

// cookieName selects exactly one namespace from operator configuration,
// never request headers. Plain-HTTP development cannot use __Host- cookies;
// its separate names are never read as a fallback in production. Keep the
// SPA's cookieName in auth.ts in sync.
func cookieName(name string, secure bool) string {
	if !secure {
		return strings.Replace(name, "__Host-patchy-", "patchy-dev-", 1)
	}
	return name
}

// chunkSize keeps each cookie under the 4KB browser limit with headroom for
// the name and attributes; maxChunks bounds a session at ~35KB, enough for
// bloated ID tokens (large group lists).
const (
	chunkSize = 3500
	maxChunks = 10
)

// providerState is the SPA-visible sign-in surface descriptor.
type providerState struct {
	Provider      string `json:"provider"`
	Authenticated bool   `json:"authenticated"`
	AutoLogin     bool   `json:"autoLogin,omitempty"`
}

// chunkName returns the i-th chunk's cookie name.
func chunkName(i int) string {
	if i == 0 {
		return cookieSession
	}
	return cookieSession + "-" + strconv.Itoa(i)
}

// writeChunked splits value across the session cookie chunks. Leftover
// chunks from a previously longer session are cleared.
func writeChunked(w http.ResponseWriter, value string, maxAge time.Duration, secure bool) error {
	chunks := (len(value) + chunkSize - 1) / chunkSize
	if chunks > maxChunks {
		return fmt.Errorf("session cookie needs %d chunks, limit %d", chunks, maxChunks)
	}
	for i := range maxChunks {
		start := i * chunkSize
		if start >= len(value) {
			clearCookie(w, chunkName(i), secure)
			continue
		}
		end := min(start+chunkSize, len(value))
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName(chunkName(i), secure),
			Value:    value[start:end],
			Path:     "/",
			MaxAge:   int(maxAge.Seconds()),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
	return nil
}

// readChunked reassembles the session cookie value; "" means no session.
func readChunked(r *http.Request, secure bool) string {
	var b strings.Builder
	for i := range maxChunks {
		c, err := r.Cookie(cookieName(chunkName(i), secure))
		if err != nil || c.Value == "" {
			break
		}
		b.WriteString(c.Value)
	}
	return b.String()
}

// clearChunked removes every session chunk.
func clearChunked(w http.ResponseWriter, secure bool) {
	for i := range maxChunks {
		clearCookie(w, chunkName(i), secure)
	}
}

// clearCookie expires one cookie with the same name and scope used to set
// it. __Host- prefix validation applies to deletions too.
func clearCookie(w http.ResponseWriter, name string, secure bool) {
	expireCookie(w, cookieName(name, secure), "/", secure)
}

func expireCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearLegacyCookies removes old host-only cookies when encountered, without
// trusting their contents or accepting them for authentication. A request
// does not reveal Domain: do not guess parent domains to clear cookies a
// sibling might have injected. Those remain inert and are never read.
func clearLegacyCookies(w http.ResponseWriter, r *http.Request, secure bool) {
	names := [maxChunks + 4]string{cookieOAuthState, CookieProvider, CookieAuthError, CookieLogout}
	for i := range maxChunks {
		names[i+4] = chunkName(i)
	}
	for _, name := range names {
		legacy := strings.TrimPrefix(name, "__Host-")
		if _, err := r.Cookie(legacy); err != nil {
			continue
		}
		path := "/"
		if name == cookieOAuthState {
			path = "/oauth2/"
		}
		expireCookie(w, legacy, path, secure)
	}
}

// setJSONCookie writes an SPA-visible base64url JSON cookie. Not HttpOnly by
// design — the SPA reads it; it must never carry a secret.
func setJSONCookie(w http.ResponseWriter, name string, v any, maxAge time.Duration, secure bool) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cookie %s: %w", name, err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(name, secure),
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// readJSONCookie decodes an SPA-visible cookie into v, reporting presence.
func readJSONCookie(r *http.Request, name string, v any, secure bool) bool {
	c, err := r.Cookie(cookieName(name, secure))
	if err != nil || c.Value == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}
