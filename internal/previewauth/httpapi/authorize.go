// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// Authorize results, the authorize counter's and the audit line's.
const (
	resultIssued     = "issued"
	resultLogin      = "login"
	resultDenied     = "denied"
	resultNoPreview  = "no_preview"
	resultBadRequest = "bad_request"
	resultError      = "error"
)

// handleAuthorize answers the ALB's authorization request. It never
// redirects anything but a code to a validated preview callback, or a
// sign-in to Dex.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	a := rec(r)
	ctx := r.Context()
	fail := func(result string, status int, p pageData) {
		a.result = result
		count(ctx, authorizeCounter, resultAttr(result))
		renderPage(w, status, p)
	}
	q, err := query(r)
	if err != nil {
		fail(resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return
	}
	req, err := previewauth.ParseAuthorize(q, s.cfg.SlotCount, s.cfg.Callbacks)
	if err != nil {
		fail(resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return
	}
	a.slot, a.label = req.Slot, req.Label
	view, err := s.cfg.Lookup.ByLabel(ctx, req.Label)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "preview lookup failed", slog.String("label", req.Label),
			slog.Any("error", err))
		fail(resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	if previewauth.Admit(req.Label, req.Slot, view) != nil {
		fail(resultNoPreview, http.StatusNotFound, pageNoPreview)
		return
	}
	a.project = view.Project
	if c, err := r.Cookie(previewauth.RelaySessionCookie); err == nil {
		if sess, err := s.cfg.Ring.OpenSession(c.Value, s.now()); err == nil {
			s.issue(w, r, req, view, sess)
			return
		}
	}
	s.startLogin(w, r, req, view)
}

// startLogin sends the browser to Dex with a sealed login state and its
// own login cookie.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request, req previewauth.AuthorizeRequest,
	view previewauth.View) {
	ctx := r.Context()
	a := rec(r)
	st, csrf, err := previewauth.NewLogin(s.now(), s.cfg.Lifetimes, req, view.UID)
	if err != nil {
		s.unavailable(w, r, "start sign-in", err)
		return
	}
	state, err := s.cfg.Ring.SealLogin(st)
	if err != nil {
		s.unavailable(w, r, "seal sign-in state", err)
		return
	}
	loc, err := s.cfg.Upstream.AuthURL(ctx, state, st.UpstreamNonce, st.UpstreamChallenge())
	if err != nil {
		s.unavailable(w, r, "identity provider", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: previewauth.LoginCookieName(state), Value: csrf, Path: "/",
		MaxAge: int(s.cfg.Lifetimes.Login.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	a.result = resultLogin
	count(ctx, authorizeCounter, resultAttr(resultLogin))
	http.Redirect(w, r, loc, http.StatusFound)
}

func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.LogAttrs(r.Context(), slog.LevelError, "preview sign-in: "+what, slog.Any("error", err))
	rec(r).result = resultError
	count(r.Context(), authorizeCounter, resultAttr(resultError))
	renderPage(w, http.StatusServiceUnavailable, pageUnavailable)
}

// issue runs the access review for a signed-in viewer and sends the
// browser back to the preview host with a code.
func (s *Server) issue(w http.ResponseWriter, r *http.Request, req previewauth.AuthorizeRequest,
	view previewauth.View, sess previewauth.Session) {
	ctx := r.Context()
	a := rec(r)
	allowed, err := s.review(ctx, sess.Identity, view)
	switch {
	case errors.Is(err, previewauth.ErrNoProject) || err == nil && !allowed:
		a.result = resultDenied
		count(ctx, authorizeCounter, resultAttr(resultDenied))
		renderPage(w, http.StatusForbidden, pageDenied)
		return
	case err != nil:
		s.unavailable(w, r, "access review", err)
		return
	}
	now := s.now()
	code, err := s.cfg.Ring.IssueCode(now, s.cfg.Lifetimes, req, view, sess)
	if err != nil {
		s.unavailable(w, r, "issue code", err)
		return
	}
	if g, err := s.cfg.Ring.OpenCode(code, now); err == nil {
		a.bind(g.Bound, g.Sub, g.JTI)
	}
	a.result = resultIssued
	count(ctx, authorizeCounter, resultAttr(resultIssued))
	http.Redirect(w, r, previewauth.CodeRedirect(req.RedirectURI, code, req.State), http.StatusFound)
}

// review asks whether id may view v's previews. A refusal is logged at
// warn with the login, for operators; nothing else logs it.
func (s *Server) review(ctx context.Context, id previewauth.Identity, v previewauth.View) (bool, error) {
	ar, err := previewauth.ReviewFor(id, v)
	if err != nil {
		return false, err
	}
	ok, err := s.cfg.Authorizer.Allowed(ctx, ar)
	if err != nil {
		return false, err
	}
	if !ok {
		s.log.LogAttrs(ctx, slog.LevelWarn, "preview viewer refused by access review",
			slog.String("user", id.Username), slog.String("project", v.Project), slog.String("label", v.Label))
	}
	return ok, nil
}

// handleCallback is the one Dex redirect URI.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := rec(r)
	login := func(result string, status int, p pageData) {
		a.result = result
		count(ctx, upstreamCounter, resultAttr(result))
		renderPage(w, status, p)
	}
	q, err := query(r)
	if err != nil || hasRepeats(q) || q.Get("state") == "" {
		login(resultBadRequest, http.StatusBadRequest, pageBadRequest)
		return
	}
	state := q.Get("state")
	st, err := s.cfg.Ring.OpenLogin(state, s.now())
	if err != nil {
		login("expired", http.StatusBadRequest, pageSignInExpired)
		return
	}
	cookieName := previewauth.LoginCookieName(state)
	c, err := r.Cookie(cookieName)
	if err != nil || st.CheckCSRF(c.Value) != nil {
		login("csrf", http.StatusBadRequest, pageSignInExpired)
		return
	}
	http.SetCookie(w, expire(cookieName))
	a.slot, a.label = st.Slot, st.Label
	if q.Has("error") {
		login("refused", http.StatusForbidden, pageUpstreamRefused)
		return
	}
	id, err := s.cfg.Upstream.Exchange(ctx, q.Get("code"), st.UpstreamVerifier, st.UpstreamNonce)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "preview sign-in: identity provider exchange", slog.Any("error", err))
		login("failed", http.StatusBadGateway, pageUpstreamFailed)
		return
	}
	now := s.now()
	sess, err := previewauth.NewSession(now, s.cfg.Lifetimes, id)
	if err != nil {
		login("failed", http.StatusBadGateway, pageUpstreamFailed)
		return
	}
	tok, err := s.cfg.Ring.SealSession(sess)
	if errors.Is(err, previewauth.ErrIdentityTooLarge) {
		login("too_large", http.StatusBadRequest, pageTooManyGroups)
		return
	}
	if err != nil {
		login(resultError, http.StatusServiceUnavailable, pageUnavailable)
		return
	}
	http.SetCookie(w, s.sessionCookie(tok))
	count(ctx, upstreamCounter, resultAttr("ok"))

	// Resume the authorize request against the same Preview.
	view, err := s.cfg.Lookup.ByLabel(ctx, st.Label)
	if err != nil {
		s.unavailable(w, r, "preview lookup", err)
		return
	}
	if st.Resume(view) != nil {
		a.result = resultNoPreview
		count(ctx, authorizeCounter, resultAttr(resultNoPreview))
		renderPage(w, http.StatusNotFound, pageNoPreview)
		return
	}
	a.project = view.Project
	s.issue(w, r, st.Request(), view, sess)
}

func hasRepeats(v url.Values) bool {
	for _, vals := range v {
		if len(vals) > 1 {
			return true
		}
	}
	return false
}
