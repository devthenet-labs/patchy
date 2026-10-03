// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// agentYAML is acme/web's declaration.
const agentYAML = "image: ghcr.io/acme/web-agent:1\n"

// githubAPI is an in-memory GitHub REST API: one repository, acme/web, on
// trunk. At its head, .patchy holds agent.yaml (a regular file) beside a
// symlink to it, an executable, a directory and a submodule;
// .devcontainer is a symlink to a directory, so GitHub refuses to list it
// as a tree (422); big's listing is truncated. It records the
// Authorization header and path of every request.
type githubAPI struct {
	mu    sync.Mutex
	auths []string
	paths []string
}

// The Accept headers the API is read with.
const (
	acceptJSON = "application/vnd.github+json"
	acceptSHA  = "application/vnd.github.sha"
	acceptRaw  = "application/vnd.github.raw"
)

// route is one answer of the fake API: the Accept header it is for ("" for
// any), and its status (0 for 200) and body.
type route struct {
	accept string
	status int
	body   string
}

// githubRoutes are the fake API's answers, by request path.
func githubRoutes() map[string]route {
	const trees = "/repos/acme/web/git/trees/"
	const blobs = "/repos/acme/web/git/blobs/"
	return map[string]route{
		"/repos/acme/web":               {accept: acceptJSON, body: `{"default_branch": "trunk"}`},
		"/repos/acme/web/commits/trunk": {accept: acceptSHA, body: sha("b")},
		"/repos/acme/bad/commits/main":  {body: "not a sha"},
		"/repos/acme/bad":               {body: `{"default_branch": "main"}`},
		trees + sha("b"): {accept: acceptJSON, body: `{"truncated": false, "tree": [
			{"path": ".patchy", "mode": "040000", "type": "tree", "sha": "tree-patchy"},
			{"path": "README.md", "mode": "100644", "type": "blob", "sha": "blob-readme", "size": 6}]}`},
		trees + sha("b") + ":.patchy": {accept: acceptJSON, body: fmt.Sprintf(`{"truncated": false, "tree": [
			{"path": "agent.yaml", "mode": "100644", "type": "blob", "sha": "blob-agent", "size": %d},
			{"path": "link.yaml", "mode": "120000", "type": "blob", "sha": "blob-link", "size": 10},
			{"path": "run.sh", "mode": "100755", "type": "blob", "sha": "blob-run", "size": 5},
			{"path": "conf", "mode": "040000", "type": "tree", "sha": "tree-conf"},
			{"path": "tool", "mode": "160000", "type": "commit", "sha": "c0ffee"}]}`, len(agentYAML))},
		trees + sha("b") + ":.devcontainer": {status: http.StatusUnprocessableEntity,
			body: `{"message": "Invalid object requested. SHA must identify a commit or a tree."}`},
		trees + sha("b") + ":big": {accept: acceptJSON, body: `{"truncated": true, "tree": []}`},
		blobs + "blob-agent":      {accept: acceptRaw, body: agentYAML},
		blobs + "blob-run":        {accept: acceptRaw, body: "true\n"},
		// What a symlink's blob holds: its target's path.
		blobs + "blob-link": {accept: acceptRaw, body: "agent.yaml"},
	}
}

func (a *githubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auths = append(a.auths, r.Header.Get("Authorization"))
	a.paths = append(a.paths, r.URL.Path)
	a.mu.Unlock()
	if r.Header.Get("X-GitHub-Api-Version") == "" {
		http.Error(w, "no API version", http.StatusBadRequest)
		return
	}
	rt, ok := githubRoutes()[r.URL.Path]
	if !ok || (rt.accept != "" && rt.accept != r.Header.Get("Accept")) {
		http.NotFound(w, r)
		return
	}
	if rt.status != 0 {
		http.Error(w, rt.body, rt.status)
		return
	}
	_, _ = w.Write([]byte(rt.body))
}

func (a *githubAPI) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

func (a *githubAPI) requested() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}

func newGitHub(t *testing.T) (*githubAPI, string) {
	t.Helper()
	api := &githubAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv.URL
}

