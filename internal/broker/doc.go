// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package broker is the egress credential broker's engine: a reconciler-less
// reverse proxy that agent pods address directly (base-URL env overrides in
// the pod point the claude CLI at it) and that injects or signs the model
// credential outbound. It is what makes agent pods fully credential-less —
// the pod authenticates to the broker with an audience-bound projected
// ServiceAccount token (an identity document, not a capability), and the
// broker alone holds the Anthropic API key or the cloud workload identity
// that signs Bedrock/Vertex/Foundry traffic.
//
// One route per upstream, keyed by path prefix. The route set is closed:
// Config.validate admits only the model providers in provider.Names
// (anthropic, bedrock, vertex, foundry), because every route is a positive
// method+path surface (surface.go) and a metered model-inference contract,
// not an open proxy. Brokering another upstream (forge-minted GitHub tokens,
// package registries) means adding its name and a surface for it, not only a
// route. Every request is audited as a single slog line (never bodies or
// headers) and streamed through with immediate flushing so SSE responses
// survive multi-minute runs.
//
// The broker is also the enforcement point for what a pod may ask of the
// model API and how much, because under a repository-declared agent image
// the in-pod kill switch is advisory and the caller token is readable by
// anything in the pod. Enforcement runs in layers before any upstream
// contact: pre-authentication (a syntactic token check, a per-source-IP
// bucket and a global TokenReview limiter, so a token-header flood degrades
// only its source); the TokenReview with its verdict cache; a positive
// method+path surface per route, 404 elsewhere, so Files, Batches and the
// rest of the upstream API are unreachable; body inspection that refuses
// server-side tools, MCP servers, containers and file references and strips
// denied anthropic-beta entries; the model allowlist, read from the path on
// bedrock and vertex and from the body elsewhere; and the spend ledger —
// per-pod request, concurrency and token counters plus a broker-wide hourly
// token ceiling, charged from every usage field the response reports and
// from a request-size estimate when a stream is cut. Over any limit is a 429
// whose message carries provider.LimitMessagePrefix, which the in-pod runtime
// maps to budget_exceeded. TokenReview remains the broker's only Kubernetes
// access; the ledger is in-memory per replica.
package broker
