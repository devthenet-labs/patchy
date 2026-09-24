// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	batchv1 "k8s.io/api/batch/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
)

const (
	testNS        = "patchy"
	intentRepoURL = "https://github.com/acme/intents"
	appRepoURL    = "https://github.com/acme/app"
	testBot       = "patchy[bot]"
	approver      = "peter"
	baseSHA       = "1111111111111111111111111111111111111111"
	repoImage     = "registry.example/app@sha256:" + "abababababababababababababababababababababababababababababababab"
)

// validPlan passes report.ParsePlan and names the Project's one repository.
const validPlan = `---
summary: "Add GET /version returning {sha, built} as JSON"
repositories:
  - "https://github.com/acme/app"
new_dependencies: []
questions: []
confidence: 0.8
estimated_max_turns: 40
estimated_token_budget: 200000
---

## Approach

Add a handler.
`

const buildReport = `---
success: true
summary: "Added GET /version"
tests:
  ran: true
  passed: true
  command: "go test ./..."
notes: []
---

Added the handler and its test.
`

// fakeClock is the reconcilers' and the fake GitHub's shared clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// errTransient is a failure a retry cures.
var errTransient = errors.New("transient failure")

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ghError is a GitHub API error with the status the ghclient predicates read.
func ghError(status int, msg string) error {
	return &github.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
}

type fakeIssue struct {
	number   int64
	title    string
	body     string
	state    string
	labels   []string
	events   []*ghclient.IssueEvent
	comments []*ghclient.Comment
}

type fakePR struct {
	pr   ghclient.PullRequest
	head string
	url  string
	body string
	// author opened it, base is what it merges into, headRepo is where its
	// head branch lives ("acme/app", or a fork).
	author, base, headRepo string
}

// view is the pull request as the list and create answers render it.
func (p *fakePR) view(n int64) *ghclient.PR {
	return &ghclient.PR{Number: int(n), HTMLURL: p.url, NodeID: p.pr.NodeID, HeadSHA: p.pr.HeadSHA,
		Author: p.author, Base: p.base, HeadRepo: p.headRepo}
}

// fakeGitHub is an in-memory GitHub over one intent repository and one app
// repository, recording every write. Errors queued in errs are returned by
// the named method, one per call.
type fakeGitHub struct {
	mu      sync.Mutex
	clock   *fakeClock
	nextID  int64
	version int

	bot       string
	perms     map[string]string // lower-cased login → permission; "404" answers ErrNoSuchUser
	remaining int
	// appRemaining, when set, is the rate budget of the app repository's
	// installation, a different one from the intent repository's.
	appRemaining *int

	labels        map[string]bool
	createdLabels []string
	issues        map[int64]*fakeIssue
	reactions     map[int64]int
	closes        map[int64][]string

	commits  []ghclient.CommitRequest
	branches map[string]string
	prs      map[int64]*fakePR
	heads    map[string]string

	resolveErr   error
	installedErr error
	errs         map[string][]error
	calls        map[string]int
	// sinces are the since of every comment listing, in order.
	sinces []time.Time
}

func newFakeGitHub(clock *fakeClock) *fakeGitHub {
	return &fakeGitHub{
		clock: clock, nextID: 1000, bot: testBot,
		perms:     map[string]string{approver: ghclient.PermissionAdmin},
		remaining: 5000,
		labels:    map[string]bool{},
		issues:    map[int64]*fakeIssue{},
		reactions: map[int64]int{},
		closes:    map[int64][]string{},
		branches:  map[string]string{},
		prs:       map[int64]*fakePR{},
		heads:     map[string]string{"main": baseSHA},
		errs:      map[string][]error{},
		calls:     map[string]int{},
	}
}

// call counts the method and returns its next queued error.
func (f *fakeGitHub) call(name string) error {
	f.calls[name]++
	if q := f.errs[name]; len(q) > 0 {
		f.errs[name] = q[1:]
		return q[0]
	}
	return nil
}

