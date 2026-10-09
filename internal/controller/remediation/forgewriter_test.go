// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// fakeGitHub is a GHES-shaped API serving what forgeWriter drives: the Git
// Data push, the open-PR lookup, the repository and PR creation.
type fakeGitHub struct {
	mu        sync.Mutex
	existing  string // JSON array the PR list answers with
	repoFails bool
	prFails   bool
	auth      []string
	created   []map[string]any
	refs      []string
}

// snapshot copies what the fake recorded, under its lock (handlers run on
// server goroutines).
func (g *fakeGitHub) snapshot() (auth, refs []string, created []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...), append([]string(nil), g.refs...),
		append([]map[string]any(nil), g.created...)
}

func (g *fakeGitHub) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
	}
	reply := func(w http.ResponseWriter, code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("POST /repos/acme/orders/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, http.StatusCreated, `{"sha":"blob1"}`)
	})
	mux.HandleFunc("POST /repos/acme/orders/git/trees", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, http.StatusCreated, `{"sha":"tree1"}`)
	})
	mux.HandleFunc("POST /repos/acme/orders/git/commits", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		reply(w, http.StatusCreated, `{"sha":"commit1"}`)
	})
	mux.HandleFunc("POST /repos/acme/orders/git/refs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var body struct{ Ref string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.refs = append(g.refs, body.Ref)
		g.mu.Unlock()
		reply(w, http.StatusCreated, `{"ref":"`+body.Ref+`","object":{"sha":"commit1"}}`)
	})
	mux.HandleFunc("GET /repos/acme/orders/pulls", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if got := r.URL.Query().Get("head"); got != "acme:patchy/finding-aa-1" {
			t.Errorf("PR list head = %q, want acme:patchy/finding-aa-1", got)
		}
		reply(w, http.StatusOK, g.existing)
	})
	mux.HandleFunc("GET /repos/acme/orders", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if g.repoFails {
			reply(w, http.StatusInternalServerError, `{"message":"boom"}`)
			return
		}
		reply(w, http.StatusOK, `{"default_branch":"trunk"}`)
	})
	mux.HandleFunc("POST /repos/acme/orders/pulls", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if g.prFails {
			reply(w, http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.created = append(g.created, body)
		g.mu.Unlock()
		reply(w, http.StatusCreated, `{"number":12,"html_url":"https://ghe.example/acme/orders/pull/12"}`)
	})
	srv := httptest.NewServer(http.StripPrefix("/api/v3", mux))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writerOver builds the production ForgeWriter over a fake cluster holding a
// PAT-credentialed Forge for acme at baseURL.
func writerOver(t *testing.T, baseURL string, withSecret bool) ForgeWriter {
	t.Helper()
	objs := []client.Object{&v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "ghe"},
		Spec: v1alpha1.ForgeSpec{
			Provider:  v1alpha1.ForgeProviderGitHub,
			BaseURL:   baseURL,
			Orgs:      []string{"acme"},
			SecretRef: v1alpha1.LocalSecretReference{Name: "ghe-cred"},
		},
	}}
	if withSecret {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "ghe-cred"},
			Data:       map[string][]byte{forge.SecretKeyToken: []byte("pat-write")},
		})
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).Build()
	return NewForgeWriter(forge.NewStore(c))
}

// repoURLOn is acme/orders on the fake forge's host.
func repoURLOn(base string) string {
	return base + "/acme/orders"
}

var writerChangeset = &envelope.Changeset{
	BaseSHA:       "abc123",
	CommitMessage: "fix",
	Upserts:       []envelope.FileChange{{Path: "a.go", Mode: "100644", ContentB64: "eA=="}},
}

// TestForgeWriterPush: the push resolves the covering Forge, authenticates
// with its credential and lands the branch on that Forge's API.
func TestForgeWriterPush(t *testing.T) {
	gh := &fakeGitHub{}
	base := gh.serve(t)
	w := writerOver(t, base, true)
	commit, err := w.Push(t.Context(), "patchy", repoURLOn(base), "patchy/finding-aa-1", writerChangeset)
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if commit != "commit1" {
		t.Errorf("Push() = %q, want commit1", commit)
	}
	auth, refs, _ := gh.snapshot()
	if len(refs) != 1 || refs[0] != "refs/heads/patchy/finding-aa-1" {
		t.Errorf("refs = %v, want the patchy branch", refs)
	}
	if len(auth) == 0 {
		t.Error("forge saw no requests")
	}
	for _, a := range auth {
		if !strings.HasSuffix(a, "pat-write") {
			t.Errorf("Authorization = %q, want the Forge credential", a)
		}
	}
}

