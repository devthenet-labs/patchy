// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/go-github/v90/github"
)

// EnsureLabel creates the label name in repo when the repository has none by
// that name, and reports whether it did. An existing label is left exactly as
// it is, whatever its color or description: a human may have styled it.
// GitHub looks labels up case-insensitively. A create that loses a race to
// another (422, already exists) counts as the label existing.
func (c *Client) EnsureLabel(ctx context.Context, repo Repo, name, color, description string) (bool, error) {
	_, _, err := c.gh.Issues.GetLabel(ctx, repo.Owner, repo.Name, url.PathEscape(name))
	switch {
	case err == nil:
		return false, nil
	case !IsNotFound(err):
		return false, fmt.Errorf("ghclient: get label %q in %s: %w", name, repo, err)
	}
	req := github.CreateIssueLabelRequest{Name: name}
	if color != "" {
		req.Color = new(color)
	}
	if description != "" {
		req.Description = new(description)
	}
	if _, _, err := c.gh.Issues.CreateLabel(ctx, repo.Owner, repo.Name, req); err != nil {
		if IsUnprocessable(err) {
			return false, nil
		}
		return false, fmt.Errorf("ghclient: create label %q in %s: %w", name, repo, err)
	}
	return true, nil
}
