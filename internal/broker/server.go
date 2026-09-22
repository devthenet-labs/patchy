// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// Server is the broker engine: authenticated, audited, limit-enforcing
// reverse proxies for the configured upstream routes.
type Server struct {
	cfg    Config
	auth   *authenticator
	routes map[string]*route
	ips    *ipLimiter
	ledger *ledger
	models *modelPolicy
	betas  []string
	now    func() time.Time
	log    *slog.Logger
}

// New builds a Server. cs performs the TokenReviews; the broker needs no
// other Kubernetes access of any kind.
func New(cs kubernetes.Interface, cfg Config, log *slog.Logger) (*Server, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := time.Now
	s := &Server{
		cfg:    cfg,
		auth:   newAuthenticator(cs, cfg, newReviewLimiter(cfg.TokenReviewsPerSecond)),
		routes: map[string]*route{},
		ips:    newIPLimiter(cfg.PreauthRequestsPerSecond, cfg.PreauthBurst, now),
		ledger: newLedger(cfg.Limits, now),
		models: newModelPolicy(cfg.Limits.ModelAllowlist),
		betas:  cfg.BetaDenylist,
		now:    now,
		log:    log,
	}
	for name, u := range cfg.Upstreams {
		s.routes[name] = newRoute(name, u, log)
	}
	return s, nil
}

// Handler is the proxy surface: one subtree per configured route, everything
// under it requiring a valid caller token — plus the (unauthenticated) probe
// endpoints, so controllers can check readiness over the same Service URL
// agent pods proxy through.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for name, rt := range s.routes {
		prefix := "/" + name
		mux.Handle(prefix+"/", http.StripPrefix(prefix, s.handle(rt)))
	}
	s.mountHealth(mux)
	return mux
}

// rejection is a request refused before forwarding: the status and error
// envelope the caller sees, and the reason the audit line and counters
// record.
type rejection struct {
	status int
	kind   string
	msg    string
	reason string
}

// admission is a request that passed every layer and may be forwarded.
type admission struct {
	start    time.Time
	id       identity
	ep       endpoint
	model    string
	estimate int64
	release  func()
}

// handle runs the enforcement layers in order — pre-authentication,
// authentication, the route surface and body checks, the spend ledger — and
// forwards only what passes all of them.
func (s *Server) handle(rt *route) http.Handler {
	steps := []func(*http.Request, *route, *admission) *rejection{
		s.preauthenticate, s.authenticate, s.checkRequest, s.reserve,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adm := &admission{start: s.now(), release: func() {}}
		for _, step := range steps {
			if rej := step(r, rt, adm); rej != nil {
				adm.release()
				s.reject(w, r, rt, adm, rej)
				return
			}
		}
		s.proxy(w, r, rt, adm)
	})
}

// preauthenticate runs the checks that need no API server: the token's
// shape and the source IP's bucket. A refusal here is what keeps a flood of
// junk tokens from turning into TokenReview traffic.
func (s *Server) preauthenticate(r *http.Request, _ *route, adm *admission) *rejection {
	if err := tokenShape(r.Header.Get(TokenHeader), s.cfg.Audience, adm.start); err != nil {
		countPreauth(r.Context(), "token_shape")
		return &rejection{http.StatusUnauthorized, "authentication_error", err.Error(), "preauth_token"}
	}
	release, reason := s.ips.acquire(sourceIP(r))
	if reason != "" {
		countPreauth(r.Context(), "source_rate")
		return &rejection{http.StatusTooManyRequests, "rate_limit_error", "egress broker: " + reason, "preauth_rate"}
	}
	adm.release = release
	return nil
}

// authenticate resolves the caller via TokenReview. A failure is charged to
// the source IP; with any per-pod limit configured, a token that is not
// bound to a pod is refused rather than counted in an anonymous bucket.
func (s *Server) authenticate(r *http.Request, _ *route, adm *admission) *rejection {
	id, err := s.auth.authenticate(r)
	if err != nil {
		s.ips.penalize(sourceIP(r))
		return &rejection{http.StatusUnauthorized, "authentication_error", err.Error(), "unauthenticated"}
	}
	adm.id = id
	if s.cfg.Limits.perPod() && id.pod == "" {
		return &rejection{http.StatusUnauthorized, "authentication_error",
			"token is not bound to a pod", "no_pod"}
	}
	return nil
}

// checkRequest matches the route surface, buffers and inspects the body,
// checks the model against the allowlist and strips denied betas.
func (s *Server) checkRequest(r *http.Request, rt *route, adm *admission) *rejection {
	ep, pathModel, ok := rt.surface.match(r.Method, r.URL.EscapedPath())
	if !ok {
		return &rejection{http.StatusNotFound, "not_found_error",
			"egress broker: not a brokered endpoint", "surface"}
	}
	adm.ep, adm.model = ep, pathModel
	if ep.body {
		if rej := s.inspectRequest(r, rt, adm); rej != nil {
			return rej
		}
	}
	if ep.model != modelNone && !s.models.allows(adm.model) {
		return &rejection{http.StatusForbidden, "permission_error",
			fmt.Sprintf("egress broker: model %q is not allowlisted", adm.model), "model"}
	}
	filterBetas(r.Header, s.betas)
	return nil
}

