// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

const (
	// commandRecheck paces a pending command's wait for an Integration to
	// read GitHub through, as reviewCloseRecheck paces a review close's:
	// nothing watches Integrations, so the finding re-queues itself.
	commandRecheck = reviewCloseRecheck
	// commandAnswerTimeout bounds how long an answer GitHub keeps failing
	// is retried, from the command's AnsweringSince. Past it the command is
	// consumed without its reaction or reply (its effect, if any, is
	// already on the spec), so a tracking issue that will not take a reply
	// holds no pending slot, and no command behind it, for longer.
	commandAnswerTimeout = time.Hour
	// answerSkew widens the listing that looks for a reply an earlier pass
	// may have posted: GitHub filters comments by its own clock, and
	// AnsweringSince is this controller's.
	answerSkew = 5 * time.Minute
)

var (
	// errCommandGone: the command a step was taken for is no longer pending
	// on the API server's copy of the finding. An earlier pass answered it,
	// or the finding is gone.
	errCommandGone = errors.New("command no longer pending")
	// errIssueUnlinked: the tracking issue the reply was for is gone, and
	// its link was dropped. The projection opens a fresh issue, and the
	// command is answered there.
	errIssueUnlinked = errors.New("tracking issue unlinked")
)

// settleCommands takes the next command pending on the finding
// (status.commands.pending, which Signals records; nextCommand says which)
// as far as it can go. Decisions and effects are taken in comment-id order:
// GitHub's ids grow with creation, so a suspend and the resume written after
// it apply in that order when both are pending, and one written before the
// last suspend or resume decided is superseded, however late it arrives. A
// command moves through four steps, each a durable write, and a retry after
// any failure resumes at the step that failed:
//
//  1. Decide. A verb a tracking issue does not offer is answered with the
//     ones it does. Otherwise the commenter needs write access to the
//     tracking issue's repository (mayCommand), a suspend or resume must be
//     newer than the last one decided Done (LastToggle), and then the
//     finding's phase must admit the verb, gated by action.Apply exactly as
//     the status page and the CLI gate it. The outcome is written to the
//     command before anything acts on it, so a retry never decides again
//     and the reply never changes.
//  2. Apply (Done only). The effect is written to the spec through
//     action.Apply, as the status page writes it: spec only, so the phase
//     stays with the controller that owns the edge. Then it is marked
//     applied, so a retry never writes the spec twice.
//  3. Answer. An eyes reaction on the comment, then exactly one reply
//     giving the outcome, headed by a marker keyed by the comment id; a
//     refusal to an account already sent one on this finding gets the
//     reaction alone. The thread is never listed to post it (answer says
//     how the reply stays single), so a command costs the same GitHub calls
//     however many comments the issue holds.
//  4. Consume. The command leaves pending and, unless it was refused, its
//     id joins consumed, so a redelivered or replayed delivery never
//     records it again.
//
// Every GitHub call goes through the Integration Signals is handed. A
// GitHub failure while deciding is returned for the reconcile's backoff to
// retry, so a command is never decided without GitHub's answer; an answer
// GitHub keeps failing is given up after commandAnswerTimeout. With no such
// Integration (suspended, its issues turned off, or deleted) the command
// waits, re-checked every commandRecheck; with no tracking issue linked it
// waits for the projection to open one.
//
// settled reports that this pass wrote the finding, or found it changed
// under the steps, so this reconcile should stop: the write re-queues it. An
// error with settled false wrote nothing, and the caller carries on with the
// projection before returning it, so a command GitHub keeps failing holds up
// nothing else the reconcile does.
func (r *FindingReconciler) settleCommands(
	ctx context.Context, fnd *v1alpha1.Finding,
) (settled bool, wait time.Duration, err error) {
	if fnd.Status.Commands == nil || len(fnd.Status.Commands.Pending) == 0 {
		return false, 0, nil
	}
	// The cache can lag this reconciler's own writes, so a command it shows
	// pending may already be answered: the API server's copy decides. Two
	// reconciles of one finding never run at once, so nothing else answers
	// a command that copy shows pending while this one works on it.
	cur, err := r.latest(ctx, fnd)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return true, 0, nil
		}
		return false, 0, err
	}
	cmd := nextCommand(cur)
	if cmd == nil {
		return false, 0, nil
	}
	attrs := []slog.Attr{
		slog.String("finding", cur.Name), slog.Int64("comment", cmd.CommentID),
		slog.String("login", cmd.Actor.Login), slog.String("verb", cmd.Verb),
	}
	tr := cur.Status.Tracking
	if tr == nil || tr.IssueNumber == 0 {
		// Unlinked since the command was made: its issue is gone. The
		// projection opens a fresh one, and its link write re-queues us.
		return false, 0, nil
	}
	_, repo, err := forge.ParseRepoURL(tr.URL)
	if err != nil {
		r.log().LogAttrs(ctx, slog.LevelWarn, "tracking issue URL unreadable; the command waits",
			append(attrs, slog.String("url", tr.URL), slog.Any("error", err))...)
		return false, 0, nil
	}
	integ, err := selectIntegration(ctx, r.Client, cur.Namespace, issuesEnabled)
	if errors.Is(err, ErrNoIntegration) {
		r.log().LogAttrs(ctx, slog.LevelInfo, "no issues-enabled integration to read GitHub through; the command waits",
			append(attrs, slog.Duration("recheck", commandRecheck))...)
		return false, commandRecheck, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("finding %s: settle command: %w", cur.Name, err)
	}
	gh, err := r.clientFor(ctx, integ, repo)
	if err != nil {
		return false, 0, fmt.Errorf("finding %s: settle command: tracking client: %w", cur.Name, err)
	}
	ans := commandAnswer{r: r, gh: gh, repo: repo, number: int(tr.IssueNumber), id: cmd.CommentID, attrs: attrs}
	switch wrote, err := ans.settle(ctx, cur); {
	case errors.Is(err, errCommandGone), errors.Is(err, errIssueUnlinked):
		return true, 0, nil
	case err != nil:
		return wrote, 0, fmt.Errorf("finding %s: command in comment %d: %w", cur.Name, cmd.CommentID, err)
	}
	return true, 0, nil
}