// TestForgeWriterPushErrors: no covering Forge, or no credential, pushes
// nothing.
func TestForgeWriterPushErrors(t *testing.T) {
	gh := &fakeGitHub{}
	base := gh.serve(t)
	if _, err := writerOver(t, base, true).Push(t.Context(), "patchy", base+"/other/orders", "b",
		writerChangeset); !errors.Is(err, forge.ErrNoMatch) {
		t.Errorf("Push(uncovered org) error = %v, want ErrNoMatch", err)
	}
	_, err := writerOver(t, base, false).Push(t.Context(), "patchy", repoURLOn(base), "b", writerChangeset)
	if err == nil || !strings.Contains(err.Error(), "mint push token") {
		t.Errorf("Push(no secret) error = %v, want a token failure", err)
	}
	if auth, _, _ := gh.snapshot(); len(auth) != 0 {
		t.Errorf("forge saw %d requests, want none", len(auth))
	}
}

// TestForgeWriterEnsurePR: an open PR for the head branch is reused, never
// duplicated; otherwise one is opened against the default branch.
func TestForgeWriterEnsurePR(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		gh := &fakeGitHub{existing: `[{"number":7,"html_url":"https://ghe.example/acme/orders/pull/7"}]`}
		base := gh.serve(t)
		n, url, err := writerOver(t, base, true).EnsurePR(t.Context(), "patchy", repoURLOn(base),
			"patchy/finding-aa-1", "title", "body")
		if err != nil {
			t.Fatalf("EnsurePR() error = %v", err)
		}
		if n != 7 || url != "https://ghe.example/acme/orders/pull/7" {
			t.Errorf("EnsurePR() = %d %q, want the existing #7", n, url)
		}
		if _, _, created := gh.snapshot(); len(created) != 0 {
			t.Errorf("created %d PRs, want none", len(created))
		}
	})
	t.Run("new", func(t *testing.T) {
		gh := &fakeGitHub{existing: `[]`}
		base := gh.serve(t)
		n, url, err := writerOver(t, base, true).EnsurePR(t.Context(), "patchy", repoURLOn(base),
			"patchy/finding-aa-1", "Fix CVE", "the body")
		if err != nil {
			t.Fatalf("EnsurePR() error = %v", err)
		}
		if n != 12 || url != "https://ghe.example/acme/orders/pull/12" {
			t.Errorf("EnsurePR() = %d %q, want the new #12", n, url)
		}
		_, _, created := gh.snapshot()
		if len(created) != 1 {
			t.Fatalf("created %d PRs, want 1", len(created))
		}
		got := created[0]
		if got["head"] != "patchy/finding-aa-1" || got["base"] != "trunk" || got["title"] != "Fix CVE" ||
			got["body"] != "the body" {
			t.Errorf("PR request = %v, want head/base/title/body passed through", got)
		}
	})
}

// TestForgeWriterEnsurePRErrors: every failure on the way to a PR is
// returned rather than reported as an opened PR.
func TestForgeWriterEnsurePRErrors(t *testing.T) {
	tests := []struct {
		name   string
		gh     *fakeGitHub
		secret bool
		repo   func(base string) string
	}{
		{"uncovered", &fakeGitHub{existing: `[]`}, true, func(b string) string { return b + "/other/orders" }},
		{"no credential", &fakeGitHub{existing: `[]`}, false, repoURLOn},
		{"default branch", &fakeGitHub{existing: `[]`, repoFails: true}, true, repoURLOn},
		{"create", &fakeGitHub{existing: `[]`, prFails: true}, true, repoURLOn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.gh.serve(t)
			n, url, err := writerOver(t, base, tt.secret).EnsurePR(t.Context(), "patchy", tt.repo(base),
				"patchy/finding-aa-1", "t", "b")
			if err == nil {
				t.Fatalf("EnsurePR() = %d %q, want an error", n, url)
			}
			if n != 0 || url != "" {
				t.Errorf("EnsurePR() = %d %q alongside an error, want zero values", n, url)
			}
		})
	}
}