func TestHTTPGitHubHeadAndFile(t *testing.T) {
	api, root := newGitHub(t)
	g := &HTTPGitHub{APIURL: root, Token: "gho_dotcom", TokenSource: "GH_TOKEN"}
	ctx := context.Background()
	const web = "https://github.com/acme/web"

	head, err := g.Head(ctx, web)
	if err != nil {
		t.Fatal(err)
	}
	if head != (Head{Branch: "trunk", SHA: sha("b")}) {
		t.Errorf("Head = %+v", head)
	}
	data, size, found, err := g.File(ctx, web, head.SHA, ".patchy/agent.yaml", 1024)
	if err != nil || !found || string(data) != agentYAML || size != int64(len(agentYAML)) {
		t.Errorf("File = %q, %d, %v, %v", data, size, found, err)
	}
	// Over the limit: found, sized from the tree, and the blob never read.
	before := len(api.requested())
	data, size, found, err = g.File(ctx, web, head.SHA, ".patchy/agent.yaml", 4)
	if err != nil || !found || data != nil || size != int64(len(agentYAML)) {
		t.Errorf("File over limit = %q, %d, %v, %v", data, size, found, err)
	}
	for _, p := range api.requested()[before:] {
		if strings.Contains(p, "/git/blobs/") {
			t.Errorf("an oversize file's blob was read: %s", p)
		}
	}
	// Missing, at the root, in a directory that does not exist, and under
	// a path that is no directory: not found, no error.
	for _, p := range []string{"nothing", ".github/agent.yaml", ".devcontainer/devcontainer.json"} {
		if _, _, found, err := g.File(ctx, web, head.SHA, p, 1024); found || err != nil {
			t.Errorf("File(%s) = %v, %v, want not found", p, found, err)
		}
	}
	// A truncated listing that lacks the file proves nothing.
	if _, _, _, err := g.File(ctx, web, head.SHA, "big/agent.yaml", 1024); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Errorf("File in a truncated listing = %v, want a truncation error", err)
	}
	for _, auth := range api.seen() {
		if auth != "Bearer gho_dotcom" {
			t.Errorf("a github.com request carried %q", auth)
		}
	}
}

// TestHTTPGitHubFileRegularOnly: source-controller's archive read honours
// regular files only, so a symlink (which GitHub's contents API would
// follow to its target's content), a directory or a submodule at the
// declaration's path is not there, and its blob is never read.
func TestHTTPGitHubFileRegularOnly(t *testing.T) {
	api, root := newGitHub(t)
	g := &HTTPGitHub{APIURL: root}
	ctx := context.Background()
	for _, p := range []string{".patchy/link.yaml", ".patchy/conf", ".patchy/tool"} {
		data, size, found, err := g.File(ctx, "https://github.com/acme/web", sha("b"), p, 1024)
		if found || err != nil || data != nil || size != 0 {
			t.Errorf("File(%s) = %q, %d, %v, %v, want not found", p, data, size, found, err)
		}
	}
	data, _, found, err := g.File(ctx, "https://github.com/acme/web", sha("b"), ".patchy/run.sh", 1024)
	if !found || err != nil || string(data) != "true\n" {
		t.Errorf("an executable regular file = %q, %v, %v, want found", data, found, err)
	}
	for _, p := range api.requested() {
		if strings.Contains(p, "/git/blobs/") && !strings.HasSuffix(p, "/blob-run") {
			t.Errorf("read the blob of an entry that is not a regular file: %s", p)
		}
	}
}

