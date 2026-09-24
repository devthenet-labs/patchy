// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// issueEvent is one entry of an issue's timeline, as GitHub renders it.
type issueEvent struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Event     string    `json:"event"`
	Actor     Actor     `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Label     *label    `json:"label,omitempty"`
	// ViaApp is always null: GitHub reports no performed_via_github_app on
	// label events even when an App applied the label (verified live), so
	// the actor is the only way to tell the App's own events.
	ViaApp *appRef `json:"performed_via_github_app"`
}

// IssueEvent is a snapshot of one recorded event, for assertions.
type IssueEvent struct {
	ID    int64
	Event string
	Label string
	Actor Actor
}

// recordEvent appends an event to the issue's timeline. Callers hold s.mu.
func (s *Server) recordEvent(number int, event, labelName string, actor Actor) {
	s.nextEventID++
	ev := issueEvent{
		ID:        s.nextEventID,
		NodeID:    fmt.Sprintf("IE_fake%d", s.nextEventID),
		Event:     event,
		Actor:     actor,
		CreatedAt: s.now(),
	}
	if labelName != "" {
		ev.Label = &label{Name: labelName}
	}
	s.events[number] = append(s.events[number], ev)
}

// getIssueSub answers the GET issue sub-resources that overlap as mux
// patterns: issues/{number}/comments, issues/{number}/events and
// issues/comments/{id}.
func (s *Server) getIssueSub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.PathValue("number") == "comments":
		s.getComment(w, r)
	case r.PathValue("sub") == "comments":
		s.listComments(w, r)
	case r.PathValue("sub") == "events":
		s.listEvents(w, r)
	default:
		notFound(w)
	}
}

// listEvents answers GET /repos/{o}/{r}/issues/{number}/events, oldest
// first; an unknown issue is a 404.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.issues[number]; !ok {
		notFound(w)
		return
	}
	out := append([]issueEvent{}, s.events[number]...)
	writeJSONTagged(w, r, out)
}

// Events returns an issue's timeline, oldest first.
func (s *Server) Events(number int) []IssueEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]IssueEvent, 0, len(s.events[number]))
	for _, ev := range s.events[number] {
		snap := IssueEvent{ID: ev.ID, Event: ev.Event, Actor: ev.Actor}
		if ev.Label != nil {
			snap.Label = ev.Label.Name
		}
		out = append(out, snap)
	}
	return out
}

// OpenIssue opens an issue in owner/repo as author — a human filing an
// intent through an issue form, whose labels GitHub records as applied by
// the opener (not verified live) — and returns its number.
func (s *Server) OpenIssue(owner, repo, title, body string, labels []string, author Actor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openIssue(owner, repo, title, body, labels, author).Number
}

// LabelIssue applies a label as actor, as a human does in the UI; a label
// already on the issue records nothing, so re-applying one means removing
// it first (UnlabelIssue).
func (s *Server) LabelIssue(number int, name string, actor Actor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is, ok := s.issues[number]; ok {
		s.label(is, name, actor)
	}
}

// UnlabelIssue removes a label as actor.
func (s *Server) UnlabelIssue(number int, name string, actor Actor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is, ok := s.issues[number]; ok {
		s.unlabel(is, name, actor)
	}
}

// CloseIssueAs closes an issue as actor with a state_reason ("completed",
// "not_planned", or ""), recording the closed event.
func (s *Server) CloseIssueAs(number int, reason string, actor Actor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is, ok := s.issues[number]; ok {
		s.setState(is, "closed", reason, actor)
	}
}
