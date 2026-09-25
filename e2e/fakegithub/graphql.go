// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// graphqlPath is where go-github's client, pointed at a non-github.com base
// URL, finds the GraphQL endpoint: beside the REST prefix /api/v3.
const graphqlPath = "/api/graphql"

// withGraphQL serves the GraphQL endpoint in front of the REST API.
func (s *Server) withGraphQL(rest http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == graphqlPath {
			s.graphql(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	})
}

// graphql answers edit-record queries for issue comments, PR reviews and
// inline review comments. lastEditedAt is null only when never edited; an
// unknown id is GitHub's NOT_FOUND error. A token needs issues read for an
// issue comment, pull_requests read for PR feedback.
func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "fakegithub: graphql takes POST", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.Contains(body.Query, "lastEditedAt") {
		http.Error(w, "fakegithub: unhandled graphql query", http.StatusNotFound)
		return
	}
	// A query is a read, whatever its method.
	read := r.Clone(r.Context())
	read.Method = http.MethodGet
	id, _ := body.Variables["id"].(string)
	kind := "IssueComment"
	if strings.Contains(body.Query, "... on PullRequestReviewComment") {
		kind = "PullRequestReviewComment"
	} else if strings.Contains(body.Query, "... on PullRequestReview") {
		kind = "PullRequestReview"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if repo := s.feedbackRepository(kind, id); repo != "" {
		read.SetPathValue("repo", repo)
	}
	perm := permIssues
	if kind != "IssueComment" {
		perm = permPullRequests
	}
	if !s.permits(read, perm) {
		writeJSON(w, map[string]any{
			"data":   map[string]any{"node": nil},
			"errors": []map[string]any{{"type": "FORBIDDEN", "message": "Resource not accessible by integration"}},
		})
		return
	}
	if kind == "PullRequestReview" {
		for _, rs := range s.reviews {
			for _, review := range rs {
				if review.NodeID == id {
					writeEditedNode(w, kind, review.edited, review.SubmittedAt)
					return
				}
			}
		}
	}
	if kind == "PullRequestReviewComment" {
		for _, cs := range s.reviewComments {
			for _, comment := range cs {
				if comment.NodeID == id {
					writeEditedNode(w, kind, comment.edited, comment.UpdatedAt)
					return
				}
			}
		}
	}
	for _, cs := range s.comments {
		if kind != "IssueComment" {
			break
		}
		for i := range cs {
			if c := &cs[i]; c.NodeID == id {
				writeEditedNode(w, kind, c.edited, c.UpdatedAt)
				return
			}
		}
	}
	writeJSON(w, map[string]any{
		"data":   map[string]any{"node": nil},
		"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + id + "'"}},
	})
}

// feedbackRepository finds the repository name behind a GraphQL node so a
// one-repository installation token cannot read another repository's node.
// Fabricated PRs (OpenPull) have no repository, as elsewhere in the fake.
// Callers hold s.mu.
func (s *Server) feedbackRepository(kind, id string) string {
	repoName := func(full string) string {
		return full[strings.LastIndex(full, "/")+1:]
	}
	switch kind {
	case "PullRequestReview":
		for n, reviews := range s.reviews {
			for _, review := range reviews {
				if review.NodeID == id && s.pulls[n] != nil {
					return repoName(s.pulls[n].repository)
				}
			}
		}
	case "PullRequestReviewComment":
		for n, comments := range s.reviewComments {
			for _, comment := range comments {
				if comment.NodeID == id && s.pulls[n] != nil {
					return repoName(s.pulls[n].repository)
				}
			}
		}
	case "IssueComment":
		for n, comments := range s.comments {
			for _, comment := range comments {
				if comment.NodeID != id {
					continue
				}
				if issue := s.issues[n]; issue != nil {
					return repoName(issue.RepositoryURL)
				}
				if pull := s.pulls[n]; pull != nil {
					return repoName(pull.repository)
				}
			}
		}
	}
	return ""
}

func writeEditedNode(w http.ResponseWriter, kind string, edited bool, at time.Time) {
	var last any
	if edited {
		last = at
	}
	writeJSON(w, map[string]any{"data": map[string]any{"node": map[string]any{
		"__typename": kind, "lastEditedAt": last, "includesCreatedEdit": edited,
	}}})
}