// commandAnswer is one pending command being answered through gh, on the
// tracking issue number in repo.
type commandAnswer struct {
	r      *FindingReconciler
	gh     trackerClient
	repo   ghclient.Repo
	number int
	id     int64 // the command's comment id
	attrs  []slog.Attr
}

// settle takes the command from wherever an earlier pass left it through to
// consumed. fnd is the API server's copy of the finding. wrote reports that
// this pass wrote the finding, error or not.
func (a commandAnswer) settle(ctx context.Context, fnd *v1alpha1.Finding) (wrote bool, err error) {
	cmd := pendingCommand(fnd, a.id)
	if cmd == nil {
		return false, errCommandGone
	}
	if cmd.Outcome == "" {
		if fnd, err = a.decideAndRecord(ctx, fnd, *cmd); err != nil {
			return false, err
		}
		wrote = true
	}
	if cmd = pendingCommand(fnd, a.id); cmd.Outcome == v1alpha1.CommandDone && !cmd.Applied {
		if fnd, err = a.apply(ctx, fnd); err != nil {
			return wrote, err
		}
		wrote = true
	}
	fnd, answerWrote, err := a.answer(ctx, fnd)
	wrote = wrote || answerWrote
	if err != nil {
		return wrote, err
	}
	if err := a.consume(ctx, fnd); err != nil {
		return wrote, err
	}
	return true, nil
}