// inspectRequest buffers the body under the route's cap and refuses what
// the upstream must never be asked for on a pod's behalf.
func (s *Server) inspectRequest(r *http.Request, rt *route, adm *admission) *rejection {
	limit := s.cfg.MaxAnthropicRequestBytes
	if rt.upstream.BufferBody {
		limit = s.cfg.MaxRequestBytes
	}
	body, fit, err := bufferBody(r, limit)
	if err != nil {
		return &rejection{http.StatusBadRequest, "invalid_request_error",
			"egress broker: read request body", "body_read"}
	}
	if !fit {
		return &rejection{http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("egress broker: request body exceeds %d bytes", limit), "body_size"}
	}
	insp, err := inspectBody(body)
	if err != nil {
		return &rejection{http.StatusForbidden, "permission_error", "egress broker: " + err.Error(), "body"}
	}
	adm.estimate = insp.estimate
	if adm.ep.model == modelBody {
		adm.model = insp.model
	}
	if c := s.cfg.Limits.MaxTokensCeiling; c > 0 && insp.maxTokens > c {
		return &rejection{http.StatusTooManyRequests, "rate_limit_error",
			fmt.Sprintf("%s: max_tokens %d exceeds the ceiling %d", provider.LimitMessagePrefix, insp.maxTokens, c),
			"max_tokens"}
	}
	return nil
}

// reserve admits the request to the spend ledger: request count, in-flight
// slot, the pod's token total and the hourly ceiling.
func (s *Server) reserve(_ *http.Request, _ *route, adm *admission) *rejection {
	release, reason := s.ledger.admit(adm.id.pod)
	if reason != "" {
		return &rejection{http.StatusTooManyRequests, "rate_limit_error", reason, "limit"}
	}
	prev := adm.release
	adm.release = func() { release(); prev() }
	return nil
}

// reject answers a refused request with the error envelope and records it.
func (s *Server) reject(w http.ResponseWriter, r *http.Request, rt *route, adm *admission, rej *rejection) {
	writeAPIError(w, rej.status, rej.kind, rej.msg)
	countRequest(r.Context(), rt.name, rej.reason)
	audit(s.log, r, auditEntry{
		pod: adm.id.pod, route: rt.name, model: adm.model, reason: rej.reason,
		status: rej.status, elapsed: s.now().Sub(adm.start), totals: s.ledger.totals(adm.id.pod),
	})
}

// proxy forwards an admitted request, charging usage as the response streams
// through and settling the estimate when it ends short.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, rt *route, adm *admission) {
	pod := adm.id.pod
	aw := &auditWriter{ResponseWriter: w}
	var inner http.ResponseWriter = aw
	var scan *usageScanner
	var charged usage
	if adm.ep.metered {
		scan = &usageScanner{}
		inner = &usageWriter{ResponseWriter: aw, scan: scan, onUsage: func(d usage) {
			charged = charged.mergeAdd(d)
			s.ledger.charge(pod, d.total())
		}}
	}
	sw := newSSEWriter(inner, s.cfg.PingInterval)
	// Settlement is deferred so it also runs when the proxy aborts on a
	// client disconnect (it panics with http.ErrAbortHandler): a cut stream
	// must still be charged and audited. sw.stop is deferred after it so it
	// runs first and no ping can write during settlement.
	defer func() {
		var estimated int64
		if scan != nil {
			if d := scan.finish(); d.total() > 0 {
				charged = charged.mergeAdd(d)
				s.ledger.charge(pod, d.total())
			}
			if !scan.complete && aw.status/100 == 2 && charged.total() < adm.estimate {
				estimated = adm.estimate - charged.total()
				s.ledger.charge(pod, estimated)
			}
			countTokens(r.Context(), rt.name, charged, estimated)
		}
		adm.release()
		countRequest(r.Context(), rt.name, "proxied")
		audit(s.log, r, auditEntry{
			pod: pod, route: rt.name, model: adm.model, status: aw.status, bytes: aw.bytes,
			elapsed: s.now().Sub(adm.start), tokens: charged.total() + estimated, estimated: estimated > 0,
			totals: s.ledger.totals(pod),
		})
	}()
	defer sw.stop()
	rt.proxy.ServeHTTP(sw, r)
}

// HealthHandler serves the probe endpoints on the health listener: healthz
// is liveness, readyz additionally requires every configured route's
// credential source to be usable — which is what replaces controller-side
// Secret probing as "is the model credential present". Neither touches the
// API server: TokenReview being down must fail requests, not probes.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	s.mountHealth(mux)
	return mux
}

// mountHealth registers the probe endpoints on a mux.
func (s *Server) mountHealth(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for _, name := range slices.Sorted(maps.Keys(s.routes)) {
			rt := s.routes[name]
			if rt.upstream.Ready == nil {
				continue
			}
			if err := rt.upstream.Ready(ctx); err != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "route not ready",
					slog.String("route", name), slog.Any("error", err))
				http.Error(w, fmt.Sprintf("route %s: credential source unusable", name),
					http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}
