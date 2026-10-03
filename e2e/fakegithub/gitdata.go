// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func decodeJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// BaseSHA is the fixed head of every branch the fake has not been pushed to;
// controllers resolving a clone base get this.
const BaseSHA = "00000000000000000000000000000000000000ba"

// gitData is the Git Data API surface: enough blob/tree/commit/ref state for
// the remediation-controller's API push (internal/ghpush) to run against the
// fake, and for tests to read back what was pushed.
type gitData struct {
	blobs   map[string][]byte            // blob sha -> raw content
	trees   map[string]map[string]string // tree sha -> path -> blob sha ("" = delete)
	commits map[string]commitRec
	// refs are each repository's own refs, by repoKey and then
	// "heads/<branch>": a branch pushed to one repository is in no other,
	// as on GitHub, so two repositories each have their own
	// patchy-intent/<intent>. shared are the refs a test pointed with
	// SetBranch, which every repository sees until it has its own.
	refs   map[string]map[string]string
	shared map[string]string
	next   int
	// writes are every ref create and update asked for, refused ones
	// included, in order, each with the repository it was asked of.
	writes []refWrite
}

// refWrite is a RefWrite and the repository (a repoKey) it was asked of.
type refWrite struct {
	repo string
	RefWrite
}

type commitRec struct {
	Message string
	Tree    string
	Parents []string
}

// RefWrite is one request to create or move a ref, as the fake answered it:
// Op is "create" (POST git/refs) or "update" (PATCH git/refs, with Force as
// asked), Ref is "heads/<branch>", and Status the HTTP status answered.
type RefWrite struct {
	Op     string
	Ref    string
	SHA    string
	Force  bool
	Status int
}

// RefWrites returns every ref create and update asked for so far, refused
// ones included, in order, in every repository: what a test reads to prove
// a branch was never forced.
func (s *Server) RefWrites() []RefWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RefWrite, 0, len(s.git.writes))
	for _, w := range s.git.writes {
		out = append(out, w.RefWrite)
	}
	return out
}

// RepoRefWrites is RefWrites in owner/repo alone.
func (s *Server) RepoRefWrites(owner, repo string) []RefWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, repo)
	var out []RefWrite
	for _, w := range s.git.writes {
		if w.repo == key {
			out = append(out, w.RefWrite)
		}
	}
	return out
}

// Commit is a snapshot of one pushed commit: its message, its parents, and
// the files its tree carries (a deleted path maps to nil).
type Commit struct {
	Message string
	Parents []string
	Files   map[string][]byte
}

// CommitOf returns the pushed commit sha, or false when the fake never
// received it.
func (s *Server) CommitOf(sha string) (Commit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.git.commits[sha]
	if !ok {
		return Commit{}, false
	}
	c := Commit{Message: rec.Message, Parents: append([]string(nil), rec.Parents...), Files: map[string][]byte{}}
	for path, blob := range s.git.trees[rec.Tree] {
		if blob == "" {
			c.Files[path] = nil
			continue
		}
		c.Files[path] = s.git.blobs[blob]
	}
	return c, true
}

func newGitData() gitData {
	return gitData{
		blobs:   map[string][]byte{},
		trees:   map[string]map[string]string{},
		commits: map[string]commitRec{},
		refs:    map[string]map[string]string{},
		shared:  map[string]string{},
	}
}

// refIn is where ref points in the repository repo (a repoKey): its own
// ref, else a shared one SetBranch pointed. Callers hold the server's mu.
func (g *gitData) refIn(repo, ref string) (string, bool) {
	if sha, ok := g.refs[repo][ref]; ok {
		return sha, true
	}
	sha, ok := g.shared[ref]
	return sha, ok
}

// setRef points ref in the repository repo (a repoKey) at sha. Callers
// hold the server's mu.
func (g *gitData) setRef(repo, ref, sha string) {
	if g.refs[repo] == nil {
		g.refs[repo] = map[string]string{}
	}
	g.refs[repo][ref] = sha
}

// owners counts the repositories that have ref as their own, and returns
// where it points in the last one counted. Callers hold the server's mu.
func (g *gitData) owners(ref string) (n int, sha string) {
	for _, refs := range g.refs {
		if at, ok := refs[ref]; ok {
			n, sha = n+1, at
		}
	}
	return n, sha
}

