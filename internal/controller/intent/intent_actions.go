// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// intentParser reads commands on an intent issue: the /patchy grammar, and
// never the Finding issue's legacy /approve.
var intentParser = command.Parser{Surface: command.IntentIssue}

// humanAction is one human action on the intent issue, as GitHub's API
// reports it: a labeled issue event, or a comment carrying a command.
type humanAction struct {
	source v1alpha1.IntentActionSource
	id     int64
	at     time.Time
	actor  ghclient.Actor
	// verb is what the action asks: a label is its verb's alias (the
	// trigger label replan, the approve label approve); a command's verb is
	// as parsed, "" when the command line names none.
	verb  string
	label string
	// recorded: the action was decided and applied, and only its reply may
	// still be owed (an approval recorded as status.approval, a replan as
	// status.lastTrigger).
	recorded bool
	// edited: the command's comment was edited after it was posted. GitHub
	// lets anyone with write access edit anyone's comment and still names
	// the original author, so it is never taken as its author's command.
	edited bool
}

// key names what a notice answering the action answers: its id space and id.
func (a humanAction) key() string {
	if a.source == v1alpha1.IntentActionLabel {
		return "event-" + strconv.FormatInt(a.id, 10)
	}
	return "comment-" + strconv.FormatInt(a.id, 10)
}

func (a humanAction) record() v1alpha1.IntentAction {
	return v1alpha1.IntentAction{Source: a.source, EventID: a.id, Login: a.actor.Login, At: metav1.NewTime(a.at)}
}

// sourceRank orders two actions GitHub dated the same second: a label event,
// then a comment. Ids order actions within one id space.
func sourceRank(s v1alpha1.IntentActionSource) int {
	if s == v1alpha1.IntentActionLabel {
		return 0
	}
	return 1
}

// after reports whether action (at, source, id) is newer than rec. GitHub
// dates actions to the second, and its clock is never compared with this
// controller's.
func after(at time.Time, source v1alpha1.IntentActionSource, id int64, rec v1alpha1.IntentAction) bool {
	switch a, b := at.Unix(), rec.At.Unix(); {
	case a != b:
		return a > b
	case sourceRank(source) != sourceRank(rec.Source):
		return sourceRank(source) > sourceRank(rec.Source)
	}
	return id > rec.EventID
}

// lastTrigger is the newest trigger action consumed: status.lastTrigger, or
// the trigger that created the Intent.
func (p *pass) lastTrigger() v1alpha1.IntentAction {
	if lt := p.in.Status.LastTrigger; lt != nil {
		return *lt
	}
	rb := p.in.Spec.RequestedBy
	return v1alpha1.IntentAction{Source: v1alpha1.IntentActionLabel, EventID: rb.EventID, Login: rb.Login, At: rb.At}
}

// rateOK reports whether the rate budget of the installation repoURL is read
// with is at or over the floor, read once per repository per pass: under it
// the pass polls nothing there (the intent repository's issue, an app
// repository's pull requests or a blocked build's default branch), so
// intents never take the security flow's share of that installation's
// requests. The intent repository and an app repository may be two
// installations, so each poll asks about the repository it reads. The floor
// is a coarse guard: GitHub's headers are not consistent from one response to
// the next.
func (p *pass) rateOK(ctx context.Context, repoURL string) (bool, error) {
	key := normalizeRepoURL(repoURL)
	if above, ok := p.rates[key]; ok {
		return above, nil
	}
	if p.rates == nil {
		p.rates = map[string]bool{}
	}
	floor := p.set.RateLimitFloor
	if floor <= 0 {
		p.rates[key] = true
		return true, nil
	}
	remaining, err := p.r.GitHub.RateRemaining(ctx, repoURL)
	if err != nil {
		return false, fmt.Errorf("read the rate budget of %s: %w", repoURL, err)
	}
	above := remaining >= floor
	p.rates[key] = above
	if !above {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "installation rate budget under the floor; intent poll paused",
			slog.String("intent", p.in.Name), slog.String("repository", repoURL),
			slog.Int("remaining", remaining), slog.Int("floor", floor))
	}
	return above, nil
}

