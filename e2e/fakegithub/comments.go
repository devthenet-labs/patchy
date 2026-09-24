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

// reaction is one reaction on a comment, as GitHub renders it.
type reaction struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	User      Actor     `json:"user"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// reactionContents are the reactions GitHub accepts; anything else is 422.
var reactionContents = []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"}

// getComment answers GET /repos/{o}/{r}/issues/comments/{id} (routed
// through getIssueSub, where the id is the sub segment).
func (s *Server) getComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("sub"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.findComment(id)
	if c == nil {
		notFound(w)
		return
	}
	writeJSON(w, c)
}

// createReaction answers POST /repos/{o}/{r}/issues/comments/{id}/reactions
// as the caller: 201 with a new reaction, 200 with the existing one when the
// caller already reacted so (GitHub's idempotent answer), 422 for a content
// GitHub does not know, 404 for an unknown comment.
func (s *Server) createReaction(w http.ResponseWriter, r *http.Request) {
	actor := s.caller(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !slices.Contains(reactionContents, body.Content) {
		unprocessable(w, "Validation Failed")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findComment(id) == nil {
		notFound(w)
		return
	}
	for _, re := range s.reactions[id] {
		if re.User == actor && re.Content == body.Content {
			writeJSON(w, re)
			return
		}
	}
	s.nextReactionID++
	re := reaction{
		ID:        s.nextReactionID,
		NodeID:    fmt.Sprintf("REA_fake%d", s.nextReactionID),
		User:      actor,
		Content:   body.Content,
		CreatedAt: s.now(),
	}
	s.reactions[id] = append(s.reactions[id], re)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, re)
}

// Reactions returns the contents of a comment's reactions, in order.
func (s *Server) Reactions(commentID int64) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.reactions[commentID]))
	for _, re := range s.reactions[commentID] {
		out = append(out, re.Content)
	}
	return out
}

// CommentAs adds a comment to an issue as actor — a human writing a
// command or feedback — and returns its id.
func (s *Server) CommentAs(number int, body string, actor Actor) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(number, body, actor).ID
}

// EditCommentBody replaces a comment's body and moves its updated_at, as a
// human editing it does; it reports false for an unknown comment.
func (s *Server) EditCommentBody(id int64, body string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.findComment(id)
	if c == nil {
		return false
	}
	c.Body, c.UpdatedAt, c.edited = body, s.now(), true
	return true
}
