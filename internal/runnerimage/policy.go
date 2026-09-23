// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"fmt"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
)

// Policy is the operator's registry allowlist: the host/path prefixes a
// declared image must sit under. Entries are validated and normalized at
// construction so a typo fails at flag parse rather than admitting an image.
type Policy struct {
	entries []string
}

// NewPolicy validates every entry with NormalizeEntry and refuses an empty
// list: a policy that allows nothing is a configuration error, not a policy.
func NewPolicy(entries []string) (Policy, error) {
	if len(entries) == 0 {
		return Policy{}, fmt.Errorf("registry allowlist is empty")
	}
	p := Policy{entries: make([]string, 0, len(entries))}
	for _, e := range entries {
		n, err := NormalizeEntry(e)
		if err != nil {
			return Policy{}, err
		}
		p.entries = append(p.entries, n)
	}
	return p, nil
}

// Entries returns the normalized entries in the order given.
func (p Policy) Entries() []string {
	return append([]string(nil), p.entries...)
}

// NormalizeEntry validates one allowlist entry and returns its canonical
// form: "host/segment[/...]/". The entry is lowercased, as ParseDeclared
// lowercases references, and index.docker.io becomes docker.io; at least one path segment is required (a host alone
// would admit every image on a shared registry); no segment may be empty or
// carry a tag or digest; glob characters and whitespace are refused. The
// chart's values schema admits a stricter subset (ASCII host[:port] and
// segment characters, no comma), so a helm render never hands
// source-controller an entry this refuses; TestChartRegistryPatternIsSound
// in cmd/source-controller holds the two together.
func NormalizeEntry(entry string) (string, error) {
	if entry == "" {
		return "", fmt.Errorf("registry allowlist entry is empty")
	}
	if strings.ContainsAny(entry, "*?[]") {
		return "", fmt.Errorf("registry allowlist entry `%s` may not contain glob characters", entry)
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return "", fmt.Errorf("registry allowlist entry `%s` contains whitespace", entry)
	}
	host, path, ok := strings.Cut(entry, "/")
	path = strings.TrimSuffix(path, "/")
	if !ok || host == "" || path == "" {
		return "", fmt.Errorf("registry allowlist entry `%s` must be `host/path/` with at least one path segment", entry)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			return "", fmt.Errorf("registry allowlist entry `%s` has an empty path segment", entry)
		}
		if strings.ContainsAny(seg, ":@") {
			return "", fmt.Errorf("registry allowlist entry `%s` may not name a tag or digest", entry)
		}
	}
	return canonicalHost(host) + "/" + strings.ToLower(path) + "/", nil
}

// Allow reports, as a *Rejection, a reference whose repository is not under
// any entry. Matching is on segment boundaries: "ghcr.io/org/" admits
// "ghcr.io/org/app" and "ghcr.io/org/team/app", never "ghcr.io/org-evil/app".
// The repository must already be canonical and grammatical (ParseDeclared's
// output); anything else is refused rather than matched, so no spelling can
// match an entry its canonical form does not.
func (p Policy) Allow(ref imageref.Ref) error {
	if canonicalRepository(ref.Repository) != ref.Repository || !validRepository(ref.Repository) {
		return reject("image `%s` is not a valid OCI image reference", ref.String())
	}
	candidate := ref.Repository + "/"
	for _, e := range p.entries {
		if strings.HasPrefix(candidate, e) {
			return nil
		}
	}
	return reject("image `%s` is not under an allowlisted registry path (%s)", ref.String(),
		strings.Join(p.entries, ", "))
}

// canonicalHost lowercases a registry host (DNS names are case-insensitive)
// and folds docker's alias for its hub onto the name imageref.Normalize
// emits.
func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if host == "index.docker.io" {
		return "docker.io"
	}
	return host
}
