// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import "github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"

// The checks, in the order a report lists them.
const (
	CheckReady        = "ready"
	CheckIntentNames  = "intent-names"
	CheckForge        = "forge"
	CheckLabels       = "labels"
	CheckAgentImage   = "agent-image"
	CheckPreviews     = "previews"
	CheckPreviewImage = "preview-image"
	CheckPreviewDNS   = "preview-dns"
	CheckPreviewTLS   = "preview-tls"
	// The preview sign-in checks, reported only while the chart's
	// previewAuth is on.
	CheckPreviewAuth     = "preview-auth"
	CheckPreviewAuthDex  = "preview-auth-dex"
	CheckPreviewAuthHost = "preview-auth-host"
)

// IntentRepository is the Repository of a check about the Project's intent
// repository. It is 17 characters, one more than a repository key may have,
// so it never collides with one.
const IntentRepository = "intent-repository"

// Check is one line of a report.
type Check struct {
	Name   string             `json:"name"`
	Status checkreport.Status `json:"status"`
	// Repository is what the check is about: a key of spec.repositories
	// (which also names that repository's preview component), or
	// IntentRepository; empty for a check of the Project as a whole.
	Repository string `json:"repository,omitempty"`
	Reason     string `json:"reason"`
}

// Report is the outcome of checking one Project.
type Report struct {
	Project   string  `json:"project"`
	Namespace string  `json:"namespace"`
	Checks    []Check `json:"checks"`
}

// Lines renders the report's checks as table lines: the check, then the
// repository it is about, "-" for the Project as a whole, so the reasons
// line up.
func (r Report) Lines() []checkreport.Line {
	lines := make([]checkreport.Line, 0, len(r.Checks))
	for _, c := range r.Checks {
		repository := c.Repository
		if repository == "" {
			repository = "-"
		}
		lines = append(lines, checkreport.Line{Status: c.Status, Cells: []string{c.Name, repository}, Reason: c.Reason})
	}
	return lines
}

// Failed counts the checks that failed.
func (r Report) Failed() int {
	return checkreport.Failed(r.Lines())
}

// add appends one check. Reasons carry text from outside the CLI (condition
// messages, GitHub's and a registry's errors), so each is made printable.
func (r *Report) add(name, repository string, status checkreport.Status, reason string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Repository: repository,
		Reason: checkreport.Printable(reason)})
}
