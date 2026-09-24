// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"maps"
	"net/http"
	"strings"
)

// SetParents records commit ancestry for the compare endpoint: each commit
// mapped to its parent. A linear history is all the flows need; commits it
// does not name are unrelated to every other.
func (s *Server) SetParents(parents map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.parents, parents)
}

// Compares reports how many compare calls the fake has answered.
func (s *Server) Compares() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compares
}

// compare mimics GET /repos/{owner}/{repo}/compare/{base}...{head}: how head
// relates to base — "identical", "behind" (head is an ancestor of base),
// "ahead" (head descends from base), or "diverged".
func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	base, head, ok := strings.Cut(r.PathValue("spec"), "...")
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compares++
	status, ahead, behind := "diverged", 0, 0
	switch {
	case base == head:
		status = "identical"
	case s.steps(base, head) > 0:
		status, behind = "behind", s.steps(base, head)
	case s.steps(head, base) > 0:
		status, ahead = "ahead", s.steps(head, base)
	}
	writeJSON(w, map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind})
}

// steps is how many parent hops lead from descendant back to ancestor, or 0
// when ancestor is not in descendant's history. Callers hold s.mu.
func (s *Server) steps(descendant, ancestor string) int {
	n := 0
	for c := s.parents[descendant]; c != ""; c = s.parents[c] {
		n++
		if c == ancestor {
			return n
		}
	}
	return 0
}
