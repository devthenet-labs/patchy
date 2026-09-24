// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
)

// Secret keys a Forge credential Secret may carry (shared shape with
// Integration secrets — see internal/ghsecret).
const (
	SecretKeyToken      = ghsecret.KeyToken
	SecretKeyAppID      = ghsecret.KeyAppID
	SecretKeyPrivateKey = ghsecret.KeyPrivateKey
)

// Scope is the credential scope a caller needs.
type Scope string

// Credential scopes: read mints contents:read (clone/tarball); write mints
// contents:write (branch push) — pull-request creation rides the same
// installation client.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
)

// Store resolves Forges and mints their credentials.
type Store struct {
	c    client.Reader
	apps *ghsecret.Apps

	// now is the clock TokenWith's cache expires against; tests pin it.
	now func() time.Time

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken // TokenWith's minted tokens
}

// NewStore builds a Store reading Forges and Secrets through r. Pass the
// manager's API reader (not the cache) so Secrets need no list/watch grant.
func NewStore(r client.Reader) *Store {
	return &Store{
		c:      r,
		apps:   ghsecret.NewApps(),
		now:    time.Now,
		tokens: make(map[tokenKey]cachedToken),
	}
}

// ErrNoBotIdentity reports a Forge that authenticates with a personal access
// token, which acts as its user and has no App bot login.
var ErrNoBotIdentity = errors.New("forge credential is a personal access token: no App bot identity")

// tokenRefreshMargin is how long before its expiry a cached TokenWith token
// is replaced: longer than any one operation (a push of a large changeset)
// runs with it.
const tokenRefreshMargin = 5 * time.Minute

// tokenKey identifies one cached token: the App that minted it (a rotated
// Secret builds a new App, so rotation never serves an old App's token),
// the lower-cased repository, and the exact permission set.
type tokenKey struct {
	app   *ghclient.App
	repo  ghclient.Repo
	perms ghclient.TokenPerms
}

// cachedToken is a minted token and its expiry.
type cachedToken struct {
	token   string
	expires time.Time
}

// TokenWith returns a short-lived installation token for the resolved
// repository scoped to exactly that one repository and exactly perms —
// never the installation's full permission set, so perms must request at
// least one permission, each read or write. Tokens are cached per
// (App, repository, perms) and minted again once within tokenRefreshMargin
// of expiry, so an operation-scoped caller does not mint one per call.
// With a PAT the static token is returned as-is and uncached (the PAT
// cannot be narrowed — dev only), with a zero expiry.
func (s *Store) TokenWith(
	ctx context.Context, res *Resolved, perms ghclient.TokenPerms,
) (string, time.Time, error) {
	if err := perms.Validate(); err != nil {
		return "", time.Time{}, err
	}
	secret, err := s.secret(ctx, res.Forge)
	if err != nil {
		return "", time.Time{}, err
	}
	if tok, ok := ghsecret.Token(secret); ok {
		return tok, time.Time{}, nil
	}
	app, err := s.apps.FromSecret(secret, res.Forge.Spec.BaseURL, proxyURL(res.Forge))
	if err != nil {
		return "", time.Time{}, err
	}
	key := tokenKey{
		app:   app,
		repo:  ghclient.Repo{Owner: strings.ToLower(res.Repo.Owner), Name: strings.ToLower(res.Repo.Name)},
		perms: perms,
	}
	s.mu.Lock()
	cached, ok := s.tokens[key]
	s.mu.Unlock()
	if ok && s.now().Add(tokenRefreshMargin).Before(cached.expires) {
		return cached.token, cached.expires, nil
	}
	tok, exp, err := app.ScopedToken(ctx, res.Repo, perms)
	if err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Evict what can no longer be served, so the cache stays bounded by the
	// (App, repository, perms) combinations used within one token lifetime.
	cutoff := s.now().Add(tokenRefreshMargin)
	for k, v := range s.tokens {
		if !cutoff.Before(v.expires) {
			delete(s.tokens, k)
		}
	}
	s.tokens[key] = cachedToken{token: tok, expires: exp}
	return tok, exp, nil
}

