// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Status is one check's outcome.
type Status string

// The three outcomes. Only Fail makes the check as a whole fail; Skip means
// the check could not or need not run, and its reason says which.
const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
)

// The checks, in the order a report lists them: the static ones source-
// controller applies, then the sandbox run: the runner image it takes
// agent-runner and claude from, once, then its per-platform checks.
const (
	CheckReference   = "reference"
	CheckAllowlist   = "allowlist"
	CheckResolve     = "resolve"
	CheckPlatform    = "platform"
	CheckSize        = "size"
	CheckVolume      = "volume"
	CheckEnv         = "env"
	CheckPath        = "path"
	CheckSignature   = "signature"
	CheckRunnerImage = "runner-image"
	CheckRunner      = "runner"
	CheckPreflight   = "preflight"
	CheckBash        = "bash"
	CheckGit         = "git"
)

// Check is one line of a report.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Platform is the platform a sandbox check ran as; empty for the static
	// checks, which judge every platform at once.
	Platform string `json:"platform,omitempty"`
	Reason   string `json:"reason"`
}

// Platform is one manifest a linux/amd64 or linux/arm64 node could run.
type Platform struct {
	Platform        string `json:"platform"`
	Digest          string `json:"digest"`
	CompressedBytes int64  `json:"compressedBytes"`
}

// Report is the outcome of checking one reference.
type Report struct {
	// Reference is the reference as given.
	Reference string `json:"reference"`
	// Canonical is the reference as source-controller reads it
	// (runnerimage.ParseDeclared); empty when it does not parse.
	Canonical string `json:"canonical,omitempty"`
	// Image is the digest-pinned reference source-controller would record;
	// empty when the reference did not resolve.
	Image string `json:"image,omitempty"`
	// Index is true when Image names an image index.
	Index bool `json:"index,omitempty"`
	// Platforms are the runnable manifests, with their compressed sizes.
	Platforms []Platform `json:"platforms,omitempty"`
	// SearchPath is the sanitized image PATH the pod would get after
	// /patchy/bin; empty when it could not be derived.
	SearchPath []string `json:"searchPath,omitempty"`
	// RunnerImage is the trusted image the sandbox run took agent-runner and
	// claude from, with the digest it ran at; nil without a sandbox run or
	// when none could be chosen.
	RunnerImage *RunnerImage `json:"runnerImage,omitempty"`
	// Checks lists every check, in order.
	Checks []Check `json:"checks"`
}

// Failed counts the checks that failed.
func (r Report) Failed() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == Fail {
			n++
		}
	}
	return n
}

// add appends one check. Every reason passes through here, and many carry
// text the image under test controls (what its commands printed, paths
// from its config), so each is made printable first.
func (r *Report) add(name string, status Status, reason string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Reason: printable(reason)})
}

// printable writes every control character in s but newline and tab, and
// every byte that is not UTF-8, as a visible \xNN escape. The report is
// read on a terminal, and piped from -o json into jq -r: an escape sequence
// an image prints must show as what it is, never move the cursor over the
// lines above it or reach the clipboard. encoding/json alone escapes only
// C0, not the C1 controls (U+0080 to U+009F) a terminal also obeys.
func printable(s string) string {
	escape := func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }
	if utf8.ValidString(s) && !strings.ContainsFunc(s, escape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case escape(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// skip appends a Skip line for each named check.
func (r *Report) skip(reason string, names ...string) {
	for _, n := range names {
		r.add(n, Skip, reason)
	}
}
