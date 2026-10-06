// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"

	"github.com/google/go-github/v90/github"
)

// PullRequestFile is one file a pull request changes: its path, and the
// path it was renamed from, which the pull request changes too ("" unless
// it was renamed). Both are untrusted text: anyone who can push to the pull
// request's branch chose them.
type PullRequestFile struct {
	Path         string
	PreviousPath string
}

// ListPullRequestFiles returns the first files a pull request changes, in
// GitHub's order, at most limit (1..100) of them. It reads one page and
// never walks further, so a pull request changing more files than limit is
// cut short: the caller reads the total from GetPullRequest's ChangedFiles.
func (c *Client) ListPullRequestFiles(ctx context.Context, repo Repo, number, limit int) (
	[]PullRequestFile, error) {
	if limit <= 0 || limit > listPageSize {
		return nil, fmt.Errorf("ghclient: file limit %d must be 1..%d", limit, listPageSize)
	}
	files, _, err := c.gh.PullRequests.ListFiles(ctx, repo.Owner, repo.Name, number,
		&github.ListOptions{PerPage: limit})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list files of PR %s#%d: %w", repo, number, err)
	}
	out := make([]PullRequestFile, 0, min(len(files), limit))
	for _, f := range files {
		if f == nil || f.GetFilename() == "" || len(out) == limit {
			continue
		}
		out = append(out, PullRequestFile{Path: f.GetFilename(), PreviousPath: f.GetPreviousFilename()})
	}
	return out, nil
}