// anyRef is where ref points when no repository is named: in the one
// repository that has it as its own, else a shared one. ok is false when
// none has it, or when more than one repository has it as its own and the
// answer would depend on which. Callers hold the server's mu.
func (g *gitData) anyRef(ref string) (string, bool) {
	switch n, sha := g.owners(ref); {
	case n == 1:
		return sha, true
	case n > 1:
		return "", false
	}
	sha, ok := g.shared[ref]
	return sha, ok
}

func (g *gitData) sha(kind string) string {
	g.next++
	return fmt.Sprintf("%s%07d", strings.Repeat(kind[:1], 33), g.next)
}

// BranchHead returns the pushed head of "heads/<branch>" in the one
// repository it was pushed to, or BaseSHA if it was never pushed; "" when
// more than one repository has it (RepoBranchHead names which).
func (s *Server) BranchHead(branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, _ := s.git.owners("heads/" + branch); n > 1 {
		return ""
	}
	return s.headSHA("", branch)
}

// RepoBranchHead returns the head of "heads/<branch>" in owner/repo, or
// BaseSHA if it was never pushed there.
func (s *Server) RepoBranchHead(owner, repo, branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headSHA(repoKey(owner, repo), branch)
}

// BranchFiles returns the file contents committed to a pushed branch (deleted
// paths map to nil), plus the commit message; ok is false when the branch was
// never pushed, or when more than one repository has it.
func (s *Server) BranchFiles(branch string) (files map[string][]byte, message string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sha, ok := s.git.anyRef("heads/" + branch)
	if !ok {
		return nil, "", false
	}
	commit := s.git.commits[sha]
	files = map[string][]byte{}
	for path, blob := range s.git.trees[commit.Tree] {
		if blob == "" {
			files[path] = nil
			continue
		}
		files[path] = s.git.blobs[blob]
	}
	return files, commit.Message, true
}

// gitRoutes registers the Git Data endpoints, all under the contents
// permission.
func (s *Server) gitRoutes(handle func(pattern, perm string, h http.HandlerFunc)) {
	handle("GET /repos/{owner}/{repo}/git/ref/{ref...}", permContents, s.getRef)
	handle("POST /repos/{owner}/{repo}/git/blobs", permContents, s.createBlob)
	handle("POST /repos/{owner}/{repo}/git/trees", permContents, s.createTree)
	handle("POST /repos/{owner}/{repo}/git/commits", permContents, s.createCommit)
	handle("POST /repos/{owner}/{repo}/git/refs", permContents, s.createRef)
	handle("PATCH /repos/{owner}/{repo}/git/refs/{ref...}", permContents, s.updateRef)
}

func (s *Server) getRef(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	s.mu.Lock()
	sha, ok := s.git.refIn(repoKey(r.PathValue("owner"), r.PathValue("repo")), ref)
	s.mu.Unlock()
	if !ok {
		// Every un-pushed branch sits at the fixed base, except an intent
		// branch, which (as on GitHub) exists only once it is created:
		// intent-controller reads it before a build to find one an earlier
		// intent left behind.
		if !strings.HasPrefix(ref, "heads/") || strings.HasPrefix(ref, "heads/patchy-intent/") {
			http.NotFound(w, r)
			return
		}
		sha = BaseSHA
	}
	writeJSON(w, map[string]any{
		"ref":    "refs/" + ref,
		"object": map[string]any{"type": "commit", "sha": sha},
	})
}

func (s *Server) createBlob(w http.ResponseWriter, r *http.Request) {
	var body struct{ Content, Encoding string }
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := []byte(body.Content)
	if body.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw = decoded
	}
	s.mu.Lock()
	sha := s.git.sha("b")
	s.git.blobs[sha] = raw
	s.mu.Unlock()
	writeJSON(w, map[string]any{"sha": sha})
}

