// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// Default knob values, applied by Config.withDefaults.
const (
	// DefaultAudience is the projected-token audience callers must present.
	DefaultAudience = "patchy-egress-broker"
	// DefaultVerdictTTL bounds TokenReview QPS: one review per caller token
	// per TTL, not one per request.
	DefaultVerdictTTL = time.Minute
	// DefaultPingInterval is the SSE idle keep-alive period.
	DefaultPingInterval = 30 * time.Second
	// DefaultMaxRequestBytes caps a buffered request body on a
	// payload-signing route (bedrock).
	DefaultMaxRequestBytes = 10 << 20
	// DefaultMaxAnthropicRequestBytes caps a buffered request body on every
	// other route. Every route buffers, so the body can be inspected before
	// it is forwarded.
	DefaultMaxAnthropicRequestBytes = 2 << 20
)

// DefaultBetaDenylist is the anthropic-beta entries stripped when
// Config.BetaDenylist is empty and the deny-list is not disabled: the betas that would have the upstream reach
// MCP servers, the web, a code-execution container or the Files API on the
// pod's behalf, plus the 1M context window (a spend multiplier). A
// deny-list rather than an allowlist because the CLI's beta set changes
// every release.
var DefaultBetaDenylist = []string{"mcp-client-*", "web-fetch-*", "code-execution-*", "files-api-*", "context-1m-*"}

// HelperModelFamilies are the model families the claude CLI calls for its
// own helper turns (summaries, titles) in every run. They are admitted
// beside any configured Limits.ModelAllowlist, so an allowlist naming only
// the stage models does not break every run; an entry matches a wire id
// equal to it or continuing with "-".
var HelperModelFamilies = []string{"claude-haiku"}

// CredentialFunc attaches the upstream credential to one outbound request:
// inject an API key header, attach an OAuth bearer, or SigV4-sign. It runs on
// the rewritten outbound request, after the caller's broker token and any
// inbound authorization headers have been stripped.
type CredentialFunc func(ctx context.Context, req *http.Request) error

// Upstream is one brokered route: where it forwards and how it credentials.
type Upstream struct {
	// Target is the upstream base URL the route prefix maps onto.
	Target *url.URL
	// Credential attaches the upstream credential; nil forwards unmodified.
	Credential CredentialFunc
	// BufferBody marks a route whose Credential hashes the payload, as
	// SigV4 does; its bodies are bounded by Config.MaxRequestBytes rather
	// than MaxAnthropicRequestBytes. Every route buffers regardless, so the
	// body can be inspected.
	BufferBody bool
	// Ready reports whether the route's credential source is usable; readyz
	// fails while any configured route's is not. Nil means always ready.
	Ready func(ctx context.Context) error
	// Project and Location pin the vertex route's surface to one GCP
	// project and location: the path's projects/{project}/locations/
	// {location} segments must equal them literally, so a pod cannot bill
	// or reach another project with the broker's identity. Required on
	// vertex, unused elsewhere.
	Project, Location string
}

// Limits is the spend and capability bound the broker enforces per caller
// pod (the identity TokenReview reports) and broker-wide. Under an untrusted
// agent image the in-pod kill switch is advisory and the caller token is
// readable by anything in the pod, so this is the enforcement point. A zero
// value disables that limit; an empty ModelAllowlist admits every model.
type Limits struct {
	// RequestsPerPod caps the requests one pod may make in its lifetime.
	RequestsPerPod int64
	// ConcurrentPerPod caps a pod's in-flight requests.
	ConcurrentPerPod int64
	// TokensPerPod caps the tokens (input, cache creation, cache read and
	// output; the request-size estimate for a cut or usage-less response)
	// charged to one pod.
	TokensPerPod int64
	// TokensPerHour is the broker-wide trailing-hour token ceiling, the
	// backstop against attacker-minted findings.
	TokensPerHour int64
	// MaxTokensCeiling caps a request's max_tokens.
	MaxTokensCeiling int64
	// ModelAllowlist is the model ids a pod may name, in canonical
	// ("anthropic/claude-sonnet-5") or wire form; dated and versioned
	// variants of an entry match, and HelperModelFamilies are always
	// admitted beside it.
	ModelAllowlist []string
}

// perPod reports whether any per-pod limit is configured, in which case a
// token whose review lacks the bound pod name is rejected outright.
func (l Limits) perPod() bool {
	return l.RequestsPerPod > 0 || l.ConcurrentPerPod > 0 || l.TokensPerPod > 0
}