// decideAndRecord decides the command and writes the outcome to it,
// returning the finding as written. The decision time is stamped to the
// second the API server keeps, so the time a Done command's effect is
// recorded with reads back exactly as it was written (appliedBy). The same
// write records what the decision leaves on the finding's commands: a
// suspend or resume decided Done becomes LastToggle, and a refusal's author
// joins RefusedActors, its reply made Quiet if they were there already.
func (a commandAnswer) decideAndRecord(
	ctx context.Context, fnd *v1alpha1.Finding, cmd v1alpha1.FindingCommand,
) (*v1alpha1.Finding, error) {
	now := a.r.now().Truncate(time.Second)
	outcome, available, err := a.decide(ctx, fnd, cmd, now)
	if err != nil {
		return nil, err
	}
	at := metav1.NewTime(now)
	out, err := a.update(ctx, fnd, func(cmds *v1alpha1.FindingCommands, c *v1alpha1.FindingCommand) {
		c.Outcome, c.DecidedAt, c.Available = outcome, &at, available
		switch {
		case outcome == v1alpha1.CommandDone && isToggle(c.Verb):
			cmds.LastToggle = max(cmds.LastToggle, c.CommentID)
		case refusal(outcome) && c.Actor.ID > 0:
			c.Quiet = slices.Contains(cmds.RefusedActors, c.Actor.ID)
			cmds.RefusedActors = rememberActor(cmds.RefusedActors, c.Actor.ID)
		}
	})
	if err != nil {
		return nil, err
	}
	a.r.log().LogAttrs(ctx, slog.LevelInfo, "command decided",
		append(a.attrs, slog.String("outcome", string(outcome)))...)
	return out, nil
}

// decide answers the command as it stands against fnd at now: whether its
// verb is one a tracking issue offers, whether its author may command the
// finding, whether a suspend or resume is newer than the last one decided
// Done, and whether the finding's phase admits the verb. For an Unavailable
// outcome it also returns the verbs the phase does admit.
func (a commandAnswer) decide(
	ctx context.Context, fnd *v1alpha1.Finding, c v1alpha1.FindingCommand, now time.Time,
) (v1alpha1.CommandOutcome, []string, error) {
	if !slices.Contains(command.Available(command.FindingIssue), c.Verb) {
		return v1alpha1.CommandUnknownVerb, nil, nil
	}
	ok, err := a.mayCommand(ctx, c.Actor)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return v1alpha1.CommandNotAllowed, nil, nil
	}
	// Written before a suspend or resume already decided, delivered after
	// it: applying it now would undo the later one.
	if isToggle(c.Verb) && fnd.Status.Commands != nil && c.CommentID < fnd.Status.Commands.LastToggle {
		return v1alpha1.CommandSuperseded, nil, nil
	}
	// A probe on a copy: the effect is written once decided (apply).
	_, err = action.Apply(fnd.DeepCopy(), c.Verb, c.Actor.Login, c.Note, now)
	switch {
	case errors.Is(err, action.ErrUnavailable):
		return v1alpha1.CommandUnavailable, action.Available(fnd, now), nil
	case errors.Is(err, action.ErrUnknownVerb):
		return v1alpha1.CommandUnknownVerb, nil, nil
	case err != nil:
		return "", nil, err
	}
	return v1alpha1.CommandDone, nil, nil
}

// mayCommand reports whether actor may command the finding: write access or
// more to the tracking issue's repository (ghclient.CanWrite: admin,
// maintain or write). GitHub's "read" is no authority, since a public
// repository grants it to every account, and a login GitHub answers 404 for
// has no account at all. A bot never may, and a credential GitHub refuses
// the lookup (403) fails closed. author_association plays no part: an
// organization member without write access is refused.
func (a commandAnswer) mayCommand(ctx context.Context, actor v1alpha1.CommandActor) (bool, error) {
	if isBot(actor) {
		return false, nil
	}
	perm, err := a.gh.CollaboratorPermission(ctx, a.repo, actor.Login)
	switch {
	case errors.Is(err, ghclient.ErrNoSuchUser):
		return false, nil
	case ghclient.IsForbidden(err):
		a.r.log().LogAttrs(ctx, slog.LevelWarn, "collaborator permission unreadable; the command is refused",
			append(a.attrs, slog.String("repository", a.repo.String()), slog.Any("error", err))...)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read permission of %s on %s: %w", actor.Login, a.repo, err)
	}
	return ghclient.CanWrite(perm), nil
}

