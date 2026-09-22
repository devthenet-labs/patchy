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

// ValidateDigest rejects anything but "sha256:" followed by 64 lowercase hex
// characters.
func ValidateDigest(digest string) error {
	if !digestPattern.MatchString(digest) {
		return reject("digest `%s` is not `sha256:` followed by 64 hex characters", digest)
	}
	return nil
}

// ParseDeclared normalizes a declared reference the way container runtimes
// do (a bare name becomes docker.io/library/name), splits it, and enforces
// the strict digest form on a pinned reference. Every failure is a
// *Rejection: the declaration is wrong, not the registry.
func ParseDeclared(declared string) (imageref.Ref, error) {
	if declared == "" {
		return imageref.Ref{}, reject("image reference is empty")
	}
	if strings.ContainsFunc(declared, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return imageref.Ref{}, reject("image reference `%s` contains whitespace", declared)
	}
	ref, err := imageref.Parse(imageref.Normalize(declared))
	if err != nil {
		return imageref.Ref{}, reject("image reference `%s` is invalid: %v", declared, err)
	}
	if ref.Digest != "" && !digestPattern.MatchString(ref.Digest) {
		return imageref.Ref{}, reject("image `%s` pins digest `%s`, which is not `sha256:` followed by 64 hex characters",
			declared, ref.Digest)
	}
	return ref, nil
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