// rateOKForPullRequests is rateOK for every repository the Intent's pull
// requests are in.
func (p *pass) rateOKForPullRequests(ctx context.Context) (bool, error) {
	for _, pr := range p.in.Status.PullRequests {
		if ok, err := p.rateOK(ctx, pr.Repository); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// seen is the newest comment the poll has settled, or nil.
func (p *pass) seen() *v1alpha1.IntentCommentRef {
	if c := p.in.Status.Commands; c != nil {
		return c.Seen
	}
	return nil
}

// listSince is where the poll's comment listing starts: at the newest
// trigger action consumed (commands before it were answered before it, since
// actions are answered oldest first, and commands before the trigger that
// created the Intent are not the Intent's), or at the newest comment already
// settled when that is later. The thread is never listed from its start on
// every poll. GitHub dates to the second and filters by updated_at, so the
// listing starts a second early; commands drops by id what was already seen.
func (p *pass) listSince() time.Time {
	at := p.lastTrigger().At.Time
	if s := p.seen(); s != nil && s.At.After(at) {
		at = s.At.Time
	}
	return at.Add(-time.Second)
}

// poll reads the issue, answers every human action on it not yet answered,
// oldest first, and closes the Intent when a human closed the issue. stop
// reports that the Intent ended, so the phase step must not run. Each
// command answered is recorded as seen as soon as its reply is posted, and
// the whole listing once every command in it is answered, so a command is
// never answered twice, even after patchy's reply to it is deleted.
func (p *pass) poll(ctx context.Context) (stop bool, err error) {
	p.r.memo(func() { p.r.polled[p.in.Name] = p.now })
	if ok, err := p.rateOK(ctx, p.repo()); err != nil || !ok {
		return false, err
	}
	p.polled = true
	issue, err := p.r.GitHub.GetIssue(ctx, p.repo(), p.number())
	if err != nil {
		return false, fmt.Errorf("read the issue: %w", err)
	}
	events, err := p.r.GitHub.ListIssueEvents(ctx, p.repo(), p.number())
	if err != nil {
		return false, fmt.Errorf("list the issue's events: %w", err)
	}
	if err := p.listComments(ctx, p.listSince()); err != nil {
		return false, err
	}
	for _, c := range p.comments {
		if p.pollNewest == nil || c.ID > p.pollNewest.ID {
			p.pollNewest = c
		}
	}
	actions, err := p.gather(ctx, issue, events)
	if err != nil {
		return false, err
	}
	if p.deferred != 0 {
		// A command waits for a later poll: the listing is settled only up
		// to it, so the next poll reads it again.
		p.pollNewest = nil
		for _, c := range p.comments {
			if c.ID < p.deferred && (p.pollNewest == nil || c.ID > p.pollNewest.ID) {
				p.pollNewest = c
			}
		}
	}
	if stop, err := p.answer(ctx, actions, issue); stop || err != nil {
		return stop, err
	}
	if issue.State == "closed" {
		// The pull requests decide first: patchy closes the issue itself
		// when every one merged, before it writes Merged, and a lost write
		// must not turn that close into a human's.
		if len(p.in.Status.PullRequests) > 0 {
			// Under a pull request repository's floor nothing is decided:
			// the close waits for the pull requests to be read.
			if ok, err := p.rateOKForPullRequests(ctx); err != nil || !ok {
				return true, err
			}
			if ended, err := p.reviewNow(ctx); ended || err != nil {
				return true, err
			}
		}
		// A human closed the issue: that needs nothing more from patchy.
		return true, p.setPhase(ctx, v1alpha1.IntentClosed, func(cur *v1alpha1.Intent) {
			cur.Status.ActiveRun = nil
		})
	}
	return false, nil
}

// answer settles the actions gathered, oldest first, recording each command
// answered as seen as soon as its reply is posted, and then the whole
// listing; stop reports that an action ended the Intent. Nothing at or after
// a deferred command is recorded as seen: a command answered after it is
// found answered again by its reply's marker.
func (p *pass) answer(ctx context.Context, actions []humanAction, issue *ghclient.Issue) (stop bool, err error) {
	for _, a := range actions {
		answered, err := p.settle(ctx, a, issue)
		if err != nil {
			return false, err
		}
		if terminal(p.in.Status.Phase) {
			return true, nil
		}
		if answered && a.source == v1alpha1.IntentActionCommand && (p.deferred == 0 || a.id < p.deferred) {
			if err := p.recordSeen(ctx, a.id, a.at); err != nil {
				return false, err
			}
		}
	}
	if c := p.pollNewest; c != nil {
		return false, p.recordSeen(ctx, c.ID, c.CreatedAt)
	}
	return false, nil
}

// gather lists the human actions on the issue still to answer, oldest first:
// every command since the anchor with no reply from patchy, the newest
// trigger label event newer than the last consumed trigger, and, while the
// plan waits for approval, the newest approve label event newer than the
// plan. Actions by patchy's own bot are never answered.
func (p *pass) gather(ctx context.Context, issue *ghclient.Issue, events []*ghclient.IssueEvent) (
	[]humanAction, error) {
	out := p.commands()
	if e := newestLabeled(events, p.trigger(), p.isOwnLogin); e != nil &&
		after(e.CreatedAt, v1alpha1.IntentActionLabel, e.ID, p.lastTrigger()) {
		out = append(out, humanAction{source: v1alpha1.IntentActionLabel, id: e.ID, at: e.CreatedAt,
			actor: e.Actor, verb: action.VerbReplan, label: p.trigger()})
	}
	approve, err := p.approveLabelAction(ctx, issue, events)
	if err != nil {
		return nil, err
	}
	if approve != nil {
		out = append(out, *approve)
	}
	slices.SortFunc(out, func(a, b humanAction) int {
		switch {
		case after(a.at, a.source, a.id, b.record()):
			return 1
		case after(b.at, b.source, b.id, a.record()):
			return -1
		}
		return 0
	})
	return out, nil
}

// commands are the commands among the comments this pass listed that are
// still to answer: newer than the newest comment already settled, not
// before the newest trigger consumed, no reply from patchy yet, and not
// superseded.
func (p *pass) commands() []humanAction {
	var out []humanAction
	anchor := p.lastTrigger().At.Time
	var seen int64
	if s := p.seen(); s != nil {
		seen = s.ID
	}
	for _, c := range p.comments {
		if c.ID <= seen || c.CreatedAt.Before(anchor) || p.isOwn(c) || p.isOwnLogin(c.UserLogin) {
			continue
		}
		cmd, ok := intentParser.Parse(c.Body)
		if !ok {
			continue
		}
		a := humanAction{source: v1alpha1.IntentActionCommand, id: c.ID, at: c.CreatedAt, actor: c.Author(),
			verb: cmd.Verb, edited: edited(c)}
		if p.hasOwnNotice(a.key()) {
			// Answered. A refusal to an account refused without asking
			// GitHub is remembered here too, in case the pass that posted
			// it stopped before recording it.
			if refusedLocally(p.proj, a.actor) {
				p.noteRefused(a.actor)
			}
			continue
		}
		switch a.verb {
		case action.VerbApprove:
			if ap := p.in.Status.Approval; ap != nil && ap.Source == a.source && ap.EventID == a.id {
				a.recorded = true
			} else if p.approvalPending() && !a.edited && !refusedLocally(p.proj, a.actor) {
				// The plan may already be on the issue, its posting not yet
				// recorded (a write being retried, a restart): the approver
				// may have read it. The approval waits for the record, as the
				// approve label does, rather than being told the plan is
				// not there.
				if p.deferred == 0 || a.id < p.deferred {
					p.deferred = a.id
				}
				continue
			}
		case action.VerbReplan:
			if lt := p.in.Status.LastTrigger; lt != nil && lt.Source == a.source && lt.EventID == a.id {
				// Consumed, and a refusal is replied to before it is
				// consumed: this replan was accepted.
				a.recorded = true
			} else if !after(a.at, a.source, a.id, p.lastTrigger()) {
				continue // superseded by a newer trigger action
			}
		}
		out = append(out, a)
	}
	return out
}

// approvalPending reports a plan recorded while planning whose posting for
// approval is not recorded yet: writeBack posts it and then records it, and a
// retry of that record finds the comment it posted.
func (p *pass) approvalPending() bool {
	pl := p.in.Status.Plan
	return p.in.Status.Phase == v1alpha1.IntentPlanning && pl != nil && pl.PostedAt == nil
}

// approveLabelAction is the approve label event still to answer while the
// plan waits for approval: the newest, newer than the plan, not the accepted
// approval, not answered, and with the label still on the issue. One
// answered by a refusal whose label removal did not follow has the label
// removed here.
func (p *pass) approveLabelAction(ctx context.Context, issue *ghclient.Issue, events []*ghclient.IssueEvent) (
	*humanAction, error) {
	pl := p.in.Status.Plan
	if p.in.Status.Phase != v1alpha1.IntentAwaitingApproval || pl == nil || pl.PostedAt == nil {
		return nil, nil
	}
	approve := approveLabel(p.proj)
	e := newestLabeled(events, approve, p.isOwnLogin)
	if e == nil || !e.CreatedAt.After(pl.PostedAt.Time) {
		return nil, nil
	}
	a := humanAction{source: v1alpha1.IntentActionLabel, id: e.ID, at: e.CreatedAt, actor: e.Actor,
		verb: action.VerbApprove, label: approve}
	switch ap := p.in.Status.Approval; {
	case ap != nil && ap.Source == a.source && ap.EventID == a.id:
		return nil, nil
	case p.hasOwnNotice(a.key()):
		// Refused, noticed first: the label's removal may not have
		// followed. A label approval is never noticed as done.
		if hasLabel(issue, approve) {
			if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), approve); err != nil {
				return nil, fmt.Errorf("remove the refused approve label: %w", err)
			}
		}
		return nil, nil
	case !hasLabel(issue, approve):
		// Removed by a human since: nothing to approve with.
		return nil, nil
	}
	return &a, nil
}