// apply writes a Done command's effect to the spec through action.Apply,
// recorded with the command's author, note and decision time, then marks the
// command applied. It is the spec write the status page's action makes:
// spec only, so the phase stays with the controller that owns its edge.
// A finding that moved, since the decision, to a phase where the verb means
// nothing has the decision revised to Unavailable, before any reply says
// otherwise, unless the effect already on the spec is this command's own
// (appliedBy): a retry after the spec write landed and the finding moved on
// because of it.
func (a commandAnswer) apply(ctx context.Context, fnd *v1alpha1.Finding) (*v1alpha1.Finding, error) {
	var revised v1alpha1.CommandOutcome
	var available []string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		revised, available = "", nil
		cur, c, err := a.read(ctx, fnd)
		if err != nil {
			return err
		}
		if c.Outcome != v1alpha1.CommandDone || c.Applied {
			return nil
		}
		at := c.ReceivedAt.Time
		if c.DecidedAt != nil {
			at = c.DecidedAt.Time
		}
		changed, err := action.Apply(cur, c.Verb, c.Actor.Login, c.Note, at)
		switch {
		case errors.Is(err, action.ErrUnavailable):
			if !appliedBy(cur, c, at) {
				revised, available = v1alpha1.CommandUnavailable, action.Available(cur, a.r.now())
			}
			return nil
		case errors.Is(err, action.ErrUnknownVerb):
			revised = v1alpha1.CommandUnknownVerb
			return nil
		case err != nil:
			return err
		case !changed:
			return nil
		}
		return a.r.Update(ctx, cur)
	})
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	if revised != "" {
		a.r.log().LogAttrs(ctx, slog.LevelInfo, "command no longer applies; decision revised",
			append(a.attrs, slog.String("outcome", string(revised)))...)
	}
	return a.update(ctx, fnd, func(_ *v1alpha1.FindingCommands, c *v1alpha1.FindingCommand) {
		if revised != "" {
			c.Outcome, c.Available = revised, available
			return
		}
		c.Applied = true
	})
}

// appliedBy reports whether f's spec carries c's own effect, recorded by
// c's author at at.
func appliedBy(f *v1alpha1.Finding, c *v1alpha1.FindingCommand, at time.Time) bool {
	mine := func(by string, when metav1.Time) bool {
		return by == c.Actor.Login && when.Unix() == at.Unix()
	}
	switch c.Verb {
	case action.VerbApprove:
		return f.Spec.Approval != nil && mine(f.Spec.Approval.By, f.Spec.Approval.At)
	case action.VerbRetry:
		return f.Spec.Retry != nil && mine(f.Spec.Retry.By, f.Spec.Retry.At)
	case action.VerbExpedite:
		return f.Spec.Expedite != nil && mine(f.Spec.Expedite.By, f.Spec.Expedite.At)
	}
	return false
}