func (f *fakeGitHub) failNext(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = append(f.errs[method], errs...)
}

func (f *fakeGitHub) id() int64 {
	f.nextID++
	return f.nextID
}

func (f *fakeGitHub) issue(n int64) (*fakeIssue, error) {
	is, ok := f.issues[n]
	if !ok {
		return nil, ghError(http.StatusNotFound, "Not Found")
	}
	return is, nil
}

// ---- test drivers ----

// openIssue files issue n with the test Project's trigger label applied by
// actor.
func (f *fakeGitHub) openIssue(n int64, title, body, actor string) {
	f.mu.Lock()
	f.issues[n] = &fakeIssue{number: n, title: title, body: body, state: "open"}
	f.mu.Unlock()
	f.label(n, "patchy:target", actor)
}

// actorOf is the account behind login, its id derived from the login so
// every test account has its own.
func actorOf(login string) ghclient.Actor {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(login)))
	a := ghclient.Actor{Login: login, ID: int64(h.Sum32()) + 1, Type: "User"}
	if strings.HasSuffix(login, "[bot]") {
		a.Type = "Bot"
	}
	return a
}

// label records actor applying label to issue n now.
func (f *fakeGitHub) label(n int64, label, actor string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[n]
	id := f.id()
	is.events = append(is.events, &ghclient.IssueEvent{
		ID: id, Event: "labeled", CreatedAt: f.clock.Now(), Actor: actorOf(actor), Label: label,
	})
	if !slices.ContainsFunc(is.labels, func(l string) bool { return strings.EqualFold(l, label) }) {
		is.labels = append(is.labels, label)
	}
	f.version++
	return id
}

// removeTrigger records the approver removing the trigger label from issue
// 1, as they do before applying it again.
func (f *fakeGitHub) removeTrigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLabel(1, "patchy:target", approver)
}

func (f *fakeGitHub) removeLabel(n int64, label, actor string) {
	is := f.issues[n]
	before := len(is.labels)
	is.labels = slices.DeleteFunc(is.labels, func(l string) bool { return strings.EqualFold(l, label) })
	if len(is.labels) != before {
		is.events = append(is.events, &ghclient.IssueEvent{
			ID: f.id(), Event: "unlabeled", CreatedAt: f.clock.Now(), Actor: actorOf(actor), Label: label,
		})
		f.version++
	}
}

// comment records login commenting on issue 1 now.
func (f *fakeGitHub) comment(login, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addComment(1, login, body).ID
}

func (f *fakeGitHub) addComment(n int64, login, body string) *ghclient.Comment {
	is := f.issues[n]
	a := actorOf(login)
	c := &ghclient.Comment{
		ID: f.id(), Body: body, UserLogin: a.Login, UserID: a.ID, UserType: a.Type,
		CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		HTMLURL: fmt.Sprintf("%s/issues/%d#issuecomment", intentRepoURL, n),
	}
	is.comments = append(is.comments, c)
	f.version++
	return c
}

// editComment rewrites comment id, as a human editing it would.
func (f *fakeGitHub) editComment(id int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues {
		for _, c := range is.comments {
			if c.ID == id {
				c.Body, c.UpdatedAt = body, f.clock.Now()
			}
		}
	}
	f.version++
}

// deleteComment deletes comment id, as a human with write access may.
func (f *fakeGitHub) deleteComment(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues {
		is.comments = slices.DeleteFunc(is.comments, func(c *ghclient.Comment) bool { return c.ID == id })
	}
	f.version++
}

// humanClose closes issue n as a human would.
func (f *fakeGitHub) humanClose(n int64, actor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeIssue(n, actor)
}

func (f *fakeGitHub) closeIssue(n int64, actor string) {
	is := f.issues[n]
	if is.state != "closed" {
		is.state = "closed"
		is.events = append(is.events, &ghclient.IssueEvent{
			ID: f.id(), Event: "closed", CreatedAt: f.clock.Now(), Actor: actorOf(actor),
		})
		f.version++
	}
}