func TestHTTPGitHubErrors(t *testing.T) {
	_, root := newGitHub(t)
	ctx := context.Background()
	cases := []struct {
		name string
		g    *HTTPGitHub
		repo string
		want []string
	}{
		{"private, anonymous", &HTTPGitHub{APIURL: root}, "https://github.com/acme/private",
			[]string{"404 Not Found", "(anonymously)", "GH_TOKEN or GITHUB_TOKEN"}},
		{"private, with a token", &HTTPGitHub{APIURL: root, Token: "t", TokenSource: "GITHUB_TOKEN"},
			"https://github.com/acme/private", []string{"(with GITHUB_TOKEN)"}},
		{"enterprise hint", &HTTPGitHub{APIURL: root}, "https://ghe.acme.test/acme/private",
			[]string{"GH_ENTERPRISE_TOKEN or GITHUB_ENTERPRISE_TOKEN", "GH_HOST=ghe.acme.test"}},
		{"enterprise token withheld", &HTTPGitHub{APIURL: root, EnterpriseToken: "t",
			EnterpriseTokenSource: "GH_ENTERPRISE_TOKEN", EnterpriseHost: "ghe.corp.example"},
			"https://ghe.acme.test/acme/private", []string{"(anonymously)",
				"GH_ENTERPRISE_TOKEN is sent only to the host GH_HOST names", "GH_HOST=ghe.acme.test"}},
		{"not a SHA", &HTTPGitHub{APIURL: root}, "https://github.com/acme/bad", []string{`"not a sha"`}},
		{"not a repository URL", &HTTPGitHub{APIURL: root}, "https://github.com/acme", []string{"acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.g.Head(ctx, tc.repo)
			if err == nil {
				t.Fatal("Head succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
		})
	}
}

// TestHTTPGitHubTokenStaysOnItsHost: a repository URL comes from cluster
// configuration, so whoever wrote it picks the host. A github.com token is
// never sent about a repository on another host, an enterprise token never
// to github.com, and never to any host but the one GH_HOST names.
func TestHTTPGitHubTokenStaysOnItsHost(t *testing.T) {
	ctx := context.Background()
	heads := func(g *HTTPGitHub, api *githubAPI, repos ...string) []string {
		t.Helper()
		for _, repo := range repos {
			_, _ = g.Head(ctx, repo)
		}
		return api.seen()
	}

	t.Run("github.com token, another host", func(t *testing.T) {
		api, root := newGitHub(t)
		g := &HTTPGitHub{APIURL: root, Token: "gho_dotcom", TokenSource: "GH_TOKEN"}
		for _, auth := range heads(g, api, "https://evil.example/acme/web") {
			if auth != "" {
				t.Errorf("a request about another host carried %q", auth)
			}
		}
	})

	// The regression: an operator holds an enterprise token for their own
	// GHES host, and a Project names a host someone else runs.
	for _, ghHost := range []string{"", "ghe.corp.example", "github.com"} {
		t.Run("enterprise token, unrelated host, GH_HOST="+ghHost, func(t *testing.T) {
			api, root := newGitHub(t)
			g := &HTTPGitHub{APIURL: root, Token: "gho_dotcom", EnterpriseToken: "ghe_token",
				EnterpriseTokenSource: "GH_ENTERPRISE_TOKEN", EnterpriseHost: ghHost}
			got := heads(g, api, "https://attacker.example/acme/web")
			if len(got) == 0 {
				t.Fatal("no request was made")
			}
			for _, auth := range got {
				if auth != "" {
					t.Errorf("a request about attacker.example carried %q", auth)
				}
			}
		})
	}

	for _, ghHost := range []string{"ghe.acme.test", "GHE.acme.test", "https://ghe.acme.test/", "ghe.acme.test:8443"} {
		t.Run("enterprise token, its own host, GH_HOST="+ghHost, func(t *testing.T) {
			api, root := newGitHub(t)
			g := &HTTPGitHub{APIURL: root, Token: "gho_dotcom", EnterpriseToken: "ghe_token",
				EnterpriseTokenSource: "GH_ENTERPRISE_TOKEN", EnterpriseHost: ghHost}
			got := heads(g, api, "https://GHE.acme.test/acme/web", "https://github.com/acme/web",
				"https://other.acme.test/acme/web")
			want := []string{"Bearer ghe_token", "Bearer ghe_token", "Bearer gho_dotcom", "Bearer gho_dotcom", "", ""}
			if !slices.Equal(got, want) {
				t.Errorf("Authorization headers = %q, want %q", got, want)
			}
		})
	}
}

func TestRepoAPI(t *testing.T) {
	g := &HTTPGitHub{}
	cases := map[string]string{
		"https://github.com/acme/web":         "https://api.github.com/repos/acme/web",
		"https://github.com/acme/web.git":     "https://api.github.com/repos/acme/web",
		"https://ghe.acme.test/acme/shop-api": "https://ghe.acme.test/api/v3/repos/acme/shop-api",
	}
	for in, want := range cases {
		got, _, err := g.repoAPI(in)
		if err != nil || got != want {
			t.Errorf("repoAPI(%s) = %s, %v, want %s", in, got, err, want)
		}
	}
}
