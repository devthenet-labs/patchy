// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Plan report bounds, beyond the ones every intent report shares
// (SummaryMaxChars, ItemMaxChars, BodyMaxBytes, ReportMaxBytes).
const (
	// PlanMaxRepositories is the most repositories one plan may name: a
	// Project's own bound.
	PlanMaxRepositories = 8
	// RepositoryURLMaxBytes bounds one repository URL, as the Project and
	// Intent schemas do.
	RepositoryURLMaxBytes = 256
	// PlanMaxNewDependencies is the most new dependencies a plan may list.
	PlanMaxNewDependencies = 16
	// DependencyMaxBytes bounds one new dependency, in bytes: a name and a
	// version need far less. The approval comment repeats every dependency
	// above the plan, so their size counts twice against the comment's cap
	// (see ReportMaxBytes), and a bound in characters could not keep them
	// small: four bytes each is a character too.
	DependencyMaxBytes = 200
	// PlanMaxQuestions is the most questions a plan may put to its approver.
	PlanMaxQuestions = 10
	// PlanMaxBacktickRun bounds a run of backticks anywhere in a plan. The
	// approval comment shows the plan in a code block whose fence is one
	// backtick longer than the plan's longest run, and each dependency in
	// a code span delimited the same way, so a longer run would lengthen
	// them past the room ReportMaxBytes leaves; a markdown fence needs three
	// or four.
	PlanMaxBacktickRun = 16
)

// repositoryURL is the shape of a repository URL a plan may name: https,
// host, owner and name, and nothing else — no credentials, query, fragment
// or trailing path. It is the Project schema's own pattern, so a URL that
// passes here can be compared with the Project's repositories directly.
//
// It is a copy. The source of truth is the kubebuilder Pattern marker on
// ProjectRepository.URL in api/v1alpha1/project_types.go (the Intent and
// IntentRun repository fields carry the same one), which exports no Go
// constant for it, and report stays free of the API types. Change the two
// together: TestRepositoryURLMatchesProjectSchema compares this copy with
// the generated Project CRD.
var repositoryURL = regexp.MustCompile(`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`)

// Plan is the parsed plan report: the intent plan stage's contract. The
// agent writes it read-only; intent-controller posts it for approval, and
// the approved report is what the build stage then follows. Its frontmatter
// is strict:
//
//	---
//	summary: "<one line: what the change does>"   # required, at most 200 characters
//	repositories:                                  # required, 1-8 unique https URLs
//	  - "https://github.com/<owner>/<name>"
//	new_dependencies: []                           # at most 16 one-line items
//	questions: []                                  # at most 10 one-line items
//	confidence: <0.0-1.0>                          # required
//	estimated_max_turns: <integer>                 # required, positive
//	estimated_token_budget: <integer>              # required, positive
//	---
//
// followed by the plan itself in markdown — approach, per-repository steps,
// test plan and risks — of at most BodyMaxBytes, in a document of at most
// ReportMaxBytes. Every list item is one line of at most ItemMaxChars
// characters, and a new dependency of at most DependencyMaxBytes bytes;
// free-text values are trimmed, and none may carry a line break or a
// character that renders invisibly. Every value is plain YAML, written in
// one document with no explicit tag (decodeFrontmatter), so it is the text
// the document shows. The document as a whole, frontmatter and body, holds
// only visible characters, tabs and line breaks (checkVisible), laid out so
// that none of its text sits out of view (checkLayout), with no run of more
// than PlanMaxBacktickRun backticks: the approver reads it verbatim, and the
// digest the approval binds the build to covers every byte of it.
type Plan struct {
	// Summary is the change in one line.
	Summary string `yaml:"summary"`
	// Repositories are the URLs of the repositories the plan changes. The
	// controller rejects a plan naming one outside its Project.
	Repositories []string `yaml:"repositories"`
	// NewDependencies are the dependencies the build needs that its image
	// may lack: the build has no network, so a human must bake each into
	// the repository's image before approving.
	NewDependencies []string `yaml:"new_dependencies"`
	// Questions are the assumptions the approver should confirm.
	Questions []string `yaml:"questions"`
	// Confidence is the probability that a build following the plan exactly
	// delivers the request with its tests passing. Pointer so absence is
	// detectable.
	Confidence *float64 `yaml:"confidence"`
	// EstimatedMaxTurns/EstimatedTokenBudget are the plan's ESTIMATE of what
	// its build will spend. Like the investigation's, they never bind the
	// run they describe; the controller grants the build's budget.
	EstimatedMaxTurns    int `yaml:"estimated_max_turns"`
	EstimatedTokenBudget int `yaml:"estimated_token_budget"`
	// Body is the markdown plan following the frontmatter.
	Body string `yaml:"-"`
}

// planFreeText are the plan keys whose values are prose.
var planFreeText = map[string]bool{"summary": true}

// ParsePlan parses and validates a plan report as the plan stage wrote it:
// the document bound, that every byte of it is visible and in view, its
// backtick runs, the frontmatter, and the body bound. A document holding a
// character that renders invisibly or reorders text is refused with the
// first one's code point, line and column; one laying text out of view,
// with where that starts.
func ParsePlan(data []byte) (*Plan, error) {
	if err := checkDocument("plan", data); err != nil {
		return nil, err
	}
	if err := checkBacktickRuns(data); err != nil {
		return nil, err
	}
	p, err := parsePlanFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if err := checkBody("plan", p.Body); err != nil {
		return nil, err
	}
	return p, nil
}