// answer reacts to the command's comment with eyes and posts the one reply
// giving its outcome (none for a Quiet refusal), returning the finding as
// last written and whether this pass wrote it.
//
// AnsweringSince is written first. A pass that finds it already set knows
// an earlier one may have posted the reply before failing, and looks for it
// among the issue's comments since then (replyPosted) rather than posting
// another. That is the only listing, and it is bounded by time rather than
// by the thread: a first attempt, however long the thread, costs a reaction
// and a post.
//
// A comment deleted since (404) is answered without the reaction. A
// tracking issue gone (404) is unlinked: the command stays pending and is
// answered on the fresh issue the projection opens (errIssueUnlinked). Any
// other failure is returned for the backoff, until commandAnswerTimeout
// past AnsweringSince, when the answer is given up and the command is left
// to be consumed without it.
func (a commandAnswer) answer(
	ctx context.Context, fnd *v1alpha1.Finding,
) (_ *v1alpha1.Finding, wrote bool, err error) {
	resumed := pendingCommand(fnd, a.id).AnsweringSince != nil
	if !resumed {
		since := metav1.NewTime(a.r.now())
		fnd, err = a.update(ctx, fnd, func(_ *v1alpha1.FindingCommands, c *v1alpha1.FindingCommand) {
			if c.AnsweringSince == nil {
				c.AnsweringSince = &since
			}
		})
		if err != nil {
			return nil, false, err
		}
		wrote = true
	}
	c := pendingCommand(fnd, a.id)
	err = a.reactAndReply(ctx, fnd, *c, resumed)
	switch {
	case err == nil:
		return fnd, wrote, nil
	case errors.Is(err, errIssueUnlinked):
		return nil, wrote, err
	case a.r.now().Sub(c.AnsweringSince.Time) >= commandAnswerTimeout:
		a.r.log().LogAttrs(ctx, slog.LevelWarn, "GitHub kept failing the command's answer; given up",
			append(a.attrs, slog.Duration("after", commandAnswerTimeout), slog.Any("error", err))...)
		return fnd, wrote, nil
	}
	return nil, wrote, err
}

// reactAndReply makes the answer's two GitHub calls: the eyes reaction, then
// the reply, unless the command is Quiet or resumed finds it already
// posted.
func (a commandAnswer) reactAndReply(
	ctx context.Context, fnd *v1alpha1.Finding, c v1alpha1.FindingCommand, resumed bool,
) error {
	switch err := a.gh.CreateIssueCommentReaction(ctx, a.repo, c.CommentID, ghclient.ReactionEyes); {
	case ghclient.IsNotFound(err):
		a.r.log().LogAttrs(ctx, slog.LevelInfo, "command comment gone; answering without a reaction", a.attrs...)
	case err != nil:
		return fmt.Errorf("react to the comment: %w", err)
	}
	if c.Quiet {
		return nil
	}
	if resumed {
		posted, err := a.replyPosted(ctx, commandMarker(c.CommentID), c.AnsweringSince.Add(-answerSkew))
		if err != nil {
			return a.issueFailed(ctx, fnd, err)
		}
		if posted {
			return nil
		}
	}
	if _, err := a.gh.CreateComment(ctx, a.repo, a.number, commandReply(c, a.repo)); err != nil {
		return a.issueFailed(ctx, fnd, fmt.Errorf("reply: %w", err))
	}
	return nil
}

// replyPosted reports whether the reply headed by marker is already on the
// issue, posted by a pass that failed before it consumed the command. Only
// the comments since since are listed, and only the projection's own count:
// markers are predictable, so a comment anyone else wrote with one proves
// nothing (issueComments.own).
func (a commandAnswer) replyPosted(ctx context.Context, marker string, since time.Time) (bool, error) {
	issue, err := a.gh.GetIssue(ctx, a.repo, a.number)
	if err != nil {
		return false, fmt.Errorf("get tracking issue: %w", err)
	}
	cs, err := a.gh.ListIssueComments(ctx, a.repo, a.number, since)
	if err != nil {
		return false, fmt.Errorf("list recent comments: %w", err)
	}
	var own []*ghclient.Comment
	for _, cm := range cs {
		if issue.Author != "" && cm.UserLogin == issue.Author {
			own = append(own, cm)
		}
	}
	return findSticky(own, marker) != nil, nil
}

// issueFailed maps a failed call on the tracking issue: a 404 means the
// issue is gone, so its link is dropped (errIssueUnlinked); anything else is
// returned as it is.
func (a commandAnswer) issueFailed(ctx context.Context, fnd *v1alpha1.Finding, err error) error {
	if !ghclient.IsNotFound(err) {
		return err
	}
	if err := a.r.unlinkIfGone(ctx, fnd, err); err != nil {
		return err
	}
	return errIssueUnlinked
}

