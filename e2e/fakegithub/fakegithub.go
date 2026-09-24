// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package fakegithub is an in-memory GitHub REST API good enough to run the
// patchy controllers against: code-scanning alerts, issues, labels,
// comments, issue events, reactions, search, the Git Data surface (refs,
// blobs, trees, commits) the API push uses, and the App endpoints (GET /app,
// installation tokens, collaborator permission). It exists so the e2e suite
// can drive the real binaries end to end with no network and no credentials.
//
// Where GitHub's behaviour was verified live (2026-09-24) the fake mirrors
// it exactly: conditional list requests (ETag, 304), the Git refs 422
// messages, event actors (label events carry no performed_via_github_app),
// and the public-repository permission answers ("read" for anyone, "none"
// for the App's bot, 404 for a nonexistent login). Issue numbers, refs and
// permissions are global to the fake, not per repository.
//
// Credentials matter as they do on GitHub: an installation token the fake
// minted is held to the repositories and permissions it was minted with
// (403 "Resource not accessible by integration" outside them), and its
// writes are the App's bot's; a PAT's writes are PATUser's. The listings
// order as GitHub's do (sort, direction), and an issue's updated_at moves
// with every change to it.
package fakegithub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fake App's identity: every API write made with an installation token
// is made as its bot user, the actor GitHub records on everything an
// installation token does (a PAT's writes are PATUser's).
const (
	AppSlug   = "patchy"
	BotLogin  = AppSlug + "[bot]"
	BotUserID = int64(100000001)
)

// Bot is the fake App's bot user.
var Bot = Actor{Login: BotLogin, ID: BotUserID, Type: "Bot"}

// Issue is the fake's issue record.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []label   `json:"labels"`
	Assignees []Actor   `json:"assignees"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt moves with every change to the issue — its state, labels,
	// assignees, body, or a new comment — as GitHub's does, so the listing
	// sorted by it and its ETag move too.
	UpdatedAt time.Time `json:"updated_at"`
	// Comments is the issue's comment count.
	Comments int `json:"comments"`
	// RepositoryURL lets the client recover owner/name from search results.
	RepositoryURL string `json:"repository_url"`
	// HTMLURL is the issue's page.
	HTMLURL string `json:"html_url,omitempty"`
	// StateReason is why a closed issue was closed ("completed",
	// "not_planned"), "" when no reason was given.
	StateReason string `json:"state_reason,omitempty"`
	// User opened the issue: patchy[bot], the login its comments carry too,
	// unless a test opened it as a human (OpenIssue).
	User Actor `json:"user"`
}

type label struct {
	Name string `json:"name"`
}

// Actor is a GitHub account as the API renders it.
type Actor struct {
	Login string `json:"login"`
	ID    int64  `json:"id,omitempty"`
	// Type is "User" or "Bot".
	Type string `json:"type,omitempty"`
}

// appRef is the performed_via_github_app object.
type appRef struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// viaApp is the fake App as performed_via_github_app renders it.
var viaApp = &appRef{ID: 1, Slug: AppSlug}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      Actor     `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ViaApp is set on the App's own comments — unlike label events,
	// comments do carry performed_via_github_app.
	ViaApp *appRef `json:"performed_via_github_app"`
}

