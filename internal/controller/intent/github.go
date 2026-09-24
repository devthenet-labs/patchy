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
	FindPullRequest(ctx context.Context, repoURL, head string) (*ghclient.PR, error)
	CreatePullRequest(ctx context.Context, repoURL string, req ghclient.PRRequest) (*ghclient.PR, error)
	GetPullRequest(ctx context.Context, repoURL string, number int64) (*ghclient.PullRequest, error)
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
)

// rateCacheTTL is how long one rate-limit reading stands for its
// installation: the floor is a coarse guard, and asking before every poll
// would double the requests it guards.
const rateCacheTTL = time.Minute

// forgeGitHub is the production GitHub: every call resolves the repository's
// Forge through the shared forge store, mints a token scoped to that
// repository and the call's one permission (forge.Store.TokenWith, which
// caches it until shortly before it expires), and makes the call with a
// client built on that token alone.
type forgeGitHub struct {
	forges    *forge.Store
	namespace string
	now       func() time.Time

	mu      sync.Mutex
	clients map[clientKey]cachedClient
	rates   map[string]rateReading
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
	}
}

// client returns a client authenticated with a token for repoURL carrying
// perms alone, and the repository.
func (g *forgeGitHub) client(ctx context.Context, repoURL string, perms ghclient.TokenPerms) (
	*ghclient.Client, ghclient.Repo, error) {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	token, expires, err := g.forges.TokenWith(ctx, res, perms)
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	proxy, err := g.forges.ProxyURL(ctx, res)
	if err != nil {
		return nil, ghclient.Repo{}, err
	}
	key := clientKey{token: token, proxy: proxy, baseURL: res.Forge.Spec.BaseURL}
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.clients[key]; ok {
		return c.client, res.Repo, nil
	}
	c, err := ghclient.NewToken(token, res.Forge.Spec.BaseURL, ghclient.WithProxy(proxy))
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
	g.clients[key] = cachedClient{client: c, expires: expires}
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

func (g *forgeGitHub) BotLogin(ctx context.Context, repoURL string) (string, error) {
	res, err := g.forges.Resolve(ctx, g.namespace, repoURL)
	if err != nil {
		return "", err
	}
	login, err := g.forges.BotLogin(ctx, res)
	if errors.Is(err, forge.ErrNoBotIdentity) {
		return "", nil
	}
	return login, err
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

func (g *forgeGitHub) FindPullRequest(ctx context.Context, repoURL, head string) (*ghclient.PR, error) {
	c, repo, err := g.client(ctx, repoURL, pullsRead)
	if err != nil {
		return nil, err
	}
	return c.FindPRByHead(ctx, repo, head)
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
