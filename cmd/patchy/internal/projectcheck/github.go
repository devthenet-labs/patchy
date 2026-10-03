// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/forge"
)

// Head is a repository's default branch and the commit at its head.
type Head struct {
	Branch string
	SHA    string
}

// GitHub is what the check reads from a repository's forge, with the
// operator's own identity. Never the App's: the App's permissions are
// intent-controller's to prove, and its Ready condition reports them.
type GitHub interface {
	// Head returns the repository's default branch and its head commit.
	Head(ctx context.Context, repoURL string) (Head, error)
	// File reads path at ref. found is false when no such file exists;
	// data is nil when the file is larger than limit bytes, with size
	// still its size.
	File(ctx context.Context, repoURL, ref, path string, limit int64) (data []byte, size int64, found bool, err error)
}

// shaPattern is a full commit SHA.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// githubTimeout bounds one GitHub request.
const githubTimeout = 30 * time.Second

// HTTPGitHub reads the GitHub REST API over plain net/http: two requests
// for a repository's head and one per file, with no client library.
type HTTPGitHub struct {
	// Token authenticates every request; empty reads anonymously, which
	// sees public repositories only.
	Token string
	// TokenSource names where Token came from (an environment variable),
	// for the errors; empty with no token.
	TokenSource string
	// APIURL overrides the API root for every host (tests); empty derives
	// it from the repository's host: api.github.com for github.com, and
	// https://<host>/api/v3 for GitHub Enterprise Server.
	APIURL string
	// Client sends the requests; nil is a client with githubTimeout.
	Client *http.Client
}

// Head implements GitHub.
func (g *HTTPGitHub) Head(ctx context.Context, repoURL string) (Head, error) {
	base, err := g.repoAPI(repoURL)
	if err != nil {
		return Head{}, err
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	body, _, err := g.get(ctx, base, "application/vnd.github+json", 1<<20)
	if err != nil {
		return Head{}, err
	}
	if err := json.Unmarshal(body, &repo); err != nil || repo.DefaultBranch == "" {
		return Head{}, fmt.Errorf("GitHub answered %s without a default branch", base)
	}
	body, _, err = g.get(ctx, base+"/commits/"+url.PathEscape(repo.DefaultBranch), "application/vnd.github.sha", 1<<10)
	if err != nil {
		return Head{}, err
	}
	sha := strings.TrimSpace(string(body))
	if !shaPattern.MatchString(sha) {
		return Head{}, fmt.Errorf("GitHub answered the head of %s with %q, not a commit SHA", repo.DefaultBranch, sha)
	}
	return Head{Branch: repo.DefaultBranch, SHA: sha}, nil
}

// File implements GitHub.
func (g *HTTPGitHub) File(ctx context.Context, repoURL, ref, path string, limit int64) ([]byte, int64, bool, error) {
	base, err := g.repoAPI(repoURL)
	if err != nil {
		return nil, 0, false, err
	}
	u := base + "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	body, size, err := g.get(ctx, u, "application/vnd.github.raw", limit)
	var he *httpError
	switch {
	case errors.As(err, &he) && he.status == http.StatusNotFound:
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, err
	case size > limit:
		return nil, size, true, nil
	}
	return body, size, true, nil
}

// repoAPI is the API URL of the repository a URL names.
func (g *HTTPGitHub) repoAPI(repoURL string) (string, error) {
	host, repo, err := forge.ParseRepoURL(repoURL)
	if err != nil {
		return "", err
	}
	root := g.APIURL
	switch {
	case root != "":
	case host == "github.com":
		root = "https://api.github.com"
	default:
		root = "https://" + host + "/api/v3"
	}
	return strings.TrimRight(root, "/") + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name), nil
}

// httpError is a GitHub answer other than 200.
type httpError struct {
	status int
	url    string
	who    string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("GitHub answered %d %s for %s (%s)", e.status, http.StatusText(e.status), e.url, e.who)
	if e.status == http.StatusNotFound || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
		msg += "; a private repository needs GH_TOKEN or GITHUB_TOKEN set to a token that can read it"
	}
	return msg
}

// who says whose identity a request carried.
func (g *HTTPGitHub) who() string {
	if g.Token == "" {
		return "anonymously"
	}
	if g.TokenSource == "" {
		return "with your token"
	}
	return "with " + g.TokenSource
}

// get fetches u and returns at most limit+1 bytes of its body (so a caller
// can tell a body over limit), with the body's size: Content-Length when
// GitHub sent one, else what was read.
func (g *HTTPGitHub) get(ctx context.Context, u, accept string, limit int64) ([]byte, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: githubTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response; nothing to flush
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, 0, &httpError{status: resp.StatusCode, url: u, who: g.who()}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(body))
	if resp.ContentLength > size {
		size = resp.ContentLength
	}
	return body, size, nil
}