// consume retires the answered command in one status write: it leaves
// pending and, unless it was refused, its id joins consumed. A refused
// command's id is left out, so no amount of commenting by accounts without
// write access pushes a maintainer's command out of consumed; delivered
// again, a refused command is only refused again.
func (a commandAnswer) consume(ctx context.Context, fnd *v1alpha1.Finding) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, c, err := a.read(ctx, fnd)
		if errors.Is(err, errCommandGone) {
			return nil
		}
		if err != nil {
			return err
		}
		cmds := cur.Status.Commands
		if !refusal(c.Outcome) {
			cmds.Consumed = consumeID(cmds.Consumed, a.id)
		}
		cmds.Pending = slices.DeleteFunc(cmds.Pending, func(c v1alpha1.FindingCommand) bool {
			return c.CommentID == a.id
		})
		return a.r.Status().Update(ctx, cur)
	})
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	a.r.log().LogAttrs(ctx, slog.LevelInfo, "command answered", a.attrs...)
	return nil
}

// update applies change to the pending command, and to the finding's
// commands around it, on the API server's copy of the finding, under
// conflict retry, and returns that copy as written.
func (a commandAnswer) update(
	ctx context.Context, fnd *v1alpha1.Finding, change func(*v1alpha1.FindingCommands, *v1alpha1.FindingCommand),
) (*v1alpha1.Finding, error) {
	var out *v1alpha1.Finding
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, c, err := a.read(ctx, fnd)
		if err != nil {
			return err
		}
		change(cur.Status.Commands, c)
		if err := a.r.Status().Update(ctx, cur); err != nil {
			return err
		}
		out = cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// read returns the API server's copy of the finding and the pending
// command in it; errCommandGone when either is gone.
func (a commandAnswer) read(
	ctx context.Context, fnd *v1alpha1.Finding,
) (*v1alpha1.Finding, *v1alpha1.FindingCommand, error) {
	var cur v1alpha1.Finding
	if err := a.r.apiReader().Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil, errCommandGone
		}
		return nil, nil, fmt.Errorf("re-read finding: %w", err)
	}
	c := pendingCommand(&cur, a.id)
	if c == nil {
		return nil, nil, errCommandGone
	}
	return &cur, c, nil
}

// nextCommand is the pending command to take a step on next, or nil: the
// smallest comment id still to be decided or applied, since those steps
// take effect and follow the order the commands were written in; failing
// that, the smallest still to be answered. An answer GitHub keeps failing
// so holds up no later command's effect.
func nextCommand(f *v1alpha1.Finding) *v1alpha1.FindingCommand {
	if f.Status.Commands == nil {
		return nil
	}
	var next *v1alpha1.FindingCommand
	for i := range f.Status.Commands.Pending {
		if c := &f.Status.Commands.Pending[i]; next == nil || stepBefore(c, next) {
			next = c
		}
	}
	return next
}

// stepBefore reports whether a's next step comes before b's: a step that
// takes effect before an answer, then comment-id order.
func stepBefore(a, b *v1alpha1.FindingCommand) bool {
	if ea, eb := toTakeEffect(a), toTakeEffect(b); ea != eb {
		return ea
	}
	return a.CommentID < b.CommentID
}

// toTakeEffect reports whether c still has its decision, or its effect, to
// write.
func toTakeEffect(c *v1alpha1.FindingCommand) bool {
	return c.Outcome == "" || (c.Outcome == v1alpha1.CommandDone && !c.Applied)
}

// isToggle reports the verbs that undo each other, whose order is kept
// beyond the pending list (FindingCommands.LastToggle).
func isToggle(verb string) bool {
	return verb == action.VerbSuspend || verb == action.VerbResume
}

