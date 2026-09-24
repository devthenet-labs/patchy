// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// These tests drive the fake with the real client, so the two cannot drift
// apart: every behaviour here is one verified against GitHub (2026-09-24)
// that the intent flow depends on.

var (
	intents = ghclient.Repo{Owner: "devthenet-labs", Name: "intents"}
	target  = ghclient.Repo{Owner: "devthenet-labs", Name: "patchy-target"}
	human   = fakegithub.Actor{Login: "peter", ID: 42, Type: "User"}
)

// clock is a settable time source for the fake.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newFake starts the fake with a settable clock and a PAT client on it.
func newFake(t *testing.T) (*fakegithub.Server, *ghclient.Client, *clock) {
	t.Helper()
	srv := fakegithub.New()
	t.Cleanup(srv.Close)
	clk := &clock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	srv.Now = clk.now
	c, err := ghclient.NewToken("e2e-token", srv.URL)
	if err != nil {
		t.Fatalf("NewToken() error = %v", err)
	}
	return srv, c, clk
}

// appKey is one throwaway App private key for the whole package.
var appKey = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
})

// newApp authenticates as the fake's App.
func newApp(t *testing.T, srv *fakegithub.Server) *ghclient.App {
	t.Helper()
	key, err := appKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	app, err := ghclient.NewApp(ghclient.AppConfig{AppID: 1, PrivateKey: key, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
	return app
}

// botClient is an installation client on the fake: an unscoped installation
// token, whose writes are the App's bot's.
func botClient(t *testing.T, srv *fakegithub.Server) *ghclient.Client {
	t.Helper()
	c, err := newApp(t, srv).Installation(context.Background(), intents)
	if err != nil {
		t.Fatalf("Installation() error = %v", err)
	}
	return c
}

// scopedClient is a client holding one token minted for repo and perms.
func scopedClient(t *testing.T, srv *fakegithub.Server, app *ghclient.App,
	repo ghclient.Repo, perms ghclient.TokenPerms,
) *ghclient.Client {
	t.Helper()
	tok, _, err := app.ScopedToken(context.Background(), repo, perms)
	if err != nil {
		t.Fatalf("ScopedToken(%s, %+v) error = %v", repo, perms, err)
	}
	c, err := ghclient.NewToken(tok, srv.URL)
	if err != nil {
		t.Fatalf("NewToken() error = %v", err)
	}
	return c
}

func TestConditionalIssueList(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	n := srv.OpenIssue(intents.Owner, intents.Name, "Add GET /version", "as JSON", []string{"patchy:target"}, human)

	first, err := c.ListIssues(ctx, intents, []string{"patchy:target"}, "open", "")
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if first.NotModified || first.ETag == "" || len(first.Issues) != 1 {
		t.Fatalf("first ListIssues() = %+v, want one issue and an ETag", first)
	}
	is := first.Issues[0]
	if is.Number != n || is.Author != "peter" || is.HTMLURL != "https://github.com/devthenet-labs/intents/issues/101" {
		t.Errorf("issue = %+v, want #%d by peter with its page URL", is, n)
	}

	again, err := c.ListIssues(ctx, intents, []string{"patchy:target"}, "open", first.ETag)
	if err != nil {
		t.Fatalf("conditional ListIssues() error = %v", err)
	}
	if !again.NotModified || again.ETag != first.ETag {
		t.Errorf("conditional ListIssues() = %+v, want NotModified", again)
	}

	srv.LabelIssue(n, "patchy:approved", human)
	changed, err := c.ListIssues(ctx, intents, []string{"patchy:target"}, "open", first.ETag)
	if err != nil {
		t.Fatalf("changed ListIssues() error = %v", err)
	}
	if changed.NotModified || changed.ETag == first.ETag {
		t.Errorf("changed ListIssues() = %+v, want a fresh listing with a new tag", changed)
	}

	srv.CloseIssueAs(n, "not_planned", human)
	for state, want := range map[string]int{"open": 0, "closed": 1, "all": 1} {
		got, err := c.ListIssues(ctx, intents, []string{"patchy:target"}, state, "")
		if err != nil {
			t.Fatalf("ListIssues(%s) error = %v", state, err)
		}
		if len(got.Issues) != want {
			t.Errorf("ListIssues(%s) = %d issues, want %d", state, len(got.Issues), want)
		}
	}
}

// TestIssueEventActors: a human's label carries the human; everything an
// installation token does carries the App's bot, everything a PAT does its
// user (as GitHub attributes them), and label events carry no App slug.
func TestIssueEventActors(t *testing.T) {
	asActor := func(a fakegithub.Actor) ghclient.Actor {
		return ghclient.Actor{Login: a.Login, ID: a.ID, Type: a.Type}
	}
	tests := []struct {
		name   string
		client func(*testing.T, *fakegithub.Server, *ghclient.Client) *ghclient.Client
		want   ghclient.Actor
	}{
		{
			name: "installation token",
			client: func(t *testing.T, srv *fakegithub.Server, _ *ghclient.Client) *ghclient.Client {
				return botClient(t, srv)
			},
			want: asActor(fakegithub.Bot),
		},
		{
			name:   "personal access token",
			client: func(_ *testing.T, _ *fakegithub.Server, pat *ghclient.Client) *ghclient.Client { return pat },
			want:   asActor(fakegithub.PATUser),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, pat, _ := newFake(t)
			c := tt.client(t, srv, pat)
			ctx := context.Background()
			n := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", []string{"patchy:target"}, human)
			if err := c.AddLabels(ctx, intents, n, []string{"patchy:approved"}); err != nil {
				t.Fatalf("AddLabels() error = %v", err)
			}
			if err := c.RemoveLabel(ctx, intents, n, "patchy:approved"); err != nil {
				t.Fatalf("RemoveLabel() error = %v", err)
			}
			if err := c.RemoveLabel(ctx, intents, n, "patchy:approved"); err != nil {
				t.Fatalf("RemoveLabel(absent) error = %v, want the 404 as success", err)
			}
			if err := c.CloseIssue(ctx, intents, n, ghclient.CloseCompleted); err != nil {
				t.Fatalf("CloseIssue() error = %v", err)
			}

			events, err := c.ListIssueEvents(ctx, intents, n)
			if err != nil {
				t.Fatalf("ListIssueEvents() error = %v", err)
			}
			type seen struct {
				event, label string
				actor        ghclient.Actor
			}
			got := make([]seen, 0, len(events))
			for _, ev := range events {
				if ev.ID == 0 || ev.NodeID == "" || ev.CreatedAt.IsZero() || ev.ViaApp != "" {
					t.Errorf("event %+v: want id, node_id, created_at and no performed_via_github_app", *ev)
				}
				got = append(got, seen{ev.Event, ev.Label, ev.Actor})
			}
			want := []seen{
				{"labeled", "patchy:target", asActor(human)},
				{"labeled", "patchy:approved", tt.want},
				{"unlabeled", "patchy:approved", tt.want},
				{"closed", "", tt.want},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("events = %+v, want %+v", got, want)
			}

			is, err := c.GetIssue(ctx, intents, n)
			if err != nil {
				t.Fatalf("GetIssue() error = %v", err)
			}
			if is.State != "closed" {
				t.Errorf("issue state = %q, want closed", is.State)
			}
		})
	}
}

// TestCommentsSince: the App's comments carry its bot and slug, a human's
// neither; since filters on last update, so an edit brings an older comment
// back, and a re-read shows the edit.
func TestCommentsSince(t *testing.T) {
	srv, _, clk := newFake(t)
	c := botClient(t, srv)
	ctx := context.Background()
	n := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", nil, human)

	planID, err := c.CreateComment(ctx, intents, n, "<!-- patchy:plan --> r1")
	if err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	postedAt := clk.now()
	clk.advance(time.Minute)
	cmdID := srv.CommentAs(n, "/patchy approve", human)

	all, err := c.ListIssueComments(ctx, intents, n, time.Time{})
	if err != nil {
		t.Fatalf("ListIssueComments() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListIssueComments() = %d comments, want 2", len(all))
	}
	if a := all[0].Author(); !a.IsBot() || a.Login != fakegithub.BotLogin || all[0].ViaApp != fakegithub.AppSlug {
		t.Errorf("plan comment author %+v via %q, want the App's bot", a, all[0].ViaApp)
	}
	if a := all[1].Author(); a.IsBot() || a.Login != "peter" || all[1].ViaApp != "" {
		t.Errorf("command author %+v via %q, want the human", a, all[1].ViaApp)
	}

	later, err := c.ListIssueComments(ctx, intents, n, postedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("ListIssueComments(since) error = %v", err)
	}
	if len(later) != 1 || later[0].ID != cmdID {
		t.Errorf("ListIssueComments(since) = %d comments, want only the command", len(later))
	}

	// Editing the plan comment moves its updated_at past since and changes
	// what a re-read returns.
	clk.advance(time.Minute)
	if !srv.EditCommentBody(planID, "tampered") {
		t.Fatal("EditCommentBody() = false")
	}
	later, err = c.ListIssueComments(ctx, intents, n, postedAt.Add(time.Second))
	if err != nil || len(later) != 2 {
		t.Errorf("ListIssueComments(since) after edit = %d, %v, want both comments", len(later), err)
	}
	plan, err := c.GetIssueComment(ctx, intents, planID)
	if err != nil || plan.Body != "tampered" || !plan.UpdatedAt.After(plan.CreatedAt) {
		t.Errorf("GetIssueComment() = %+v, %v, want the edited body", plan, err)
	}
	if _, err := c.GetIssueComment(ctx, intents, 9999); !ghclient.IsNotFound(err) {
		t.Errorf("GetIssueComment(missing) error = %v, want not found", err)
	}
}

// TestCommentReactions: acknowledging a command twice leaves one reaction
// (GitHub's 200 for an existing one), and an unknown reaction or comment is
// refused.
func TestCommentReactions(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	n := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", nil, human)
	cmdID := srv.CommentAs(n, "/patchy approve", human)

	for range 2 {
		if err := c.CreateIssueCommentReaction(ctx, intents, cmdID, ghclient.ReactionEyes); err != nil {
			t.Fatalf("CreateIssueCommentReaction() error = %v", err)
		}
	}
	if got := srv.Reactions(cmdID); !reflect.DeepEqual(got, []string{"eyes"}) {
		t.Errorf("reactions = %v, want one eyes (idempotent)", got)
	}
	if err := c.CreateIssueCommentReaction(ctx, intents, cmdID, "shrug"); err == nil {
		t.Error("CreateIssueCommentReaction(shrug) error = nil, want 422")
	}
	if err := c.CreateIssueCommentReaction(ctx, intents, 9999, ghclient.ReactionEyes); err == nil {
		t.Error("CreateIssueCommentReaction(missing comment) error = nil, want 404")
	}
}

// TestRefSemantics is the verified Git refs table, end to end: create-only
// adoption, fast-forward-only moves, and the 422 message mapping — while
// the Finding flow's force-moving push keeps working.
func TestRefSemantics(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	commit := func(parent, msg string) string {
		t.Helper()
		sha, err := c.CreateCommit(ctx, target, ghclient.CommitRequest{
			BaseSHA: parent, Message: msg,
			Files: []ghclient.CommitFile{{Path: "v.go", Mode: "100644", Content: []byte(msg)}},
		})
		if err != nil {
			t.Fatalf("CreateCommit(%s) error = %v", msg, err)
		}
		return sha
	}
	const branch = "patchy-intent/target-1"
	build := commit(fakegithub.BaseSHA, "build")
	human := commit(build, "human")
	revise := commit(human, "revise")
	sideways := commit(fakegithub.BaseSHA, "sideways")

	steps := []struct {
		name      string
		do        func() error
		wantErr   bool
		wantIs    error
		wantHead  string
		notSentry bool // a plain error: neither sentinel
	}{
		{name: "create", do: func() error { return c.CreateBranchRef(ctx, target, branch, build) }, wantHead: build},
		{name: "create again at the same commit adopts", do: func() error {
			return c.CreateBranchRef(ctx, target, branch, build)
		}, wantHead: build},
		{name: "create at another commit refuses", do: func() error {
			return c.CreateBranchRef(ctx, target, branch, sideways)
		}, wantErr: true, wantIs: ghclient.ErrBranchExists, wantHead: build},
		{name: "create at an unknown commit", do: func() error {
			return c.CreateBranchRef(ctx, target, "patchy-intent/other", "feedface")
		}, wantErr: true, notSentry: true, wantHead: build},
		{name: "fast-forward", do: func() error { return c.FastForwardRef(ctx, target, branch, human) }, wantHead: human},
		{name: "same commit is a no-op", do: func() error { return c.FastForwardRef(ctx, target, branch, human) },
			wantHead: human},
		{name: "fast-forward two steps", do: func() error { return c.FastForwardRef(ctx, target, branch, revise) },
			wantHead: revise},
		{name: "rewind refuses", do: func() error { return c.FastForwardRef(ctx, target, branch, build) },
			wantErr: true, wantIs: ghclient.ErrNotFastForward, wantHead: revise},
		{name: "diverged refuses", do: func() error { return c.FastForwardRef(ctx, target, branch, sideways) },
			wantErr: true, wantIs: ghclient.ErrNotFastForward, wantHead: revise},
		{name: "unknown commit", do: func() error { return c.FastForwardRef(ctx, target, branch, "feedface") },
			wantErr: true, notSentry: true, wantHead: revise},
		{name: "missing branch", do: func() error {
			return c.FastForwardRef(ctx, target, "patchy-intent/none", revise)
		}, wantErr: true, wantIs: ghclient.ErrRefNotFound, wantHead: revise},
	}
	for _, step := range steps {
		err := step.do()
		if (err != nil) != step.wantErr {
			t.Fatalf("%s: error = %v, wantErr %v", step.name, err, step.wantErr)
		}
		if step.wantIs != nil && !errors.Is(err, step.wantIs) {
			t.Errorf("%s: error = %v, want %v", step.name, err, step.wantIs)
		}
		if step.notSentry && (errors.Is(err, ghclient.ErrBranchExists) ||
			errors.Is(err, ghclient.ErrNotFastForward) || errors.Is(err, ghclient.ErrRefNotFound)) {
			t.Errorf("%s: error = %v, want a plain error", step.name, err)
		}
		if head := srv.BranchHead(branch); head != step.wantHead {
			t.Errorf("%s: branch head = %s, want %s", step.name, head, step.wantHead)
		}
	}

	// The Finding flow's push still creates, then force-moves on a retry.
	for i := range 2 {
		sha, err := c.PushBranch(ctx, target, ghclient.BranchPush{
			Branch: "patchy/issue-9",
			CommitRequest: ghclient.CommitRequest{
				BaseSHA: fakegithub.BaseSHA, Message: "fix",
				Files: []ghclient.CommitFile{{Path: "a.go", Mode: "100644", Content: []byte{byte(i)}}},
			},
		})
		if err != nil {
			t.Fatalf("PushBranch() #%d error = %v", i, err)
		}
		if head := srv.BranchHead("patchy/issue-9"); head != sha {
			t.Errorf("PushBranch() #%d head = %s, want %s", i, head, sha)
		}
	}
}

// TestCollaboratorPermissions mirrors a public repository: anyone reads,
// the App's bot has none, a missing login is 404, and only roles with write
// pass CanWrite.
func TestCollaboratorPermissions(t *testing.T) {
	srv, c, _ := newFake(t)
	srv.SetRole("peter", "admin")
	srv.SetRole("maint", "maintain")
	srv.SetRole("tri", "triage")
	srv.MarkUserMissing("no-such-user")
	tests := []struct {
		login      string
		want       string
		canWrite   bool
		wantNoUser bool
	}{
		{login: "peter", want: "admin", canWrite: true},
		{login: "maint", want: "write", canWrite: true},
		{login: "tri", want: "read"},
		{login: "octocat", want: "read"},
		{login: fakegithub.BotLogin, want: "none"},
		{login: "no-such-user", wantNoUser: true},
	}
	for _, tt := range tests {
		got, err := c.CollaboratorPermission(context.Background(), intents, tt.login)
		if errors.Is(err, ghclient.ErrNoSuchUser) != tt.wantNoUser || (err != nil && !tt.wantNoUser) {
			t.Errorf("CollaboratorPermission(%s) error = %v, wantNoUser %v", tt.login, err, tt.wantNoUser)
		}
		if got != tt.want || ghclient.CanWrite(got) != tt.canWrite {
			t.Errorf("CollaboratorPermission(%s) = %q (CanWrite %v), want %q (%v)",
				tt.login, got, ghclient.CanWrite(got), tt.want, tt.canWrite)
		}
	}
}

// TestAppAuth: the App endpoints — slug, installation lookup and scoped
// tokens — work against the fake, which records each token's scope.
func TestAppAuth(t *testing.T) {
	srv, _, _ := newFake(t)
	app := newApp(t, srv)
	ctx := context.Background()
	if login, err := app.BotLogin(ctx); err != nil || login != fakegithub.BotLogin {
		t.Errorf("BotLogin() = %q, %v, want %s", login, err, fakegithub.BotLogin)
	}
	tok, exp, err := app.ScopedToken(ctx, intents, ghclient.TokenPerms{Issues: ghclient.PermWrite})
	if err != nil || tok == "" || !exp.After(srv.Now()) {
		t.Fatalf("ScopedToken() = %q, %v, %v, want a live token", tok, exp, err)
	}
	inst, err := app.Installation(ctx, intents)
	if err != nil {
		t.Fatalf("Installation() error = %v", err)
	}
	if _, err := inst.DefaultBranch(ctx, intents); err != nil {
		t.Fatalf("unscoped installation call error = %v", err)
	}

	reqs := srv.TokenRequests()
	if len(reqs) != 2 {
		t.Fatalf("token requests = %+v, want the scoped one then the unscoped one", reqs)
	}
	scoped := fakegithub.TokenRequest{Repositories: []string{"intents"}, Permissions: map[string]string{"issues": "write"}}
	if !reflect.DeepEqual(reqs[0], scoped) {
		t.Errorf("scoped token request = %+v, want %+v", reqs[0], scoped)
	}
	if len(reqs[1].Repositories) != 0 || len(reqs[1].Permissions) != 0 {
		t.Errorf("installation token request = %+v, want unscoped", reqs[1])
	}
}

// TestScopedTokenEnforcement: a minted token reaches only the repository and
// permissions it was minted for — GitHub's 403 "Resource not accessible by
// integration" anywhere else — while metadata is always readable and a
// pull_requests grant also covers the issues endpoints for a pull request.
func TestScopedTokenEnforcement(t *testing.T) {
	srv, pat, _ := newFake(t)
	app := newApp(t, srv)
	ctx := context.Background()
	intentIssue := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", nil, human)
	targetIssue := srv.OpenIssue(target.Owner, target.Name, "t", "b", nil, human)
	issueComment := srv.CommentAs(targetIssue, "on the issue", human)
	pr, err := pat.CreatePR(ctx, target, ghclient.PRRequest{Title: "t", Head: "patchy-intent/target-1", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	prComment := srv.CommentAs(pr.Number, "on the PR", human)

	issuesWrite := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Issues: ghclient.PermWrite})
	issuesRead := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Issues: ghclient.PermRead})
	contentsWrite := scopedClient(t, srv, app, target, ghclient.TokenPerms{Contents: ghclient.PermWrite})
	pullsWrite := scopedClient(t, srv, app, target, ghclient.TokenPerms{PullRequests: ghclient.PermWrite})

	commit := func(c *ghclient.Client, repo ghclient.Repo) error {
		_, err := c.CreateCommit(ctx, repo, ghclient.CommitRequest{
			BaseSHA: fakegithub.BaseSHA, Message: "m",
			Files: []ghclient.CommitFile{{Path: "a.go", Mode: "100644", Content: []byte("a")}},
		})
		return err
	}
	comment := func(c *ghclient.Client, repo ghclient.Repo, n int) error {
		_, err := c.CreateComment(ctx, repo, n, "hi")
		return err
	}
	openPR := func(c *ghclient.Client) error {
		_, err := c.CreatePR(ctx, target, ghclient.PRRequest{Title: "t", Head: "patchy-intent/target-2", Base: "main"})
		return err
	}
	tests := []struct {
		name          string
		call          func() error
		wantForbidden bool
	}{
		{name: "issues write labels", call: func() error {
			return issuesWrite.AddLabels(ctx, intents, intentIssue, []string{"patchy:approved"})
		}},
		{name: "issues write comments", call: func() error { return comment(issuesWrite, intents, intentIssue) }},
		{name: "issues write reads repository metadata", call: func() error {
			_, err := issuesWrite.DefaultBranch(ctx, intents)
			return err
		}},
		{name: "issues write reads a collaborator permission", call: func() error {
			_, err := issuesWrite.CollaboratorPermission(ctx, intents, "octocat")
			return err
		}},
		{name: "issues write in another repository", wantForbidden: true,
			call: func() error { return comment(issuesWrite, target, targetIssue) }},
		{name: "issues write pushes", wantForbidden: true, call: func() error { return commit(issuesWrite, intents) }},
		{name: "issues read lists", call: func() error {
			_, err := issuesRead.ListIssues(ctx, intents, nil, "open", "")
			return err
		}},
		{name: "issues read comments", wantForbidden: true,
			call: func() error { return comment(issuesRead, intents, intentIssue) }},
		{name: "contents write pushes", call: func() error { return commit(contentsWrite, target) }},
		{name: "contents write reads a branch", call: func() error {
			_, err := contentsWrite.HeadSHA(ctx, target, "main")
			return err
		}},
		{name: "contents write in another repository", wantForbidden: true,
			call: func() error { return commit(contentsWrite, intents) }},
		{name: "contents write comments", wantForbidden: true,
			call: func() error { return comment(contentsWrite, target, targetIssue) }},
		{name: "contents write opens a PR", wantForbidden: true, call: func() error { return openPR(contentsWrite) }},
		{name: "pull requests write opens a PR", call: func() error { return openPR(pullsWrite) }},
		{name: "pull requests write comments on a PR", call: func() error { return comment(pullsWrite, target, pr.Number) }},
		{name: "pull requests write reacts on a PR comment", call: func() error {
			return pullsWrite.CreateIssueCommentReaction(ctx, target, prComment, ghclient.ReactionEyes)
		}},
		{name: "pull requests write comments on an issue", wantForbidden: true,
			call: func() error { return comment(pullsWrite, target, targetIssue) }},
		{name: "pull requests write reacts on an issue comment", wantForbidden: true, call: func() error {
			return pullsWrite.CreateIssueCommentReaction(ctx, target, issueComment, ghclient.ReactionEyes)
		}},
		{name: "pull requests write reads a branch", wantForbidden: true, call: func() error {
			_, err := pullsWrite.HeadSHA(ctx, target, "main")
			return err
		}},
	}
	for _, tt := range tests {
		err := tt.call()
		switch {
		case !tt.wantForbidden && err != nil:
			t.Errorf("%s: error = %v, want success", tt.name, err)
		case tt.wantForbidden && (!ghclient.IsForbidden(err) ||
			!strings.Contains(err.Error(), "Resource not accessible by integration")):
			t.Errorf("%s: error = %v, want 403 Resource not accessible by integration", tt.name, err)
		}
	}
	// The refused calls changed nothing.
	if got := srv.Comments(targetIssue); !reflect.DeepEqual(got, []string{"on the issue"}) {
		t.Errorf("target issue comments = %q, want only the human's", got)
	}
	if got := srv.Reactions(issueComment); len(got) != 0 {
		t.Errorf("issue comment reactions = %v, want none", got)
	}
}

