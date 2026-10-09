// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestStoreResolveListsNamespace: Store.Resolve considers only the Forges in
// the asked namespace and resolves the repository against them.
func TestStoreResolveListsNamespace(t *testing.T) {
	inNS := mkForge("gh", "", []string{"acme"}, nil)
	inNS.Namespace = "ns"
	elsewhere := mkForge("gh-other", "", nil, nil)
	elsewhere.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(&inNS, &elsewhere).Build()
	s := NewStore(c)
	ctx := context.Background()

	res, err := s.Resolve(ctx, "ns", "https://github.com/acme/app")
	if err != nil {
		t.Fatalf("Resolve(acme/app) error = %v", err)
	}
	if res.Forge.Name != "gh" || res.Repo != (ghclient.Repo{Owner: "acme", Name: "app"}) || res.Host != "github.com" {
		t.Errorf("Resolve(acme/app) = %s %+v %q, want gh acme/app github.com", res.Forge.Name, res.Repo, res.Host)
	}

	// An org the namespace's only Forge does not cover is no match, even
	// though an unconstrained Forge in another namespace would cover it.
	if _, err := s.Resolve(ctx, "ns", "https://github.com/other/app"); !errors.Is(err, ErrNoMatch) {
		t.Errorf("Resolve(other/app) error = %v, want ErrNoMatch", err)
	}
	if res, err := s.Resolve(ctx, "other", "https://github.com/other/app"); err != nil || res.Forge.Name != "gh-other" {
		t.Errorf("Resolve in other namespace = %v, %v, want gh-other", res, err)
	}
	if _, err := s.Resolve(ctx, "ns", "ftp://github.com/acme/app"); err == nil {
		t.Error("Resolve(ftp url) error = nil, want a parse refusal")
	}
}

// TestStoreResolveListError: a failing Forge list surfaces wrapped, never as
// a no-match.
func TestStoreResolveListError(t *testing.T) {
	boom := errors.New("apiserver down")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	}).Build()
	_, err := NewStore(c).Resolve(context.Background(), "ns", "https://github.com/acme/app")
	if !errors.Is(err, boom) || errors.Is(err, ErrNoMatch) {
		t.Errorf("Resolve() error = %v, want the wrapped list error", err)
	}
	if err == nil || !strings.Contains(err.Error(), "list forges") {
		t.Errorf("Resolve() error = %v, want it to name the list", err)
	}
}

// TestStoreMissingSecret: every credential operation reports a missing
// Secret as a not-found error naming it, and never succeeds.
func TestStoreMissingSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	s := NewStore(c)
	ctx := context.Background()
	res := &Resolved{Forge: proxyForge("http://proxy.corp.example:3128"), Repo: ghclient.Repo{Owner: "o", Name: "r"}}

	ops := map[string]func() error{
		"Token": func() error {
			_, _, err := s.Token(ctx, res, ScopeRead)
			return err
		},
		"TokenWith": func() error {
			_, _, err := s.TokenWith(ctx, res, ghclient.TokenPerms{Issues: ghclient.PermRead})
			return err
		},
		"BotLogin": func() error {
			_, err := s.BotLogin(ctx, res)
			return err
		},
		"Client": func() error {
			_, err := s.Client(ctx, res)
			return err
		},
		"Validate": func() error { return s.Validate(ctx, res.Forge) },
		"ProxyURL": func() error {
			_, err := s.ProxyURL(ctx, res)
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op()
			if !apierrors.IsNotFound(err) {
				t.Fatalf("%s() error = %v, want a not-found", name, err)
			}
			if !strings.Contains(err.Error(), "ns/cred") {
				t.Errorf("%s() error = %v, want it to name the Secret ns/cred", name, err)
			}
		})
	}
}

// TestStoreUnusableAppCredential: a Secret that is neither a PAT nor a
// parseable App credential is refused by every operation that needs the App,
// before anything reaches a forge.
func TestStoreUnusableAppCredential(t *testing.T) {
	data := map[string][]byte{"appID": []byte("7"), "privateKey": []byte("not a pem key")}
	s, _ := tokenStore(t, data)
	ctx := context.Background()
	res := resolvedAt("http://127.0.0.1:1", "r")

	if _, _, err := s.Token(ctx, res, ScopeRead); err == nil {
		t.Error("Token() error = nil, want an App credential refusal")
	}
	if _, _, err := s.TokenWith(ctx, res, ghclient.TokenPerms{Issues: ghclient.PermRead}); err == nil {
		t.Error("TokenWith() error = nil, want an App credential refusal")
	}
	if _, err := s.BotLogin(ctx, res); err == nil {
		t.Error("BotLogin() error = nil, want an App credential refusal")
	}
	if _, err := s.Client(ctx, res); err == nil {
		t.Error("Client() error = nil, want an App credential refusal")
	}
	if err := s.Validate(ctx, res.Forge); err == nil {
		t.Error("Validate() error = nil, want an App credential refusal")
	}
}

