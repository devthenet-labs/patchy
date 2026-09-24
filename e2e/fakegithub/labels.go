// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// RepoLabel is one label a repository defines, as GitHub renders it.
type RepoLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// repoKey is how the fake files per-repository state: GitHub compares
// owner and repository names case-insensitively.
func repoKey(owner, repo string) string { return strings.ToLower(owner + "/" + repo) }

// findRepoLabel returns the repository's label named name, compared as
// GitHub compares label names (case-insensitively), or nil. Callers hold
// s.mu.
func (s *Server) findRepoLabel(owner, repo, name string) *RepoLabel {
	labels := s.repoLabels[repoKey(owner, repo)]
	for i := range labels {
		if strings.EqualFold(labels[i].Name, name) {
			return &labels[i]
		}
	}
	return nil
}

// getRepoLabel answers GET /repos/{o}/{r}/labels/{name}: the label, or 404
// when the repository defines none by that name.
func (s *Server) getRepoLabel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.findRepoLabel(r.PathValue("owner"), r.PathValue("repo"), r.PathValue("name"))
	if l == nil {
		notFound(w)
		return
	}
	writeJSON(w, l)
}

// createRepoLabel answers POST /repos/{o}/{r}/labels: 201 with the new
// label, or GitHub's 422 when the repository already defines one by that
// name (compared case-insensitively).
func (s *Server) createRepoLabel(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	var body struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if body.Name == "" || s.findRepoLabel(owner, repo, body.Name) != nil {
		unprocessable(w, "Validation Failed")
		return
	}
	s.nextLabelID++
	l := RepoLabel{ID: s.nextLabelID, Name: body.Name, Color: body.Color, Description: body.Description}
	key := repoKey(owner, repo)
	s.repoLabels[key] = append(s.repoLabels[key], l)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, l)
}

// SeedRepoLabel defines a label on owner/repo, as a human creating it in the
// UI does.
func (s *Server) SeedRepoLabel(owner, repo, name, color string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findRepoLabel(owner, repo, name) != nil {
		return
	}
	s.nextLabelID++
	key := repoKey(owner, repo)
	s.repoLabels[key] = append(s.repoLabels[key], RepoLabel{ID: s.nextLabelID, Name: name, Color: color})
}

// RepoLabels returns the labels owner/repo defines, by name.
func (s *Server) RepoLabels(owner, repo string) []RepoLabel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.repoLabels[repoKey(owner, repo)])
	slices.SortFunc(out, func(a, b RepoLabel) int { return strings.Compare(a.Name, b.Name) })
	return out
}
