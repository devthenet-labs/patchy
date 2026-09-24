// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import "time"

// Issue is patchy's thin view of a GitHub issue, decoupled from go-github.
type Issue struct {
	// Repo is the issue's repository, populated from the list/search
	// response so cross-repository sweeps can act on their results.
	Repo      Repo
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []string
	Assignees []string
	CreatedAt time.Time
	// Author is the login that opened the issue.
	Author string
}

// Comment is one issue comment.
type Comment struct {
	ID                int64
	Body              string
	UserLogin         string
	AuthorAssociation string
}

// PR is a freshly created pull request.
type PR struct {
	Number  int
	HTMLURL string
}

// IssueRequest is the payload for creating an issue.
type IssueRequest struct {
	Title  string
	Body   string
	Labels []string
}

// PRRequest is the payload for creating a pull request.
type PRRequest struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// Alert is patchy's view of a code-scanning alert: the rule, the severity
// (security_severity_level falling back to the rule severity), and the most
// recent instance's location, message snippet, and commit.
type Alert struct {
	Number          int
	RuleID          string
	RuleName        string
	RuleDescription string
	RuleHelp        string
	Tags            []string
	Severity        string
	State           string
	HTMLURL         string
	Path            string
	StartLine       int
	EndLine         int
	Snippet         string
	MostRecentSHA   string
}
