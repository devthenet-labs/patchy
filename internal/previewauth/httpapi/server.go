// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// MaxBodyBytes caps every request body.
const MaxBodyBytes = 8 << 10

// Config is everything the relay's HTTP surface is built from.
type Config struct {
	// Issuer is the relay's issuer: https://<relay host>, byte-equal in
	// discovery, every ID token and the preview Ingress annotations.
	Issuer    string
	SlotCount int
	Callbacks previewauth.Callbacks
	Lifetimes previewauth.Lifetimes
	Ring      *previewauth.KeyRing

	Lookup     previewauth.PreviewLookup
	Authorizer previewauth.Authorizer
	Ledger     previewauth.CodeLedger
	Upstream   previewauth.Upstream
	Signer     previewauth.Signer

	RateLimit RateLimit
	Log       *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (c Config) validate() error {
	switch {
	case !previewauth.ValidIssuer(c.Issuer):
		return fmt.Errorf("issuer %q is not an https origin with no path", c.Issuer)
	case c.SlotCount < 1 || c.SlotCount > previewauth.MaxSlots:
		return fmt.Errorf("slot count %d is not in [1, %d]", c.SlotCount, previewauth.MaxSlots)
	case c.Callbacks.Suffix() == "":
		return errors.New("no preview host suffix")
	case c.Ring == nil || c.Lookup == nil || c.Authorizer == nil || c.Ledger == nil || c.Upstream == nil ||
		c.Signer == nil:
		return errors.New("a key ring, lookup, authorizer, ledger, upstream and signer are all required")
	}
	if host := strings.TrimPrefix(c.Issuer, "https://"); host == c.Callbacks.Suffix() ||
		strings.HasSuffix(host, "."+c.Callbacks.Suffix()) {
		return fmt.Errorf("the relay host %s sits under the preview host suffix %s", host, c.Callbacks.Suffix())
	}
	return c.Lifetimes.Validate()
}

// Server is the relay's HTTP surface.
type Server struct {
	cfg       Config
	log       *slog.Logger
	now       func() time.Time
	limiter   *limiter
	discovery []byte
}

// New validates cfg and builds the Server.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("preview-auth: %w", err)
	}
	s := &Server{cfg: cfg, log: cfg.Log, now: cfg.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.limiter = newLimiter(cfg.RateLimit, s.now)
	d, err := discoveryDocument(cfg.Issuer)
	if err != nil {
		return nil, err
	}
	s.discovery = d
	return s, nil
}

// Handler is the relay's public handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.only(http.MethodGet, s.handleDiscovery))
	mux.HandleFunc("/jwks", s.only(http.MethodGet, s.handleJWKS))
	mux.HandleFunc("/authorize", s.only(http.MethodGet, s.handleAuthorize))
	mux.HandleFunc("/dex/callback", s.only(http.MethodGet, s.handleCallback))
	mux.HandleFunc("/token", s.only(http.MethodPost, s.handleToken))
	mux.HandleFunc("/userinfo", s.handleUserinfo)
	mux.HandleFunc("/logout", s.only(http.MethodPost, s.handleLogout))
	mux.HandleFunc("/", s.handleRoot)
	return s.envelope(mux)
}

// auditKey carries a request's audit record through its handler.
type auditKey struct{}

// record is one request's audit line.
type record struct {
	endpoint string
	slot     int
	label    string
	project  string
	result   string
	sub      string
	jti      string
}

func rec(r *http.Request) *record {
	if a, ok := r.Context().Value(auditKey{}).(*record); ok {
		return a
	}
	return &record{}
}

// bind fills the audit record from a token's binding.
func (a *record) bind(b previewauth.Bound, sub, jti string) {
	a.slot, a.label, a.sub = b.Slot, b.Label, sub
	if jti != "" {
		sum := sha256.Sum256([]byte(jti))
		a.jti = hex.EncodeToString(sum[:4])
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// envelope sets the headers every response carries, refuses methods other
// than GET and POST, caps the body, applies the rate limit and writes the
// audit line.
func (s *Server) envelope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; "+
			"frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		a := &record{endpoint: endpointName(r.URL.Path), slot: -1}
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			observe(r.Context(), a.endpoint, s.now().Sub(start))
			s.log.LogAttrs(r.Context(), slog.LevelInfo, "preview-auth",
				slog.String("endpoint", a.endpoint), slog.String("method", r.Method), slog.Int("status", sw.status),
				slog.Int("slot", a.slot), slog.String("label", a.label), slog.String("project", a.project),
				slog.String("result", a.result), slog.String("sub", a.sub), slog.String("jti", a.jti),
				slog.Duration("duration", s.now().Sub(start)))
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			a.result = "method_not_allowed"
			h.Set("Allow", "GET, POST")
			renderPage(sw, http.StatusMethodNotAllowed, pageNotAllowed)
			return
		}
		if !s.limiter.allow(r) {
			a.result = "rate_limited"
			count(r.Context(), limitedCounter)
			h.Set("Retry-After", "1")
			if a.endpoint == "token" || a.endpoint == "userinfo" {
				// The ALB's backchannel: a 503, which it does not treat as a
				// sign-out.
				writeOAuthError(sw, &previewauth.Error{Status: http.StatusServiceUnavailable,
					Code: previewauth.CodeTemporarilyUnavailable, Description: "try again"})
				return
			}
			renderPage(sw, http.StatusTooManyRequests, pageRateLimited)
			return
		}
		r.Body = http.MaxBytesReader(sw, r.Body, MaxBodyBytes)
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, a)))
	})
}

func endpointName(path string) string {
	switch path {
	case "/.well-known/openid-configuration":
		return "discovery"
	case "/jwks", "/authorize", "/token", "/userinfo", "/logout":
		return path[1:]
	case "/dex/callback":
		return "callback"
	case "/":
		return "root"
	}
	return "other"
}

func (s *Server) only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			rec(r).result = "method_not_allowed"
			w.Header().Set("Allow", method)
			renderPage(w, http.StatusMethodNotAllowed, pageNotAllowed)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		rec(r).result = "not_found"
		renderPage(w, http.StatusNotFound, pageNotFound)
		return
	}
	rec(r).result = "ok"
	renderPage(w, http.StatusOK, pageHome)
}

// query parses the raw query string strictly: a malformed one is an error,
// where URL.Query would silently drop the bad pairs.
func query(r *http.Request) (url.Values, error) {
	return url.ParseQuery(r.URL.RawQuery)
}

// form reads a POST body as application/x-www-form-urlencoded, and only the
// body: a parameter in the query string is never read from it.
func form(r *http.Request) (url.Values, error) {
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(strings.ToLower(mt)) !=
		"application/x-www-form-urlencoded" {
		if r.ContentLength == 0 && ct == "" {
			return url.Values{}, nil
		}
		return nil, errors.New("body is not a form")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(body))
}

// isTooLarge reports whether err came from the body cap.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func (s *Server) sessionCookie(value string) *http.Cookie {
	return &http.Cookie{Name: previewauth.RelaySessionCookie, Value: value, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

func expire(name string) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode}
}