// Server is the fake API.
type Server struct {
	*httptest.Server

	// externalURL is the address clients reach the fake at — stamped into
	// the absolute URLs it hands out (the tarball redirect).
	externalURL string

	mu            sync.Mutex
	issues        map[int]*Issue
	comments      map[int][]comment
	nextCommentID int64
	dismissed     map[int]string
	// alerts are the seeded code-scanning alerts the list endpoints serve —
	// the pre-existing estate a backfill walks. The get/update endpoints
	// keep fabricating alerts on the fly for webhook-driven flows.
	alerts []seededAlert
	// parents is the commit ancestry the compare endpoint answers from
	// (each commit mapped to its parent); compares counts its calls.
	parents  map[string]string
	compares int
	// moved are alerts a later analysis moved without a webhook (SetAlert):
	// their state and most recent instance, by number.
	moved map[int]movedAlert
	pulls map[int]*pull
	// pullReadFailures is how many single-PR reads still answer 502.
	pullReadFailures int
	git              gitData
	next             int
	// events are each issue's timeline, oldest first.
	events      map[int][]issueEvent
	nextEventID int64
	// reactions are each comment's reactions, by comment id.
	reactions      map[int64][]reaction
	nextReactionID int64
	// roles are explicit collaborator roles by lower-cased login; missing
	// are logins with no GitHub account. Everyone else reads as "read".
	roles   map[string]string
	missing map[string]bool
	// tokens are the installation-token requests answered, in order;
	// minted maps each token handed out to the scope it is held to.
	tokens []TokenRequest
	minted map[string]TokenRequest
	// Now stamps created_at; tests override it to age issues instantly.
	Now func() time.Time
}

func newState() (*Server, *http.ServeMux) {
	s := &Server{
		issues:    make(map[int]*Issue),
		comments:  make(map[int][]comment),
		dismissed: make(map[int]string),
		parents:   make(map[string]string),
		moved:     make(map[int]movedAlert),
		pulls:     make(map[int]*pull),
		git:       newGitData(),
		next:      100,
		events:    make(map[int][]issueEvent),
		reactions: make(map[int64][]reaction),
		roles:     make(map[string]string),
		missing:   make(map[string]bool),
		minted:    make(map[string]TokenRequest),
		Now:       time.Now,
	}
	mux := http.NewServeMux()
	s.routes(mux)
	return s, mux
}

// New starts the fake on an ephemeral localhost listener. The returned URL is
// what an Integration/Forge baseURL should point at.
func New() *Server {
	s, mux := newState()
	// go-github appends /api/v3 for a non-api.github.com base URL.
	s.Server = httptest.NewServer(http.StripPrefix("/api/v3", mux))
	s.externalURL = s.Server.URL
	return s
}

// NewStandalone builds the fake without starting a listener — the handler
// for a caller-owned http.Server (the `mise run fakegithub` dev server).
// externalURL is the address clients will reach that server at. The embedded
// httptest.Server stays nil: Close and URL are not available in this mode.
func NewStandalone(externalURL string) (*Server, http.Handler) {
	s, mux := newState()
	s.externalURL = externalURL
	return s, http.StripPrefix("/api/v3", mux)
}

// Issues returns a snapshot of every issue, ordered by number.
func (s *Server) Issues() []Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Issue, 0, len(s.issues))
	for _, is := range s.issues {
		out = append(out, *is)
	}
	slices.SortFunc(out, func(a, b Issue) int { return a.Number - b.Number })
	return out
}

// Comments returns an issue's comment bodies.
func (s *Server) Comments(number int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.comments[number]))
	for _, c := range s.comments[number] {
		out = append(out, c.Body)
	}
	return out
}

