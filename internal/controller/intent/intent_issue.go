// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// markerPrefix opens every comment patchy writes on an intent issue.
const markerPrefix = "<!-- patchy:"

// markerOf is the marker line a comment body opens with, or "".
func markerOf(body string) string {
	line, _, _ := strings.Cut(strings.TrimLeft(body, " \t\r\n"), "\n")
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, markerPrefix) && strings.HasSuffix(line, "-->") {
		return line
	}
	return ""
}

func (p *pass) repo() string    { return p.in.Spec.Issue.Repository }
func (p *pass) number() int64   { return p.in.Spec.Issue.Number }
func (p *pass) trigger() string { return v1alpha1.ProjectTriggerLabel(p.proj) }

// readBot reads the App's own login on the intent repository once per pass
// into p.bot; "" when the Forge uses a personal access token (dev only),
// which has no bot identity.
func (p *pass) readBot(ctx context.Context) error {
	if p.botRead {
		return nil
	}
	bot, err := p.r.GitHub.BotLogin(ctx, p.repo())
	if err != nil {
		return fmt.Errorf("read the App's bot login: %w", err)
	}
	p.bot, p.botRead = bot, true
	return nil
}

// isOwnLogin reports patchy's own account. With a personal access token
// there is no bot identity; only a body carrying patchy's marker then counts
// as its own (isOwn), and no actor is ever skipped as patchy's.
func (p *pass) isOwnLogin(login string) bool {
	return p.bot != "" && strings.EqualFold(login, p.bot)
}

// isOwn reports a comment patchy wrote. Markers are predictable, so a marker
// alone proves nothing: the author must be patchy's bot.
func (p *pass) isOwn(c *ghclient.Comment) bool {
	if p.bot == "" {
		return markerOf(c.Body) != ""
	}
	return strings.EqualFold(c.UserLogin, p.bot)
}

