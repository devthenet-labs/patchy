// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"time"
)

// IssueEvent is one entry of an issue's event timeline: who did what to it,
// and when, as GitHub recorded it — the evidence authority decisions rest on,
// never a handler-time stamp.
type IssueEvent struct {
	ID     int64
	NodeID string
	// Event is the kind: "labeled", "unlabeled", "closed", "reopened", ...
	Event     string
	CreatedAt time.Time
	Actor     Actor
	// Label is the label's name on "labeled" and "unlabeled" events, ""
	// otherwise.
	Label string
	// ViaApp is performed_via_github_app's slug. GitHub reports none on
	// label events even when an App applied the label, so it never
	// identifies patchy's own label events — Actor does.
	ViaApp string
}

// wireIssueEvent is an issue event as the REST API renders it. go-github's
// IssueEvent has no node_id, so events decode into this instead.
type wireIssueEvent struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
	Actor     *wireUser `json:"actor"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
	ViaApp *wireApp `json:"performed_via_github_app"`
}

// ListIssueEvents returns the issue's events, oldest first as GitHub lists
// them, following pagination.
func (c *Client) ListIssueEvents(ctx context.Context, repo Repo, number int) ([]*IssueEvent, error) {
	w := pageWalk{path: fmt.Sprintf("%s/issues/%d/events", repoPath(repo), number)}
	var out []*IssueEvent
	_, _, err := walk(ctx, c, w, func(page []wireIssueEvent) {
		for _, we := range page {
			ev := &IssueEvent{
				ID:        we.ID,
				NodeID:    we.NodeID,
				Event:     we.Event,
				CreatedAt: we.CreatedAt,
				Actor:     we.Actor.actor(),
				ViaApp:    we.ViaApp.slug(),
			}
			if we.Label != nil {
				ev.Label = we.Label.Name
			}
			out = append(out, ev)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list events of %s#%d: %w", repo, number, err)
	}
	return out, nil
}
