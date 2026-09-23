// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
)

// digestPattern is the only digest form accepted: sha256 plus 64 lowercase
// hex digits. imageref.Parse accepts anything after "@"; this is where the
// strictness lives.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// The OCI distribution reference grammar, applied to the canonical
// (lowercased) repository: a registry host of DNS labels with an optional
// port, then one or more path components of lowercase alphanumeric runs
// joined by ".", "_", "__" or dashes. No empty, "." or ".." component can
// match, so a segment-boundary allowlist match is a match on what is pulled.
const (
	pathComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	domainLabel   = `(?:[a-z0-9]|[a-z0-9][a-z0-9-]*[a-z0-9])`
	// maxRepositoryLength is the distribution limit on a full name.
	maxRepositoryLength = 255
)

var (
	repositoryPattern = regexp.MustCompile(`^` + domainLabel + `(?:\.` + domainLabel + `)*(?::[0-9]+)?` +
		`(?:/` + pathComponent + `)+$`)
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// ValidateDigest rejects anything but "sha256:" followed by 64 lowercase hex
// characters.
func ValidateDigest(digest string) error {
	if !digestPattern.MatchString(digest) {
		return reject("digest `%s` is not `sha256:` followed by 64 hex characters", digest)
	}
	return nil
}

// ParseDeclared canonicalizes a declared reference and validates it against
// the OCI distribution reference grammar before anything matches on it. The
// repository is lowercased, repeated and trailing slashes are collapsed,
// index.docker.io folds to docker.io, and shorthand expands the way container
// runtimes do (a bare name becomes docker.io/library/name); the tag keeps its
// case. The strict digest form is enforced on a pinned reference. Every
// failure is a *Rejection: the declaration is wrong, not the registry.
func ParseDeclared(declared string) (imageref.Ref, error) {
	if declared == "" {
		return imageref.Ref{}, reject("image reference is empty")
	}
	if strings.ContainsFunc(declared, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return imageref.Ref{}, reject("image reference `%s` contains whitespace", declared)
	}
	ref, err := imageref.Parse(declared)
	if err != nil {
		return imageref.Ref{}, reject("image reference `%s` is invalid: %v", declared, err)
	}
	ref.Repository = canonicalRepository(ref.Repository)
	if !validRepository(ref.Repository) || (ref.Tag != "" && !tagPattern.MatchString(ref.Tag)) {
		return imageref.Ref{}, reject("image reference `%s` is not a valid OCI image reference", declared)
	}
	if ref.Digest != "" && !digestPattern.MatchString(ref.Digest) {
		return imageref.Ref{}, reject("image `%s` pins digest `%s`, which is not `sha256:` followed by 64 hex characters",
			declared, ref.Digest)
	}
	return ref, nil
}

// canonicalRepository lowercases a repository, drops empty path segments
// (doubled or trailing slashes), folds index.docker.io onto docker.io and
// expands hub shorthand with imageref.Normalize. It is idempotent.
func canonicalRepository(repo string) string {
	segs := strings.FieldsFunc(strings.ToLower(repo), func(r rune) bool { return r == '/' })
	if len(segs) == 0 {
		return ""
	}
	if len(segs) > 1 {
		segs[0] = canonicalHost(segs[0])
	}
	return imageref.Normalize(strings.Join(segs, "/"))
}

// validRepository reports whether repo is a canonical repository name under
// the distribution grammar and length limit.
func validRepository(repo string) bool {
	return len(repo) <= maxRepositoryLength && repositoryPattern.MatchString(repo)
}

// Pin builds the digest-pinned reference every check after resolution runs
// against and the pod pulls: "<repository>@<digest>", with no tag, so nothing
// downstream can observe a tag move.
func Pin(ref imageref.Ref, digest string) (string, error) {
	if err := ValidateDigest(digest); err != nil {
		return "", err
	}
	return ref.Repository + "@" + digest, nil
}