// ownComments are the comments patchy posted on issue 1, oldest first.
func (f *fakeGitHub) ownComments() []*ghclient.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ghclient.Comment
	for _, c := range f.issues[1].comments {
		if c.UserLogin == f.bot {
			out = append(out, c)
		}
	}
	return out
}

// withMarker are patchy's comments on issue 1 whose marker contains key.
func (f *fakeGitHub) withMarker(key string) []*ghclient.Comment {
	var out []*ghclient.Comment
	for _, c := range f.ownComments() {
		if m := markerOf(c.Body); m != "" && strings.Contains(m, key) {
			out = append(out, c)
		}
	}
	return out
}

// hasLabel reports whether issue 1 carries label.
func (f *fakeGitHub) hasLabel(label string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.issues[1].labels, func(l string) bool { return strings.EqualFold(l, label) })
}

// state is issue 1's state.
func (f *fakeGitHub) state() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[1].state
}

// closePR closes pull request 1, merged or not.
func (f *fakeGitHub) closePR(merged bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.prs[1]
	pr.pr.State = "closed"
	if merged {
		pr.pr.Merged = true
		pr.pr.MergedAt = f.clock.Now()
		pr.pr.MergeCommitSHA = "2222222222222222222222222222222222222222"
	}
}

// ---- GitHub ----

func (f *fakeGitHub) Resolve(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Resolve"); err != nil {
		return err
	}
	return f.resolveErr
}

func (f *fakeGitHub) Installed(context.Context, string, ghclient.TokenPerms) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Installed"); err != nil {
		return err
	}
	return f.installedErr
}

func (f *fakeGitHub) BotLogin(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bot, f.call("BotLogin")
}

func (f *fakeGitHub) Permission(_ context.Context, _, login string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Permission"); err != nil {
		return "", err
	}
	perm, ok := f.perms[strings.ToLower(login)]
	switch {
	case !ok:
		return ghclient.PermissionRead, nil
	case perm == "404":
		return "", fmt.Errorf("permission: %w", ghclient.ErrNoSuchUser)
	}
	return perm, nil
}

func (f *fakeGitHub) RateRemaining(_ context.Context, repoURL string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appRemaining != nil && sameRepo(repoURL, appRepoURL) {
		return *f.appRemaining, f.call("RateRemaining")
	}
	return f.remaining, f.call("RateRemaining")
}

func (f *fakeGitHub) EnsureLabel(_ context.Context, _, name, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("EnsureLabel"); err != nil {
		return err
	}
	if !f.labels[strings.ToLower(name)] {
		f.labels[strings.ToLower(name)] = true
		f.createdLabels = append(f.createdLabels, name)
	}
	return nil
}

func (f *fakeGitHub) ListIssues(_ context.Context, _ string, labels []string, etag string) (
	*ghclient.IssueList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListIssues"); err != nil {
		return nil, err
	}
	tag := fmt.Sprintf(`"v%d"`, f.version)
	if etag == tag {
		return &ghclient.IssueList{ETag: tag, NotModified: true}, nil
	}
	var out []*ghclient.Issue
	for _, is := range f.issues {
		if is.state != "open" {
			continue
		}
		all := true
		for _, l := range labels {
			all = all && slices.ContainsFunc(is.labels, func(x string) bool { return strings.EqualFold(x, l) })
		}
		if all {
			out = append(out, f.view(is))
		}
	}
	slices.SortFunc(out, func(a, b *ghclient.Issue) int { return a.Number - b.Number })
	return &ghclient.IssueList{Issues: out, ETag: tag}, nil
}

func (f *fakeGitHub) view(is *fakeIssue) *ghclient.Issue {
	return &ghclient.Issue{
		Number: int(is.number), Title: is.title, Body: is.body, State: is.state,
		Labels: slices.Clone(is.labels), HTMLURL: fmt.Sprintf("%s/issues/%d", intentRepoURL, is.number),
	}
}

