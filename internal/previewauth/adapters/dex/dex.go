// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package dex

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// CallbackPath is the relay's one Dex redirect path.
const CallbackPath = "/dex/callback"

// Scopes are what the relay asks Dex for: enough to map an identity, and no
// offline_access (the relay never refreshes at Dex).
var Scopes = []string{gooidc.ScopeOpenID, "profile", "email", "groups"}

// discoveryTimeout bounds one discovery attempt.
const discoveryTimeout = 10 * time.Second

// discoveryBackoff is how long a failed discovery is the answer before the
// next attempt, so sign-ins arriving while Dex is down fail at once instead
// of each waiting out a timeout in turn.
const discoveryBackoff = 5 * time.Second

// Config configures the upstream.
type Config struct {
	// IssuerURL is Dex's issuer.
	IssuerURL string
	// ClientID and ClientSecret are the relay's static client at Dex.
	ClientID     string
	ClientSecret string
	// RedirectURL is <relay issuer>/dex/callback.
	RedirectURL string
	// Claims maps the ID token onto an identity.
	Claims auth.ClaimsConfig
	// HTTPClient, when set, is used for every call to Dex.
	HTTPClient *http.Client
	// AllowHTTP admits an http issuer (tests only).
	AllowHTTP bool
}

// Validate checks the configuration, including the claims posture.
func (c Config) Validate() error {
	u, err := url.Parse(c.IssuerURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (!c.AllowHTTP || u.Scheme != "http")) {
		return fmt.Errorf("dex issuer %q is not an https URL", c.IssuerURL)
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("dex client id and client secret are required")
	}
	r, err := url.Parse(c.RedirectURL)
	if err != nil || r.Scheme != "https" || r.Path != CallbackPath || r.RawQuery != "" || r.Fragment != "" {
		return fmt.Errorf("dex redirect URL %q is not https://<relay host>%s", c.RedirectURL, CallbackPath)
	}
	return CheckClaims(c.Claims)
}

// CheckClaims is the posture a relay's claims mapping must meet. Prefixes
// keep a binding written for a signed-in viewer from matching a cluster
// identity of the same name, and a provider group such as system:masters
// from arriving unprefixed; an email username must be a verified one.
func CheckClaims(c auth.ClaimsConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Username == "" || c.Groups == "" {
		return errors.New("claims: the username and groups claims are required")
	}
	if c.UsernamePrefix == "" || c.GroupsPrefix == "" {
		return errors.New("claims: a username prefix and a groups prefix are required, so a viewer binding " +
			"cannot match a cluster identity of the same name")
	}
	if c.Username == "email" && !c.RequireVerifiedEmail {
		return errors.New("claims: the email username claim needs requireVerifiedEmail")
	}
	return nil
}

// ClientWithCA is an HTTP client for Dex that trusts the PEM certificates in
// caPEM besides the system's roots: a Dex behind a private CA. Every
// certificate in it must parse, and there must be at least one.
func ClientWithCA(caPEM []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	n := 0
	for rest := caPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("dex CA bundle: a %s block, want CERTIFICATE", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("dex CA bundle: %w", err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		return nil, errors.New("dex CA bundle holds no PEM certificate")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: discoveryTimeout}, nil
}

// Upstream signs viewers in at Dex.
type Upstream struct {
	cfg Config

	mu       sync.Mutex
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	failed   time.Time
	lastErr  error
	now      func() time.Time
}

var _ previewauth.Upstream = (*Upstream)(nil)

// New validates cfg and returns an Upstream; discovery happens on first use.
func New(cfg Config) (*Upstream, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Upstream{cfg: cfg, now: time.Now}, nil
}

func (u *Upstream) ctx(ctx context.Context) context.Context {
	if u.cfg.HTTPClient != nil {
		return gooidc.ClientContext(ctx, u.cfg.HTTPClient)
	}
	return ctx
}

// discover returns the provider, discovering it once it is reachable.
func (u *Upstream) discover(ctx context.Context) (*gooidc.Provider, *gooidc.IDTokenVerifier, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.provider != nil {
		return u.provider, u.verifier, nil
	}
	if u.lastErr != nil && u.now().Sub(u.failed) < discoveryBackoff {
		return nil, nil, u.lastErr
	}
	dctx, cancel := context.WithTimeout(u.ctx(context.WithoutCancel(ctx)), discoveryTimeout)
	defer cancel()
	p, err := gooidc.NewProvider(dctx, u.cfg.IssuerURL)
	if err != nil {
		u.failed, u.lastErr = u.now(), fmt.Errorf("dex discovery %s: %w", u.cfg.IssuerURL, err)
		return nil, nil, u.lastErr
	}
	u.provider = p
	u.verifier = p.Verifier(&gooidc.Config{ClientID: u.cfg.ClientID})
	return u.provider, u.verifier, nil
}

// Ready reports whether discovery has succeeded, attempting it if not.
func (u *Upstream) Ready(ctx context.Context) error {
	_, _, err := u.discover(ctx)
	return err
}

func (u *Upstream) oauth2(p *gooidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: u.cfg.ClientID, ClientSecret: u.cfg.ClientSecret, Endpoint: p.Endpoint(),
		RedirectURL: u.cfg.RedirectURL, Scopes: Scopes,
	}
}

// AuthURL is Dex's authorization URL for one sign-in.
func (u *Upstream) AuthURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	p, _, err := u.discover(ctx)
	if err != nil {
		return "", err
	}
	return u.oauth2(p).AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		gooidc.Nonce(nonce)), nil
}

// Exchange redeems code at Dex with the PKCE verifier, verifies the ID token
// (issuer, the relay's client id as audience, expiry, signature) and its
// nonce, and maps its claims.
func (u *Upstream) Exchange(ctx context.Context, code, verifier, nonce string) (previewauth.Identity, error) {
	p, v, err := u.discover(ctx)
	if err != nil {
		return previewauth.Identity{}, err
	}
	if code == "" {
		return previewauth.Identity{}, errors.New("dex returned no code")
	}
	tok, err := u.oauth2(p).Exchange(u.ctx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return previewauth.Identity{}, fmt.Errorf("dex code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return previewauth.Identity{}, errors.New("dex returned no ID token")
	}
	idt, err := v.Verify(u.ctx(ctx), raw)
	if err != nil {
		return previewauth.Identity{}, fmt.Errorf("dex ID token: %w", err)
	}
	if idt.Nonce == "" || idt.Nonce != nonce {
		return previewauth.Identity{}, errors.New("dex ID token nonce does not match")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return previewauth.Identity{}, fmt.Errorf("dex ID token claims: %w", err)
	}
	mapped, err := auth.MapClaims(claims, u.cfg.Claims)
	if err != nil {
		return previewauth.Identity{}, fmt.Errorf("dex ID token: %w", err)
	}
	id := previewauth.Identity{Username: mapped.Username, Groups: mapped.Groups}
	if err := id.Validate(); err != nil {
		return previewauth.Identity{}, err
	}
	return id, nil
}
