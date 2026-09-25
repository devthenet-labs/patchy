// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// GitHub is every GitHub call the intent reconcilers make. Repositories are
// named by their https URL. The production implementation (NewForgeGitHub)
// makes each call with a token minted for that one call's repository and the
// one permission it needs; the unscoped installation client is never used.
// Tests substitute an in-memory fake.
type GitHub interface {
	// Resolve reports whether repoURL resolves to exactly one Forge;
	// forge.ErrNoMatch or forge.ErrAmbiguous when it does not.
	Resolve(ctx context.Context, repoURL string) error
	// Installed mints a token for repoURL with perms, which proves the App
	// is installed there with them.
	Installed(ctx context.Context, repoURL string, perms ghclient.TokenPerms) error
	// BotLogin is the App's own bot login ("<slug>[bot]") on repoURL's
	// Forge; "" when the Forge authenticates with a personal access token.
	BotLogin(ctx context.Context, repoURL string) (string, error)
	// Permission is login's permission on repoURL (ghclient.CollaboratorPermission).
	Permission(ctx context.Context, repoURL, login string) (string, error)
	// RateRemaining is the core requests left to the credential repoURL is
	// read with.
	RateRemaining(ctx context.Context, repoURL string) (int, error)
	// EnsureLabel creates the label when repoURL has none by that name.
	EnsureLabel(ctx context.Context, repoURL, name, color, description string) error

	// The intent issue.
	ListIssues(ctx context.Context, repoURL string, labels []string, etag string) (*ghclient.IssueList, error)
	GetIssue(ctx context.Context, repoURL string, number int64) (*ghclient.Issue, error)
	ListIssueEvents(ctx context.Context, repoURL string, number int64) ([]*ghclient.IssueEvent, error)
	ListIssueComments(ctx context.Context, repoURL string, number int64, since time.Time) ([]*ghclient.Comment, error)
	GetIssueComment(ctx context.Context, repoURL string, id int64) (*ghclient.Comment, error)
	// CommentEdited reports whether the comment with GraphQL node id nodeID
	// was ever edited (ghclient.CommentEdited): GitHub's own fact. REST
	// updated_at can move without an edit or miss one within the same second.
	CommentEdited(ctx context.Context, repoURL, nodeID string) (bool, error)
	CreateIssueComment(ctx context.Context, repoURL string, number int64, body string) (*ghclient.Comment, error)
	EditIssueComment(ctx context.Context, repoURL string, id int64, body string) error
	// React adds the eyes reaction to a comment: a command was seen.
	React(ctx context.Context, repoURL string, commentID int64) error
	RemoveLabel(ctx context.Context, repoURL string, number int64, name string) error
	CloseIssue(ctx context.Context, repoURL string, number int64, reason string) error

	// Application repositories.
	DefaultBranch(ctx context.Context, repoURL string) (string, error)
	HeadSHA(ctx context.Context, repoURL, branch string) (string, error)
	CreateCommit(ctx context.Context, repoURL string, req ghclient.CommitRequest) (string, error)
	CreateBranchRef(ctx context.Context, repoURL, branch, sha string) error
	FastForwardRef(ctx context.Context, repoURL, branch, sha string) error
	// FindPullRequest is the open pull request from head into base, or nil.
	FindPullRequest(ctx context.Context, repoURL, head, base string) (*ghclient.PR, error)
	CreatePullRequest(ctx context.Context, repoURL string, req ghclient.PRRequest) (*ghclient.PR, error)
	GetPullRequest(ctx context.Context, repoURL string, number int64) (*ghclient.PullRequest, error)
	ListPullRequestReviews(ctx context.Context, repoURL string, number int64) ([]ghclient.Review, error)
	ListPullRequestReviewComments(ctx context.Context, repoURL string, number int64) ([]ghclient.ReviewComment, error)
	ReviewEdited(ctx context.Context, repoURL, nodeID string) (bool, error)
	ReviewCommentEdited(ctx context.Context, repoURL, nodeID string) (bool, error)
	ComparePatch(ctx context.Context, repoURL, base, head string) (string, error)
	RequestReviewers(ctx context.Context, repoURL string, number int64, logins []string) error
	ListCheckRuns(ctx context.Context, repoURL, sha string) ([]ghclient.CheckRun, error)
	ListCheckAnnotations(ctx context.Context, repoURL string, id int64, limit int) ([]ghclient.CheckAnnotation, error)
	ListCommitStatuses(ctx context.Context, repoURL, sha string) ([]ghclient.CommitStatus, error)
	ListWorkflowJobs(ctx context.Context, repoURL string, runID int64) ([]ghclient.WorkflowJob, error)
	GetJobLogTail(ctx context.Context, repoURL string, jobID int64, tailBytes int) (string, error)
}