func (f *fakeGitHub) GetIssue(_ context.Context, _ string, number int64) (*ghclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetIssue"); err != nil {
		return nil, err
	}
	is, err := f.issue(number)
	if err != nil {
		return nil, err
	}
	return f.view(is), nil
}

func (f *fakeGitHub) ListIssueEvents(_ context.Context, _ string, number int64) ([]*ghclient.IssueEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListIssueEvents"); err != nil {
		return nil, err
	}
	is, err := f.issue(number)
	if err != nil {
		return nil, err
	}
	out := make([]*ghclient.IssueEvent, len(is.events))
	for i, e := range is.events {
		c := *e
		out[i] = &c
	}
	return out, nil
}

func (f *fakeGitHub) ListIssueComments(_ context.Context, _ string, number int64, since time.Time) (
	[]*ghclient.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListIssueComments"); err != nil {
		return nil, err
	}
	f.sinces = append(f.sinces, since)
	is, err := f.issue(number)
	if err != nil {
		return nil, err
	}
	var out []*ghclient.Comment
	for _, c := range is.comments {
		if since.IsZero() || !c.UpdatedAt.Before(since) {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeGitHub) GetIssueComment(_ context.Context, _ string, id int64) (*ghclient.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetIssueComment"); err != nil {
		return nil, err
	}
	for _, is := range f.issues {
		for _, c := range is.comments {
			if c.ID == id {
				cp := *c
				return &cp, nil
			}
		}
	}
	return nil, ghError(http.StatusNotFound, "Not Found")
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, _ string, number int64, body string) (
	*ghclient.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateIssueComment"); err != nil {
		return nil, err
	}
	if _, err := f.issue(number); err != nil {
		return nil, err
	}
	c := f.addComment(number, f.bot, body)
	cp := *c
	return &cp, nil
}

func (f *fakeGitHub) EditIssueComment(_ context.Context, _ string, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("EditIssueComment"); err != nil {
		return err
	}
	for _, is := range f.issues {
		for _, c := range is.comments {
			if c.ID == id {
				c.Body, c.UpdatedAt = body, f.clock.Now()
				return nil
			}
		}
	}
	return ghError(http.StatusNotFound, "Not Found")
}

func (f *fakeGitHub) React(_ context.Context, _ string, commentID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("React"); err != nil {
		return err
	}
	f.reactions[commentID]++
	return nil
}

func (f *fakeGitHub) RemoveLabel(_ context.Context, _ string, number int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("RemoveLabel"); err != nil {
		return err
	}
	if _, err := f.issue(number); err != nil {
		return err
	}
	f.removeLabel(number, name, f.bot)
	return nil
}

func (f *fakeGitHub) CloseIssue(_ context.Context, _ string, number int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CloseIssue"); err != nil {
		return err
	}
	if _, err := f.issue(number); err != nil {
		return err
	}
	f.closes[number] = append(f.closes[number], reason)
	f.closeIssue(number, f.bot)
	return nil
}

func (f *fakeGitHub) DefaultBranch(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "main", f.call("DefaultBranch")
}

func (f *fakeGitHub) HeadSHA(_ context.Context, _, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("HeadSHA"); err != nil {
		return "", err
	}
	if sha, ok := f.heads[branch]; ok {
		return sha, nil
	}
	if sha, ok := f.branches[branch]; ok {
		return sha, nil
	}
	return "", ghError(http.StatusNotFound, "Not Found")
}

func (f *fakeGitHub) CreateCommit(_ context.Context, _ string, req ghclient.CommitRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateCommit"); err != nil {
		return "", err
	}
	f.commits = append(f.commits, req)
	return fmt.Sprintf("%040x", 0xc0ffee00+len(f.commits)), nil
}

func (f *fakeGitHub) CreateBranchRef(_ context.Context, _, branch, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateBranchRef"); err != nil {
		return err
	}
	if cur, ok := f.branches[branch]; ok && cur != sha {
		return fmt.Errorf("create branch %s: it is at %s: %w", branch, cur, ghclient.ErrBranchExists)
	}
	f.branches[branch] = sha
	return nil
}