// newestLabeled is the newest labeled event adding label by anyone but
// patchy's own bot, or nil.
func newestLabeled(events []*ghclient.IssueEvent, label string, own func(string) bool) *ghclient.IssueEvent {
	var newest *ghclient.IssueEvent
	for _, e := range events {
		if e.Event != "labeled" || !strings.EqualFold(e.Label, label) || e.ID < 1 || e.Actor.Login == "" ||
			own(e.Actor.Login) {
			continue
		}
		if newest == nil || after(e.CreatedAt, v1alpha1.IntentActionLabel, e.ID,
			v1alpha1.IntentAction{Source: v1alpha1.IntentActionLabel, EventID: newest.ID,
				At: metav1.NewTime(newest.CreatedAt)}) {
			newest = e
		}
	}
	return newest
}

// settle answers one action. A command is acknowledged with the eyes
// reaction and exactly one reply, with one exception: an account refused
// without asking GitHub (a bot, or not an approver) gets its first refusal
// on the intent and nothing after it, neither reaction nor reply, so
// commenting repeatedly cannot make patchy write to GitHub once per comment.
// Such an account is refused whatever its command says, an unknown verb or
// an edited comment included. A label, which has no comment to react to, is
// answered with a notice when it is refused (its acceptance shows on the
// status comment). answered is false only for a command answered quietly.
func (p *pass) settle(ctx context.Context, a humanAction, issue *ghclient.Issue) (answered bool, err error) {
	if a.source != v1alpha1.IntentActionCommand {
		return true, p.decide(ctx, a, issue)
	}
	local := refusedLocally(p.proj, a.actor)
	if local && !a.recorded && p.wasRefused(a.actor) {
		return false, nil
	}
	if err := p.r.GitHub.React(ctx, p.repo(), a.id); err != nil && !ghclient.IsNotFound(err) {
		return false, fmt.Errorf("react to comment %d: %w", a.id, err)
	}
	switch {
	case a.recorded:
		return true, p.replyDone(ctx, a)
	case local:
		if err := p.notAllowed(ctx, a, isBot(a.actor), false, false); err != nil {
			return false, err
		}
		p.noteRefused(a.actor)
		return true, nil
	case a.edited:
		body, err := templates.RenderEditedCommandNotice(templates.EditedCommandNotice{
			Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Verb: a.verb,
		})
		return true, p.notice(ctx, a.key(), a.at, body, err)
	case !slices.Contains(command.Available(command.IntentIssue), a.verb):
		// An approver's: the help, once GitHub confirms their write access.
		ok, bot, err := p.authorize(ctx, a.actor)
		if err != nil {
			return false, err
		}
		if !ok {
			return true, p.notAllowed(ctx, a, bot, false, false)
		}
		body, err := templates.RenderUnknownCommandNotice(templates.UnknownCommandNotice{
			Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Verb: a.verb,
		})
		return true, p.notice(ctx, a.key(), a.at, body, err)
	}
	return true, p.decide(ctx, a, issue)
}