// refusal reports the outcomes that refuse a command outright: it had no
// effect, and needs no remembering.
func refusal(o v1alpha1.CommandOutcome) bool {
	return o == v1alpha1.CommandNotAllowed || o == v1alpha1.CommandUnknownVerb
}

// rememberActor adds id to ids, keeping the latest MaxRefusedActors of them.
func rememberActor(ids []int64, id int64) []int64 {
	if slices.Contains(ids, id) {
		return ids
	}
	out := append(slices.Clone(ids), id)
	if over := len(out) - v1alpha1.MaxRefusedActors; over > 0 {
		out = slices.Clone(out[over:])
	}
	return out
}

// pendingCommand is the pending command in comment id, or nil.
func pendingCommand(f *v1alpha1.Finding, id int64) *v1alpha1.FindingCommand {
	if f.Status.Commands == nil {
		return nil
	}
	for i := range f.Status.Commands.Pending {
		if f.Status.Commands.Pending[i].CommentID == id {
			return &f.Status.Commands.Pending[i]
		}
	}
	return nil
}

// consumeID adds id to ids, keeping the largest MaxConsumedCommands of
// them, ascending.
func consumeID(ids []int64, id int64) []int64 {
	out := slices.Clone(ids)
	if !slices.Contains(out, id) {
		out = append(out, id)
	}
	slices.Sort(out)
	if over := len(out) - v1alpha1.MaxConsumedCommands; over > 0 {
		out = slices.Clone(out[over:])
	}
	return out
}

// commandMarker heads the reply to the command in comment id.
func commandMarker(id int64) string {
	return fmt.Sprintf("<!-- patchy:command %d -->", id)
}

// commandDone says what each verb's Done outcome means for the finding.
var commandDone = map[string]string{
	action.VerbApprove:  "patchy takes this finding forward.",
	action.VerbRetry:    "patchy retries this finding from the state it failed in.",
	action.VerbExpedite: "this finding skips the accumulation window, the minimum age and the queue.",
	action.VerbSuspend: "this finding's progress through the pipeline is paused until `" +
		command.Prefix + " " + action.VerbResume + "`.",
	action.VerbResume: "this finding's progress through the pipeline continues.",
}

// commandReply is the one reply to a decided command, headed by its
// marker. It echoes the verb only as command.Parse left it: 1-32 ASCII
// letters, or empty.
func commandReply(c v1alpha1.FindingCommand, repo ghclient.Repo) string {
	var b strings.Builder
	b.WriteString(commandMarker(c.CommentID) + "\n@" + c.Actor.Login + " ")
	used := "`" + command.Prefix + " " + c.Verb + "`"
	switch c.Outcome {
	case v1alpha1.CommandDone:
		b.WriteString(used + " is done: " + commandDone[c.Verb])
	case v1alpha1.CommandUnavailable:
		b.WriteString(used + " is not available for this finding in its current phase.")
		if help := command.HelpFor(command.FindingIssue, c.Available); help != "" {
			b.WriteString("\n\n" + help)
		} else {
			b.WriteString(" No command is available for it now.")
		}
	case v1alpha1.CommandNotAllowed:
		b.WriteString("you may not use " + used + " here: commands on this issue need write access to " +
			repo.String() + ".")
	case v1alpha1.CommandSuperseded:
		b.WriteString(used + " was written before the last `" + command.Prefix + " " + action.VerbSuspend +
			"` or `" + command.Prefix + " " + action.VerbResume + "` patchy applied, so it is not applied: " +
			"the later command stands.")
	default:
		if c.Verb == "" {
			b.WriteString("that is not a command patchy knows.")
		} else {
			b.WriteString(used + " is not a command patchy knows.")
		}
		b.WriteString("\n\n" + command.Help(command.FindingIssue))
	}
	if c.Legacy {
		b.WriteString("\n\nThe approve comment you used is deprecated: comment `" +
			command.Prefix + " " + action.VerbApprove + "` instead.")
	}
	return b.String()
}
