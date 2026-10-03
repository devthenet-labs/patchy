// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxSlug bounds an image name: a DNS label, the most any of the names
// built from it (registry repository leaves, Kubernetes labels) assumes.
const MaxSlug = 63

// slugPattern is the image name grammar: lowercase alphanumeric runs joined
// by single hyphens. That is both an ECR repository segment, which refuses
// doubled separators, and a match for the preview-controller's namePattern,
// which the image leaf under the preview prefix must satisfy.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ValidateSlug checks an image name given as is (--image-name).
func ValidateSlug(s string) error {
	if len(s) > MaxSlug {
		return fmt.Errorf("image name %q is %d characters; the limit is %d", s, len(s), MaxSlug)
	}
	if !slugPattern.MatchString(s) {
		return fmt.Errorf("image name %q must be lowercase letters and digits, joined by single hyphens", s)
	}
	return nil
}

// Sanitize derives an image name from a repository name, which GitHub lets
// carry upper case, dots and underscores the image leaf cannot: lowercased,
// every run of other characters a single hyphen, the ends trimmed and the
// result cut to MaxSlug. Hello.Web becomes hello-web. A name with no
// letter or digit in it has no image name, and the error says to pass one.
func Sanitize(name string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > MaxSlug {
		s = strings.TrimRight(s[:MaxSlug], "-")
	}
	if s == "" {
		return "", fmt.Errorf("%q has no letter or digit to make an image name of; pass --image-name", name)
	}
	return s, nil
}
