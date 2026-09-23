// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// modelSource says where a request names its model.
type modelSource int

const (
	modelNone modelSource = iota // no model (a listing)
	modelBody                    // the body's "model" field (anthropic, foundry)
	modelPath                    // the path's {id} segment (bedrock, vertex)
)

// endpoint is one admitted method+path on a route. Everything a route's
// surface does not list is answered 404 before any upstream contact, which is
// what makes the broker a model-inference proxy rather than an open proxy to
// the whole upstream API: Files, Batches and everything else are unreachable.
type endpoint struct {
	method string
	// pattern is the path split on "/": a literal segment, "{_}" for any
	// non-empty segment, "{id}" for the model id, or "{id}:suffix" for a
	// segment that must end in suffix with a non-empty id before it.
	pattern []string
	// metered marks an inference call: usage is accounted and a usage-less
	// or cut response is charged the request-size estimate.
	metered bool
	// model says where the model id is read from for the allowlist.
	model modelSource
	// body marks a messages-shaped body that is buffered and inspected.
	body bool
}

// surface is a route's endpoint list, matched in order.
type surface []endpoint

// surfaceFor returns the positive surface of a named route; u supplies the
// literal segments a route pins (vertex's project and location).
func surfaceFor(name string, u Upstream) surface {
	switch name {
	case provider.Anthropic:
		return anthropicSurface("")
	case provider.Foundry:
		// Foundry serves the Anthropic wire format with the deployment
		// name in the body; the CLI reaches it under /anthropic on the
		// resource host, so both spellings are admitted.
		return append(anthropicSurface(""), anthropicSurface("/anthropic")...)
	case provider.Bedrock:
		return surface{
			{method: http.MethodPost, pattern: segs("/model/{id}/invoke"),
				metered: true, model: modelPath, body: true},
			{method: http.MethodPost, pattern: segs("/model/{id}/invoke-with-response-stream"),
				metered: true, model: modelPath, body: true},
		}
	case provider.Vertex:
		models := "/v1/projects/" + u.Project + "/locations/" + u.Location + "/publishers/anthropic/models/"
		return surface{
			// Vertex counts tokens through a pseudo-model; it is not an
			// inference call and names no model to allowlist.
			{method: http.MethodPost, pattern: segs(models + "count-tokens:rawPredict"), body: true},
			{method: http.MethodPost, pattern: segs(models + "{id}:streamRawPredict"),
				metered: true, model: modelPath, body: true},
			{method: http.MethodPost, pattern: segs(models + "{id}:rawPredict"),
				metered: true, model: modelPath, body: true},
		}
	}
	return nil
}

// anthropicSurface is the first-party Messages surface under prefix.
func anthropicSurface(prefix string) surface {
	return surface{
		{method: http.MethodPost, pattern: segs(prefix + "/v1/messages"),
			metered: true, model: modelBody, body: true},
		{method: http.MethodPost, pattern: segs(prefix + "/v1/messages/count_tokens"),
			model: modelBody, body: true},
		{method: http.MethodGet, pattern: segs(prefix + "/v1/models")},
	}
}

// segs splits a pattern path into its segments.
func segs(pattern string) []string {
	return strings.Split(strings.TrimPrefix(pattern, "/"), "/")
}

// match finds the endpoint for method and the request's escaped path,
// returning the model id captured from the path (unescaped) when the pattern
// has one.
func (s surface) match(method, escapedPath string) (endpoint, string, bool) {
	path := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	for _, ep := range s {
		if ep.method != method {
			continue
		}
		if id, ok := matchSegments(ep.pattern, path); ok {
			return ep, id, true
		}
	}
	return endpoint{}, "", false
}

// matchSegments matches path against pattern segment by segment; an escaped
// "/" inside a segment (a bedrock inference-profile ARN) stays inside it.
func matchSegments(pattern, path []string) (string, bool) {
	if len(pattern) != len(path) {
		return "", false
	}
	var id string
	for i, p := range pattern {
		seg := path[i]
		switch {
		case p == "{_}":
			if seg == "" {
				return "", false
			}
		case strings.HasPrefix(p, "{id}"):
			suffix := strings.TrimPrefix(p, "{id}")
			raw, ok := strings.CutSuffix(seg, suffix)
			if !ok || raw == "" {
				return "", false
			}
			unescaped, err := url.PathUnescape(raw)
			if err != nil || unescaped == "" {
				return "", false
			}
			id = unescaped
		case p != seg:
			return "", false
		}
	}
	return id, true
}
