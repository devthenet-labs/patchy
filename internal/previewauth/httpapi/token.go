// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

func resultAttr(result string) attribute.KeyValue { return attribute.String("result", result) }

// tokenResponse is RFC 6749 §5.1 plus the ID token.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, e *previewauth.Error) {
	if e.Basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="preview-auth"`)
	}
	if e.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, e.Status, map[string]string{"error": e.Code, "error_description": e.Description})
}

// handleToken is the ALB's backchannel.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := rec(r)
	grant := "unknown"
	fail := func(err error) {
		e := previewauth.TokenError(err)
		if e.Code == previewauth.CodeTemporarilyUnavailable {
			s.log.LogAttrs(ctx, slog.LevelError, "preview-auth token: transient failure", slog.Any("error", err))
		}
		a.result = e.Code
		count(ctx, tokenCounter, attribute.String("grant", grant), resultAttr(e.Code))
		writeOAuthError(w, e)
	}
	f, err := form(r)
	if err != nil {
		if isTooLarge(err) {
			fail(&previewauth.Error{Status: http.StatusRequestEntityTooLarge, Code: previewauth.CodeInvalidRequest,
				Description: "body too large"})
			return
		}
		fail(&previewauth.Error{Status: http.StatusBadRequest, Code: previewauth.CodeInvalidRequest,
			Description: "body must be a form"})
		return
	}
	client, err := s.cfg.Ring.AuthenticateClient(r.Header.Get("Authorization"), f, s.cfg.SlotCount)
	if err != nil {
		fail(err)
		return
	}
	a.slot = client.Slot
	req, err := previewauth.ParseTokenRequest(f)
	if err != nil {
		fail(err)
		return
	}
	grant = req.GrantType
	var toks previewauth.Tokens
	switch req.GrantType {
	case previewauth.GrantAuthorizationCode:
		toks, err = s.redeem(r, client, req)
	default:
		toks, err = s.refresh(r, client, req)
	}
	if err != nil {
		fail(err)
		return
	}
	idToken, err := s.cfg.Signer.Sign(toks.IDToken)
	if err != nil {
		fail(err)
		return
	}
	a.result = "ok"
	count(ctx, tokenCounter, attribute.String("grant", grant), resultAttr("ok"))
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: toks.AccessToken, TokenType: "Bearer", ExpiresIn: toks.ExpiresIn, IDToken: idToken,
		RefreshToken: toks.RefreshToken,
	})
}

// redeem answers a code: open it, judge it against the client and the
// request, re-check the Preview and the access review, and only then spend
// it in the ledger, so a transient failure does not burn a good code.
func (s *Server) redeem(r *http.Request, client previewauth.ClientAuth,
	req previewauth.TokenRequest) (previewauth.Tokens, error) {
	ctx := r.Context()
	now := s.now()
	g, err := s.cfg.Ring.OpenCode(req.Code, now)
	if err != nil {
		return previewauth.Tokens{}, err
	}
	rec(r).bind(g.Bound, g.Sub, g.JTI)
	if err := previewauth.RedeemCode(g, client, req); err != nil {
		return previewauth.Tokens{}, err
	}
	if err := s.stillAllowed(r, g); err != nil {
		return previewauth.Tokens{}, err
	}
	if err := s.cfg.Ledger.Consume(ctx, previewauth.CodeKey(g), timeOf(g.Expires)); err != nil {
		return previewauth.Tokens{}, err
	}
	return s.cfg.Ring.TokensForCode(now, s.cfg.Lifetimes, s.cfg.Issuer, g)
}

func timeOf(unix int64) time.Time { return time.Unix(unix, 0) }

// refresh answers a refresh token with the same refresh token.
func (s *Server) refresh(r *http.Request, client previewauth.ClientAuth,
	req previewauth.TokenRequest) (previewauth.Tokens, error) {
	now := s.now()
	g, err := s.cfg.Ring.OpenRefresh(req.RefreshToken, now)
	if err != nil {
		return previewauth.Tokens{}, err
	}
	rec(r).bind(g.Bound, g.Sub, g.JTI)
	if err := previewauth.CheckRefresh(g, client); err != nil {
		return previewauth.Tokens{}, err
	}
	if err := s.stillAllowed(r, g); err != nil {
		return previewauth.Tokens{}, err
	}
	return s.cfg.Ring.TokensForRefresh(now, s.cfg.Lifetimes, s.cfg.Issuer, g, req.RefreshToken)
}

// stillAllowed re-checks a grant's Preview (same UID, label and slot, still
// live) and re-runs the access review on its sealed identity.
func (s *Server) stillAllowed(r *http.Request, g previewauth.Grant) error {
	ctx := r.Context()
	view, err := s.cfg.Lookup.ByLabel(ctx, g.Label)
	if err != nil {
		return err
	}
	if err := g.Matches(view); err != nil {
		return err
	}
	rec(r).project = view.Project
	ok, err := s.review(ctx, g.Identity, view)
	if err != nil {
		return err
	}
	if !ok {
		return previewauth.ErrDenied
	}
	return nil
}

// handleUserinfo answers {"sub"} for a live access token.
func (s *Server) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := rec(r)
	fail := func(err error) {
		e := previewauth.UserinfoError(err)
		switch e.Status {
		case http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		case http.StatusBadRequest:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
		case http.StatusServiceUnavailable:
			s.log.LogAttrs(ctx, slog.LevelError, "preview-auth userinfo: transient failure", slog.Any("error", err))
		}
		a.result = e.Code
		count(ctx, userinfoCounter, resultAttr(e.Code))
		writeOAuthError(w, e)
	}
	var f map[string][]string
	if r.Method == http.MethodPost {
		v, err := form(r)
		if err != nil {
			fail(&previewauth.Error{Status: http.StatusBadRequest, Code: previewauth.CodeInvalidRequest,
				Description: "body must be a form"})
			return
		}
		f = v
	}
	tok, err := previewauth.ParseBearer(r.Method, r.Header.Get("Authorization"), f)
	if err != nil {
		var e *previewauth.Error
		if errors.As(err, &e) && e.Status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", "GET, POST")
		}
		fail(err)
		return
	}
	acc, err := s.cfg.Ring.OpenAccess(tok, s.now())
	if err != nil {
		fail(err)
		return
	}
	a.bind(acc.Bound, acc.Sub, acc.JTI)
	view, err := s.cfg.Lookup.ByLabel(ctx, acc.Label)
	if err != nil {
		fail(err)
		return
	}
	if err := acc.Matches(view); err != nil {
		fail(err)
		return
	}
	a.project = view.Project
	a.result = "ok"
	count(ctx, userinfoCounter, resultAttr("ok"))
	writeJSON(w, http.StatusOK, map[string]string{"sub": acc.Sub})
}

// handleLogout ends the relay session. Only a same-origin form may.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	a := rec(r)
	if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		a.result = "cross_site"
		renderPage(w, http.StatusForbidden, pageForbidden)
		return
	}
	http.SetCookie(w, expire(previewauth.RelaySessionCookie))
	a.result = "ok"
	renderPage(w, http.StatusOK, pageSignedOut)
}
