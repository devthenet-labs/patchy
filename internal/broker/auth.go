// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// TokenHeader carries the caller's projected ServiceAccount token. It is
// stripped before any byte is forwarded upstream: the token authenticates the
// caller to the broker and must never travel further.
const TokenHeader = provider.BrokerTokenHeader

// podNameExtraKey is where the API server reports the bound pod of a
// pod-bound projected token on the TokenReview user info.
const podNameExtraKey = "authentication.kubernetes.io/pod-name"

// identity is who a validated caller token belongs to.
type identity struct {
	user string // e.g. system:serviceaccount:patchy-agents:patchy-agent
	pod  string // bound pod name: the audit and ledger key; may be empty
}

// errReviewThrottled and errReviewUnavailable are a review the broker could
// not perform: its own limiter would not admit it in time, or the API server
// did not answer. Neither is a verdict on the token, so neither is cached
// or charged to the caller.
var (
	errReviewThrottled   = errors.New("token review throttled")
	errReviewUnavailable = errors.New("token review unavailable")
)

// verdict is one cached TokenReview outcome. Denials are cached too — a
// misbehaving caller retrying a bad token must not translate into API-server
// QPS.
type verdict struct {
	id      identity
	allowed bool
	reason  string
	expiry  time.Time
}

// authenticator validates caller tokens via TokenReview, caching verdicts by
// token hash for the configured TTL and, when configured, queueing behind a
// broker-wide review limiter.
type authenticator struct {
	cs       kubernetes.Interface
	audience string
	subject  string // the only accepted username
	ttl      time.Duration
	reviews  *reviewLimiter // nil: unlimited
	now      func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]verdict
}

func newAuthenticator(cs kubernetes.Interface, cfg Config, reviews *reviewLimiter) *authenticator {
	return &authenticator{
		cs:       cs,
		audience: cfg.Audience,
		subject:  fmt.Sprintf("system:serviceaccount:%s:%s", cfg.AgentNamespace, cfg.AgentServiceAccount),
		ttl:      cfg.VerdictTTL,
		reviews:  reviews,
		now:      time.Now,
		cache:    map[[sha256.Size]byte]verdict{},
	}
}

// authenticate resolves the request's caller token to an identity, reviewing
// it with the API server on cache miss. The error string is caller-safe (it
// never echoes the token); a review that could not be performed returns
// errReviewThrottled or errReviewUnavailable and caches nothing.
func (a *authenticator) authenticate(r *http.Request) (identity, error) {
	token := r.Header.Get(TokenHeader)
	if token == "" {
		return identity{}, fmt.Errorf("missing %s header", TokenHeader)
	}
	key := sha256.Sum256([]byte(token))

	a.mu.Lock()
	v, ok := a.cache[key]
	a.mu.Unlock()
	if !ok || a.now().After(v.expiry) {
		var err error
		if v, err = a.review(r, token); err != nil {
			return identity{}, err
		}
		a.mu.Lock()
		a.cache[key] = v
		a.prune()
		a.mu.Unlock()
	}
	if !v.allowed {
		return identity{}, fmt.Errorf("token rejected: %s", v.reason)
	}
	return v.id, nil
}

// review asks the API server for a verdict on one token, waiting its turn
// behind the review limiter first. A refused slot or an unreachable API
// server is an error, not a verdict: caching it would let a blip — or one
// pod's token flood filling the limiter — lock other callers out.
func (a *authenticator) review(r *http.Request, token string) (verdict, error) {
	if err := a.reviews.admit(r.Context()); err != nil {
		return verdict{}, errReviewThrottled
	}
	expiry := a.now().Add(a.ttl)
	tr, err := a.cs.AuthenticationV1().TokenReviews().Create(r.Context(), &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return verdict{}, errReviewUnavailable
	}
	switch {
	case !tr.Status.Authenticated:
		return verdict{allowed: false, reason: "not authenticated for audience " + a.audience, expiry: expiry}, nil
	case tr.Status.User.Username != a.subject:
		return verdict{allowed: false, reason: "identity is not the agent service account", expiry: expiry}, nil
	}
	id := identity{user: tr.Status.User.Username}
	if pods := tr.Status.User.Extra[podNameExtraKey]; len(pods) > 0 {
		id.pod = pods[0]
	}
	return verdict{allowed: true, id: id, expiry: expiry}, nil
}

// prune drops expired entries; called under mu. The cache is naturally small
// (one entry per live agent pod token), so a sweep on write is enough.
func (a *authenticator) prune() {
	now := a.now()
	for k, v := range a.cache {
		if now.After(v.expiry) {
			delete(a.cache, k)
		}
	}
}
