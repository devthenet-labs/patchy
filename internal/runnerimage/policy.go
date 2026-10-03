// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
)

// Policy is the operator's registry allowlist: the host/path prefixes a
// declared image must sit under, less any denied prefix it must never sit
// under. Entries are validated and normalized at construction so a typo
// fails at flag parse rather than admitting an image.
type Policy struct {
	entries []string
	// denied are registry paths no declared image may come from, whatever
	// entries admit (Deny), normalized as entries are.
	denied []string
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

// Deny returns a copy of p that also refuses every image under any of
// prefixes, even one an entry admits: registry paths whose images must
// never become an agent sandbox. The chart denies the preview image prefix,
// whose images are built from unreviewed pull request heads; it also refuses
// to render an allowlist that overlaps that prefix, and this holds
// source-controller to the rule on its own. Each prefix is validated and
// normalized as an entry is; an empty list leaves p's denials as they are.
func (p Policy) Deny(prefixes []string) (Policy, error) {
	out := Policy{entries: p.Entries(), denied: p.Denied()}
	for _, d := range prefixes {
		n, err := normalizePrefix("denied registry path", d)
		if err != nil {
			return Policy{}, err
		}
		out.denied = append(out.denied, n)
	}
	return out, nil
}

// Entries returns the normalized entries in the order given.
func (p Policy) Entries() []string {
	return append([]string(nil), p.entries...)
}

// Denied returns the normalized denied prefixes in the order given.
func (p Policy) Denied() []string {
	return append([]string(nil), p.denied...)
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
	return normalizePrefix("registry allowlist entry", entry)
}

// normalizePrefix is NormalizeEntry for any registry path prefix; what names
// the prefix in an error.
func normalizePrefix(what, entry string) (string, error) {
	if entry == "" {
		return "", fmt.Errorf("%s is empty", what)
	}
	if strings.ContainsAny(entry, "*?[]") {
		return "", fmt.Errorf("%s `%s` may not contain glob characters", what, entry)
	}
	if strings.ContainsAny(entry, " \t\r\n") {
		return "", fmt.Errorf("%s `%s` contains whitespace", what, entry)
	}
	host, path, ok := strings.Cut(entry, "/")
	path = strings.TrimSuffix(path, "/")
	if !ok || host == "" || path == "" {
		return "", fmt.Errorf("%s `%s` must be `host/path/` with at least one path segment", what, entry)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			return "", fmt.Errorf("%s `%s` has an empty path segment", what, entry)
		}
		if strings.ContainsAny(seg, ":@") {
			return "", fmt.Errorf("%s `%s` may not name a tag or digest", what, entry)
		}
	}
	return canonicalHost(host) + "/" + strings.ToLower(path) + "/", nil
}

// Allow reports, as a *Rejection, a reference whose repository is under a
// denied prefix or not under any entry. Matching is on segment boundaries:
// "ghcr.io/org/" admits "ghcr.io/org/app" and "ghcr.io/org/team/app", never
// "ghcr.io/org-evil/app". A denied prefix is matched on the registry the
// image is pulled from (registryKey), so another spelling of the same
// repository is denied too. The repository must already be canonical and
// grammatical (ParseDeclared's output); anything else is refused rather than
// matched, so no spelling can match an entry its canonical form does not.
func (p Policy) Allow(ref imageref.Ref) error {
	if canonicalRepository(ref.Repository) != ref.Repository || !validRepository(ref.Repository) {
		return reject("image `%s` is not a valid OCI image reference", ref.String())
	}
	candidate := ref.Repository + "/"
	key := registryKey(candidate)
	for _, d := range p.denied {
		if strings.HasPrefix(key, registryKey(d)) {
			return &Rejection{Reason: "DeniedRegistry", Message: fmt.Sprintf(
				"image `%s` is under `%s`, a registry path agent images may never come from "+
					"(the operator reserves it for other images, such as previews built from pull requests)",
				ref.String(), d)}
		}
	}
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

// ecrEndpoint matches ECR's other names for one account's regional registry:
// the dual-stack (<account>.dkr-ecr.<region>.on.aws) and FIPS
// (<account>.dkr.ecr-fips.<region>.amazonaws.com, ...) endpoints, which serve
// the same repositories as <account>.dkr.ecr.<region>.amazonaws.com.
var ecrEndpoint = regexp.MustCompile(`^([0-9]{12})\.dkr[.-]ecr(-fips)?\.([a-z0-9-]+)\.(amazonaws\.com|on\.aws)$`)

// registryKey is a "host/path/" prefix as the registry it is pulled from
// sees it: lowercased, an explicit :443 dropped (the same HTTPS endpoint),
// and an ECR endpoint alias folded onto the plain ECR host. Two prefixes
// name overlapping repositories exactly when one key is a prefix of the
// other. The chart's patchy.registryPathKey is the same fold, for its
// render-time disjointness check.
func registryKey(prefix string) string {
	host, rest, _ := strings.Cut(strings.ToLower(prefix), "/")
	host = strings.TrimSuffix(host, ":443")
	if m := ecrEndpoint.FindStringSubmatch(host); m != nil {
		host = m[1] + ".dkr.ecr." + m[3] + ".amazonaws.com"
	}
	return host + "/" + rest
}
