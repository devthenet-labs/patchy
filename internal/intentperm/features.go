// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentperm

import (
	"fmt"
	"slices"
)

// The permissions an App holds beside the intent ones: the findings
// pipeline's code scanning access, and the metadata read GitHub requires of
// every App.
const (
	SecurityEvents = "security_events"
	Metadata       = "metadata"
)

// Webhook events, as GitHub spells them in an App manifest's default_events.
const (
	EventCodeScanningAlert = "code_scanning_alert"
	EventIssueComment      = "issue_comment"
	EventIssues            = "issues"
	EventPullRequest       = "pull_request"
)

// Feature is one part of patchy a GitHub App can be registered for.
type Feature string

const (
	// FeatureSecurity is the findings pipeline: code scanning alerts in
	// through the integration-controller's webhook, tracking issues, and
	// remediation pull requests.
	FeatureSecurity Feature = "security"
	// FeatureIntents is intent-driven development. intent-controller polls
	// GitHub, so intents add permissions but no webhook event.
	FeatureIntents Feature = "intents"
	// FeatureChecks is a Project's check-fix rounds (spec.checks.fix): the
	// reads that find a failed check and what it reported. It extends
	// FeatureIntents.
	FeatureChecks Feature = "checks"
)

// Features is every feature, in the order the CLI documents them.
func Features() []Feature {
	return []Feature{FeatureSecurity, FeatureIntents, FeatureChecks}
}

// Needs is what a GitHub App must hold to serve a feature, or a set of
// them: repository permissions, one grant per permission sorted by name,
// and the webhook events it delivers to patchy, sorted.
type Needs struct {
	Grants []Grant
	Events []string
}

// Needs is f's row of the table; false for a feature it does not know. The
// intent rows are the per-repository grants For hands intent-controller,
// merged, so an App registered for intents holds every grant a Project's
// Ready can be refused for.
func (f Feature) Needs() (Needs, bool) {
	switch f {
	case FeatureSecurity:
		// docs/getting-started/github-app.md: code scanning alerts (read an
		// alert, dismiss a false positive), issues (the tracking issue),
		// contents (the archive, the remediation branch), pull requests (the
		// remediation pull request). The integration-controller receives
		// every event.
		return Needs{
			Grants: Merge([]Grant{{SecurityEvents, Write}, {Issues, Write}, {Contents, Write}, {PullRequests, Write}}),
			Events: []string{EventCodeScanningAlert, EventIssueComment, EventIssues, EventPullRequest},
		}, true
	case FeatureIntents:
		return Needs{Grants: Merge(Intent(), App(false))}, true
	case FeatureChecks:
		return Needs{Grants: Merge(CheckFix())}, true
	}
	return Needs{}, false
}

// Requires is the features f extends and means nothing without: check-fix
// rounds are part of intents.
func (f Feature) Requires() []Feature {
	if f == FeatureChecks {
		return []Feature{FeatureIntents}
	}
	return nil
}

// ForApp is what an App registered for features must hold, and nothing
// more: metadata read, which GitHub requires of every App; each feature's
// grants at the highest access any of them asks for (Merge); and the union
// of their events, sorted, nil when none has one. A feature named twice
// counts once. An unknown feature, or one named without a feature it
// Requires, is an error.
func ForApp(features ...Feature) (Needs, error) {
	sets := [][]Grant{{{Metadata, Read}}}
	var events []string
	for _, f := range features {
		row, ok := f.Needs()
		if !ok {
			return Needs{}, fmt.Errorf("intentperm: unknown feature %q", f)
		}
		for _, r := range f.Requires() {
			if !slices.Contains(features, r) {
				return Needs{}, fmt.Errorf("intentperm: feature %s extends %s, which is not selected", f, r)
			}
		}
		sets = append(sets, row.Grants)
		events = append(events, row.Events...)
	}
	slices.Sort(events)
	return Needs{Grants: Merge(sets...), Events: slices.Compact(events)}, nil
}
