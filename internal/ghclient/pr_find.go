// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"

	"github.com/google/go-github/v90/github"
)

// prFromGitHub maps a go-github pull request onto patchy's PR.
func prFromGitHub(pr *github.PullRequest) *PR {
	return &PR{
		Number: pr.GetNumber(), HTMLURL: pr.GetHTMLURL(),
		NodeID: pr.GetNodeID(), HeadSHA: pr.GetHead().GetSHA(),
		Author: pr.GetUser().GetLogin(), Base: pr.GetBase().GetRef(),
		HeadRepo: pr.GetHead().GetRepo().GetFullName(),
	}
}

// FindPRByHead returns the open pull request whose head is branch, or nil —
// the idempotency check before creating a remediation PR (a crash between
// push and status write must not open a duplicate).
func (c *Client) FindPRByHead(ctx context.Context, repo Repo, branch string) (*PR, error) {
	prs, _, err := c.gh.PullRequests.List(ctx, repo.Owner, repo.Name, &github.PullRequestListOptions{
		State:       "open",
		Head:        repo.Owner + ":" + branch,
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: find PR by head %s in %s: %w", branch, repo, err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return prFromGitHub(prs[0]), nil
}

// FindOpenPR returns the open pull request from branch into base, or nil.
// GitHub keeps at most one open pull request per head and base, so there is
// no other to miss. The head filter names branch in repo's owner's namespace,
// which a fork the same owner holds shares: the caller reads HeadRepo (and
// Author) before it takes the pull request for its own.
func (c *Client) FindOpenPR(ctx context.Context, repo Repo, branch, base string) (*PR, error) {
	prs, _, err := c.gh.PullRequests.List(ctx, repo.Owner, repo.Name, &github.PullRequestListOptions{
		State:       "open",
		Head:        repo.Owner + ":" + branch,
		Base:        base,
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: find the open PR from %s into %s in %s: %w", branch, base, repo, err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return prFromGitHub(prs[0]), nil
}
