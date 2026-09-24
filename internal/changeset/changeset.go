// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package changeset

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// DefaultMaxEntries is the default cap on the upserts plus deletes of a
// changeset held to the repository-image rules (remediation-controller's
// --changeset-max-entries). Every entry costs the forge write API at least
// one call, so a changeset of many tiny files would spend the installation
// token's budget before a human saw anything.
const DefaultMaxEntries = 500

// maxPathBytes bounds one path, PATH_MAX on Linux: far past any real
// repository's, well short of what the forge's API would choke on.
const maxPathBytes = 4096

// fileModes are the git modes a changeset may carry (envelope.FileChange
// Mode): regular, executable and symlink — all the pod's git diff emits,
// and all a blob tree entry can be at the forge.
var fileModes = []string{"100644", "100755", "120000"}

// ciDirs are the directories GitHub runs as CI with the repository's
// secrets on any branch pushed to it, before a human has reviewed anything.
var ciDirs = []string{".github/workflows", ".github/actions"}

// Rules is what one changeset is validated against. A Finding
// remediation's rules carry no Deny; IntentRules are an intent's.
type Rules struct {
	// Base is the Repository's pinned commit (status.resolvedSHA): the one
	// base a legitimate run reports, since the pod was handed it. Empty
	// when it cannot be read, which no changeset matches.
	Base string
	// MaxEntries caps upserts plus deletes.
	MaxEntries int
	// RepositoryImage applies the repository-image rules: this run, or the
	// Investigation whose report and parameters it acts on, ran a
	// repository-declared image.
	RepositoryImage bool
	// Deny lists repository-root directories no path may be in or replace,
	// matched without regard to case, whichever image the run used.
	Deny []string
}

// intentDeny is what an intent run may never change: CI definitions
// (.github), the agent-image declaration and recipe (.patchy, including the
// .patchy/Dockerfile main's CI builds into the allowlisted registry prefix),
// and the devcontainer declaration. source-controller reads the image
// declaration from whatever tree it pins, and a revise round pins the pull
// request's head, so without this an agent could choose its own next
// sandbox.
var intentDeny = []string{".github", ".patchy", ".devcontainer"}

// IntentRules are the rules an intent build or revise changeset is held to:
// based on base (the Repository's pinned commit — for a revise round, the
// pull request head it pinned), the repository-image rules whichever image
// ran (intent runs require one, and its process wrote the changeset), capped
// at maxEntries (<= 0 means DefaultMaxEntries), and nothing under .github,
// .patchy or .devcontainer.
func IntentRules(base string, maxEntries int) Rules {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return Rules{
		Base:            base,
		MaxEntries:      maxEntries,
		RepositoryImage: true,
		Deny:            slices.Clone(intentDeny),
	}
}

// Validate checks a changeset before a controller makes any forge call with
// it, returning an error naming the limit or the path it breaks.
// remediation-controller holds a Finding's changesets to Rules without a
// Deny list, which leaves their verdicts exactly what they were before Deny
// existed; intent-controller holds an intent's to IntentRules.
//
// Every changeset is held to what a legitimate run always produces, since
// the pod builds the changeset from a git diff of the tree it was handed:
// the Repository's pinned commit as its base (the push builds its tree on,
// and parents, the base the pod reports, so another base would push the
// branch onto a tree nobody reviewed — a fork's head carrying its own
// workflows, say — and a changeset diffed against one tree but pushed onto
// another silently reverts whatever differs); paths git itself could have
// produced (relative, no empty, "." or ".." component, nothing inside .git,
// no NUL, valid UTF-8, bounded length); and upserts the forge can take (a
// regular, executable or symlink mode, base64 content — otherwise refused
// by the forge on every retry, after a write token is minted and, for a
// mode, after every blob is created, while the run holds its slot). None of
// these changes the outcome of a real changeset, so with repository images
// off nothing observable changes.
//
// A changeset held to the repository-image rules — that image controls the
// process the changeset came out of, or the analysis it followed — is also
// refused what a legitimate
// diff can contain but patchy will not push unreviewed from such a run:
// more than MaxEntries entries (upserts plus deletes), a control character
// in a path, and any change to CI definitions (.github/workflows,
// .github/actions, or .github itself replaced), since a patchy branch in
// the same repository triggers CI with its secrets before a human has
// looked. Default-image runs keep all three as they were: a vendored
// dependency bump rewrites hundreds of files, git allows a tab in a file
// name, and leaving workflows alone also spares them the forge's refusal
// when the App lacks the workflows permission.
//
// A path in or replacing a Deny directory is refused on any run.
func Validate(cs *envelope.Changeset, rules Rules) error {
	switch {
	case rules.Base == "":
		return fmt.Errorf("the repository's pinned commit is unknown, so the changeset's base cannot be checked")
	case cs.BaseSHA != rules.Base:
		return fmt.Errorf("changeset base %.64q is not the repository's pinned commit %q", cs.BaseSHA, rules.Base)
	}
	if n := len(cs.Upserts) + len(cs.Deletes); rules.RepositoryImage && n > rules.MaxEntries {
		return fmt.Errorf("changeset has %d entries (upserts plus deletes), over the %d-entry limit", n, rules.MaxEntries)
	}
	check := func(p string) error {
		if err := checkPath(p); err != nil {
			return err
		}
		if dir := deniedDir(p, rules.Deny); dir != "" {
			return fmt.Errorf("changeset path %q is under %s, which this run may not change", p, dir)
		}
		if !rules.RepositoryImage {
			return nil
		}
		if strings.ContainsFunc(p, unicode.IsControl) {
			return fmt.Errorf("changeset path %q contains a control character, which a run on a "+
				"repository-declared image may not use", p)
		}
		if ciPath(p) {
			return fmt.Errorf("changeset path %q is a CI definition, which a run on a repository-declared "+
				"image may not change", p)
		}
		return nil
	}
	for _, up := range cs.Upserts {
		if err := check(up.Path); err != nil {
			return err
		}
		if err := checkUpsert(up); err != nil {
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

// checkPath refuses a path git could not have produced from a checked-out
// commit, or that would reach outside the work tree.
func checkPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("changeset has an empty path")
	case len(p) > maxPathBytes:
		return fmt.Errorf("changeset path %.64q… is %d bytes, over the %d-byte limit",
			p, len(p), maxPathBytes)
	case !utf8.ValidString(p):
		return fmt.Errorf("changeset path %q is not valid UTF-8", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("changeset path %q contains NUL", p)
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

// checkUpsert refuses an upsert the forge would refuse on every attempt: a
// mode no blob can have, or content that does not decode.
func checkUpsert(up envelope.FileChange) error {
	if !slices.Contains(fileModes, up.Mode) {
		return fmt.Errorf("changeset path %q has mode %.16q, not one of %s",
			up.Path, up.Mode, strings.Join(fileModes, ", "))
	}
	if _, err := base64.StdEncoding.DecodeString(up.ContentB64); err != nil {
		return fmt.Errorf("changeset path %q content is not base64: %w", up.Path, err)
	}
	return nil
}

// deniedDir returns the deny directory p is in or would replace, or "".
// Matched without regard to case, like ciPath.
func deniedDir(p string, deny []string) string {
	lower := strings.ToLower(p)
	for _, dir := range deny {
		d := strings.ToLower(dir)
		if lower == d || strings.HasPrefix(lower, d+"/") {
			return dir
		}
	}
	return ""
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