// The permission set of each token. Every token requests exactly one
// permission on exactly one repository; GitHub adds metadata read on its own,
// which is what the collaborator-permission and rate-limit reads need.
var (
	issuesRead    = ghclient.TokenPerms{Issues: ghclient.PermRead}
	issuesWrite   = ghclient.TokenPerms{Issues: ghclient.PermWrite}
	contentsRead  = ghclient.TokenPerms{Contents: ghclient.PermRead}
	contentsWrite = ghclient.TokenPerms{Contents: ghclient.PermWrite}
	pullsRead     = ghclient.TokenPerms{PullRequests: ghclient.PermRead}
	pullsWrite    = ghclient.TokenPerms{PullRequests: ghclient.PermWrite}
	checksRead    = ghclient.TokenPerms{Checks: ghclient.PermRead}
	statusesRead  = ghclient.TokenPerms{Statuses: ghclient.PermRead}
	actionsRead   = ghclient.TokenPerms{Actions: ghclient.PermRead}
)

// rateCacheTTL is how long one rate-limit reading stands for its
// installation: the floor is a coarse guard, and asking before every poll
// would double the requests it guards.
const rateCacheTTL = time.Minute

// forgeGitHub is the production GitHub: every call resolves the repository's
// Forge through the shared forge store, takes a token scoped to that
// repository and the call's one permission (forge.Store.TokenWith), and makes
// the call with a client built on that token alone.
//
// The token and the Forge's proxy URL are kept per (Forge, repository,
// permissions), because the store reads the Forge's Secret live, uncached,
// before it looks at its own token cache: without this every GitHub call
// would be a Secret read on the API server, several per intent per poll.
// A kept credential is taken again once its token is near expiry, once it
// has been kept credentialReread (so a rotated personal access token or
// proxy password is picked up), or at once when the Forge changes.
type forgeGitHub struct {
	forges    *forge.Store
	namespace string
	now       func() time.Time

	mu        sync.Mutex
	clients   map[clientKey]cachedClient
	rates     map[string]rateReading
	creds     map[credKey]cachedCred
	botLogins map[string]botReading
}

// credentialRefresh is how long before its expiry a kept token is taken
// again: the forge store's own refresh margin, longer than any one call.
const credentialRefresh = 5 * time.Minute

// credentialReread is the longest a credential is kept without reading the
// Forge's Secret again.
const credentialReread = 10 * time.Minute

// credKey identifies one kept credential: the Forge as it is now (its
// resourceVersion moves with any change to it), the repository, and the
// exact permission set.
type credKey struct {
	forge, version, repo string
	perms                ghclient.TokenPerms
}

type cachedCred struct {
	token, proxy string
	expires      time.Time // zero for a personal access token
	read         time.Time
}

type botReading struct {
	login string
	read  time.Time
}

type clientKey struct{ token, proxy, baseURL string }

type cachedClient struct {
	client  *ghclient.Client
	expires time.Time // zero for a personal access token, which does not expire
}

type rateReading struct {
	remaining int
	at        time.Time
}

// NewForgeGitHub builds the production GitHub over forges, resolving Forges
// in namespace.
func NewForgeGitHub(forges *forge.Store, namespace string) GitHub {
	return &forgeGitHub{
		forges:    forges,
		namespace: namespace,
		now:       time.Now,
		clients:   map[clientKey]cachedClient{},
		rates:     map[string]rateReading{},
		creds:     map[credKey]cachedCred{},
		botLogins: map[string]botReading{},
	}
}

// forgeKey names a resolved Forge as it is now.
func forgeKey(res *forge.Resolved) (name, version string) {
	return res.Forge.Namespace + "/" + res.Forge.Name, res.Forge.ResourceVersion
}

// fresh reports a kept credential still to be served at now.
func (c cachedCred) fresh(now time.Time) bool {
	if now.Sub(c.read) >= credentialReread {
		return false
	}
	return c.expires.IsZero() || now.Add(credentialRefresh).Before(c.expires)
}

