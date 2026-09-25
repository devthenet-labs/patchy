// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/go-github/v90/github"
)

// Review is a submitted pull-request review. The caller, not this transport
// seam, decides whether Author is an approver and whether State is actionable.
type Review struct {
	ID          int64
	NodeID      string
	Author      Actor
	Body        string
	State       string
	CommitSHA   string
	SubmittedAt time.Time
	HTMLURL     string
}

// ReviewComment is an inline pull-request comment with its location and
// diff hunk. Body and DiffHunk are untrusted data, not instructions.
type ReviewComment struct {
	ID        int64
	NodeID    string
	ReviewID  int64
	Author    Actor
	Body      string
	Path      string
	Line      int
	Side      string
	DiffHunk  string
	CreatedAt time.Time
	UpdatedAt time.Time
	HTMLURL   string
}

// ListPullRequestReviews lists every review, following pagination. A walk
// beyond the common 50-page cap errors rather than silently losing the
// newest feedback.
func (c *Client) ListPullRequestReviews(ctx context.Context, repo Repo, number int) ([]Review, error) {
	opts := &github.ListOptions{PerPage: listPageSize}
	var out []Review
	for range walkPageCap {
		page, resp, err := c.gh.PullRequests.ListReviews(ctx, repo.Owner, repo.Name, number, opts)
		if err != nil {
			return nil, fmt.Errorf("ghclient: list reviews on %s#%d: %w", repo, number, err)
		}
		for _, v := range page {
			if v == nil {
				continue
			}
			out = append(out, Review{ID: v.GetID(), NodeID: v.GetNodeID(), Author: actorFromUser(v.User), Body: v.GetBody(),
				State: v.GetState(), CommitSHA: v.GetCommitID(), SubmittedAt: v.GetSubmittedAt().Time,
				HTMLURL: v.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("ghclient: reviews on %s#%d run past %d pages", repo, number, walkPageCap)
}

// ListPullRequestReviewComments lists inline comments, including the review
// ID that links each comment to its submitted review.
func (c *Client) ListPullRequestReviewComments(ctx context.Context, repo Repo, number int) ([]ReviewComment, error) {
	opts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: listPageSize}}
	var out []ReviewComment
	for range walkPageCap {
		page, resp, err := c.gh.PullRequests.ListComments(ctx, repo.Owner, repo.Name, number, opts)
		if err != nil {
			return nil, fmt.Errorf("ghclient: list review comments on %s#%d: %w", repo, number, err)
		}
		for _, v := range page {
			if v == nil {
				continue
			}
			out = append(out, ReviewComment{ID: v.GetID(), NodeID: v.GetNodeID(), ReviewID: v.GetPullRequestReviewID(),
				Author: actorFromUser(v.User), Body: v.GetBody(), Path: v.GetPath(), Line: v.GetLine(),
				Side: v.GetSide(), DiffHunk: v.GetDiffHunk(), CreatedAt: v.GetCreatedAt().Time,
				UpdatedAt: v.GetUpdatedAt().Time, HTMLURL: v.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("ghclient: review comments on %s#%d run past %d pages", repo, number, walkPageCap)
}

func actorFromUser(u *github.User) Actor {
	return Actor{Login: u.GetLogin(), ID: u.GetID(), Type: u.GetType()}
}

const maxPatchBytes = 48 << 10

const patchTruncated = "\n[compare patch truncated at 48 KiB]\n"

var errPatchLimit = errors.New("patch byte limit reached")

type patchWriter struct{ bytes.Buffer }

func (w *patchWriter) Write(p []byte) (int, error) {
	room := maxPatchBytes + 1 - w.Len()
	if len(p) <= room {
		return w.Buffer.Write(p)
	}
	_, _ = w.Buffer.Write(p[:room])
	return room, errPatchLimit
}

// ComparePatch returns GitHub's patch for base...head, capped at 48 KiB
// including a notice if truncated. The caller still has to visibly escape
// untrusted characters before presenting it to an agent.
func (c *Client) ComparePatch(ctx context.Context, repo Repo, base, head string) (string, error) {
	path := fmt.Sprintf("%s/compare/%s...%s", repoPath(repo), url.QueryEscape(base), url.QueryEscape(head))
	req, err := c.gh.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("ghclient: compare patch %s %s...%s: %w", repo, base, head, err)
	}
	req.Header.Set("Accept", "application/vnd.github.patch")
	var body patchWriter
	_, err = c.gh.Do(req, &body)
	if err != nil && !errors.Is(err, errPatchLimit) {
		return "", fmt.Errorf("ghclient: compare patch %s %s...%s: %w", repo, base, head, err)
	}
	if body.Len() > maxPatchBytes {
		return body.String()[:maxPatchBytes-len(patchTruncated)] + patchTruncated, nil
	}
	return body.String(), nil
}

// RequestReviewers requests user reviewers on a PR. The controller passes
// only a Project's configured approvers, never agent-supplied names.
func (c *Client) RequestReviewers(ctx context.Context, repo Repo, number int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	_, _, err := c.gh.PullRequests.RequestReviewers(ctx, repo.Owner, repo.Name, number,
		github.ReviewersRequest{Reviewers: logins})
	if err != nil {
		return fmt.Errorf("ghclient: request reviewers on %s#%d: %w", repo, number, err)
	}
	return nil
}