func (f *fakeGitHub) FindPullRequest(_ context.Context, _, head, base string) (*ghclient.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("FindPullRequest"); err != nil {
		return nil, err
	}
	for n, pr := range f.prs {
		if pr.head == head && pr.base == base && pr.pr.State == "open" {
			return pr.view(n), nil
		}
	}
	return nil, nil
}

func (f *fakeGitHub) CreatePullRequest(_ context.Context, _ string, req ghclient.PRRequest) (*ghclient.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreatePullRequest"); err != nil {
		return nil, err
	}
	n := int64(len(f.prs) + 1)
	pr := &fakePR{
		head: req.Head, url: fmt.Sprintf("%s/pull/%d", appRepoURL, n), body: req.Body,
		pr: ghclient.PullRequest{Number: int(n), State: "open", NodeID: fmt.Sprintf("PR_%d", n),
			HeadSHA: f.branches[req.Head]},
		author: f.bot, base: req.Base, headRepo: "acme/app",
	}
	f.prs[n] = pr
	return pr.view(n), nil
}

func (f *fakeGitHub) GetPullRequest(_ context.Context, _ string, number int64) (*ghclient.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetPullRequest"); err != nil {
		return nil, err
	}
	pr, ok := f.prs[number]
	if !ok {
		return nil, ghError(http.StatusNotFound, "Not Found")
	}
	cp := pr.pr
	return &cp, nil
}

// ---- jobs ----

// fakeJobs stands in for the jobs client: it records each launch and serves
// each Job's status and output from what the test set.
type fakeJobs struct {
	mu        sync.Mutex
	specs     map[string]jobs.Spec
	envs      map[string]map[string]string
	deleted   []string
	status    map[string]jobs.Status
	defaultOn bool // report the default image even when the spec pinned one
	output    func(spec jobs.Spec) jobs.RunOutput
	createErr error
	// results counts the Result calls: each reads a Job's whole log.
	results int
	// gone are the Jobs that no longer exist (their TTL ran out).
	gone map[string]bool
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{specs: map[string]jobs.Spec{}, envs: map[string]map[string]string{},
		status: map[string]jobs.Status{}, output: defaultOutput}
}

func (j *fakeJobs) Create(_ context.Context, spec jobs.Spec, env map[string]string) (
	string, v1alpha1.RunnerImageRef, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.createErr != nil {
		return "", v1alpha1.RunnerImageRef{}, j.createErr
	}
	name := jobs.NameFor(spec.Finding, spec.Kind, int32(spec.Attempt))
	j.specs[name], j.envs[name] = spec, env
	ref := v1alpha1.RunnerImageRef{Image: "patchy/claude-agent-runner:test", Source: v1alpha1.RunnerImageSourceDefault}
	if spec.RunnerImage != "" && !j.defaultOn {
		ref = v1alpha1.RunnerImageRef{Image: spec.RunnerImage, Source: v1alpha1.RunnerImageSourceRepository}
	}
	return name, ref, nil
}

func (j *fakeJobs) Result(_ context.Context, name string) (jobs.RunOutput, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.results++
	if j.gone[name] {
		return jobs.RunOutput{}, kerrors.NewNotFound(batchv1.Resource("jobs"), name)
	}
	return j.output(j.specs[name]), nil
}

func (j *fakeJobs) Status(_ context.Context, name string) (jobs.Status, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.gone[name] {
		return jobs.Status{}, kerrors.NewNotFound(batchv1.Resource("jobs"), name)
	}
	if st, ok := j.status[name]; ok {
		return st, nil
	}
	return jobs.Status{Done: true, Succeeded: 1}, nil
}

func (j *fakeJobs) Delete(_ context.Context, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.deleted = append(j.deleted, name)
	return nil
}

