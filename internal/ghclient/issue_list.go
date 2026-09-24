// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-github/v90/github"
)

// IssueList is one conditional issue listing.
type IssueList struct {
	// Issues are the matching issues, most recently updated first, pull
	// requests skipped. Nil when NotModified.
	Issues []*Issue
	// ETag is the listing's entity tag: pass it back as the next call's
	// etag. On NotModified it is the tag the caller already holds.
	ETag string
	// NotModified reports GitHub answered 304 to the etag: nothing on the
	// listing's first page changed, and the caller's previous result
	// stands. A 304 does not count against the rate limit.
	NotModified bool
}

// ListIssues lists repo's issues carrying all of labels in state ("open",
// "closed", "all"; "" is GitHub's default, open) as a conditional request:
// a non-empty etag from a previous call is sent as If-None-Match, and an
// unchanged listing answers NotModified without consuming the rate limit.
//
// The condition covers the first page only. The listing is sorted by last
// update, so any change to a matching issue — a newly applied label, an
// edit — brings it to the first page and changes the tag; an issue that
// leaves the filter from a later page does not. It is a discovery feed, not
// an exact membership set.
func (c *Client) ListIssues(ctx context.Context, repo Repo, labels []string, state, etag string) (*IssueList, error) {
	w := pageWalk{
		path:  repoPath(repo) + "/issues",
		query: url.Values{"sort": {"updated"}, "direction": {"desc"}},
		etag:  etag,
	}
	if state != "" {
		w.query.Set("state", state)
	}
	if len(labels) > 0 {
		w.query.Set("labels", strings.Join(labels, ","))
	}
	var issues []*Issue
	tag, notModified, err := walk(ctx, c, w, func(page []*github.Issue) {
		for _, is := range page {
			if is.IsPullRequest() {
				continue
			}
			issue := issueFromGitHub(is)
			issue.Repo = repo
			issues = append(issues, issue)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list issues in %s: %w", repo, err)
	}
	if notModified {
		return &IssueList{ETag: tag, NotModified: true}, nil
	}
	return &IssueList{Issues: issues, ETag: tag}, nil
}