// TestIssueListOrder: the listing honours GitHub's sort and direction —
// ghclient's most-recently-updated-first among them — and a new comment
// moves an issue's updated_at and comment count, so the listing's tag moves
// and the issue comes to the top.
func TestIssueListOrder(t *testing.T) {
	srv, c, clk := newFake(t)
	ctx := context.Background()
	trigger := []string{"patchy:target"}
	open := func(title string) int {
		clk.advance(time.Minute)
		return srv.OpenIssue(intents.Owner, intents.Name, title, "b", trigger, human)
	}
	a, b, d := open("a"), open("b"), open("d")
	numbers := func(list *ghclient.IssueList) []int {
		out := make([]int, 0, len(list.Issues))
		for _, is := range list.Issues {
			out = append(out, is.Number)
		}
		return out
	}

	first, err := c.ListIssues(ctx, intents, trigger, "open", "")
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	if got := numbers(first); !reflect.DeepEqual(got, []int{d, b, a}) {
		t.Errorf("ListIssues() = %v, want most recently updated first %v", got, []int{d, b, a})
	}

	clk.advance(time.Minute)
	srv.CommentAs(a, "a question", human)
	afterComment, err := c.ListIssues(ctx, intents, trigger, "open", first.ETag)
	if err != nil {
		t.Fatalf("ListIssues() after comment error = %v", err)
	}
	if afterComment.NotModified || afterComment.ETag == first.ETag {
		t.Fatalf("ListIssues() after comment = NotModified %v, want a fresh listing", afterComment.NotModified)
	}
	if got := numbers(afterComment); !reflect.DeepEqual(got, []int{a, d, b}) {
		t.Errorf("ListIssues() after comment = %v, want the commented issue first %v", got, []int{a, d, b})
	}

	clk.advance(time.Minute)
	srv.LabelIssue(b, "patchy:approved", human)
	afterLabel, err := c.ListIssues(ctx, intents, trigger, "open", afterComment.ETag)
	if err != nil {
		t.Fatalf("ListIssues() after label error = %v", err)
	}
	if got := numbers(afterLabel); !reflect.DeepEqual(got, []int{b, a, d}) {
		t.Errorf("ListIssues() after label = %v, want the labelled issue first %v", got, []int{b, a, d})
	}

	// The other orders, on the raw endpoint.
	for query, want := range map[string][]int{
		"":                              {d, b, a}, // GitHub's default: created, desc
		"?direction=asc":                {a, b, d},
		"?sort=updated&direction=asc":   {d, a, b},
		"?sort=comments":                {a, d, b}, // one comment; ties newest number first
		"?sort=nonsense&direction=desc": {d, b, a},
	} {
		got := rawIssueNumbers(t, srv.URL+"/api/v3/repos/devthenet-labs/intents/issues"+query)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GET issues%s = %v, want %v", query, got, want)
		}
	}
}