// launched are the specs of the Jobs created, in no order.
func (j *fakeJobs) launched() []jobs.Spec {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]jobs.Spec, 0, len(j.specs))
	for _, s := range j.specs {
		out = append(out, s)
	}
	return out
}

// defaultOutput is a stage that ran well: the valid plan, or a build with a
// one-file changeset on the pinned base.
func defaultOutput(spec jobs.Spec) jobs.RunOutput {
	stage := envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
		Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.25}}
	if spec.Phase == "plan" {
		return planOutput(stage, validPlan)
	}
	return buildOutput(stage, spec.BaseSHA, "version.go")
}

// failingBuild plans well and fails every build with a runtime error.
func failingBuild(spec jobs.Spec) jobs.RunOutput {
	if spec.Phase == "plan" {
		return defaultOutput(spec)
	}
	return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
		Remediation: &envelope.Remediation{Stage: envelope.Stage{Outcome: envelope.OutcomeRuntimeError,
			Detail: "the CLI crashed", Usage: envelope.Usage{CostUSD: 0.1}}}}}}
}

func planOutput(stage envelope.Stage, report string) jobs.RunOutput {
	return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypePlan,
		Plan: &envelope.Plan{Stage: stage, ReportMarkdown: report}}}}
}

func buildOutput(stage envelope.Stage, base, path string) jobs.RunOutput {
	return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
		Remediation: &envelope.Remediation{Stage: stage, ReportMarkdown: buildReport, Success: true,
			Changeset: &envelope.Changeset{BaseSHA: base, CommitMessage: "agent's message",
				Upserts: []envelope.FileChange{{Path: path, Mode: "100644",
					ContentB64: base64.StdEncoding.EncodeToString([]byte("package main\n"))}}}}}}}
}

// ---- environment ----

// env wires the four reconcilers over one fake cluster, one fake GitHub,
// fake Jobs and a shared clock.
type env struct {
	t       *testing.T
	c       client.Client
	clock   *fakeClock
	gh      *fakeGitHub
	jobs    *fakeJobs
	nudger  *Nudger
	project *ProjectReconciler
	intent  *IntentReconciler
	runs    *RunReconciler
	ttl     *TTLReconciler
	uid     int
	// failStatus fails the next Intent status writes, one per entry.
	failStatus []error
	// failStatusIf fails the first Intent status write it matches, once.
	failStatusIf func(*v1alpha1.Intent) bool
	failed       int
	// failEvery fails every failEvery-th Intent status write (0: none), and
	// failRunEvery every failRunEvery-th IntentRun status write.
	failEvery    int
	writes       int
	failRunEvery int
	runWrites    int
	// failRepoDeletes fails the next Repository deletes, one per count.
	failRepoDeletes int
	// refuseUsage fails every Intent status write that changes the usage.
	refuseUsage bool
}

