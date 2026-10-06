// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"strings"
)

// The hardened browser envelope, on whenever the intents views are. Preview
// apps run pull request code on a host that is same-site with the status
// host, and same-site requests carry the session's SameSite=Lax cookies, so
// a preview page could otherwise open the intents streams with the viewer's
// credentials (pinning the live-follow budget) or post an action. The
// checks:
//
//   - every /api request and /events refuses Sec-Fetch-Site same-site and
//     cross-site (a missing header passes on reads: curl, the CLI, older
//     browsers);
//   - every write also refuses a missing Sec-Fetch-Site, failing closed;
//   - streams are no-store, like every other API response;
//   - a Content-Security-Policy whose script-src admits only the embedded
//     bundle's own inline scripts, by hash, and HSTS.

// maxEventSubscribers bounds the public /events stream's subscribers while
// the envelope is hardened: it is unauthenticated, so the cap is global, and
// a subscriber past it drops the oldest rather than being refused
// (broker.subscribeCapped), so filling it locks no one out.
const maxEventSubscribers = 512

// hstsValue keeps browsers on HTTPS for the status host for a year. Not
// includeSubDomains: the status host's own subdomains are not the
// operator's to promise.
const hstsValue = "max-age=31536000"

// buildCSP is the policy for the embedded SPA. The single-file build inlines
// its scripts, so script-src lists each inline script's sha256 rather than
// 'unsafe-inline'; a script injected into the page has no matching hash.
// Styles stay 'unsafe-inline' (the components set style attributes), plus
// the font stylesheet the page loads.
func buildCSP(assets fs.FS) string {
	scripts := []string{"'self'"}
	if assets != nil {
		if index, err := fs.ReadFile(assets, "index.html"); err == nil {
			for _, body := range inlineScripts(string(index)) {
				sum := sha256.Sum256([]byte(body))
				scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
			}
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
		"font-src 'self' https://fonts.gstatic.com data:",
		"img-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// inlineScripts returns the body of every <script> element in html that has
// no src attribute, byte for byte as the browser hashes it.
func inlineScripts(html string) []string {
	var out []string
	rest := html
	for {
		i := strings.Index(rest, "<script")
		if i < 0 {
			return out
		}
		rest = rest[i:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return out
		}
		tag := rest[:end]
		rest = rest[end+1:]
		closing := strings.Index(rest, "</script>")
		if closing < 0 {
			return out
		}
		if !strings.Contains(tag, " src=") {
			out = append(out, rest[:closing])
		}
		rest = rest[closing+len("</script>"):]
	}
}

// hardened reports whether the hardened envelope is on.
func (s *Server) hardened() bool { return s.intents != nil }

// hardenedRequest applies the hardened envelope's request checks and
// headers, answering a refused request itself; ok is false when it did.
func (s *Server) hardenedRequest(w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	h.Set("Content-Security-Policy", s.csp)
	h.Set("Strict-Transport-Security", hstsValue)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	data := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/events"
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if !data && !write {
		// Page loads and the sign-in navigations (the provider's redirect
		// back to /oauth2/callback is cross-site by nature).
		return true
	}
	site := r.Header.Get("Sec-Fetch-Site")
	switch {
	case site == "same-site" || site == "cross-site":
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	case site == "" && write:
		http.Error(w, "request rejected: no Sec-Fetch-Site header", http.StatusForbidden)
		return false
	}
	return true
}

// streamCacheControl is the Cache-Control a stream sets: no-store when the
// envelope is hardened, else no-cache, as streams always have.
func (s *Server) streamCacheControl() string {
	if s.hardened() {
		return "no-store"
	}
	return "no-cache"
}