// credential is the token for repoURL's resolved repository carrying perms
// alone, and the Forge's proxy URL, kept as forgeGitHub says.
func (g *forgeGitHub) credential(ctx context.Context, res *forge.Resolved, perms ghclient.TokenPerms) (
	cachedCred, error) {
	name, version := forgeKey(res)
	key := credKey{forge: name, version: version, repo: strings.ToLower(res.Repo.String()), perms: perms}
	now := g.now()
	g.mu.Lock()
	cred, ok := g.creds[key]
	g.mu.Unlock()
	if ok && cred.fresh(now) {
		return cred, nil
	}
	token, expires, err := g.forges.TokenWith(ctx, res, perms)
	if err != nil {
		return cachedCred{}, err
	}
	proxy, err := g.forges.ProxyURL(ctx, res)
	if err != nil {
		return cachedCred{}, err
	}
	cred = cachedCred{token: token, proxy: proxy, expires: expires, read: now}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Evict what can no longer be served, so the map stays bounded by the
	// (Forge, repository, permissions) combinations in use.
	for k, v := range g.creds {
		if !v.fresh(now) {
			delete(g.creds, k)
		}
	}
	g.creds[key] = cred
	return cred, nil
}

// client returns a client authenticated with a token for repoURL carrying
// perms alone, and the repository.
func (g *forgeGitHub) client(ctx context.Context, repoURL string, perms ghclient.TokenPerms) (
	*ghclient.Client, ghclient.Repo, error) {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	cred, err := g.credential(ctx, res, perms)
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	key := clientKey{token: cred.token, proxy: cred.proxy, baseURL: res.Forge.Spec.BaseURL}
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.clients[key]; ok {
		return c.client, res.Repo, nil
	}
	c, err := ghclient.NewToken(cred.token, res.Forge.Spec.BaseURL, ghclient.WithProxy(cred.proxy))
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	// Tokens rotate hourly: evict the clients of expired ones, so the map
	// stays bounded by the tokens alive at once.
	now := g.now()
	for k, v := range g.clients {
		if !v.expires.IsZero() && !now.Before(v.expires) {
			delete(g.clients, k)
		}
	}
	g.clients[key] = cachedClient{client: c, expires: cred.expires}
	return c, res.Repo, nil
}

func (g *forgeGitHub) Resolve(ctx context.Context, repoURL string) error {
	_, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	return err
}

func (g *forgeGitHub) Installed(ctx context.Context, repoURL string, perms ghclient.TokenPerms) error {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return err
	}
	_, _, err = g.forges.TokenWith(ctx, res, perms)
	return err
}

// BotLogin is read once per Forge version and credentialReread: every intent
// pass asks for it, and the store reads the Secret to answer.
func (g *forgeGitHub) BotLogin(ctx context.Context, repoURL string) (string, error) {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return "", err
	}
	name, version := forgeKey(res)
	key := name + "@" + version
	now := g.now()
	g.mu.Lock()
	b, ok := g.botLogins[key]
	g.mu.Unlock()
	if ok && now.Sub(b.read) < credentialReread {
		return b.login, nil
	}
	login, err := g.forges.BotLogin(ctx, res)
	switch {
	case errors.Is(err, forge.ErrNoBotIdentity):
		login = ""
	case err != nil:
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range g.botLogins {
		if now.Sub(v.read) >= credentialReread {
			delete(g.botLogins, k)
		}
	}
	g.botLogins[key] = botReading{login: login, read: now}
	return login, nil
}

func (g *forgeGitHub) Permission(ctx context.Context, repoURL, login string) (string, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return "", err
	}
	return c.CollaboratorPermission(ctx, repo, login)
}

func (g *forgeGitHub) RateRemaining(ctx context.Context, repoURL string) (int, error) {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return 0, err
	}
	// One installation per owner, one budget per installation.
	key := res.Forge.Name + "/" + strings.ToLower(res.Repo.Owner)
	g.mu.Lock()
	r, ok := g.rates[key]
	g.mu.Unlock()
	if ok && g.now().Sub(r.at) < rateCacheTTL {
		return r.remaining, nil
	}
	c, _, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return 0, err
	}
	remaining, err := c.RateRemaining(ctx)
	if err != nil {
		return 0, err
	}
	g.mu.Lock()
	g.rates[key] = rateReading{remaining: remaining, at: g.now()}
	g.mu.Unlock()
	return remaining, nil
}

