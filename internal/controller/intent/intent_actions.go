// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
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

// commentAnchor is the earliest time a command still to answer can date
// from: every command before the newest consumed trigger action was
// answered before it (actions are answered oldest first), and commands before
// the trigger that created the Intent are not the Intent's.
func (p *pass) commentAnchor() time.Time {
	return p.lastTrigger().At.Time
}

// poll reads the issue, answers every human action on it not yet answered,
// oldest first, and closes the Intent when a human closed the issue. stop
// reports that the Intent ended, so the phase step must not run.
func (p *pass) poll(ctx context.Context) (stop bool, err error) {
	p.r.memo(func() { p.r.polled[p.in.Name] = p.now })
	p.polled = true
	if floor := p.set.RateLimitFloor; floor > 0 {
		remaining, err := p.r.GitHub.RateRemaining(ctx, p.repo())
		if err != nil {
			return false, fmt.Errorf("read the rate budget: %w", err)
		}
		if remaining < floor {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "installation rate budget under the floor; intent poll paused",
				slog.String("intent", p.in.Name), slog.Int("remaining", remaining), slog.Int("floor", floor))
			return false, nil
		}
	}
	issue, err := p.r.GitHub.GetIssue(ctx, p.repo(), p.number())
	if err != nil {
		return false, fmt.Errorf("read the issue: %w", err)
	}
	events, err := p.r.GitHub.ListIssueEvents(ctx, p.repo(), p.number())
	if err != nil {
		return false, fmt.Errorf("list the issue's events: %w", err)
	}
	if err := p.listComments(ctx, p.commentAnchor()); err != nil {
		return false, err
	}
	actions, err := p.gather(ctx, issue, events)
	if err != nil {
		return false, err
	}
	for _, a := range actions {
		if err := p.settle(ctx, a, issue); err != nil {
			return false, err
		}
		if terminal(p.in.Status.Phase) {
			return true, nil
		}
	}
	if issue.State == "closed" {
		// The pull requests decide first: patchy closes the issue itself
		// when every one merged, before it writes Merged, and a lost write
		// must not turn that close into a human's.
		if len(p.in.Status.PullRequests) > 0 {
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
// still to answer: no reply from patchy yet, and not superseded.
func (p *pass) commands() []humanAction {
	var out []humanAction
	anchor := p.commentAnchor()
	for _, c := range p.comments {
		if c.CreatedAt.Before(anchor) || p.isOwn(c) || p.isOwnLogin(c.UserLogin) {
			continue
		}
		cmd, ok := intentParser.Parse(c.Body)
		if !ok {
			continue
		}
		a := humanAction{source: v1alpha1.IntentActionCommand, id: c.ID, at: c.CreatedAt, actor: c.Author(),
			verb: cmd.Verb}
		if p.hasOwnNotice(a.key()) {
			continue // answered
		}
		switch a.verb {
		case action.VerbApprove:
			if ap := p.in.Status.Approval; ap != nil && ap.Source == a.source && ap.EventID == a.id {
				a.recorded = true
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
// reaction and exactly one reply; a label, which has no comment to react to,
// with a notice when it is refused (its acceptance shows on the status
// comment).
func (p *pass) settle(ctx context.Context, a humanAction, issue *ghclient.Issue) error {
	if a.source == v1alpha1.IntentActionCommand {
		if err := p.r.GitHub.React(ctx, p.repo(), a.id); err != nil && !ghclient.IsNotFound(err) {
			return fmt.Errorf("react to comment %d: %w", a.id, err)
		}
	}
	if a.recorded {
		return p.replyDone(ctx, a)
	}
	switch {
	case a.source == v1alpha1.IntentActionCommand && !slices.Contains(command.Available(command.IntentIssue), a.verb):
		body, err := templates.RenderUnknownCommandNotice(templates.UnknownCommandNotice{
			Namespace: p.in.Namespace, Intent: p.in.Name, Key: a.key(), Verb: a.verb,
		})
		return p.notice(ctx, a.key(), a.at, body, err)
	case a.verb == action.VerbCancel:
		return p.settleCancel(ctx, a)
	case a.verb == action.VerbApprove:
		return p.settleApprove(ctx, a, issue)
	case a.verb == action.VerbReplan:
		return p.settleReplan(ctx, a, issue)
	}
	return nil
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
// posted it (re-fetched, it no longer hashes to what was recorded; deleted
// counts), and whether the issue changed since the plan was made (re-read,
// its title and body no longer render to the input snapshot's digest).
func (p *pass) approvalChanged(ctx context.Context, issue *ghclient.Issue) (planChanged, issueChanged bool, err error) {
	pl, input := p.in.Status.Plan, p.in.Status.Input
	c, err := p.r.GitHub.GetIssueComment(ctx, p.repo(), pl.CommentID)
	switch {
	case ghclient.IsNotFound(err):
		planChanged = true
	case err != nil:
		return false, false, fmt.Errorf("re-read the plan comment: %w", err)
	default:
		planChanged = digest([]byte(c.Body)) != pl.CommentDigest
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
