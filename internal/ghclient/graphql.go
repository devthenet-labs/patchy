// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v90/github"
)

// ErrNodeNotFound reports that GitHub's GraphQL API resolved no node of the
// kind asked for under the id: the object was deleted, the credential cannot
// see it, or the id is another kind's.
var ErrNodeNotFound = errors.New("no such node")

// graphQL posts one GraphQL query with vars and decodes its data into out. A
// refused request is go-github's error for the response (so IsRefused,
// IsForbidden and the rate-limit types apply); GraphQL reports most failures
// as a 200 with errors, of which NOT_FOUND is ErrNodeNotFound and any other
// the first error's message.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL(c.gh), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.gh.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if err := github.CheckResponse(res); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode the graphql response: %w", err)
	}
	for _, e := range envelope.Errors {
		if e.Type == "NOT_FOUND" {
			return fmt.Errorf("%s: %w", e.Message, ErrNodeNotFound)
		}
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("graphql: %s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 {
		return errors.New("graphql: the response carries no data")
	}
	return json.Unmarshal(envelope.Data, out)
}

// commentEditedQuery reads whether an issue comment was ever edited:
// lastEditedAt is null, and includesCreatedEdit false, only for one never
// edited.
const commentEditedQuery = `query($id: ID!) {
  node(id: $id) {
    __typename
    ... on IssueComment { lastEditedAt includesCreatedEdit }
  }
}`

// CommentEdited reports whether the issue comment with GraphQL node id nodeID
// (Comment.NodeID) was ever edited, whoever edited it. The REST API dates a
// comment's updated_at to the second, so an edit made within the second the
// comment was posted in leaves it equal to created_at; GraphQL's lastEditedAt
// is the edit itself. A comment that is gone, or that the id does not name,
// is ErrNodeNotFound.
func (c *Client) CommentEdited(ctx context.Context, nodeID string) (bool, error) {
	if strings.TrimSpace(nodeID) == "" {
		return false, fmt.Errorf("ghclient: comment edited: no node id: %w", ErrNodeNotFound)
	}
	var out struct {
		Node *struct {
			Typename            string     `json:"__typename"`
			LastEditedAt        *time.Time `json:"lastEditedAt"`
			IncludesCreatedEdit bool       `json:"includesCreatedEdit"`
		} `json:"node"`
	}
	if err := c.graphQL(ctx, commentEditedQuery, map[string]any{"id": nodeID}, &out); err != nil {
		return false, fmt.Errorf("ghclient: comment edited %s: %w", nodeID, err)
	}
	if out.Node == nil || out.Node.Typename != "IssueComment" {
		return false, fmt.Errorf("ghclient: comment edited %s: not an issue comment: %w", nodeID, ErrNodeNotFound)
	}
	return out.Node.LastEditedAt != nil || out.Node.IncludesCreatedEdit, nil
}

const reviewEditedQuery = `query($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequestReview { lastEditedAt includesCreatedEdit }
  }
}`

const reviewCommentEditedQuery = `query($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequestReviewComment { lastEditedAt includesCreatedEdit }
  }
}`

// ReviewEdited reports whether a PR review's body was ever edited. An edit
// makes the review ineligible as approver-authored feedback, even when REST
// still attributes its body to the original reviewer.
func (c *Client) ReviewEdited(ctx context.Context, nodeID string) (bool, error) {
	return c.pullFeedbackEdited(ctx, nodeID, "PullRequestReview", reviewEditedQuery)
}

// ReviewCommentEdited is the same check for an inline review comment.
func (c *Client) ReviewCommentEdited(ctx context.Context, nodeID string) (bool, error) {
	return c.pullFeedbackEdited(ctx, nodeID, "PullRequestReviewComment", reviewCommentEditedQuery)
}

func (c *Client) pullFeedbackEdited(ctx context.Context, nodeID, kind, query string) (bool, error) {
	if strings.TrimSpace(nodeID) == "" {
		return false, fmt.Errorf("ghclient: %s edited: no node id: %w", kind, ErrNodeNotFound)
	}
	var out struct {
		Node *struct {
			Typename            string     `json:"__typename"`
			LastEditedAt        *time.Time `json:"lastEditedAt"`
			IncludesCreatedEdit bool       `json:"includesCreatedEdit"`
		} `json:"node"`
	}
	if err := c.graphQL(ctx, query, map[string]any{"id": nodeID}, &out); err != nil {
		return false, fmt.Errorf("ghclient: %s edited %s: %w", kind, nodeID, err)
	}
	if out.Node == nil || out.Node.Typename != kind {
		return false, fmt.Errorf("ghclient: %s edited %s: wrong node kind: %w", kind, nodeID, ErrNodeNotFound)
	}
	return out.Node.LastEditedAt != nil || out.Node.IncludesCreatedEdit, nil
}
