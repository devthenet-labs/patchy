// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"time"

	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// PR conversation comments share REST endpoints and wire types with issue
// comments, not their permission. Public reads can hide a wrongly scoped token;
// writes (and private projects) require the PR's own permission.
func (g *forgeGitHub) ListPullRequestComments(ctx context.Context, repoURL string, number int64, since time.Time) (
	[]*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.ListIssueComments(ctx, repo, int(number), since)
}

func (g *forgeGitHub) GetPullRequestComment(ctx context.Context, repoURL string, id int64) (*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.GetIssueComment(ctx, repo, id)
}

func (g *forgeGitHub) PullRequestCommentEdited(ctx context.Context, repoURL, nodeID string) (bool, error) {
	c, _, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return false, err
	}
	return c.CommentEdited(ctx, nodeID)
}

func (g *forgeGitHub) CreatePullRequestComment(ctx context.Context, repoURL string, number int64, body string) (
	*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, pullsWrite)
	if err != nil {
		return nil, err
	}
	return c.CreateIssueComment(ctx, repo, int(number), body)
}

func (g *forgeGitHub) ReactPullRequestComment(ctx context.Context, repoURL string, commentID int64) error {
	c, repo, err := g.client(ctx, repoURL, pullsWrite)
	if err != nil {
		return err
	}
	return c.CreateIssueCommentReaction(ctx, repo, commentID, ghclient.ReactionEyes)
}
