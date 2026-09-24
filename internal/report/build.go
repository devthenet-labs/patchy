// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"strings"
)

// Build report bounds, beyond the ones every intent report shares
// (SummaryMaxChars, ItemMaxChars, BodyMaxBytes, ReportMaxBytes).
const (
	// BuildMaxNotes is the most notes a build report may carry.
	BuildMaxNotes = 10
	// ReasonMaxChars bounds the reason a build could not be done.
	ReasonMaxChars = 1000
)

// Build is the parsed build report: the intent build stage's contract, for
// the first build of an approved plan and for every revise round after it.
// Its frontmatter is strict:
//
//	---
//	success: true | false          # required: the plan is built and committed
//	summary: "<one line>"          # required, at most 200 characters
//	tests:                         # required
//	  ran: true | false            # required
//	  passed: true | false         # required; false when ran is false
//	  command: "<go test ./...>"   # required when ran is true
//	notes: []                      # at most 10 one-line items for reviewers
//	reason: "<one line>"           # required exactly when success is false
//	---
//
// followed by the change's description in markdown — what changed and why,
// how it was verified — of at most BodyMaxBytes, in a document of at most
// ReportMaxBytes. It becomes the pull request's description, so it holds
// the plan's rule: only visible characters, tabs and line breaks, in the
// free text and everywhere else (checkVisible), laid out in view
// (checkLayout), in a frontmatter of plain YAML (decodeFrontmatter) — a
// reviewer reads what the agent wrote, and nothing in it may render
// invisibly, reorder the text around it or sit out of view.
//
// A report claiming success while its own tests failed contradicts itself
// and is refused. Success is still only the agent's claim: the runner
// downgrades it unless commit.sh leaves real commits, exactly as it does a
// remediation's.
type Build struct {
	// Success reports whether the agent built the approved plan. Pointer so
	// absence is detectable.
	Success *bool `yaml:"success"`
	// Summary is the change in one line.
	Summary string `yaml:"summary"`
	// Tests is what the agent ran to verify the change in this image.
	Tests *BuildTests `yaml:"tests"`
	// Notes are what a reviewer should look at closely.
	Notes []string `yaml:"notes"`
	// Reason says why the plan could not be built; set exactly when Success
	// is false.
	Reason string `yaml:"reason"`
	// Body is the markdown description following the frontmatter.
	Body string `yaml:"-"`
}

// BuildTests is what a build ran to verify its change.
type BuildTests struct {
	// Ran reports whether any tests ran. Pointer so absence is detectable.
	Ran *bool `yaml:"ran"`
	// Passed reports whether every test that ran passed. Pointer so absence
	// is detectable.
	Passed *bool `yaml:"passed"`
	// Command is the command that ran them.
	Command string `yaml:"command"`
}

// buildFreeText are the build keys whose values are prose.
var buildFreeText = map[string]bool{"summary": true, "command": true, "reason": true}

// ParseBuild parses and validates a build report: the document bound, that
// every byte of it is visible and in view — refused, as a plan is, with
// where the first offending character or run sits — the body bound, and
// the frontmatter.
func ParseBuild(data []byte) (*Build, error) {
	if err := checkDocument("build", data); err != nil {
		return nil, err
	}
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if err := checkBody("build", body); err != nil {
		return nil, err
	}
	var b Build
	if err := decodeRepairing(block, &b, buildFreeText, func() { b = Build{} }); err != nil {
		return nil, err
	}
	b.Body = body
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("report: build: %w", err)
	}
	return &b, nil
}

// validate checks the frontmatter and trims its free text in place.
func (b *Build) validate() error {
	var errs []error
	var err error
	if b.Success == nil {
		errs = append(errs, errors.New("success is required"))
	}
	if b.Summary, err = oneLine("summary", b.Summary, SummaryMaxChars); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, b.validateTests())
	errs = append(errs, lines("notes", b.Notes, BuildMaxNotes, ItemMaxChars))
	if b.Success != nil {
		switch {
		case !*b.Success:
			if b.Reason, err = oneLine("reason", b.Reason, ReasonMaxChars); err != nil {
				errs = append(errs, fmt.Errorf("%w when success is false", err))
			}
		case b.Reason != "":
			errs = append(errs, errors.New("reason is set but success is true"))
		}
	}
	return errors.Join(errs...)
}

// validateTests checks the tests block, including against the success claim.
func (b *Build) validateTests() error {
	t := b.Tests
	if t == nil {
		return errors.New("tests is required")
	}
	var errs []error
	if t.Ran == nil {
		errs = append(errs, errors.New("tests.ran is required"))
	}
	if t.Passed == nil {
		errs = append(errs, errors.New("tests.passed is required"))
	}
	if t.Ran == nil || t.Passed == nil {
		return errors.Join(errs...)
	}
	ran, passed := *t.Ran, *t.Passed
	var err error
	t.Command = strings.TrimSpace(t.Command)
	switch {
	case ran:
		if t.Command, err = oneLine("tests.command", t.Command, ItemMaxChars); err != nil {
			errs = append(errs, fmt.Errorf("%w when tests ran", err))
		}
	case passed:
		errs = append(errs, errors.New("tests.passed is true but tests.ran is false"))
	case t.Command != "":
		// A command that never ran is still named on one bounded line.
		if t.Command, err = oneLine("tests.command", t.Command, ItemMaxChars); err != nil {
			errs = append(errs, err)
		}
	}
	if b.Success != nil && *b.Success && ran && !passed {
		errs = append(errs, errors.New("success is true but the tests that ran did not pass"))
	}
	return errors.Join(errs...)
}
