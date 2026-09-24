// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// wireComment is an issue comment as the REST API renders it. go-github's
// IssueComment drops performed_via_github_app, which is how a comment an App
// wrote is told from a human's, so comments decode into this instead.
type wireComment struct {
	ID                int64     `json:"id"`
	Body              string    `json:"body"`
	User              *wireUser `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	HTMLURL           string    `json:"html_url"`
	ViaApp            *wireApp  `json:"performed_via_github_app"`
}

// comment maps the wire form onto patchy's Comment.
func (w *wireComment) comment() *Comment {
	author := w.User.actor()
	return &Comment{
		ID:                w.ID,
		Body:              w.Body,
		UserLogin:         author.Login,
		AuthorAssociation: w.AuthorAssociation,
		UserID:            author.ID,
		UserType:          author.Type,
		CreatedAt:         w.CreatedAt,
		UpdatedAt:         w.UpdatedAt,
		ViaApp:            w.ViaApp.slug(),
		HTMLURL:           w.HTMLURL,
	}
}

// ListIssueComments returns the issue's comments, oldest first, following
// pagination. A non-zero since keeps only comments updated at or after it:
// GitHub compares since against each comment's last update, so an older
// comment edited later is returned again — callers track what they have
// consumed by comment ID, never by position.
func (c *Client) ListIssueComments(ctx context.Context, repo Repo, number int, since time.Time) ([]*Comment, error) {
	w := pageWalk{path: fmt.Sprintf("%s/issues/%d/comments", repoPath(repo), number), query: url.Values{}}
	if !since.IsZero() {
		w.query.Set("since", wireTime(since))
	}
	var out []*Comment
	_, _, err := walk(ctx, c, w, func(page []wireComment) {
		for i := range page {
			out = append(out, page[i].comment())
		}
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list comments on %s#%d: %w", repo, number, err)
	}
	return out, nil
}

// GetIssueComment fetches one issue comment by id — the re-read that proves
// a comment still says what it said when it was recorded.
func (c *Client) GetIssueComment(ctx context.Context, repo Repo, commentID int64) (*Comment, error) {
	path := repoPath(repo) + "/issues/comments/" + strconv.FormatInt(commentID, 10)
	req, err := c.gh.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("ghclient: get comment %d on %s: %w", commentID, repo, err)
	}
	var wc wireComment
	if _, err := c.gh.Do(req, &wc); err != nil {
		return nil, fmt.Errorf("ghclient: get comment %d on %s: %w", commentID, repo, err)
	}
	return wc.comment(), nil
}

// CreateIssueComment adds a comment to the issue and returns it as GitHub
// stored it: its id, the body GitHub now holds, and GitHub's created_at.
// A caller that later proves the comment unchanged hashes this body, never
// the one it sent, and orders other events against this time, never its own
// clock.
func (c *Client) CreateIssueComment(ctx context.Context, repo Repo, number int, body string) (*Comment, error) {
	path := fmt.Sprintf("%s/issues/%d/comments", repoPath(repo), number)
	req, err := c.gh.NewRequest(ctx, http.MethodPost, path, struct {
		Body string `json:"body"`
	}{body})
	if err != nil {
		return nil, fmt.Errorf("ghclient: comment on %s#%d: %w", repo, number, err)
	}
	var wc wireComment
	if _, err := c.gh.Do(req, &wc); err != nil {
		return nil, fmt.Errorf("ghclient: comment on %s#%d: %w", repo, number, err)
	}
	return wc.comment(), nil
}

// ReactionEyes is the "seen" reaction a command is acknowledged with.
const ReactionEyes = "eyes"

// CreateIssueCommentReaction adds the credential's reaction (content is one
// of GitHub's reactions: "+1", "-1", "laugh", "confused", "heart", "hooray",
// "rocket" or "eyes") to an issue comment — the acknowledgement that a
// command was seen. It is idempotent: GitHub answers an existing reaction
// with 200 and a new one with 201, and both are success.
func (c *Client) CreateIssueCommentReaction(ctx context.Context, repo Repo, commentID int64, content string) error {
	if _, _, err := c.gh.Reactions.CreateIssueCommentReaction(ctx, repo.Owner, repo.Name, commentID, content); err != nil {
		return fmt.Errorf("ghclient: react %q to comment %d on %s: %w", content, commentID, repo, err)
	}
	return nil
}
