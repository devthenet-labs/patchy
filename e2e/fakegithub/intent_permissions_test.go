// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/controller/intent"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/ghsecret"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// Exercise the production credential boundary, including two surfaces on the
// SAME repository. Choosing scope from a repository name cannot pass this test.
func TestIntentPRConversationPermissions(t *testing.T) {
	srv, pat, _ := newFake(t)
	ctx := context.Background()
	pr, err := pat.CreatePR(ctx, target, ghclient.PRRequest{Title: "t", Head: "patchy-intent/target-1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	issue := srv.OpenIssue(target.Owner, target.Name, "t", "b", nil, human)
	key, err := appKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &v1alpha1.Forge{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "github"},
		Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, BaseURL: srv.URL,
			SecretRef: v1alpha1.LocalSecretReference{Name: "github"}}}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "github"},
		Data: map[string][]byte{ghsecret.KeyAppID: []byte("1"), ghsecret.KeyPrivateKey: key}}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(f, s).Build()
	g := intent.NewForgeGitHub(forge.NewStore(c), "patchy")
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repo := u.Scheme + "://" + u.Host + "/" + target.String()
	comment, err := g.CreatePullRequestComment(ctx, repo, int64(pr.Number), "round notice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ListPullRequestComments(ctx, repo, int64(pr.Number), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.GetPullRequestComment(ctx, repo, comment.ID); err != nil {
		t.Fatal(err)
	}
	if edited, err := g.PullRequestCommentEdited(ctx, repo, comment.NodeID); err != nil || edited {
		t.Fatalf("unedited PR comment: %t, %v", edited, err)
	}
	if err := g.ReactPullRequestComment(ctx, repo, comment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateIssueComment(ctx, repo, int64(issue), "issue notice"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateIssueComment(ctx, repo, int64(pr.Number), "wrong scope"); !ghclient.IsForbidden(err) {
		t.Fatalf("issue method on PR = %v, want forbidden", err)
	}
	for _, request := range srv.TokenRequests() {
		if len(request.Repositories) != 1 || request.Repositories[0] != target.Name || len(request.Permissions) != 1 {
			t.Errorf("token widened: %+v", request)
		}
	}
}

func TestPRCommentReadScopes(t *testing.T) {
	srv, pat, _ := newFake(t)
	ctx := context.Background()
	pr, err := pat.CreatePR(ctx, target, ghclient.PRRequest{Title: "t", Head: "patchy-intent/target-1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	id := srv.CommentAs(pr.Number, "feedback", human)
	comment, err := pat.GetIssueComment(ctx, target, id)
	if err != nil {
		t.Fatal(err)
	}
	app := newApp(t, srv)
	for _, allowed := range []bool{false, true} {
		perms := ghclient.TokenPerms{Issues: ghclient.PermRead}
		if allowed {
			perms = ghclient.TokenPerms{PullRequests: ghclient.PermRead}
		}
		c := scopedClient(t, srv, app, target, perms)
		for name, call := range map[string]func() error{
			"list":         func() error { _, err := c.ListIssueComments(ctx, target, pr.Number, time.Time{}); return err },
			"get":          func() error { _, err := c.GetIssueComment(ctx, target, id); return err },
			"edit history": func() error { _, err := c.CommentEdited(ctx, comment.NodeID); return err },
		} {
			if err := call(); (err == nil) != allowed {
				t.Errorf("%s allowed=%t: %v", name, allowed, err)
			}
		}
	}
}
