// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"net/http"
	"strings"
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

// graphql answers the one GraphQL query patchy makes: an issue comment's edit
// record by its node id (ghclient.CommentEdited), which GitHub keeps whatever
// the comment's updated_at says. lastEditedAt is null for a comment never
// edited; an unknown id is GitHub's NOT_FOUND error. A token needs issues
// read, as the REST comment reads do. Any other query (the demo reset's
// deleteIssue mutation) is unhandled, as it was before this endpoint.
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.permits(read, permIssues) {
		forbidden(w)
		return
	}
	for _, cs := range s.comments {
		for i := range cs {
			if c := &cs[i]; c.NodeID == id {
				var last any
				if c.edited {
					last = c.UpdatedAt
				}
				writeJSON(w, map[string]any{"data": map[string]any{"node": map[string]any{
					"__typename": "IssueComment", "lastEditedAt": last, "includesCreatedEdit": false,
				}}})
				return
			}
		}
	}
	writeJSON(w, map[string]any{
		"data":   map[string]any{"node": nil},
		"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + id + "'"}},
	})
}
