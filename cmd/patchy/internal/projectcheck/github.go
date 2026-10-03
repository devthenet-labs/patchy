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
	"path"
	"regexp"
	"slices"
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
	// File reads path at ref. found is false when no regular file is
	// there: none at all, or a symlink, directory or submodule, none of
	// which source-controller's archive read honours. data is nil when the
	// file is larger than limit bytes, with size still its size.
	File(ctx context.Context, repoURL, ref, filePath string, limit int64) (data []byte, size int64, found bool,
		err error)
}

// shaPattern is a full commit SHA.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// githubTimeout bounds one GitHub request.
const githubTimeout = 30 * time.Second

// HTTPGitHub reads the GitHub REST API over plain net/http: two requests
// for a repository's head and one or two per file, with no client library.
//
// A token is sent only to the host it is for: Token to github.com, and
// EnterpriseToken to the one GitHub Enterprise Server host the caller named
// (EnterpriseHost, from GH_HOST). A repository URL comes from cluster
// configuration the caller may not have written, so whoever wrote it picks
// the host a request goes to, and neither token may follow it anywhere
// else. The gh CLI needs no such rule because it talks only to hosts its
// user picked; here the Project picks them.
type HTTPGitHub struct {
	// Token authenticates requests about github.com repositories; empty
	// reads them anonymously, which sees public repositories only.
	Token string
	// TokenSource names where Token came from (an environment variable),
	// for the errors; empty with no token.
	TokenSource string
	// EnterpriseToken authenticates requests about repositories on
	// EnterpriseHost, and on no other host: every other host is read
	// anonymously.
	EnterpriseToken string
	// EnterpriseTokenSource names where EnterpriseToken came from.
	EnterpriseTokenSource string
	// EnterpriseHost is the one host EnterpriseToken is sent to (GH_HOST:
	// a host name, or a URL whose host name is taken), compared without
	// case or port, as forge.ParseRepoURL reports a repository's host;
	// empty sends EnterpriseToken nowhere.
	EnterpriseHost string
	// APIURL overrides the API root for every host (tests); empty derives
	// it from the repository's host: api.github.com for github.com, and
	// https://<host>/api/v3 for GitHub Enterprise Server. The token is
	// still chosen by the repository's host.
	APIURL string
	// Client sends the requests; nil is a client with githubTimeout.
	Client *http.Client
}

// Head implements GitHub.
func (g *HTTPGitHub) Head(ctx context.Context, repoURL string) (Head, error) {
	base, auth, err := g.repoAPI(repoURL)
	if err != nil {
		return Head{}, err
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	body, _, err := g.get(ctx, auth, base, "application/vnd.github+json", 1<<20)
	if err != nil {
		return Head{}, err
	}
	if err := json.Unmarshal(body, &repo); err != nil || repo.DefaultBranch == "" {
		return Head{}, fmt.Errorf("GitHub answered %s without a default branch", base)
	}
	body, _, err = g.get(ctx, auth, base+"/commits/"+url.PathEscape(repo.DefaultBranch), "application/vnd.github.sha",
		1<<10)
	if err != nil {
		return Head{}, err
	}
	sha := strings.TrimSpace(string(body))
	if !shaPattern.MatchString(sha) {
		return Head{}, fmt.Errorf("GitHub answered the head of %s with %q, not a commit SHA", repo.DefaultBranch, sha)
	}
	return Head{Branch: repo.DefaultBranch, SHA: sha}, nil
}

// treeEntry is one entry of a Git tree listing.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// regular reports a regular file (mode 100644, 100755 or the legacy
// 100664): never a symlink (120000), a directory (040000) or a submodule
// (160000).
func (e treeEntry) regular() bool { return e.Type == "blob" && strings.HasPrefix(e.Mode, "100") }

// treeListLimit bounds a tree listing's body; a declaration's directory
// holds a handful of entries.
const treeListLimit = 4 << 20

// File implements GitHub. It lists the file's directory as a Git tree and
// reads the entry's blob only when the entry is a regular file. The
// contents API cannot tell: it answers a symlink to a file with the
// target's content as type "file", while source-controller's archive read
// skips every entry but a regular file, so a declaration source-controller
// never sees must not be judged here either.
func (g *HTTPGitHub) File(ctx context.Context, repoURL, ref, filePath string, limit int64) ([]byte, int64, bool,
	error) {
	base, auth, err := g.repoAPI(repoURL)
	if err != nil {
		return nil, 0, false, err
	}
	dir, leaf := path.Split(filePath)
	tree := ref
	if dir = strings.TrimSuffix(dir, "/"); dir != "" {
		tree += ":" + dir
	}
	u := base + "/git/trees/" + escapeSegments(tree)
	body, _, err := g.get(ctx, auth, u, "application/vnd.github+json", treeListLimit)
	var he *httpError
	switch {
	case errors.As(err, &he) && (he.status == http.StatusNotFound || he.status == http.StatusUnprocessableEntity):
		// 404: no such directory. 422: the path names something other than
		// a directory (a file, or a symlink, even one to a directory), under
		// which the archive holds no entries either.
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, err
	}
	var listing struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, 0, false, fmt.Errorf("GitHub answered %s without a tree: %w", u, err)
	}
	i := slices.IndexFunc(listing.Tree, func(e treeEntry) bool { return e.Path == leaf })
	switch {
	case i < 0 && listing.Truncated:
		return nil, 0, false, fmt.Errorf("GitHub truncated the listing of %s, so whether it holds %s is unknown", u,
			leaf)
	case i < 0 || !listing.Tree[i].regular():
		return nil, 0, false, nil
	case listing.Tree[i].Size > limit:
		return nil, listing.Tree[i].Size, true, nil
	}
	data, size, err := g.get(ctx, auth, base+"/git/blobs/"+url.PathEscape(listing.Tree[i].SHA),
		"application/vnd.github.raw", limit)
	switch {
	case err != nil:
		return nil, 0, false, err
	case size > limit:
		return nil, size, true, nil
	}
	return data, size, true, nil
}