func (g *forgeGitHub) EnsureLabel(ctx context.Context, repoURL, name, color, description string) error {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return err
	}
	_, err = c.EnsureLabel(ctx, repo, name, color, description)
	return err
}

func (g *forgeGitHub) ListIssues(ctx context.Context, repoURL string, labels []string, etag string) (
	*ghclient.IssueList, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return nil, err
	}
	return c.ListIssues(ctx, repo, labels, "open", etag)
}

func (g *forgeGitHub) GetIssue(ctx context.Context, repoURL string, number int64) (*ghclient.Issue, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return nil, err
	}
	return c.GetIssue(ctx, repo, int(number))
}

func (g *forgeGitHub) ListIssueEvents(ctx context.Context, repoURL string, number int64) (
	[]*ghclient.IssueEvent, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return nil, err
	}
	return c.ListIssueEvents(ctx, repo, int(number))
}

func (g *forgeGitHub) ListIssueComments(ctx context.Context, repoURL string, number int64, since time.Time) (
	[]*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return nil, err
	}
	return c.ListIssueComments(ctx, repo, int(number), since)
}

func (g *forgeGitHub) GetIssueComment(ctx context.Context, repoURL string, id int64) (*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return nil, err
	}
	return c.GetIssueComment(ctx, repo, id)
}

func (g *forgeGitHub) CommentEdited(ctx context.Context, repoURL, nodeID string) (bool, error) {
	c, _, err := g.client(ctx, repoURL, issuesRead)
	if err != nil {
		return false, err
	}
	return c.CommentEdited(ctx, nodeID)
}

func (g *forgeGitHub) CreateIssueComment(ctx context.Context, repoURL string, number int64, body string) (
	*ghclient.Comment, error) {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return nil, err
	}
	return c.CreateIssueComment(ctx, repo, int(number), body)
}

func (g *forgeGitHub) EditIssueComment(ctx context.Context, repoURL string, id int64, body string) error {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return err
	}
	return c.EditComment(ctx, repo, id, body)
}

func (g *forgeGitHub) React(ctx context.Context, repoURL string, commentID int64) error {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return err
	}
	return c.CreateIssueCommentReaction(ctx, repo, commentID, ghclient.ReactionEyes)
}

func (g *forgeGitHub) RemoveLabel(ctx context.Context, repoURL string, number int64, name string) error {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return err
	}
	return c.RemoveLabel(ctx, repo, int(number), name)
}

func (g *forgeGitHub) CloseIssue(ctx context.Context, repoURL string, number int64, reason string) error {
	c, repo, err := g.client(ctx, repoURL, issuesWrite)
	if err != nil {
		return err
	}
	return c.CloseIssue(ctx, repo, int(number), reason)
}

func (g *forgeGitHub) DefaultBranch(ctx context.Context, repoURL string) (string, error) {
	c, repo, err := g.client(ctx, repoURL, contentsRead)
	if err != nil {
		return "", err
	}
	return c.DefaultBranch(ctx, repo)
}

func (g *forgeGitHub) HeadSHA(ctx context.Context, repoURL, branch string) (string, error) {
	c, repo, err := g.client(ctx, repoURL, contentsRead)
	if err != nil {
		return "", err
	}
	return c.HeadSHA(ctx, repo, branch)
}

func (g *forgeGitHub) CreateCommit(ctx context.Context, repoURL string, req ghclient.CommitRequest) (string, error) {
	c, repo, err := g.client(ctx, repoURL, contentsWrite)
	if err != nil {
		return "", err
	}
	return c.CreateCommit(ctx, repo, req)
}

func (g *forgeGitHub) CreateBranchRef(ctx context.Context, repoURL, branch, sha string) error {
	c, repo, err := g.client(ctx, repoURL, contentsWrite)
	if err != nil {
		return err
	}
	return c.CreateBranchRef(ctx, repo, branch, sha)
}

func (g *forgeGitHub) FastForwardRef(ctx context.Context, repoURL, branch, sha string) error {
	c, repo, err := g.client(ctx, repoURL, contentsWrite)
	if err != nil {
		return err
	}
	return c.FastForwardRef(ctx, repo, branch, sha)
}

func (g *forgeGitHub) FindPullRequest(ctx context.Context, repoURL, head, base string) (*ghclient.PR, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.FindOpenPR(ctx, repo, head, base)
}