// ParsePlanInput parses the build stage's input: an approved plan, which on
// a revise round patchy follows with its rendering of that round's feedback
// and compare patch. It validates the plan's frontmatter exactly as
// ParsePlan does, but not the body and document bounds, which the appended
// round was never meant to fit; only the frontmatter block is bounded.
//
// Every byte of the input must be visible all the same, the appended round
// included (checkVisible): the build agent reads all of it, and nothing it
// acts on may be hidden from the humans who approved the plan and wrote the
// feedback. The controller therefore renders a round with no character
// checkVisible refuses — a compare patch of source that holds one shows it
// as a visible escape.
//
// The layout rule (checkLayout) and the backtick bound are not applied
// here. The plan the input begins with was held to both by ParsePlan when
// it was written, and the approval's digest binds those bytes; the round
// after it is reviewers' feedback, which GitHub showed them as they wrote
// it, and a compare patch of source, whose indentation and aligned columns
// routinely pass either bound and which no approval shows in a code block.
func ParsePlanInput(data []byte) (*Plan, error) {
	if err := checkVisible("plan", data); err != nil {
		return nil, err
	}
	return parsePlanFrontmatter(data)
}

// parsePlanFrontmatter splits, decodes and validates a plan's frontmatter;
// Body is the rest of the document, unbounded.
func parsePlanFrontmatter(data []byte) (*Plan, error) {
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if len(block) > ReportMaxBytes {
		return nil, fmt.Errorf("report: plan: frontmatter is %d bytes, over the %d-byte bound",
			len(block), ReportMaxBytes)
	}
	var p Plan
	if err := decodeRepairing(block, &p, planFreeText, func() { p = Plan{} }); err != nil {
		return nil, err
	}
	p.Body = body
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("report: plan: %w", err)
	}
	return &p, nil
}

// validate checks the frontmatter and trims its free text in place.
func (p *Plan) validate() error {
	var errs []error
	var err error
	if p.Summary, err = oneLine("summary", p.Summary, SummaryMaxChars); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, validRepositories(p.Repositories))
	errs = append(errs, lines("new_dependencies", p.NewDependencies, PlanMaxNewDependencies, ItemMaxChars))
	for i, d := range p.NewDependencies {
		if len(d) > DependencyMaxBytes {
			errs = append(errs, fmt.Errorf("new_dependencies[%d] is %d bytes, over %d", i, len(d), DependencyMaxBytes))
		}
	}
	errs = append(errs, lines("questions", p.Questions, PlanMaxQuestions, ItemMaxChars))
	switch {
	case p.Confidence == nil:
		errs = append(errs, errors.New("confidence is required"))
	case math.IsNaN(*p.Confidence) || *p.Confidence < 0 || *p.Confidence > 1:
		// NaN is named because it fails both comparisons: YAML's .nan would
		// pass as in range, and JSON cannot encode it, so the plan event
		// carrying it would never leave the pod.
		errs = append(errs, fmt.Errorf("confidence %v is outside [0, 1]", *p.Confidence))
	}
	if p.EstimatedMaxTurns < 1 {
		errs = append(errs, errors.New("estimated_max_turns must be a positive integer"))
	}
	if p.EstimatedTokenBudget < 1 {
		errs = append(errs, errors.New("estimated_token_budget must be a positive integer"))
	}
	return errors.Join(errs...)
}

// validRepositories checks the plan names at least one repository, at most
// PlanMaxRepositories, each a bounded https repository URL, none twice —
// compared as the Project compares its own (case-insensitively, ignoring a
// .git suffix).
func validRepositories(urls []string) error {
	switch {
	case len(urls) == 0:
		return errors.New("repositories is required")
	case len(urls) > PlanMaxRepositories:
		return fmt.Errorf("repositories has %d items, over %d", len(urls), PlanMaxRepositories)
	}
	var errs []error
	seen := make(map[string]bool, len(urls))
	for i, u := range urls {
		switch {
		case len(u) > RepositoryURLMaxBytes:
			errs = append(errs, fmt.Errorf("repositories[%d] is %d bytes, over %d", i, len(u), RepositoryURLMaxBytes))
			continue
		case !utf8.ValidString(u) || strings.ContainsFunc(u, invisible) || !repositoryURL.MatchString(u):
			errs = append(errs, fmt.Errorf("repositories[%d] %q is not an https://<host>/<owner>/<name> URL", i, u))
			continue
		}
		// Lowercased before the suffix is cut, so ".GIT" is cut too.
		key := strings.TrimSuffix(strings.ToLower(u), ".git")
		if seen[key] {
			errs = append(errs, fmt.Errorf("repositories[%d] %q is listed twice", i, u))
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}

// checkBacktickRuns refuses a plan holding a run of more than
// PlanMaxBacktickRun backticks, with where the first one starts: line and
// column as checkVisible counts them.
func checkBacktickRuns(data []byte) error {
	for i, line := range strings.Split(string(data), "\n") {
		column, run, start := 0, 0, 0
		for _, r := range line + "\n" {
			column++
			if r == '`' {
				if run == 0 {
					start = column
				}
				run++
				continue
			}
			if run > PlanMaxBacktickRun {
				return fmt.Errorf("report: plan: line %d, column %d: a run of %d backticks, over %d: the "+
					"approval comment fences the plan one backtick longer than its longest run", i+1, start,
					run, PlanMaxBacktickRun)
			}
			run = 0
		}
	}
	return nil
}