// escapeSegments path-escapes each slash-separated segment of p.
func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// credential is the token a request carries, and where it came from.
type credential struct {
	token, source string
	// enterprise marks a host other than github.com, for the hint.
	enterprise bool
	// host is the repository's host, for the hint.
	host string
	// withheld names the enterprise token left off because host is not
	// the one it is for, for the hint.
	withheld string
}

// repoAPI is the API URL of the repository a URL names, and the credential
// for its host.
func (g *HTTPGitHub) repoAPI(repoURL string) (string, credential, error) {
	host, repo, err := forge.ParseRepoURL(repoURL)
	if err != nil {
		return "", credential{}, err
	}
	auth := credential{token: g.Token, source: g.TokenSource, host: host}
	root := "https://api.github.com"
	if !strings.EqualFold(host, "github.com") {
		auth = credential{enterprise: true, host: host}
		switch {
		case g.EnterpriseToken == "":
		case host == hostName(g.EnterpriseHost):
			auth.token, auth.source = g.EnterpriseToken, g.EnterpriseTokenSource
		default:
			auth.withheld = g.EnterpriseTokenSource
			if auth.withheld == "" {
				auth.withheld = "the enterprise token"
			}
		}
		root = "https://" + host + "/api/v3"
	}
	if g.APIURL != "" {
		root = g.APIURL
	}
	return strings.TrimRight(root, "/") + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name),
		auth, nil
}

// hostName is the lower-case host name GH_HOST names, without scheme, port
// or path, as forge.ParseRepoURL reports a repository's; "" for none.
func hostName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// who says whose identity a request carried.
func (c credential) who() string {
	switch {
	case c.token == "":
		return "anonymously"
	case c.source == "":
		return "with your token"
	}
	return "with " + c.source
}

// httpError is a GitHub answer other than 200.
type httpError struct {
	status int
	url    string
	auth   credential
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("GitHub answered %d %s for %s (%s)", e.status, http.StatusText(e.status), e.url, e.auth.who())
	if e.status == http.StatusNotFound || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
		switch {
		case e.auth.withheld != "":
			msg += fmt.Sprintf("; %s is sent only to the host GH_HOST names, and %s is not it: set GH_HOST=%s "+
				"if the token is for that host", e.auth.withheld, e.auth.host, e.auth.host)
		case e.auth.enterprise:
			msg += "; a private repository needs GH_ENTERPRISE_TOKEN or GITHUB_ENTERPRISE_TOKEN set to a token " +
				"that can read it, and GH_HOST=" + e.auth.host
		default:
			msg += "; a private repository needs GH_TOKEN or GITHUB_TOKEN set to a token that can read it"
		}
	}
	return msg
}

// get fetches u and returns at most limit+1 bytes of its body (so a caller
// can tell a body over limit), with the body's size: Content-Length when
// GitHub sent one, else what was read.
func (g *HTTPGitHub) get(ctx context.Context, auth credential, u, accept string, limit int64) ([]byte, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth.token != "" {
		req.Header.Set("Authorization", "Bearer "+auth.token)
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
		return nil, 0, &httpError{status: resp.StatusCode, url: u, auth: auth}
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