// decide applies or refuses an intent verb, a label's or an approver's
// command.
func (p *pass) decide(ctx context.Context, a humanAction, issue *ghclient.Issue) error {
	switch a.verb {
	case action.VerbCancel:
		return p.settleCancel(ctx, a)
	case action.VerbApprove:
		return p.settleApprove(ctx, a, issue)
	case action.VerbReplan:
		return p.settleReplan(ctx, a, issue)
	}
	return nil
}

// wasRefused reports an account already sent a refusal on this intent, by
// an earlier pass or this one.
func (p *pass) wasRefused(actor ghclient.Actor) bool {
	if actor.ID <= 0 {
		return false
	}
	if p.refused[actor.ID] {
		return true
	}
	c := p.in.Status.Commands
	return c != nil && slices.Contains(c.RefusedActors, actor.ID)
}

// noteRefused remembers an account sent a refusal this pass; recordSeen
// writes it.
func (p *pass) noteRefused(actor ghclient.Actor) {
	if actor.ID <= 0 {
		return
	}
	if p.refused == nil {
		p.refused = map[int64]bool{}
	}
	p.refused[actor.ID] = true
}

// recordSeen records that every comment up to id is settled (never moving
// back), with the accounts refused this pass, in one status write; nothing
// is written when neither changed. It follows the reply it records, so a
// record never stands for a reply that was not posted, and a reply found
// without its record is recorded by the next poll.
func (p *pass) recordSeen(ctx context.Context, id int64, at time.Time) error {
	next := &v1alpha1.IntentCommands{}
	if cur := p.in.Status.Commands; cur != nil {
		next = cur.DeepCopy()
	}
	changed := false
	if id > 0 && (next.Seen == nil || id > next.Seen.ID) {
		next.Seen = &v1alpha1.IntentCommentRef{ID: id, At: metav1.NewTime(at)}
		changed = true
	}
	for _, actor := range slices.Sorted(maps.Keys(p.refused)) {
		if !slices.Contains(next.RefusedActors, actor) {
			next.RefusedActors = rememberActor(next.RefusedActors, actor)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Commands = next
		return nil
	})
}

