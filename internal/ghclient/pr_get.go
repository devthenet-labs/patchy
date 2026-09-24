// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
)

// GetPullRequest fetches one pull request's current state — what a close
// seen during review consults: a tracking issue's, to tell a merged fix
// (whose "Fixes #N" closed the issue) from a human closing it, or a PR's
// from a repository renamed since it was recorded.
func (c *Client) GetPullRequest(ctx context.Context, repo Repo, number int) (*PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return nil, fmt.Errorf("ghclient: get PR %s#%d: %w", repo, number, err)
	}
	return &PullRequest{
		Number:         pr.GetNumber(),
		State:          pr.GetState(),
		Merged:         pr.GetMerged(),
		MergedAt:       pr.GetMergedAt().Time,
		MergeCommitSHA: pr.GetMergeCommitSHA(),
		NodeID:         pr.GetNodeID(),
		HeadSHA:        pr.GetHead().GetSHA(),
	}, nil
}
