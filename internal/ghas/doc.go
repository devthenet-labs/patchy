// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package ghas implements the pkg/source plugin for GitHub Advanced Security
// code-scanning alerts (CodeQL first): it consumes code_scanning_alert
// webhook deliveries, fetches the full alert for rule help and location
// detail, and normalizes it into a source.Finding — extracting CWE
// identifiers from CodeQL rule tags as the advisory categorization. Only
// alerts whose most recent instance is on the repository's default branch are
// ingested: alerts raised on other branches (patchy's own remediation
// branches included) or on pull-request refs are skipped, so a fix CodeQL
// rejects on its PR cannot loop back in as a new finding. A delivery that
// omits the ref or the default branch is ingested.
package ghas