// Config configures the broker engine.
type Config struct {
	// Audience is the projected-token audience callers must present.
	Audience string
	// AgentNamespace/AgentServiceAccount pin the only identity the broker
	// answers to: system:serviceaccount:<AgentNamespace>:<AgentServiceAccount>.
	AgentNamespace      string
	AgentServiceAccount string
	// VerdictTTL is how long one token's TokenReview verdict is cached.
	VerdictTTL time.Duration
	// PingInterval is the SSE idle keep-alive period; 0 takes the default,
	// negative disables ping injection (pure passthrough).
	PingInterval time.Duration
	// MaxRequestBytes caps a buffered request body on BufferBody routes.
	MaxRequestBytes int64
	// MaxAnthropicRequestBytes caps a buffered request body on every other
	// route.
	MaxAnthropicRequestBytes int64
	// Limits is the per-pod and broker-wide enforcement; zero values are
	// off.
	Limits Limits
	// BetaDenylist is the anthropic-beta patterns (path.Match globs) to
	// strip from the header and from a body anthropic_beta array. Empty —
	// nil or not — takes DefaultBetaDenylist, so the zero value is the safe
	// default.
	BetaDenylist []string
	// DisableBetaDenylist strips no betas at all. It is the only way to
	// turn the deny-list off, and it excludes BetaDenylist.
	DisableBetaDenylist bool
	// PreauthRequestsPerSecond and PreauthBurst are the per-source-IP token
	// bucket ahead of authentication; the burst is also the per-IP in-flight
	// cap. Zero disables.
	PreauthRequestsPerSecond float64
	PreauthBurst             int
	// TokenReviewsPerSecond bounds TokenReview calls broker-wide, with a
	// short queue; zero disables.
	TokenReviewsPerSecond float64
	// Upstreams is the route table, keyed by path prefix; only the
	// provider.Names routes, each with its own surface, are accepted.
	Upstreams map[string]Upstream
}

// withDefaults returns cfg with zero knobs defaulted.
func (c Config) withDefaults() Config {
	if c.Audience == "" {
		c.Audience = DefaultAudience
	}
	if c.VerdictTTL <= 0 {
		c.VerdictTTL = DefaultVerdictTTL
	}
	if c.PingInterval == 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.MaxAnthropicRequestBytes <= 0 {
		c.MaxAnthropicRequestBytes = DefaultMaxAnthropicRequestBytes
	}
	if len(c.BetaDenylist) == 0 && !c.DisableBetaDenylist {
		c.BetaDenylist = slices.Clone(DefaultBetaDenylist)
	}
	return c
}

// validate rejects a config the engine cannot serve.
func (c Config) validate() error {
	if len(c.Upstreams) == 0 {
		return errors.New("broker: no upstream configured")
	}
	if c.AgentNamespace == "" || c.AgentServiceAccount == "" {
		return errors.New("broker: agent namespace and service account are required")
	}
	for name, u := range c.Upstreams {
		if !slices.Contains(provider.Names, name) {
			return fmt.Errorf("broker: upstream %q is not one of %s", name, strings.Join(provider.Names, ", "))
		}
		if u.Target == nil {
			return fmt.Errorf("broker: upstream %q has no target URL", name)
		}
		if name == provider.Vertex {
			for field, v := range map[string]string{"project": u.Project, "location": u.Location} {
				if v == "" || strings.ContainsAny(v, "/{}") {
					return fmt.Errorf("broker: upstream vertex needs a %s without '/', '{' or '}'", field)
				}
			}
		}
	}
	l := c.Limits
	for name, v := range map[string]int64{
		"requests per pod": l.RequestsPerPod, "concurrent per pod": l.ConcurrentPerPod,
		"tokens per pod": l.TokensPerPod, "tokens per hour": l.TokensPerHour,
		"max tokens ceiling": l.MaxTokensCeiling,
	} {
		if v < 0 {
			return fmt.Errorf("broker: %s must not be negative", name)
		}
	}
	if c.PreauthRequestsPerSecond < 0 || c.TokenReviewsPerSecond < 0 || c.PreauthBurst < 0 {
		return errors.New("broker: pre-authentication rates must not be negative")
	}
	if c.DisableBetaDenylist && len(c.BetaDenylist) > 0 {
		return errors.New("broker: a beta deny-list and DisableBetaDenylist are exclusive")
	}
	for _, p := range c.BetaDenylist {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("broker: beta deny pattern %q: %w", p, err)
		}
	}
	return nil
}