// Dismissed returns the alert numbers dismissed so far.
func (s *Server) Dismissed() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.dismissed))
	for n := range s.dismissed {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// LabelsOf returns one issue's label names.
func (s *Server) LabelsOf(number int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	is, ok := s.issues[number]
	if !ok {
		return nil
	}
	out := make([]string, len(is.Labels))
	for i, l := range is.Labels {
		out[i] = l.Name
	}
	return out
}

// SetIssueState sets an issue open or closed, as a human doing so on GitHub
// does; the matching issues delivery is the test's to send.
func (s *Server) SetIssueState(number int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is, ok := s.issues[number]; ok && is.State != state {
		is.State = state
		s.touch(is)
	}
}

// touch moves an issue's updated_at to now, never backwards (created_at
// carries sub-second precision, now does not). Callers hold s.mu.
func (s *Server) touch(is *Issue) {
	if now := s.now(); now.After(is.UpdatedAt) {
		is.UpdatedAt = now
	}
}

// Age rewinds every issue's created_at by d, so time-gated transitions (the
// accumulation window, the remediation minimum age) fire immediately.
func (s *Server) Age(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, is := range s.issues {
		is.CreatedAt = is.CreatedAt.Add(-d)
	}
}

// routes registers every endpoint, each repository route with the
// installation-token permission it needs (scoped). The App's own endpoints
// take its JWT, and search and the tarball download hold no repository
// permission, so those are registered bare.
func (s *Server) routes(mux *http.ServeMux) {
	handle := func(pattern, perm string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.scoped(perm, h)) }
	handle("GET /repos/{owner}/{repo}/code-scanning/alerts/{number}", permSecurityEvents, s.getAlert)
	handle("PATCH /repos/{owner}/{repo}/code-scanning/alerts/{number}", permSecurityEvents, s.updateAlert)
	handle("GET /repos/{owner}/{repo}/code-scanning/alerts", permSecurityEvents, s.listRepoAlerts)
	handle("GET /orgs/{org}/code-scanning/alerts", permSecurityEvents, s.listOrgAlerts)
	handle("GET /repos/{owner}/{repo}/issues", permIssues, s.listIssues)
	handle("POST /repos/{owner}/{repo}/issues", permIssues, s.createIssue)
	handle("GET /repos/{owner}/{repo}/issues/{number}", permIssues, s.getIssue)
	handle("PATCH /repos/{owner}/{repo}/issues/{number}", permIssues, s.editIssue)
	// GET issues/{number}/comments, issues/{number}/events and
	// issues/comments/{id} overlap as mux patterns; one handler splits them.
	handle("GET /repos/{owner}/{repo}/issues/{number}/{sub}", permIssues, s.getIssueSub)
	handle("POST /repos/{owner}/{repo}/issues/{number}/comments", permIssues, s.createComment)
	handle("PATCH /repos/{owner}/{repo}/issues/comments/{id}", permIssues, s.editComment)
	handle("POST /repos/{owner}/{repo}/issues/comments/{id}/reactions", permIssues, s.createReaction)
	handle("POST /repos/{owner}/{repo}/issues/{number}/labels", permIssues, s.addLabels)
	handle("DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}", permIssues, s.removeLabel)
	handle("POST /repos/{owner}/{repo}/issues/{number}/assignees", permIssues, s.addAssignees)
	handle("GET /repos/{owner}/{repo}/collaborators/{login}/permission", permMetadata, s.permission)
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", s.installation)
	mux.HandleFunc("GET /app", s.getApp)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.accessToken)
	handle("GET /repos/{owner}/{repo}", permMetadata, s.getRepo)
	handle("GET /repos/{owner}/{repo}/compare/{spec}", permContents, s.compare)
	handle("GET /repos/{owner}/{repo}/pulls", permPullRequests, s.listPulls)
	handle("GET /repos/{owner}/{repo}/pulls/{number}", permPullRequests, s.getPull)
	handle("POST /repos/{owner}/{repo}/pulls", permPullRequests, s.createPull)
	handle("GET /repos/{owner}/{repo}/tarball/{ref...}", permContents, s.tarballRedirect)
	mux.HandleFunc("GET /_tarball/{owner}/{repo}/{ref...}", s.tarball)
	mux.HandleFunc("GET /search/issues", s.searchIssues)
	s.gitRoutes(handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, fmt.Sprintf("fakegithub: unhandled %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	})
}

func (s *Server) getAlert(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	// The state has to reflect the dismissal log: reopening an alert reads
	// its state first, and only a dismissed alert is reopened.
	s.mu.Lock()
	_, isDismissed := s.dismissed[number]
	moved, isMoved := s.moved[number]
	s.mu.Unlock()
	state := "open"
	if isMoved {
		state = moved.state
	}
	if isDismissed {
		state = "dismissed"
	}
	body := alertBody(r.PathValue("owner"), r.PathValue("repo"), number, state, false)
	if isMoved {
		inst := body["most_recent_instance"].(map[string]any)
		inst["ref"], inst["commit_sha"] = moved.ref, moved.commit
	}
	writeJSON(w, body)
}

// movedAlert is an alert's state and most recent instance after SetAlert.
type movedAlert struct {
	state, ref, commit string
}

// SetAlert moves an alert the way a later analysis does, without a webhook
// — GitHub sends code_scanning_alert events only when an alert's state
// changes, so an analysis that finds an open alert again only moves its
// most recent instance: the get endpoint reports state, and the latest
// instance on ref at commit.
func (s *Server) SetAlert(number int, state, ref, commit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moved[number] = movedAlert{state: state, ref: ref, commit: commit}
}

// seededAlert is one pre-existing code-scanning alert the list endpoints
// serve.
type seededAlert struct {
	owner, repo string
	number      int
}

// SeedAlert records a pre-existing open code-scanning alert, so a backfill
// list walk can find alerts no webhook ever delivered.
func (s *Server) SeedAlert(owner, repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, seededAlert{owner: owner, repo: repo, number: number})
}

