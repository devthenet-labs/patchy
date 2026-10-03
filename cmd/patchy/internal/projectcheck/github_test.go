// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// githubAPI is an in-memory GitHub REST API: one repository, acme/web, on
// main, holding .patchy/agent.yaml. It records the Authorization header of
// every request.
type githubAPI struct {
	mu    sync.Mutex
	auths []string
}

func (a *githubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.auths = append(a.auths, r.Header.Get("Authorization"))
	a.mu.Unlock()
	if r.Header.Get("X-GitHub-Api-Version") == "" {
		http.Error(w, "no API version", http.StatusBadRequest)
		return
	}
	switch {
	case r.URL.Path == "/repos/acme/web" && r.Header.Get("Accept") == "application/vnd.github+json":
		_, _ = w.Write([]byte(`{"default_branch": "trunk"}`))
	case r.URL.Path == "/repos/acme/web/commits/trunk" && r.Header.Get("Accept") == "application/vnd.github.sha":
		_, _ = w.Write([]byte(sha("b")))
	case r.URL.Path == "/repos/acme/bad/commits/main":
		_, _ = w.Write([]byte("not a sha"))
	case r.URL.Path == "/repos/acme/bad":
		_, _ = w.Write([]byte(`{"default_branch": "main"}`))
	case r.URL.Path == "/repos/acme/web/contents/.patchy/agent.yaml" && r.URL.Query().Get("ref") == sha("b") &&
		r.Header.Get("Accept") == "application/vnd.github.raw":
		_, _ = w.Write([]byte("image: ghcr.io/acme/web-agent:1\n"))
	default:
		http.NotFound(w, r)
	}
}

func (a *githubAPI) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
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

	head, err := g.Head(ctx, "https://github.com/acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if head != (Head{Branch: "trunk", SHA: sha("b")}) {
		t.Errorf("Head = %+v", head)
	}
	data, size, found, err := g.File(ctx, "https://github.com/acme/web", head.SHA, ".patchy/agent.yaml", 1024)
	if err != nil || !found || string(data) != "image: ghcr.io/acme/web-agent:1\n" || size != int64(len(data)) {
		t.Errorf("File = %q, %d, %v, %v", data, size, found, err)
	}
	// Over the limit: found, sized, no data.
	data, size, found, err = g.File(ctx, "https://github.com/acme/web", head.SHA, ".patchy/agent.yaml", 4)
	if err != nil || !found || data != nil || size <= 4 {
		t.Errorf("File over limit = %q, %d, %v, %v", data, size, found, err)
	}
	// Missing: not found, no error.
	if _, _, found, err := g.File(ctx, "https://github.com/acme/web", head.SHA, "nothing", 1024); found || err != nil {
		t.Errorf("missing File = %v, %v", found, err)
	}
	for _, auth := range api.seen() {
		if auth != "Bearer gho_dotcom" {
			t.Errorf("a github.com request carried %q", auth)
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
			[]string{"GH_ENTERPRISE_TOKEN or GITHUB_ENTERPRISE_TOKEN"}},
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
// configuration, so a github.com token is never sent about a repository on
// another host, and an enterprise token never to github.com.
func TestHTTPGitHubTokenStaysOnItsHost(t *testing.T) {
	api, root := newGitHub(t)
	g := &HTTPGitHub{APIURL: root, Token: "gho_dotcom", TokenSource: "GH_TOKEN"}
	_, _ = g.Head(context.Background(), "https://evil.example/acme/web")
	for _, auth := range api.seen() {
		if auth != "" {
			t.Errorf("a request about another host carried %q", auth)
		}
	}

	api, root = newGitHub(t)
	g = &HTTPGitHub{APIURL: root, Token: "gho_dotcom", EnterpriseToken: "ghe_token",
		EnterpriseTokenSource: "GH_ENTERPRISE_TOKEN"}
	if _, err := g.Head(context.Background(), "https://GHE.acme.test/acme/web"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Head(context.Background(), "https://github.com/acme/web"); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer ghe_token", "Bearer ghe_token", "Bearer gho_dotcom", "Bearer gho_dotcom"}
	if got := api.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Authorization headers = %q, want %q", got, want)
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