// rawIssueNumbers GETs an issue listing and returns its issue numbers in
// order.
func rawIssueNumbers(t *testing.T, url string) []int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var issues []struct {
		Number int `json:"number"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	out := make([]int, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.Number)
	}
	return out
}

// TestRepositoryLabels: EnsureLabel creates a missing label once, leaves an
// existing one exactly as a human styled it (found case-insensitively, as
// GitHub finds labels), and needs the issues permission.
func TestRepositoryLabels(t *testing.T) {
	srv, _, _ := newFake(t)
	app := newApp(t, srv)
	ctx := context.Background()
	c := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Issues: ghclient.PermWrite})
	srv.SeedRepoLabel(intents.Owner, intents.Name, "patchy:approved", "ffffff")

	steps := []struct {
		name, label string
		wantCreated bool
	}{
		{"a missing label is created", "patchy:target", true},
		{"then found", "patchy:target", false},
		{"found case-insensitively", "PATCHY:TARGET", false},
		{"a human's label is kept", "patchy:approved", false},
	}
	for _, step := range steps {
		created, err := c.EnsureLabel(ctx, intents, step.label, "5319e7", "patchy: plan this")
		if err != nil || created != step.wantCreated {
			t.Errorf("%s: EnsureLabel(%s) = %v, %v, want %v", step.name, step.label, created, err, step.wantCreated)
		}
	}
	want := []fakegithub.RepoLabel{
		{ID: 1, Name: "patchy:approved", Color: "ffffff"},
		{ID: 2, Name: "patchy:target", Color: "5319e7", Description: "patchy: plan this"},
	}
	if got := srv.RepoLabels(intents.Owner, intents.Name); !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %+v, want %+v", got, want)
	}
	if got := srv.RepoLabels(target.Owner, target.Name); len(got) != 0 {
		t.Errorf("labels of another repository = %+v, want none", got)
	}

	contents := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Contents: ghclient.PermWrite})
	if _, err := contents.EnsureLabel(ctx, intents, "patchy:other", "", ""); !ghclient.IsForbidden(err) {
		t.Errorf("EnsureLabel with a contents token error = %v, want 403", err)
	}
}

// TestRateRemaining: GET /rate_limit reports the core budget, which a test
// can spend down.
func TestRateRemaining(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	if got, err := c.RateRemaining(ctx); err != nil || got != 5000 {
		t.Errorf("RateRemaining() = %d, %v, want 5000", got, err)
	}
	srv.SetRateRemaining(12)
	if got, err := c.RateRemaining(ctx); err != nil || got != 12 {
		t.Errorf("RateRemaining() after spending = %d, %v, want 12", got, err)
	}
}

// TestStoredComment: a posted comment comes back as GitHub stored it — its
// id, body, author, created_at and page anchor — which is what an approval
// later re-hashes and orders against.
func TestStoredComment(t *testing.T) {
	srv, _, clk := newFake(t)
	ctx := context.Background()
	n := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", nil, human)
	got, err := botClient(t, srv).CreateIssueComment(ctx, intents, n, "<!-- patchy:plan x -->\nthe plan")
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	wantURL := "https://github.com/devthenet-labs/intents/issues/101#issuecomment-1"
	if got.ID != 1 || got.Body != "<!-- patchy:plan x -->\nthe plan" || got.UserLogin != fakegithub.BotLogin ||
		got.UserType != "Bot" || !got.CreatedAt.Equal(clk.now()) || got.HTMLURL != wantURL || got.ViaApp != "patchy" {
		t.Errorf("stored comment = %+v, want id 1 by %s at %v, %s, via the App", got, fakegithub.BotLogin, clk.now(), wantURL)
	}
	stored := srv.IssueComments(n)
	if len(stored) != 1 || stored[0].ID != got.ID || stored[0].User != fakegithub.Bot || stored[0].Body != got.Body {
		t.Errorf("IssueComments() = %+v, want the one posted", stored)
	}
}

// TestCommentEdits: a comment carries its node id, and the GraphQL edit
// record says whether it was ever edited, even by an edit in the second it
// was posted, when updated_at still equals created_at; an unknown id is
// NOT_FOUND. A scoped token reads it with issues read.
func TestCommentEdits(t *testing.T) {
	srv, _, _ := newFake(t)
	ctx := context.Background()
	n := srv.OpenIssue(intents.Owner, intents.Name, "t", "b", nil, human)
	id := srv.CommentAs(n, "looks good", human)
	c := scopedClient(t, srv, newApp(t, srv), intents, ghclient.TokenPerms{Issues: ghclient.PermRead})
	got, err := c.GetIssueComment(ctx, intents, id)
	if err != nil || got.NodeID == "" {
		t.Fatalf("GetIssueComment() = %+v, %v; want its node id", got, err)
	}
	if edited, err := c.CommentEdited(ctx, got.NodeID); err != nil || edited {
		t.Errorf("CommentEdited() before an edit = %v, %v; want false", edited, err)
	}
	if !srv.EditCommentBody(id, "/patchy approve") {
		t.Fatal("EditCommentBody() found no comment")
	}
	after, err := c.GetIssueComment(ctx, intents, id)
	if err != nil || !after.UpdatedAt.Equal(after.CreatedAt) {
		t.Fatalf("GetIssueComment() = %+v, %v; want an edit in the same second", after, err)
	}
	if edited, err := c.CommentEdited(ctx, got.NodeID); err != nil || !edited {
		t.Errorf("CommentEdited() after an edit = %v, %v; want true", edited, err)
	}
	if _, err := c.CommentEdited(ctx, "IC_nothing"); !errors.Is(err, ghclient.ErrNodeNotFound) {
		t.Errorf("CommentEdited(unknown) error = %v, want ErrNodeNotFound", err)
	}
}

// TestPullRequestIdentity: a pull request carries its own node id and the
// head commit its branch points at, on create, read and find alike, and
// the merge leaves both behind.
func TestPullRequestIdentity(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	const branch = "patchy-intent/target-1"
	sha, err := c.CreateCommit(ctx, target, ghclient.CommitRequest{
		BaseSHA: fakegithub.BaseSHA, Message: "build",
		Files: []ghclient.CommitFile{{Path: "VERSION", Mode: "100644", Content: []byte("0.1.0\n")}},
	})
	if err != nil {
		t.Fatalf("CreateCommit() error = %v", err)
	}
	if err := c.CreateBranchRef(ctx, target, branch, sha); err != nil {
		t.Fatalf("CreateBranchRef() error = %v", err)
	}
	pr, err := c.CreatePR(ctx, target, ghclient.PRRequest{Title: "target: v", Head: branch, Base: "main", Body: "Part of x"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	wantNode := "PR_fake" + strconv.Itoa(pr.Number)
	if pr.NodeID != wantNode || pr.HeadSHA != sha {
		t.Errorf("CreatePR() = %+v, want node %s head %s", pr, wantNode, sha)
	}
	found, err := c.FindPRByHead(ctx, target, branch)
	if err != nil || found == nil || found.Number != pr.Number || found.NodeID != wantNode || found.HeadSHA != sha {
		t.Errorf("FindPRByHead() = %+v, %v, want #%d %s %s", found, err, pr.Number, wantNode, sha)
	}
	got, err := c.GetPullRequest(ctx, target, pr.Number)
	if err != nil || got.NodeID != wantNode || got.HeadSHA != sha || got.State != "open" || got.Merged {
		t.Errorf("GetPullRequest() = %+v, %v, want open %s at %s", got, err, wantNode, sha)
	}
	snap, ok := srv.Pull(pr.Number)
	want := fakegithub.PullRequest{
		Number: pr.Number, Repository: "devthenet-labs/patchy-target", NodeID: wantNode, Title: "target: v",
		Body: "Part of x", Head: branch, HeadSHA: sha, Base: "main", State: "open",
	}
	if !ok || snap != want {
		t.Errorf("Pull() = %+v, %v, want %+v", snap, ok, want)
	}

	srv.MergePull(pr.Number, branch, "mergedsha")
	got, err = c.GetPullRequest(ctx, target, pr.Number)
	if err != nil || !got.Merged || got.MergeCommitSHA != "mergedsha" || got.NodeID != wantNode || got.HeadSHA != sha {
		t.Errorf("GetPullRequest() after merge = %+v, %v, want merged at mergedsha, %s, head %s", got, err, wantNode, sha)
	}
}

// TestRefWriteLog: every ref create and update is recorded as asked and as
// answered, so a test can prove a branch was created once and never forced
// — and see the Finding push's forced move for what it is.
func TestRefWriteLog(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	commit := func(parent string) string {
		t.Helper()
		sha, err := c.CreateCommit(ctx, target, ghclient.CommitRequest{
			BaseSHA: parent, Message: "m", Files: []ghclient.CommitFile{{Path: "a", Mode: "100644", Content: []byte(parent)}},
		})
		if err != nil {
			t.Fatalf("CreateCommit() error = %v", err)
		}
		return sha
	}
	build := commit(fakegithub.BaseSHA)
	next := commit(build)
	if err := c.CreateBranchRef(ctx, target, "patchy-intent/target-1", build); err != nil {
		t.Fatalf("CreateBranchRef() error = %v", err)
	}
	if err := c.CreateBranchRef(ctx, target, "patchy-intent/target-1", build); err != nil {
		t.Fatalf("CreateBranchRef() again error = %v", err)
	}
	if err := c.FastForwardRef(ctx, target, "patchy-intent/target-1", next); err != nil {
		t.Fatalf("FastForwardRef() error = %v", err)
	}
	for range 2 {
		if _, err := c.PushBranch(ctx, target, ghclient.BranchPush{Branch: "patchy/issue-9", CommitRequest: ghclient.CommitRequest{
			BaseSHA: fakegithub.BaseSHA, Message: "fix",
		}}); err != nil {
			t.Fatalf("PushBranch() error = %v", err)
		}
	}

	got := srv.RefWrites()
	if len(got) != 6 {
		t.Fatalf("ref writes = %+v, want 6", got)
	}
	want := []fakegithub.RefWrite{
		{Op: "create", Ref: "heads/patchy-intent/target-1", SHA: build, Status: http.StatusCreated},
		{Op: "create", Ref: "heads/patchy-intent/target-1", SHA: build, Status: http.StatusUnprocessableEntity},
		{Op: "update", Ref: "heads/patchy-intent/target-1", SHA: next, Status: http.StatusOK},
		{Op: "create", Ref: "heads/patchy/issue-9", SHA: got[3].SHA, Status: http.StatusCreated},
		{Op: "create", Ref: "heads/patchy/issue-9", SHA: got[4].SHA, Status: http.StatusUnprocessableEntity},
		{Op: "update", Ref: "heads/patchy/issue-9", SHA: got[4].SHA, Force: true, Status: http.StatusOK},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ref writes = %+v, want %+v", got, want)
	}
	pushed, ok := srv.CommitOf(next)
	if !ok || !reflect.DeepEqual(pushed.Parents, []string{build}) || string(pushed.Files["a"]) != build {
		t.Errorf("CommitOf(%s) = %+v, %v, want parent %s and file a", next, pushed, ok, build)
	}
}

// TestRepoFilesInTarball: a file a test adds to a repository's tree is in
// that repository's tarball, under GitHub's top-level directory, and in no
// other repository's.
func TestRepoFilesInTarball(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	srv.SetRepoFile(target.Owner, target.Name, ".patchy/agent.yaml", "image: registry.example/app:v1\n")
	files := func(repo ghclient.Repo) map[string]string {
		t.Helper()
		rc, err := c.Tarball(ctx, repo, fakegithub.HeadSHA)
		if err != nil {
			t.Fatalf("Tarball(%s) error = %v", repo, err)
		}
		defer func() { _ = rc.Close() }()
		gz, err := gzip.NewReader(rc)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		tr := tar.NewReader(gz)
		out := map[string]string{}
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return out
			}
			if err != nil {
				t.Fatalf("tar: %v", err)
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("tar body: %v", err)
			}
			out[hdr.Name] = string(body)
		}
	}
	top := "devthenet-labs-patchy-target-" + fakegithub.HeadSHA[:7] + "/"
	if got := files(target)[top+".patchy/agent.yaml"]; got != "image: registry.example/app:v1\n" {
		t.Errorf("tarball agent.yaml = %q, want the added file", got)
	}
	for name := range files(intents) {
		if strings.HasSuffix(name, ".patchy/agent.yaml") {
			t.Errorf("another repository's tarball carries %s", name)
		}
	}
}