// intentStatusSchema refuses what the Intent CRD's status schema refuses of
// the usage: every total has minimum 0.
func intentStatusSchema(in *v1alpha1.Intent) error {
	u := in.Status.Usage
	var errs field.ErrorList
	path := field.NewPath("status", "usage")
	for name, v := range map[string]int64{
		"costMicroUSD": u.CostMicroUSD, "inputTokens": u.InputTokens, "outputTokens": u.OutputTokens,
		"cacheReadTokens": u.CacheReadTokens, "cacheCreationTokens": u.CacheCreationTokens,
	} {
		if v < 0 {
			errs = append(errs, field.Invalid(path.Child(name), v, "should be greater than or equal to 0"))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return kerrors.NewInvalid(v1alpha1.GroupVersion.WithKind("Intent").GroupKind(), in.Name, errs)
}

func testSettings() Settings {
	return Settings{
		Namespace: testNS, AgentNamespace: "patchy-agents",
		PollInterval: time.Minute, ApprovalPollInterval: 30 * time.Second, PRPollInterval: time.Minute,
		RateLimitFloor: 1000, MaxAttempts: 2,
		Plan:  StageCeiling{MaxTurns: 40, TokenBudget: 200000, Timeout: 20 * time.Minute},
		Build: StageCeiling{MaxTurns: 150, TokenBudget: 800000, Timeout: time.Hour},
	}
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	e := &env{t: t, clock: &fakeClock{t: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}}
	e.gh = newFakeGitHub(e.clock)
	e.jobs = newFakeJobs()
	e.nudger = NewNudger()
	e.c = fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Project{}, &v1alpha1.Intent{}, &v1alpha1.IntentRun{},
			&v1alpha1.Repository{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					e.uid++
					obj.SetUID(types.UID(fmt.Sprintf("uid-%d", e.uid)))
				}
				if ts := obj.GetCreationTimestamp(); ts.IsZero() {
					obj.SetCreationTimestamp(metav1.NewTime(e.clock.Now()))
				}
				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*v1alpha1.Repository); ok && e.failRepoDeletes > 0 {
					e.failRepoDeletes--
					return errTransient
				}
				return c.Delete(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if in, ok := obj.(*v1alpha1.Intent); ok {
					// The API server holds the status to its schema, which
					// the fake client does not know.
					if err := intentStatusSchema(in); err != nil {
						return err
					}
					if e.refuseUsage {
						var stored v1alpha1.Intent
						if err := c.Get(ctx, client.ObjectKeyFromObject(in), &stored); err == nil &&
							stored.Status.Usage != in.Status.Usage {
							return kerrors.NewInternalError(errors.New("the usage write is refused"))
						}
					}
					if len(e.failStatus) > 0 {
						err := e.failStatus[0]
						e.failStatus = e.failStatus[1:]
						return err
					}
					if e.failStatusIf != nil && e.failStatusIf(in) {
						e.failStatusIf = nil
						e.failed++
						return errTransient
					}
					e.writes++
					if e.failEvery > 0 && e.writes%e.failEvery == 0 {
						e.failed++
						return errTransient
					}
				}
				if _, ok := obj.(*v1alpha1.IntentRun); ok && e.failRunEvery > 0 {
					e.runWrites++
					if e.runWrites%e.failRunEvery == 0 {
						e.failed++
						return errTransient
					}
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	set := testSettings()
	images := runnerguard.Guard{Enabled: true, Breaker: runnerguard.NewBreaker("intent-test", nil)}
	e.project = &ProjectReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: set, Nudger: e.nudger,
		Now: e.clock.Now}
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: set, Images: images,
		Nudger: e.nudger, Now: e.clock.Now}
	e.runs = &RunReconciler{Client: e.c, APIReader: e.c, Jobs: e.jobs, GitHub: e.gh, Settings: set,
		MaxConcurrent: 1, Harness: "claude", PlanModel: "anthropic/claude-sonnet-5",
		BuildModel: "anthropic/claude-opus-5", Images: images, Now: e.clock.Now}
	e.ttl = &TTLReconciler{Client: e.c, APIReader: e.c, TTL: DefaultTTL, Now: e.clock.Now}
	return e
}

// testProject is a Ready-to-validate Project over the fake repositories.
func testProject() *v1alpha1.Project {
	return &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: testNS, Generation: 1, UID: "project-uid"},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: intentRepoURL,
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: appRepoURL}},
		},
	}
}

func req(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: name}}
}

// reconcileProject runs the project reconciler once over the test Project,
// failing on an error.
func (e *env) reconcileProject() {
	e.t.Helper()
	if _, err := e.project.Reconcile(context.Background(), req("target")); err != nil {
		e.t.Fatalf("project reconcile: %v", err)
	}
}

// reconcileIntent runs the intent reconciler once and returns its error.
func (e *env) reconcileIntent(name string) error {
	_, err := e.intent.Reconcile(context.Background(), req(name))
	return err
}

// mustIntent runs the intent reconciler once, failing on an error.
func (e *env) mustIntent(name string) {
	e.t.Helper()
	if err := e.reconcileIntent(name); err != nil {
		e.t.Fatalf("intent reconcile: %v", err)
	}
}

