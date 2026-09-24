// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"reflect"
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

// TestIssueEventActors: a human's label carries the human; everything the
// client does carries the App's bot, and label events carry no App slug.
func TestIssueEventActors(t *testing.T) {
	srv, c, _ := newFake(t)
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
	bot := ghclient.Actor{Login: fakegithub.BotLogin, ID: fakegithub.BotUserID, Type: "Bot"}
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
		{"labeled", "patchy:target", ghclient.Actor{Login: "peter", ID: 42, Type: "User"}},
		{"labeled", "patchy:approved", bot},
		{"unlabeled", "patchy:approved", bot},
		{"closed", "", bot},
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
}

// TestCommentsSince: the App's comments carry its bot and slug, a human's
// neither; since filters on last update, so an edit brings an older comment
// back, and a re-read shows the edit.
func TestCommentsSince(t *testing.T) {
	srv, c, clk := newFake(t)
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	app, err := ghclient.NewApp(ghclient.AppConfig{
		AppID:      1,
		PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		BaseURL:    srv.URL,
	})
	if err != nil {
		t.Fatalf("NewApp() error = %v", err)
	}
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