// alertBody is the JSON shape both the get and list alert endpoints serve.
func alertBody(owner, repo string, number int, state string, embedRepo bool) map[string]any {
	body := map[string]any{
		"number":   number,
		"state":    state,
		"html_url": fmt.Sprintf("https://github.com/%s/%s/security/code-scanning/%d", owner, repo, number),
		"rule": map[string]any{
			"id":                      "js/reflected-xss",
			"name":                    "js/reflected-xss",
			"description":             "Reflected cross-site scripting",
			"help":                    "Escape user input before writing it to the page.",
			"severity":                "error",
			"security_severity_level": "high",
			"tags":                    []string{"security", "external/cwe/cwe-079"},
		},
		"most_recent_instance": map[string]any{
			"commit_sha": "abc123",
			"message":    map[string]any{"text": "user input flows to a sink"},
			"location": map[string]any{
				"path": "src/render.js", "start_line": 42, "end_line": 44,
			},
		},
	}
	if embedRepo {
		body["repository"] = map[string]any{
			"name":  repo,
			"owner": map[string]any{"login": owner},
		}
	}
	return body
}

// listAlerts serves the seeded alerts matching keep, honouring the state
// filter (default open; a dismissed alert leaves the open listing). One
// page — the fake never sets a Link header, so clients stop after it.
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, keep func(seededAlert) bool, embedRepo bool) {
	wantState := r.URL.Query().Get("state")
	if wantState == "" {
		wantState = "open"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, a := range s.alerts {
		if !keep(a) {
			continue
		}
		state := "open"
		if _, isDismissed := s.dismissed[a.number]; isDismissed {
			state = "dismissed"
		}
		if state != wantState {
			continue
		}
		out = append(out, alertBody(a.owner, a.repo, a.number, state, embedRepo))
	}
	writeJSON(w, out)
}

func (s *Server) listRepoAlerts(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	s.listAlerts(w, r, func(a seededAlert) bool {
		return a.owner == owner && a.repo == repo
	}, false)
}

func (s *Server) listOrgAlerts(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	s.listAlerts(w, r, func(a seededAlert) bool { return a.owner == org }, true)
}

func (s *Server) updateAlert(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var body struct {
		State           string `json:"state"`
		DismissedReason string `json:"dismissed_reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	switch body.State {
	case "dismissed":
		s.dismissed[number] = body.DismissedReason
	case "open":
		delete(s.dismissed, number) // the demo reset undoing a dismissal
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"number": number, "state": body.State})
}

// listIssues answers GET /repos/{o}/{r}/issues: the issues carrying every
// label in the filter, in the requested state (GitHub's default, open;
// "closed"; "all"), ordered as GitHub orders them — sort "created" (the
// default), "updated" or "comments", direction "desc" (the default) or
// "asc", ties broken by number the same way. The listing is ETag-tagged and
// a matching If-None-Match answers 304, as GitHub does; since it renders
// each issue's updated_at and comment count, any change to a listed issue
// changes the tag.
func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	want := splitLabels(q.Get("labels"))
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := []*Issue{}
	for _, is := range s.issues {
		if (state == "all" || is.State == state) && hasLabels(is, want) {
			out = append(out, is)
		}
	}
	slices.SortFunc(out, issueOrder(q.Get("sort"), q.Get("direction")))
	writeJSONTagged(w, r, out)
}

// issueOrder is the issue listing's comparison for GitHub's sort and
// direction parameters; an unknown sort falls back to created.
func issueOrder(sort, direction string) func(a, b *Issue) int {
	key := func(a, b *Issue) int { return a.CreatedAt.Compare(b.CreatedAt) }
	switch sort {
	case "updated":
		key = func(a, b *Issue) int { return a.UpdatedAt.Compare(b.UpdatedAt) }
	case "comments":
		key = func(a, b *Issue) int { return a.Comments - b.Comments }
	}
	sign := -1 // desc, GitHub's default
	if direction == "asc" {
		sign = 1
	}
	return func(a, b *Issue) int {
		if c := key(a, b); c != 0 {
			return sign * c
		}
		return sign * (a.Number - b.Number)
	}
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := s.caller(r)
	s.mu.Lock()
	is := s.openIssue(owner, repo, body.Title, body.Body, body.Labels, actor)
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, is)
}

// openIssue records a new open issue opened by author, each initial label a
// labeled event by author (an issue form applies its labels as the
// opener). Callers hold s.mu.
func (s *Server) openIssue(owner, repo, title, body string, labels []string, author Actor) *Issue {
	s.next++
	now := s.Now()
	is := &Issue{
		Number: s.next, Title: title, Body: body, State: "open",
		CreatedAt:     now,
		UpdatedAt:     now,
		RepositoryURL: fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo),
		HTMLURL:       fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, s.next),
		User:          author,
	}
	s.issues[is.Number] = is
	for _, l := range labels {
		s.label(is, l, author)
	}
	return is
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	s.mu.Lock()
	is, ok := s.issues[number]
	var out Issue
	if ok {
		out = *is // copied under the lock: SetIssueState and Age write it
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, &out)
}

func (s *Server) editIssue(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var body struct {
		Body        *string `json:"body"`
		State       *string `json:"state"`
		StateReason *string `json:"state_reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := s.caller(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	is, ok := s.issues[number]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if body.Body != nil && *body.Body != is.Body {
		is.Body = *body.Body
		s.touch(is)
	}
	if body.State != nil {
		reason := ""
		if body.StateReason != nil {
			reason = *body.StateReason
		}
		s.setState(is, *body.State, reason, actor)
	}
	writeJSON(w, is)
}

// setState opens or closes an issue as actor, recording the closed or
// reopened event when the state changes. Callers hold s.mu.
func (s *Server) setState(is *Issue, state, reason string, actor Actor) {
	if is.State == state {
		return
	}
	is.State = state
	is.StateReason = reason
	s.touch(is)
	switch state {
	case "closed":
		s.recordEvent(is.Number, "closed", "", actor)
	case "open":
		is.StateReason = ""
		s.recordEvent(is.Number, "reopened", "", actor)
	}
}

// listComments answers GET /repos/{o}/{r}/issues/{number}/comments: oldest
// first, filtered by since (RFC 3339) against each comment's last update,
// ETag-tagged like every list.
func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			unprocessable(w, "Validation Failed")
			return
		}
		since = t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []comment{}
	for _, c := range s.comments[number] {
		if !c.UpdatedAt.Before(since) {
			out = append(out, c)
		}
	}
	writeJSONTagged(w, r, out)
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := s.caller(r)
	s.mu.Lock()
	c := s.addComment(number, body.Body, actor)
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, c)
}