// BotLogin returns the login of the resolved Forge's App bot user,
// "<slug>[bot]" — the actor GitHub records on everything the Forge's tokens
// do, and so how a caller recognises its own events. A PAT-credentialed
// Forge has none: ErrNoBotIdentity.
func (s *Store) BotLogin(ctx context.Context, res *Resolved) (string, error) {
	secret, err := s.secret(ctx, res.Forge)
	if err != nil {
		return "", err
	}
	if _, ok := ghsecret.Token(secret); ok {
		return "", ErrNoBotIdentity
	}
	app, err := s.apps.FromSecret(secret, res.Forge.Spec.BaseURL, proxyURL(res.Forge))
	if err != nil {
		return "", err
	}
	return app.BotLogin(ctx)
}

// Resolve lists the Forges in namespace and picks the one covering repoURL.
func (s *Store) Resolve(ctx context.Context, namespace, repoURL string) (*Resolved, error) {
	var forges v1alpha1.ForgeList
	if err := s.c.List(ctx, &forges, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list forges: %w", err)
	}
	return Resolve(forges.Items, repoURL)
}

// Token mints a short-lived token for the resolved repository at the given
// scope. With App auth the token is installation-scoped to the single
// repository and contents at scope, which must be ScopeRead or ScopeWrite —
// any other is refused, never widened; with a PAT the static token is
// returned as-is (the PAT cannot be narrowed — dev only).
func (s *Store) Token(ctx context.Context, res *Resolved, scope Scope) (string, time.Time, error) {
	secret, err := s.secret(ctx, res.Forge)
	if err != nil {
		return "", time.Time{}, err
	}
	if tok, ok := ghsecret.Token(secret); ok {
		return tok, time.Time{}, nil
	}
	app, err := s.apps.FromSecret(secret, res.Forge.Spec.BaseURL, proxyURL(res.Forge))
	if err != nil {
		return "", time.Time{}, err
	}
	perms := ghclient.TokenPerms{Contents: string(scope)}
	return app.ScopedToken(ctx, res.Repo, perms)
}

// Client returns an API client authenticated for the resolved repository —
// the surface for archive downloads, head-SHA resolution, and pull requests.
func (s *Store) Client(ctx context.Context, res *Resolved) (*ghclient.Client, error) {
	secret, err := s.secret(ctx, res.Forge)
	if err != nil {
		return nil, err
	}
	if tok, ok := ghsecret.Token(secret); ok {
		purl, err := ghsecret.ProxyURL(secret, proxyURL(res.Forge))
		if err != nil {
			return nil, err
		}
		return ghclient.NewToken(tok, res.Forge.Spec.BaseURL, ghclient.WithProxy(purl))
	}
	app, err := s.apps.FromSecret(secret, res.Forge.Spec.BaseURL, proxyURL(res.Forge))
	if err != nil {
		return nil, err
	}
	return app.Installation(ctx, res.Repo)
}

// Validate checks the Forge's secret is usable: a non-empty PAT, or a
// parseable App credential, either way with a parseable proxy URL. The Forge
// reconciler calls this for the Ready condition.
func (s *Store) Validate(ctx context.Context, f *v1alpha1.Forge) error {
	secret, err := s.secret(ctx, f)
	if err != nil {
		return err
	}
	return s.apps.Validate(secret, f.Spec.BaseURL, proxyURL(f))
}

// ProxyURL returns the resolved Forge's effective proxy URL — the spec proxy
// with the Secret's basic-auth credentials attached — for callers building
// their own client (the write-path pusher). "" means no spec proxy; the
// environment applies.
func (s *Store) ProxyURL(ctx context.Context, res *Resolved) (string, error) {
	raw := proxyURL(res.Forge)
	if raw == "" {
		return "", nil
	}
	secret, err := s.secret(ctx, res.Forge)
	if err != nil {
		return "", err
	}
	return ghsecret.ProxyURL(secret, raw)
}

// proxyURL returns the Forge's raw spec proxy URL, "" when unset.
func proxyURL(f *v1alpha1.Forge) string {
	if f == nil || f.Spec.Proxy == nil {
		return ""
	}
	return f.Spec.Proxy.URL
}

// secret fetches the Forge's credential Secret from its own namespace.
func (s *Store) secret(ctx context.Context, f *v1alpha1.Forge) (*corev1.Secret, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Spec.SecretRef.Name}
	if err := s.c.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("get forge secret %s: %w", key, err)
	}
	return &secret, nil
}