func (g *forgeGitHub) CreatePullRequest(ctx context.Context, repoURL string, req ghclient.PRRequest) (
	*ghclient.PR, error) {
	c, repo, err := g.client(ctx, repoURL, pullsWrite)
	if err != nil {
		return nil, err
	}
	return c.CreatePR(ctx, repo, req)
}

func (g *forgeGitHub) GetPullRequest(ctx context.Context, repoURL string, number int64) (
	*ghclient.PullRequest, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.GetPullRequest(ctx, repo, int(number))
}

func (g *forgeGitHub) ListPullRequestReviews(ctx context.Context, repoURL string, number int64) (
	[]ghclient.Review, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.ListPullRequestReviews(ctx, repo, int(number))
}

func (g *forgeGitHub) ListPullRequestReviewComments(ctx context.Context, repoURL string, number int64) (
	[]ghclient.ReviewComment, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.ListPullRequestReviewComments(ctx, repo, int(number))
}

func (g *forgeGitHub) ReviewEdited(ctx context.Context, repoURL, nodeID string) (bool, error) {
	c, _, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return false, err
	}
	return c.ReviewEdited(ctx, nodeID)
}

func (g *forgeGitHub) ReviewCommentEdited(ctx context.Context, repoURL, nodeID string) (bool, error) {
	c, _, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return false, err
	}
	return c.ReviewCommentEdited(ctx, nodeID)
}

func (g *forgeGitHub) ComparePatch(ctx context.Context, repoURL, base, head string) (string, error) {
	c, repo, err := g.client(ctx, repoURL, contentsRead)
	if err != nil {
		return "", err
	}
	return c.ComparePatch(ctx, repo, base, head)
}

func (g *forgeGitHub) RequestReviewers(ctx context.Context, repoURL string, number int64, logins []string) error {
	c, repo, err := g.client(ctx, repoURL, pullsWrite)
	if err != nil {
		return err
	}
	return c.RequestReviewers(ctx, repo, int(number), logins)
}

func (g *forgeGitHub) ListCheckRuns(ctx context.Context, repoURL, sha string) ([]ghclient.CheckRun, error) {
	c, repo, err := g.client(ctx, repoURL, checksRead)
	if err != nil {
		return nil, err
	}
	return c.ListCheckRuns(ctx, repo, sha)
}

func (g *forgeGitHub) ListCheckAnnotations(ctx context.Context, repoURL string, id int64, limit int) (
	[]ghclient.CheckAnnotation, error) {
	c, repo, err := g.client(ctx, repoURL, checksRead)
	if err != nil {
		return nil, err
	}
	return c.ListCheckAnnotations(ctx, repo, id, limit)
}

func (g *forgeGitHub) ListCommitStatuses(ctx context.Context, repoURL, sha string) (
	[]ghclient.CommitStatus, error) {
	c, repo, err := g.client(ctx, repoURL, statusesRead)
	if err != nil {
		return nil, err
	}
	return c.ListCommitStatuses(ctx, repo, sha)
}

func (g *forgeGitHub) ListWorkflowJobs(ctx context.Context, repoURL string, runID int64) (
	[]ghclient.WorkflowJob, error) {
	c, repo, err := g.client(ctx, repoURL, actionsRead)
	if err != nil {
		return nil, err
	}
	return c.ListWorkflowJobs(ctx, repo, runID)
}

func (g *forgeGitHub) GetJobLogTail(ctx context.Context, repoURL string, jobID int64, tailBytes int) (string, error) {
	c, repo, err := g.client(ctx, repoURL, actionsRead)
	if err != nil {
		return "", err
	}
	return c.GetJobLogTail(ctx, repo, jobID, tailBytes)
}

// installationRefused reports a token GitHub will not mint for the
// repository: no installation covers it (404), or the installation does not
// include it or lacks the permission (422, 403). Anything else is transient.
func installationRefused(err error) bool {
	return ghclient.IsNotFound(err) || ghclient.IsUnprocessable(err) || ghclient.IsForbidden(err)
}

// forgeUnresolved reports a repository URL that resolves to no Forge, or to
// more than one.
func forgeUnresolved(err error) bool {
	return errors.Is(err, forge.ErrNoMatch) || errors.Is(err, forge.ErrAmbiguous)
}