func (s *Server) createTree(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseTree string `json:"base_tree"`
		Tree     []struct {
			Path string  `json:"path"`
			Mode string  `json:"mode"`
			SHA  *string `json:"sha"`
		} `json:"tree"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sha := s.git.sha("t")
	entries := map[string]string{}
	for _, e := range body.Tree {
		if e.SHA == nil {
			entries[e.Path] = "" // "sha": null — a deletion
			continue
		}
		entries[e.Path] = *e.SHA
	}
	s.git.trees[sha] = entries
	s.mu.Unlock()
	writeJSON(w, map[string]any{"sha": sha})
}

func (s *Server) createCommit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sha := s.git.sha("c")
	s.git.commits[sha] = commitRec{Message: body.Message, Tree: body.Tree, Parents: body.Parents}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"sha": sha})
}

// The Git refs endpoints' 422 messages, verified live (2026-09-24): every
// refusal is a 422, told apart only by the message.
const (
	msgRefExists      = "Reference already exists"
	msgRefMissing     = "Reference does not exist"
	msgObjectMissing  = "Object does not exist"
	msgNotFastForward = "Update is not a fast forward"
)

// createRef answers POST /repos/{o}/{r}/git/refs as GitHub does: 201, or 422
// "Reference already exists" for an existing ref and "Object does not
// exist" for a commit the fake has never seen.
func (s *Server) createRef(w http.ResponseWriter, r *http.Request) {
	var body struct{ Ref, SHA string }
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ref := strings.TrimPrefix(body.Ref, "refs/")
	repo := repoKey(r.PathValue("owner"), r.PathValue("repo"))
	s.mu.Lock()
	defer s.mu.Unlock()
	write := refWrite{repo: repo,
		RefWrite: RefWrite{Op: "create", Ref: ref, SHA: body.SHA, Status: http.StatusUnprocessableEntity}}
	defer func() { s.git.writes = append(s.git.writes, write) }()
	if _, exists := s.git.refIn(repo, ref); exists {
		unprocessable(w, msgRefExists)
		return
	}
	if !s.knownCommit(body.SHA) {
		unprocessable(w, msgObjectMissing)
		return
	}
	s.git.setRef(repo, ref, body.SHA)
	write.Status = http.StatusCreated
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"ref": body.Ref, "object": map[string]any{"type": "commit", "sha": body.SHA}})
}

// updateRef answers PATCH /repos/{o}/{r}/git/refs/{ref} as GitHub does. A
// forced update moves the ref anywhere known. Without force: the same SHA
// is a no-op 200, a descendant a fast-forward 200, and anything else 422
// "Update is not a fast forward". A missing ref is 422 "Reference does not
// exist" and an unknown SHA 422 "Object does not exist".
func (s *Server) updateRef(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	var body struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	repo := repoKey(r.PathValue("owner"), r.PathValue("repo"))
	s.mu.Lock()
	defer s.mu.Unlock()
	write := refWrite{repo: repo, RefWrite: RefWrite{Op: "update", Ref: ref, SHA: body.SHA, Force: body.Force,
		Status: http.StatusUnprocessableEntity}}
	defer func() { s.git.writes = append(s.git.writes, write) }()
	current, exists := s.git.refIn(repo, ref)
	switch {
	case !exists:
		unprocessable(w, msgRefMissing)
		return
	case !s.knownCommit(body.SHA):
		unprocessable(w, msgObjectMissing)
		return
	case !body.Force && body.SHA != current && !s.descends(body.SHA, current):
		unprocessable(w, msgNotFastForward)
		return
	}
	s.git.setRef(repo, ref, body.SHA)
	write.Status = http.StatusOK
	writeJSON(w, map[string]any{"ref": "refs/" + ref, "object": map[string]any{"type": "commit", "sha": body.SHA}})
}

// knownCommit reports whether sha names a commit the fake knows: the fixed
// base, a pushed commit, a ref's target, or one named in SetParents.
// Callers hold s.mu.
func (s *Server) knownCommit(sha string) bool {
	if sha == BaseSHA {
		return true
	}
	if _, ok := s.git.commits[sha]; ok {
		return true
	}
	if _, ok := s.parents[sha]; ok {
		return true
	}
	for _, refs := range s.git.refs {
		for _, target := range refs {
			if target == sha {
				return true
			}
		}
	}
	for _, target := range s.git.shared {
		if target == sha {
			return true
		}
	}
	for _, parent := range s.parents {
		if parent == sha {
			return true
		}
	}
	return false
}

// descends reports whether ancestor is in descendant's history, following
// pushed commits' parents and the SetParents ancestry. Callers hold s.mu.
func (s *Server) descends(descendant, ancestor string) bool {
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		parents := append([]string{}, s.git.commits[c].Parents...)
		if p, ok := s.parents[c]; ok {
			parents = append(parents, p)
		}
		for _, p := range parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}