// runRuns runs the scheduler, then every run, once.
func (e *env) runRuns() {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.runs.Reconcile(ctx, req(runSchedulerRequest)); err != nil {
		e.t.Fatalf("scheduler: %v", err)
	}
	var list v1alpha1.IntentRunList
	if err := e.c.List(ctx, &list, client.InNamespace(testNS)); err != nil {
		e.t.Fatal(err)
	}
	for i := range list.Items {
		if _, err := e.runs.Reconcile(ctx, req(list.Items[i].Name)); err != nil {
			e.t.Fatalf("run %s: %v", list.Items[i].Name, err)
		}
	}
}

// readyRepositories marks every Repository not yet Ready as source-controller
// would: pinned at baseSHA with an artifact, declaring image when set.
func (e *env) readyRepositories(image string) {
	e.t.Helper()
	ctx := context.Background()
	var list v1alpha1.RepositoryList
	if err := e.c.List(ctx, &list, client.InNamespace(testNS)); err != nil {
		e.t.Fatal(err)
	}
	for i := range list.Items {
		repo := &list.Items[i]
		if repo.Status.ResolvedSHA != "" {
			continue
		}
		repo.Status.ResolvedSHA = baseSHA
		repo.Status.Artifact = &v1alpha1.Artifact{URL: "http://artifacts/x.tar.gz", Digest: "sha256:aa"}
		repo.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Ready", LastTransitionTime: metav1.NewTime(e.clock.Now())}}
		if image != "" {
			repo.Status.RunnerImage = &v1alpha1.RunnerImage{Image: image, Manifest: ".patchy/agent.yaml",
				SearchPath: "/usr/bin:/bin"}
		}
		if err := e.c.Status().Update(ctx, repo); err != nil {
			e.t.Fatal(err)
		}
	}
}

// drive runs the intent, its runs and its repositories until the Intent's
// phase is want or no pass changes anything, advancing the clock a poll
// interval each round so every poll is due.
func (e *env) drive(name string, want v1alpha1.IntentPhase, image string) *v1alpha1.Intent {
	e.t.Helper()
	for range 60 {
		in := e.get(name)
		if in.Status.Phase == want {
			return in
		}
		e.mustIntent(name)
		e.readyRepositories(image)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	in := e.get(name)
	e.t.Fatalf("intent %s did not reach %s: phase %s, conditions %+v", name, want, in.Status.Phase,
		in.Status.Conditions)
	return nil
}

func (e *env) get(name string) *v1alpha1.Intent {
	e.t.Helper()
	var in v1alpha1.Intent
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &in); err != nil {
		e.t.Fatalf("get intent %s: %v", name, err)
	}
	return &in
}

// intentRuns are the Intent's runs, oldest first.
func (e *env) intentRuns(name string) []v1alpha1.IntentRun {
	e.t.Helper()
	var list v1alpha1.IntentRunList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS),
		client.MatchingLabels{v1alpha1.LabelIntent: name}); err != nil {
		e.t.Fatal(err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.IntentRun) int { return strings.Compare(a.Name, b.Name) })
	return list.Items
}

// newIntent creates the Intent discovery would for issue 1, requested by
// requester, with the fake issue open and labelled.
func (e *env) newIntent(requester string) string {
	e.t.Helper()
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", requester)
	ev := e.gh.issues[1].events[len(e.gh.issues[1].events)-1]
	name := v1alpha1.IntentName("target", 1)
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.IntentSpec{
			Project: "target",
			Issue:   v1alpha1.IntentIssue{Repository: intentRepoURL, Number: 1, URL: intentRepoURL + "/issues/1"},
			RequestedBy: v1alpha1.IntentRequest{
				Login: requester, At: metav1.NewTime(ev.CreatedAt), EventID: ev.ID,
			},
		},
	}
	if err := e.c.Create(context.Background(), in); err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	return name
}
