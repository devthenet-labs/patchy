// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// DefaultChangesetMaxEntries is the default cap on a changeset's upserts
// plus deletes (--changeset-max-entries). Every entry costs the forge write
// API at least one call, so a changeset of many tiny files would spend the
// installation token's budget before a human saw anything.
const DefaultChangesetMaxEntries = 500

// maxChangesetPathBytes bounds one path, PATH_MAX on Linux: far past any
// real repository's, well short of what the forge's API would choke on.
const maxChangesetPathBytes = 4096

// ciDirs are the directories GitHub runs as CI with the repository's
// secrets on any branch pushed to it, before a human has reviewed anything.
var ciDirs = []string{".github/workflows", ".github/actions"}

// validateChangeset checks a remediation changeset before the controller
// makes any forge call with it, returning an error naming the limit or the
// path it breaks. Every changeset is held to the entry cap and to paths git
// itself could have produced (relative, no empty, "." or ".." component,
// nothing inside .git, valid UTF-8 without control characters, bounded
// length) — none of which a legitimate run ever trips, since the pod builds
// the changeset from a git diff. A run on a repository-declared image is
// also refused any change to CI definitions (.github/workflows, .github/
// actions, or .github itself replaced): a patchy branch in the same
// repository triggers CI with its secrets before a human has looked, and
// that image controls the process the changeset came out of. Default-image
// runs are left as they were, which also spares them the forge's refusal
// when the App lacks the workflows permission.
func validateChangeset(cs *envelope.Changeset, maxEntries int, repositoryImage bool) error {
	if n := len(cs.Upserts) + len(cs.Deletes); n > maxEntries {
		return fmt.Errorf("changeset has %d entries (upserts plus deletes), over the %d-entry limit", n, maxEntries)
	}
	check := func(p string) error {
		if err := checkChangesetPath(p); err != nil {
			return err
		}
		if repositoryImage && ciPath(p) {
			return fmt.Errorf("changeset path %q is a CI definition, which a run on a repository-declared "+
				"image may not change", p)
		}
		return nil
	}
	for _, up := range cs.Upserts {
		if err := check(up.Path); err != nil {
			return err
		}
	}
	for _, p := range cs.Deletes {
		if err := check(p); err != nil {
			return err
		}
	}
	return nil
}

// checkChangesetPath refuses a path git could not have produced from a
// commit, or that would reach outside the work tree.
func checkChangesetPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("changeset has an empty path")
	case len(p) > maxChangesetPathBytes:
		return fmt.Errorf("changeset path %.64q… is %d bytes, over the %d-byte limit",
			p, len(p), maxChangesetPathBytes)
	case !utf8.ValidString(p):
		return fmt.Errorf("changeset path %q is not valid UTF-8", p)
	case strings.ContainsFunc(p, unicode.IsControl):
		return fmt.Errorf("changeset path %q contains a control character", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("changeset path %q is absolute", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("changeset path %q has an empty component", p)
		case seg == "." || seg == "..":
			return fmt.Errorf("changeset path %q has a %q component", p, seg)
		case strings.EqualFold(seg, ".git"):
			return fmt.Errorf("changeset path %q is inside .git", p)
		}
	}
	return nil
}

// ciPath reports whether p is a CI definition or would replace the
// directory holding them. Matched without regard to case: GitHub reads
// the exact lower-case directories, but nothing legitimate lives at a
// case-folded twin of one either.
func ciPath(p string) bool {
	lower := strings.ToLower(p)
	if lower == ".github" {
		return true
	}
	for _, dir := range ciDirs {
		if lower == dir || strings.HasPrefix(lower, dir+"/") {
			return true
		}
	}
	return false
}
