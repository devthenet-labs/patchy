// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

type review struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
	User        Actor     `json:"user"`
	edited      bool
}

type reviewComment struct {
	ID                  int64     `json:"id"`
	NodeID              string    `json:"node_id"`
	PullRequestReviewID int64     `json:"pull_request_review_id"`
	Body                string    `json:"body"`
	Path                string    `json:"path"`
	Line                int       `json:"line"`
	Side                string    `json:"side"`
	DiffHunk            string    `json:"diff_hunk"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	User                Actor     `json:"user"`
	edited              bool
}

// EditReviewBody simulates a user with write access editing another user's
// review while REST still reports the original author.
func (s *Server) EditReviewBody(id int64, body string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for number := range s.reviews {
		for i := range s.reviews[number] {
			if r := &s.reviews[number][i]; r.ID == id {
				r.Body, r.edited = body, true
				return true
			}
		}
	}
	return false
}

// EditReviewCommentBody simulates an inline comment edit within the same
// second: REST's updated_at can still equal created_at, but GraphQL knows.
func (s *Server) EditReviewCommentBody(id int64, body string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for number := range s.reviewComments {
		for i := range s.reviewComments[number] {
			if c := &s.reviewComments[number][i]; c.ID == id {
				c.Body, c.UpdatedAt, c.edited = body, s.now(), true
				return true
			}
		}
	}
	return false
}

// AddReview records a submitted review and returns its review ID.
func (s *Server) AddReview(number int, author Actor, state, body, commitSHA string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextReviewID++
	id := s.nextReviewID
	s.reviews[number] = append(s.reviews[number], review{ID: id, NodeID: fmt.Sprintf("PRR_fake%d", id), User: author, State: state,
		Body: body, CommitID: commitSHA, SubmittedAt: s.now()})
	return id
}

// AddReviewComment records an inline comment and returns its comment ID.
func (s *Server) AddReviewComment(number int, author Actor, reviewID int64, path string, line int,
	side, body, hunk string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextReviewCommentID++
	id := s.nextReviewCommentID
	stamp := s.now()
	s.reviewComments[number] = append(s.reviewComments[number], reviewComment{ID: id, NodeID: fmt.Sprintf("PRRC_fake%d", id),
		PullRequestReviewID: reviewID, User: author, Body: body, Path: path, Line: line,
		Side: side, DiffHunk: hunk, CreatedAt: stamp, UpdatedAt: stamp})
	return id
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	_, ok := s.pulls[number]
	out := slices.Clone(s.reviews[number])
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	if out == nil {
		out = []review{}
	}
	writeJSON(w, out)
}

func (s *Server) listReviewComments(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	_, ok := s.pulls[number]
	out := slices.Clone(s.reviewComments[number])
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	if out == nil {
		out = []reviewComment{}
	}
	writeJSON(w, out)
}

func (s *Server) requestReviewers(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		notFound(w)
		return
	}
	var body struct {
		Reviewers []string `json:"reviewers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.pulls[number]
	if ok {
		for _, login := range body.Reviewers {
			if !slices.ContainsFunc(p.RequestedReviewers, func(a Actor) bool { return a.Login == login }) {
				p.RequestedReviewers = append(p.RequestedReviewers, Actor{Login: login, Type: "User"})
			}
		}
	}
	var out pull
	if ok {
		out = s.rendered(p)
	}
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, out)
}