// rememberActor adds id to ids, keeping the latest MaxIntentRefusedActors.
func rememberActor(ids []int64, id int64) []int64 {
	out := append(slices.Clone(ids), id)
	if over := len(out) - v1alpha1.MaxIntentRefusedActors; over > 0 {
		out = slices.Clone(out[over:])
	}
	return out
}

// replyDone posts the done reply to a command already applied.
func (p *pass) replyDone(ctx context.Context, a humanAction) error {
	if a.source != v1alpha1.IntentActionCommand {
		return nil
	}
	var rev int32
	if ap := p.in.Status.Approval; ap != nil {
		rev = ap.PlanRevision
	}
	body, err := templates.RenderCommandDoneNotice(templates.CommandDoneNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Actor: a.actor.Login, Verb: a.verb,
		PlanRevision: rev,
	})
	return p.notice(ctx, a.key(), a.at, body, err)
}

// notAllowed answers an action refused for its actor.
func (p *pass) notAllowed(ctx context.Context, a humanAction, bot, labelRemoved, closed bool) error {
	n := templates.NotAllowedNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Actor: a.actor.Login, Bot: bot,
		LabelRemoved: labelRemoved, Closed: closed,
	}
	if a.source == v1alpha1.IntentActionLabel {
		n.Label = a.label
	} else {
		n.Verb = a.verb
	}
	body, err := templates.RenderNotAllowedNotice(n)
	return p.notice(ctx, a.key(), a.at, body, err)
}

// notAvailable answers an action the Intent's phase does not admit; phase
// is the phase to name (the one the action was made in).
func (p *pass) notAvailable(ctx context.Context, a humanAction, phase v1alpha1.IntentPhase, labelRemoved bool) error {
	n := templates.NotAvailableNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Phase: string(phase),
		LabelRemoved: labelRemoved, Available: verbs(phase),
	}
	if a.source == v1alpha1.IntentActionLabel {
		n.Label = a.label
	} else {
		n.Verb = a.verb
	}
	body, err := templates.RenderNotAvailableNotice(n)
	return p.notice(ctx, a.key(), a.at, body, err)
}