// addComment records a comment by author; the App's own carry
// performed_via_github_app. A comment counts toward the issue's comments and
// moves its updated_at. Callers hold s.mu.
func (s *Server) addComment(number int, body string, author Actor) comment {
	s.nextCommentID++
	now := s.now()
	c := comment{ID: s.nextCommentID, Body: body, User: author, CreatedAt: now, UpdatedAt: now}
	if author == Bot {
		c.ViaApp = viaApp
	}
	s.comments[number] = append(s.comments[number], c)
	if is, ok := s.issues[number]; ok {
		is.Comments++
		s.touch(is)
	}
	return c
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.findComment(id)
	if c == nil {
		http.NotFound(w, r)
		return
	}
	c.Body, c.UpdatedAt = body.Body, s.now()
	writeJSON(w, c)
}

// findComment returns the stored comment with id, or nil. Callers hold s.mu.
func (s *Server) findComment(id int64) *comment {
	for number, cs := range s.comments {
		for i := range cs {
			if cs[i].ID == id {
				return &s.comments[number][i]
			}
		}
	}
	return nil
}

func (s *Server) addLabels(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	names, err := decodeLabels(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	actor := s.caller(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	is, ok := s.issues[number]
	if !ok {
		http.NotFound(w, r)
		return
	}
	for _, name := range names {
		s.label(is, name, actor)
	}
	writeJSON(w, is.Labels)
}

// label applies a label as actor, recording the labeled event; a label the
// issue already carries is left alone and records nothing. Callers hold
// s.mu.
func (s *Server) label(is *Issue, name string, actor Actor) {
	if slices.ContainsFunc(is.Labels, func(l label) bool { return l.Name == name }) {
		return
	}
	is.Labels = append(is.Labels, label{Name: name})
	s.touch(is)
	s.recordEvent(is.Number, "labeled", name, actor)
}

// unlabel removes a label as actor, recording the unlabeled event; it
// reports false when the issue did not carry it. Callers hold s.mu.
func (s *Server) unlabel(is *Issue, name string, actor Actor) bool {
	before := len(is.Labels)
	is.Labels = slices.DeleteFunc(is.Labels, func(l label) bool { return l.Name == name })
	if len(is.Labels) == before {
		return false
	}
	s.touch(is)
	s.recordEvent(is.Number, "unlabeled", name, actor)
	return true
}

func (s *Server) removeLabel(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	name := r.PathValue("name")

	actor := s.caller(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	is, ok := s.issues[number]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.unlabel(is, name, actor) {
		// GitHub 404s an absent label; the client treats that as success.
		http.NotFound(w, r)
		return
	}
	writeJSON(w, is.Labels)
}

func (s *Server) addAssignees(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	var body struct {
		Assignees []string `json:"assignees"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	is, ok := s.issues[number]
	if !ok {
		http.NotFound(w, r)
		return
	}
	for _, login := range body.Assignees {
		is.Assignees = append(is.Assignees, Actor{Login: login})
	}
	if len(body.Assignees) > 0 {
		s.touch(is)
	}
	writeJSON(w, is)
}

func (s *Server) getRepo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"default_branch": "main"})
}

var labelQualifier = regexp.MustCompile(`label:"([^"]+)"`)

func (s *Server) searchIssues(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	var want []string
	for _, m := range labelQualifier.FindAllStringSubmatch(query, -1) {
		want = append(want, m[1])
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	items := []*Issue{}
	for _, is := range s.issues {
		if is.State == "open" && hasLabels(is, want) {
			items = append(items, is)
		}
	}
	slices.SortFunc(items, func(a, b *Issue) int { return a.Number - b.Number })
	writeJSON(w, map[string]any{"total_count": len(items), "items": items})
}

// decodeLabels accepts both shapes GitHub's API documents for the add-labels
// endpoint: a bare array of names (what go-github sends) and an object with
// a "labels" key.
func decodeLabels(r *http.Request) ([]string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return names, nil
	}
	var wrapped struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("labels payload is neither an array nor {labels: [...]}: %w", err)
	}
	return wrapped.Labels, nil
}

func hasLabels(is *Issue, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(is.Labels, func(l label) bool { return l.Name == w }) {
			return false
		}
	}
	return true
}

func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
