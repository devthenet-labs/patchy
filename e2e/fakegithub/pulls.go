// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pull is the fake's pull-request record.
type pull struct {
	Number         int        `json:"number"`
	NodeID         string     `json:"node_id"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Head           ref        `json:"head"`
	Base           ref        `json:"base"`
	User           Actor      `json:"user"` // who opened it: the create's caller
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	// repository is "owner/repo" of a pull request opened through the API.
	repository string
}

// PullRequest is a snapshot of one pull request, for assertions.
type PullRequest struct {
	Number int
	// Repository is "owner/repo", empty for a pull request a test
	// fabricated with OpenPull or MergePull.
	Repository     string
	NodeID         string
	Title          string
	Body           string
	Head           string
	HeadSHA        string
	Base           string
	State          string
	Merged         bool
	MergeCommitSHA string
}

// pullNodeID is a pull request's global node id: GitHub never gives two
// pull requests the same one.
func pullNodeID(number int) string { return fmt.Sprintf("PR_fake%d", number) }

// headSHA is where a branch points now: its pushed head, or the fixed base
// for a branch never pushed. Callers hold s.mu.
func (s *Server) headSHA(branch string) string {
	if sha, ok := s.git.refs["heads/"+branch]; ok {
		return sha
	}
	return BaseSHA
}

// rendered is p as GitHub renders it now: an open pull request's head
// follows its branch. Callers hold s.mu.
func (s *Server) rendered(p *pull) pull {
	out := *p
	if out.State == "open" {
		out.Head.SHA = s.headSHA(out.Head.Ref)
	}
	return out
}

// MergePull records pull request number, from branch head, as merged into
// mergeCommitSHA — the state a merge leaves behind — creating it when the
// fake has not seen it (a test fabricating a PR patchy opened earlier).
func (s *Server) MergePull(number int, head, mergeCommitSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pulls[number]
	if !ok {
		p = &pull{Number: number, NodeID: pullNodeID(number), Head: ref{Ref: head}, Base: ref{Ref: "main"}}
		s.pulls[number] = p
	}
	p.Head.SHA = s.headSHA(p.Head.Ref)
	at := s.Now().UTC().Truncate(time.Second)
	p.State, p.Merged, p.MergedAt, p.MergeCommitSHA = "closed", true, &at, mergeCommitSHA
}

// OpenPull records pull request number, from branch head, as open — a PR
// patchy opened earlier, fabricated by a test that runs no job controllers.
func (s *Server) OpenPull(number int, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[number] = &pull{
		Number: number, NodeID: pullNodeID(number), State: "open", Head: ref{Ref: head}, Base: ref{Ref: "main"},
	}
}

// FailPullReads makes the next n reads of a single pull request answer 502,
// as GitHub does during an outage.
func (s *Server) FailPullReads(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pullReadFailures = n
}

// getPull answers GET /repos/{o}/{r}/pulls/{number}. Pulls are keyed by
// number alone, so a renamed repository's old name still reaches its pulls,
// as GitHub's redirect does.
func (s *Server) getPull(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if s.pullReadFailures > 0 {
		s.pullReadFailures--
		s.mu.Unlock()
		http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		return
	}
	p, ok := s.pulls[n]
	var out pull
	if ok {
		out = s.rendered(p)
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, &out)
}

// ref is a pull request's head or base: the branch, and for the head the
// commit it points at and the repository the branch lives in.
type ref struct {
	Ref  string   `json:"ref"`
	SHA  string   `json:"sha,omitempty"`
	Repo *refRepo `json:"repo,omitempty"`
}

// refRepo is the repository of a pull request's head.
type refRepo struct {
	FullName string `json:"full_name"`
}

// Pulls returns a snapshot of every pull request, ordered by number.
func (s *Server) Pulls() []struct {
	Number int
	Head   string
	State  string
} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]struct {
		Number int
		Head   string
		State  string
	}, 0, len(s.pulls))
	for _, p := range s.pulls {
		out = append(out, struct {
			Number int
			Head   string
			State  string
		}{p.Number, p.Head.Ref, p.State})
	}
	return out
}

// Pull returns a snapshot of pull request number, as GitHub renders it now.
func (s *Server) Pull(number int) (PullRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pulls[number]
	if !ok {
		return PullRequest{}, false
	}
	out := s.rendered(p)
	return PullRequest{
		Number: out.Number, Repository: out.repository, NodeID: out.NodeID, Title: out.Title, Body: out.Body,
		Head: out.Head.Ref, HeadSHA: out.Head.SHA, Base: out.Base.Ref, State: out.State, Merged: out.Merged,
		MergeCommitSHA: out.MergeCommitSHA,
	}, true
}

// createPull answers POST /repos/{o}/{r}/pulls: the pull request opened
// from its head branch as that branch stands, with its own node id.
func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	author := s.caller(r) // before s.mu: caller takes it
	var body struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.next++
	p := &pull{
		Number:     s.next,
		NodeID:     pullNodeID(s.next),
		HTMLURL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, s.next),
		State:      "open",
		Title:      body.Title,
		Body:       body.Body,
		Head:       ref{Ref: body.Head, Repo: &refRepo{FullName: owner + "/" + repo}},
		Base:       ref{Ref: body.Base},
		User:       author,
		repository: owner + "/" + repo,
	}
	s.pulls[p.Number] = p
	out := s.rendered(p)
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, &out)
}

// listPulls answers GET /repos/{o}/{r}/pulls — enough of the list API for
// FindPRByHead and FindOpenPR (state, head and base filters; head arrives as
// "owner:branch").
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	head := r.URL.Query().Get("head")
	base := r.URL.Query().Get("base")
	if _, branch, ok := strings.Cut(head, ":"); ok {
		head = branch
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pull, 0, len(s.pulls))
	for _, p := range s.pulls {
		if state != "" && state != "all" && p.State != state {
			continue
		}
		if head != "" && p.Head.Ref != head {
			continue
		}
		if base != "" && p.Base.Ref != base {
			continue
		}
		out = append(out, s.rendered(p))
	}
	writeJSON(w, out)
}