// everEdited reports a comment ever edited, by GitHub's own record of its
// edits (GraphQL lastEditedAt/includesCreatedEdit). REST updated_at is not
// evidence of an edit: submitting a pending review can move it without one.
// A comment gone since it was listed counts as edited: it is nothing to act on.
func (p *pass) everEdited(ctx context.Context, c *ghclient.Comment) (bool, error) {
	e, err := p.r.GitHub.CommentEdited(ctx, p.repo(), c.NodeID)
	switch {
	case errors.Is(err, ghclient.ErrNodeNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read whether comment %d was edited: %w", c.ID, err)
	}
	return e, nil
}

// listComments lists the issue's comments since since (never the whole
// thread), unless this pass already listed from an earlier time, and indexes
// patchy's own by marker.
func (p *pass) listComments(ctx context.Context, since time.Time) error {
	if p.listed && !since.Before(p.commentsSince) {
		return nil
	}
	if err := p.readBot(ctx); err != nil {
		return err
	}
	comments, err := p.r.GitHub.ListIssueComments(ctx, p.repo(), p.number(), since)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	p.comments, p.commentsSince, p.listed = comments, since, true
	// Listed oldest first, so the newest of patchy's comments with one
	// marker wins: the one a re-post after an edit left behind.
	p.own = map[string]*ghclient.Comment{}
	for _, c := range comments {
		if m := markerOf(c.Body); m != "" && p.isOwn(c) {
			p.own[m] = c
		}
	}
	return nil
}

// findOwn is patchy's own comment headed by marker, posted since since, or
// nil.
func (p *pass) findOwn(ctx context.Context, marker string, since time.Time) (*ghclient.Comment, error) {
	if err := p.listComments(ctx, since); err != nil {
		return nil, err
	}
	if c := p.own[marker]; c != nil && !c.CreatedAt.Before(since) {
		return c, nil
	}
	return nil, nil
}

// postOnce posts body, headed by marker, unless patchy already posted a
// comment with that marker since since: a retry after a failure between
// posting and recording finds the first instead of posting a second. since
// must be no later than the first attempt could have posted it.
func (p *pass) postOnce(ctx context.Context, marker string, since time.Time, body string) (*ghclient.Comment, error) {
	if c, err := p.findOwn(ctx, marker, since); err != nil || c != nil {
		return c, err
	}
	c, err := p.r.GitHub.CreateIssueComment(ctx, p.repo(), p.number(), body)
	if err != nil {
		return nil, fmt.Errorf("post comment: %w", err)
	}
	if p.own == nil {
		p.own = map[string]*ghclient.Comment{}
	}
	p.own[marker] = c
	return c, nil
}

// notice posts a notice keyed key once; body must be headed by
// templates.NoticeMarker for the same key.
func (p *pass) notice(ctx context.Context, key string, since time.Time, body string, renderErr error) error {
	if renderErr != nil {
		return renderErr
	}
	_, err := p.postOnce(ctx, templates.NoticeMarker(p.in.Namespace, p.in.Name, key), since, body)
	return err
}

// hasOwnNotice reports a notice keyed key among the comments this pass listed.
func (p *pass) hasOwnNotice(key string) bool {
	_, ok := p.own[templates.NoticeMarker(p.in.Namespace, p.in.Name, key)]
	return ok
}

// isBot reports a bot account: GitHub's account type, or a login ending in
// [bot], which only an App's bot user carries.
func isBot(actor ghclient.Actor) bool {
	return actor.IsBot() || strings.HasSuffix(strings.ToLower(actor.Login), "[bot]")
}

// refusedLocally reports an actor refused without asking GitHub anything: a
// bot, or not one of the Project's approvers. Only such an account is ever
// answered quietly (wasRefused): an approver's commands always get their
// reply.
func refusedLocally(proj *v1alpha1.Project, actor ghclient.Actor) bool {
	return isBot(actor) || !isApprover(proj, actor.Login)
}

// authorize decides whether actor may act on the intent: one of the Project's
// approvers, with write access to the intent repository (GitHub's own
// answer, never author_association; read is no authority, since a public
// repository grants it to everyone), and not a bot. bot reports a refusal
// for being a bot. A GitHub failure other than "no such user" or a refused
// lookup is returned, so nothing is decided without GitHub's answer.
func (p *pass) authorize(ctx context.Context, actor ghclient.Actor) (ok, bot bool, err error) {
	return p.authorizeIn(ctx, p.repo(), actor)
}

// authorizeIn applies the same approver and write-permission rule on the
// repository where an action happened. PR reviews and commands are checked
// against the application repository, not the intent issue repository.
func (p *pass) authorizeIn(ctx context.Context, repoURL string, actor ghclient.Actor) (ok, bot bool, err error) {
	if isBot(actor) {
		return false, true, nil
	}
	if !isApprover(p.proj, actor.Login) {
		return false, false, nil
	}
	perm, err := p.r.GitHub.Permission(ctx, repoURL, actor.Login)
	switch {
	case errors.Is(err, ghclient.ErrNoSuchUser):
		return false, false, nil
	case ghclient.IsForbidden(err):
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "collaborator permission unreadable; the action is refused",
			slog.String("intent", p.in.Name), slog.String("login", actor.Login), slog.Any("error", err))
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("read the permission of %s: %w", actor.Login, err)
	}
	return ghclient.CanWrite(perm), false, nil
}

// hasLabel reports whether the issue carries the label, compared as GitHub
// compares label names.
func hasLabel(issue *ghclient.Issue, name string) bool {
	for _, l := range issue.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// verbs are the commands the intent issue admits in the Intent's phase.
func verbs(phase v1alpha1.IntentPhase) []string {
	switch {
	case phase == v1alpha1.IntentAwaitingApproval:
		return []string{"approve", "replan", "cancel"}
	case terminal(phase):
		return nil
	}
	return []string{"cancel"}
}