// settleCancel: an approver's /patchy cancel closes the issue (not planned)
// and the Intent. The close comes first, then the reply, then the phase, so
// a restart repeats only idempotent steps; the issue closed by it is what a
// later poll reads as closed if the phase write is lost.
func (p *pass) settleCancel(ctx context.Context, a humanAction) error {
	ok, bot, err := p.authorize(ctx, a.actor)
	if err != nil {
		return err
	}
	if !ok {
		return p.notAllowed(ctx, a, bot, false, false)
	}
	if err := p.r.GitHub.CloseIssue(ctx, p.repo(), p.number(), ghclient.CloseNotPlanned); err != nil {
		return fmt.Errorf("close the issue: %w", err)
	}
	body, err := templates.RenderCommandDoneNotice(templates.CommandDoneNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Actor: a.actor.Login, Verb: a.verb,
	})
	if err := p.notice(ctx, a.key(), a.at, body, err); err != nil {
		return err
	}
	return p.setPhase(ctx, v1alpha1.IntentClosed, func(cur *v1alpha1.Intent) { cur.Status.ActiveRun = nil })
}

// settleReplan answers a replan: the trigger label re-applied, or /patchy
// replan. An approver's, while the plan waits for approval, is a new plan;
// anything else is refused or not available, and consumed all the same, so it
// never takes effect later.
func (p *pass) settleReplan(ctx context.Context, a humanAction, issue *ghclient.Issue) error {
	ok, bot, err := p.authorize(ctx, a.actor)
	if err != nil {
		return err
	}
	rec := a.record()
	consume := func() error {
		return p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.LastTrigger = &rec
			return nil
		})
	}
	switch {
	case !ok:
		if err := p.notAllowed(ctx, a, bot, false, false); err != nil {
			return err
		}
		return consume()
	case p.in.Status.Phase != v1alpha1.IntentAwaitingApproval:
		if err := p.notAvailable(ctx, a, p.in.Status.Phase, false); err != nil {
			return err
		}
		return consume()
	}
	if err := p.startPlanning(ctx, issue, &rec); err != nil {
		return err
	}
	return p.replyDone(ctx, a)
}

// settleApprove answers an approval: the approve label, or /patchy approve.
// It is accepted only from an approver, only while the plan waits for
// approval and after it was posted, and only while the plan comment and the
// issue are what the plan was shown with and made from.
func (p *pass) settleApprove(ctx context.Context, a humanAction, issue *ghclient.Issue) error {
	pl := p.in.Status.Plan
	if p.in.Status.Phase != v1alpha1.IntentAwaitingApproval || pl == nil || pl.PostedAt == nil {
		return p.notAvailable(ctx, a, p.in.Status.Phase, false)
	}
	if !a.at.After(pl.PostedAt.Time) {
		// Made before the plan was posted, while the Intent was planning.
		return p.notAvailable(ctx, a, v1alpha1.IntentPlanning, false)
	}
	ok, bot, err := p.authorize(ctx, a.actor)
	if err != nil {
		return err
	}
	isLabel := a.source == v1alpha1.IntentActionLabel
	approve := approveLabel(p.proj)
	if !ok {
		if err := p.notAllowed(ctx, a, bot, isLabel, false); err != nil {
			return err
		}
		if isLabel {
			if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), approve); err != nil {
				return fmt.Errorf("remove the refused approve label: %w", err)
			}
		}
		return nil
	}
	planChanged, issueChanged, err := p.approvalChanged(ctx, issue)
	if err != nil {
		return err
	}
	if planChanged || issueChanged {
		return p.refuseApproval(ctx, a, issue, planChanged, issueChanged)
	}
	approval := v1alpha1.IntentApproval{
		By: a.actor.Login, Source: a.source, EventID: a.id, At: metav1.NewTime(a.at),
		PlanRevision: pl.Revision, PlanDigest: pl.Digest, InputDigest: p.in.Status.Input.Digest,
	}
	if err := p.setPhase(ctx, v1alpha1.IntentBuilding, func(cur *v1alpha1.Intent) {
		cur.Status.Approval = &approval
		setCondition(cur, v1alpha1.ConditionApprovalRejected, metav1.ConditionFalse, "Approved",
			fmt.Sprintf("plan r%d approved by %s", pl.Revision, a.actor.Login))
	}); err != nil {
		return err
	}
	return p.replyDone(ctx, a)
}

