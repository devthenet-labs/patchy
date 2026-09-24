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
	// HTMLURL is the issue's page on the forge, the link a human follows.
	HTMLURL string
}

// Actor is the account behind an issue event or a comment.
type Actor struct {
	Login string
	ID    int64
	// Type is GitHub's account type: "User", "Bot" or "Organization".
	Type string
}

// IsBot reports a bot account: a GitHub App's "<slug>[bot]" user or any
// other. Label events carry no performed_via_github_app, so the account
// type (with the login) is how an App's own events are told from a human's.
func (a Actor) IsBot() bool { return a.Type == "Bot" }

// Comment is one issue comment.
type Comment struct {
	ID int64
	// NodeID is the comment's GraphQL node id, which CommentEdited looks it
	// up by.
	NodeID            string
	Body              string
	UserLogin         string
	AuthorAssociation string
	// UserID and UserType complete the author's identity: GitHub's numeric
	// account id and account type ("User", "Bot", "Organization").
	UserID   int64
	UserType string
	// CreatedAt is when the comment was written; UpdatedAt moves on every
	// edit, and is what a since-filtered listing compares against.
	CreatedAt time.Time
	UpdatedAt time.Time
	// ViaApp is the slug of the GitHub App that wrote the comment
	// (performed_via_github_app), "" for a human's comment.
	ViaApp string
	// HTMLURL is the comment's page on the forge.
	HTMLURL string
}

// Author returns the comment author as an Actor.
func (c *Comment) Author() Actor {
	return Actor{Login: c.UserLogin, ID: c.UserID, Type: c.UserType}
}

// PR is a freshly created pull request, or one found open.
type PR struct {
	Number  int
	HTMLURL string
	// NodeID is GitHub's global node id of the pull request, and HeadSHA
	// its head commit, where the response carried them.
	NodeID  string
	HeadSHA string
	// Author is the login that opened the pull request, Base the branch it
	// merges into, and HeadRepo the "owner/name" repository its head branch
	// lives in (a fork's for a pull request from a fork), where the response
	// carried them.
	Author   string
	Base     string
	HeadRepo string
}

// PullRequest is a pull request's current state, as GetPullRequest reads
// it. State is "open" or "closed"; a closed one may be Merged, and only then
// are MergedAt and MergeCommitSHA the merge's (an open PR's merge commit is
// GitHub's trial merge).
type PullRequest struct {
	Number         int
	State          string
	Merged         bool
	MergedAt       time.Time
	MergeCommitSHA string
	// NodeID is GitHub's global node id, which no other pull request ever
	// has, and HeadSHA the head commit now; empty when not reported.
	NodeID  string
	HeadSHA string
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
// recent instance's location, message snippet, commit, and ref.
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
	MostRecentRef   string
}
