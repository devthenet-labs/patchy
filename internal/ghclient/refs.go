// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v90/github"
)

// Sentinel errors of the never-forcing branch moves.
var (
	// ErrBranchExists: CreateBranchRef found the branch already pointing at
	// a commit other than the one asked for.
	ErrBranchExists = errors.New("branch already exists at another commit")
	// ErrNotFastForward: the requested commit does not descend from the
	// branch's head — someone pushed, or the move would rewind history.
	// FastForwardRef never forces past it.
	ErrNotFastForward = errors.New("update is not a fast forward")
	// ErrRefNotFound: the branch FastForwardRef was asked to move does not
	// exist.
	ErrRefNotFound = errors.New("reference does not exist")
)

// GitHub's 422 messages on the Git refs endpoints, verified live
// (2026-09-24). Every refusal there is a 422, so the message is what tells
// them apart; an unrecognised message is a plain error, never a sentinel.
const (
	msgRefExists      = "Reference already exists"
	msgNotFastForward = "Update is not a fast forward"
	msgRefMissing     = "Reference does not exist"
)

// unprocessable returns GitHub's message when err is a 422.
func unprocessable(err error) (string, bool) {
	var ger *github.ErrorResponse
	if errors.As(err, &ger) && ger.Response != nil && ger.Response.StatusCode == http.StatusUnprocessableEntity {
		return ger.Message, true
	}
	return "", false
}

// CreateBranchRef creates refs/heads/<branch> at sha. It is create-only and
// never moves an existing branch: when the branch already exists it is
// adopted only if it already points at sha (a retry after a crash between
// creating the ref and recording it), and is otherwise ErrBranchExists.
func (c *Client) CreateBranchRef(ctx context.Context, repo Repo, branch, sha string) error {
	_, _, err := c.gh.Git.CreateRef(ctx, repo.Owner, repo.Name, github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: sha,
	})
	if err == nil {
		return nil
	}
	if msg, ok := unprocessable(err); !ok || msg != msgRefExists {
		return fmt.Errorf("ghclient: create branch %s at %s in %s: %w", branch, sha, repo, err)
	}
	head, err := c.HeadSHA(ctx, repo, branch)
	if err != nil {
		return fmt.Errorf("ghclient: create branch %s in %s: read the existing branch: %w", branch, repo, err)
	}
	if head != sha {
		return fmt.Errorf("ghclient: create branch %s in %s: it is at %s, not %s: %w",
			branch, repo, head, sha, ErrBranchExists)
	}
	return nil
}

// FastForwardRef moves refs/heads/<branch> to sha only when sha descends
// from the branch's head (force=false); a branch already at sha is a no-op
// success. A diverged or rewinding move is ErrNotFastForward, a missing
// branch ErrRefNotFound, and any other refusal (an unknown sha: "Object
// does not exist") a plain error. It never forces.
func (c *Client) FastForwardRef(ctx context.Context, repo Repo, branch, sha string) error {
	ref, _, err := c.gh.Git.UpdateRef(ctx, repo.Owner, repo.Name, "heads/"+branch,
		github.UpdateRef{SHA: sha, Force: new(false)})
	if err != nil {
		msg, _ := unprocessable(err)
		switch msg {
		case msgNotFastForward:
			err = fmt.Errorf("%w: %w", ErrNotFastForward, err)
		case msgRefMissing:
			err = fmt.Errorf("%w: %w", ErrRefNotFound, err)
		}
		return fmt.Errorf("ghclient: fast-forward %s in %s to %s: %w", branch, repo, sha, err)
	}
	if got := ref.GetObject().GetSHA(); got != sha {
		return fmt.Errorf("ghclient: fast-forward %s in %s: branch is at %s, want %s", branch, repo, got, sha)
	}
	return nil
}