// approvalChanged reports whether the plan comment was edited since patchy
// posted it (re-fetched, it no longer hashes to what was recorded, or GitHub
// dates an edit after its posting: patchy never edits a plan comment, and an
// edit restoring the original bytes still moves updated_at, so a plan shown
// edited for a while and then put back is caught; deleted counts), and
// whether the issue changed since the plan was made (re-read, its title and
// body no longer render to the input snapshot's digest).
func (p *pass) approvalChanged(ctx context.Context, issue *ghclient.Issue) (planChanged, issueChanged bool, err error) {
	pl, input := p.in.Status.Plan, p.in.Status.Input
	c, err := p.r.GitHub.GetIssueComment(ctx, p.repo(), pl.CommentID)
	switch {
	case ghclient.IsNotFound(err):
		planChanged = true
	case err != nil:
		return false, false, fmt.Errorf("re-read the plan comment: %w", err)
	default:
		planChanged = digest([]byte(c.Body)) != pl.CommentDigest || edited(c)
	}
	snap, err := p.inputSnapshot(ctx, input)
	if err != nil {
		return false, false, err
	}
	if snap == nil {
		return planChanged, true, nil
	}
	snap.Title, snap.Body = issue.Title, issue.Body
	return planChanged, digest(snap.render()) != input.Digest, nil
}

// refuseApproval answers an approval of a plan that is no longer what the
// approver was shown: the condition first, then the notice, then the label.
func (p *pass) refuseApproval(ctx context.Context, a humanAction, issue *ghclient.Issue,
	planChanged, issueChanged bool) error {
	approve := approveLabel(p.proj)
	labelPresent := hasLabel(issue, approve)
	var what []string
	if planChanged {
		what = append(what, "the plan comment was edited")
	}
	if issueChanged {
		what = append(what, "the issue changed")
	}
	if err := p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionApprovalRejected, metav1.ConditionTrue, "PlanOrIssueChanged",
			fmt.Sprintf("the approval by %s was refused: %s since the plan was posted; a replan is needed",
				a.actor.Login, strings.Join(what, " and ")))
		return nil
	}); err != nil {
		return err
	}
	n := templates.ApprovalRefusedNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Actor: a.actor.Login,
		PlanRevision: p.in.Status.Plan.Revision, PlanChanged: planChanged, IssueChanged: issueChanged,
		LabelRemoved: labelPresent, TriggerLabel: p.trigger(),
	}
	if a.source == v1alpha1.IntentActionLabel {
		n.Label = approve
	}
	body, err := templates.RenderApprovalRefusedNotice(n)
	if err := p.notice(ctx, a.key(), a.at, body, err); err != nil {
		return err
	}
	if labelPresent {
		if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), approve); err != nil {
			return fmt.Errorf("remove the approve label: %w", err)
		}
	}
	return nil
}

// handOff answers a trigger on the issue of an Intent that has ended,
// handed over by discovery: on a Failed Intent an approver's is a revival
// (a new plan); anything else gets one notice, the label is removed again,
// and the action is consumed. stop reports that the Intent was revived.
func (p *pass) handOff(ctx context.Context) (stop bool, err error) {
	if err := p.readBot(ctx); err != nil {
		return false, err
	}
	events, err := p.r.GitHub.ListIssueEvents(ctx, p.repo(), p.number())
	if err != nil {
		return false, fmt.Errorf("list the issue's events: %w", err)
	}
	e := newestLabeled(events, p.trigger(), p.isOwnLogin)
	if e == nil || !after(e.CreatedAt, v1alpha1.IntentActionLabel, e.ID, p.lastTrigger()) {
		return false, nil
	}
	a := humanAction{source: v1alpha1.IntentActionLabel, id: e.ID, at: e.CreatedAt, actor: e.Actor,
		verb: action.VerbReplan, label: p.trigger()}
	rec := a.record()
	phase := p.in.Status.Phase
	ok := false
	bot := false
	if phase == v1alpha1.IntentFailed {
		if ok, bot, err = p.authorize(ctx, a.actor); err != nil {
			return false, err
		}
	}
	if ok {
		return true, p.startPlanning(ctx, nil, &rec)
	}
	if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), p.trigger()); err != nil {
		return false, fmt.Errorf("remove the trigger label: %w", err)
	}
	if phase == v1alpha1.IntentFailed {
		err = p.notAllowed(ctx, a, bot, true, false)
	} else {
		err = p.notAvailable(ctx, a, phase, true)
	}
	if err != nil {
		return false, err
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.LastTrigger = &rec
		return nil
	})
}