// TestStoreClientPAT: a PAT Forge's client authenticates with the static
// token against the Forge's own API endpoint.
func TestStoreClientPAT(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"default_branch":"trunk"}`))
	})
	base := serve(t, mux)
	s, _ := tokenStore(t, map[string][]byte{"token": []byte("pat-token")})

	gh, err := s.Client(context.Background(), resolvedAt(base, "r"))
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	branch, err := gh.DefaultBranch(context.Background(), ghclient.Repo{Owner: "o", Name: "r"})
	if err != nil {
		t.Fatalf("DefaultBranch() error = %v", err)
	}
	if branch != "trunk" {
		t.Errorf("DefaultBranch() = %q, want trunk", branch)
	}
	if gotAuth != "Bearer pat-token" && gotAuth != "token pat-token" {
		t.Errorf("Authorization = %q, want the PAT", gotAuth)
	}
}

// TestStoreClientPATBadProxy: a PAT Forge whose spec proxy is unusable gets
// no client.
func TestStoreClientPATBadProxy(t *testing.T) {
	s := storeWith(t, map[string][]byte{"token": []byte("pat")})
	res := &Resolved{Forge: proxyForge("socks5://proxy.corp.example:1080"), Repo: ghclient.Repo{Owner: "o", Name: "r"}}
	if gh, err := s.Client(context.Background(), res); err == nil {
		t.Errorf("Client(socks proxy) = %v, want an error", gh)
	}
}

// TestStoreClientApp: an App Forge's client is the repository's installation
// client, authenticated with a token minted for that installation.
func TestStoreClientApp(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42}`))
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_inst","expires_at":"2099-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	})
	base := serve(t, mux)
	s, _ := tokenStore(t, appSecretData(t))
	gh, err := s.Client(context.Background(), resolvedAt(base, "r"))
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	branch, err := gh.DefaultBranch(context.Background(), ghclient.Repo{Owner: "o", Name: "r"})
	if err != nil {
		t.Fatalf("DefaultBranch() error = %v", err)
	}
	if branch != "main" {
		t.Errorf("DefaultBranch() = %q, want main", branch)
	}
	if !strings.HasSuffix(gotAuth, "ghs_inst") {
		t.Errorf("Authorization = %q, want the installation token", gotAuth)
	}
}

// TestStoreValidateApp: a parseable App credential validates without any
// network call.
func TestStoreValidateApp(t *testing.T) {
	s, _ := tokenStore(t, appSecretData(t))
	f := proxyForge("")
	if err := s.Validate(context.Background(), f); err != nil {
		t.Errorf("Validate(App credential) = %v, want nil", err)
	}
}

// TestStoreTokenPAT: Token on a PAT Forge returns the static token with a
// zero expiry at either scope.
func TestStoreTokenPAT(t *testing.T) {
	s, _ := tokenStore(t, map[string][]byte{"token": []byte("pat-token")})
	for _, scope := range []Scope{ScopeRead, ScopeWrite} {
		tok, exp, err := s.Token(context.Background(), resolvedAt("", "r"), scope)
		if err != nil || tok != "pat-token" || !exp.IsZero() {
			t.Errorf("Token(%s) = %q, %v, %v, want the PAT with a zero expiry", scope, tok, exp, err)
		}
	}
}

// TestTokenWithMintFailure: a failed mint is returned and nothing is cached.
func TestTokenWithMintFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	base := serve(t, mux)
	s, _ := tokenStore(t, appSecretData(t))
	if tok, _, err := s.TokenWith(context.Background(), resolvedAt(base, "r"),
		ghclient.TokenPerms{Issues: ghclient.PermRead}); err == nil {
		t.Errorf("TokenWith() = %q, want the mint error", tok)
	}
	if len(s.tokens) != 0 {
		t.Errorf("cache holds %d tokens after a failed mint, want none", len(s.tokens))
	}
}

// TestHostUnparseableBase: a baseURL that does not parse as a URL is matched
// as the lower-cased raw value.
func TestHostUnparseableBase(t *testing.T) {
	f := &v1alpha1.Forge{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	f.Spec.BaseURL = "https://GHE.Example:bad port"
	if got := Host(f); got != strings.ToLower("https://GHE.Example:bad port") {
		t.Errorf("Host(%q) = %q, want the lower-cased raw value", f.Spec.BaseURL, got)
	}
}

// serve starts a GHES-shaped fake API (REST under /api/v3) and returns its
// base URL.
func serve(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/api/v3", mux))
	t.Cleanup(srv.Close)
	return srv.URL
}
