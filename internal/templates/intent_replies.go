// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"

	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
)

// CommandDoneNotice answers a command on the intent issue that patchy
// applied: the one reply a command gets with its outcome.
type CommandDoneNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Actor is the login that made the command.
	Actor string
	// Verb is the command's verb: approve, replan or cancel.
	Verb string
	// PlanRevision is the plan an approve approved.
	PlanRevision int32
}

// RenderCommandDoneNotice renders a CommandDoneNotice.
func RenderCommandDoneNotice(n CommandDoneNotice) (string, error) {
	var outcome string
	switch n.Verb {
	case action.VerbApprove:
		outcome = fmt.Sprintf("patchy builds plan r%d exactly as it was posted.", n.PlanRevision)
	case action.VerbReplan:
		outcome = "patchy is writing a new plan, which it will post here for approval."
	case action.VerbCancel:
		outcome = "patchy has stopped work on this intent and closed the issue. Pull requests it opened are left to you."
	default:
		return "", fmt.Errorf("render command done: no outcome for verb %q", n.Verb)
	}
	return render("intent_notice_done.md.tmpl", struct {
		Marker  string
		Command string
		Actor   string
		Outcome string
	}{
		Marker:  NoticeMarker(n.Namespace, n.Intent, n.Key),
		Command: slashCommand(n.Verb),
		Actor:   oneLine(n.Actor),
		Outcome: outcome,
	})
}

// UnknownCommandNotice answers a command whose verb the intent issue does
// not offer, or that names no verb at all.
type UnknownCommandNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Verb is the verb as command.Parse left it: 1-32 ASCII letters, or
	// empty when the command line named none.
	Verb string
}

// RenderUnknownCommandNotice renders an UnknownCommandNotice, listing the
// verbs the intent issue offers with command.Help's usage lines.
func RenderUnknownCommandNotice(n UnknownCommandNotice) (string, error) {
	return render("intent_notice_unknown.md.tmpl", struct {
		Marker  string
		Command string
		Help    string
	}{
		Marker:  NoticeMarker(n.Namespace, n.Intent, n.Key),
		Command: slashCommand(n.Verb),
		Help:    command.Help(command.IntentIssue),
	})
}

// EditedCommandNotice answers a command whose comment was edited after it was
// posted. GitHub lets anyone with write access to a repository edit anyone's
// comment and still names the original author, so an edited comment is never
// taken as its author's command: the author is asked to post it again.
type EditedCommandNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Verb is the verb the edited comment now carries, as command.Parse
	// left it; empty when it names none.
	Verb string
}

// RenderEditedCommandNotice renders an EditedCommandNotice. It names no
// actor: whoever edited the comment may not be its author.
func RenderEditedCommandNotice(n EditedCommandNotice) (string, error) {
	return render("intent_notice_edited.md.tmpl", struct {
		Marker  string
		Command string
	}{
		Marker:  NoticeMarker(n.Namespace, n.Intent, n.Key),
		Command: slashCommand(n.Verb),
	})
}

// RenderAmbiguousApprovalNotice explains why a shared issue cannot approve
// either of its Intents. The approval label is left alone for the human to
// remove and reapply after resolving the conflict.
func RenderAmbiguousApprovalNotice(namespace, intent, key, other string) (string, error) {
	return render("intent_notice_ambiguous_approval.md.tmpl", struct {
		Marker string
		Other  string
	}{
		Marker: NoticeMarker(namespace, intent, key),
		Other:  oneLine(other),
	})
}

// EstimateNotice tells the approver, before they approve, that a plan's own
// estimate of its build is more than the build will be granted. The estimate
// never binds the build and approving does not raise the grant: the remedy is
// a higher Project build limit and a new plan.
type EstimateNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// PlanRevision is the plan the estimate is from.
	PlanRevision int32
	// EstimatedMaxTurns/EstimatedTokenBudget are the plan's estimate.
	EstimatedMaxTurns    int
	EstimatedTokenBudget int
	// GrantMaxTurns/GrantTokenBudget are what its build is granted.
	GrantMaxTurns    int32
	GrantTokenBudget int64
	// TriggerLabel is the Project's trigger label, re-applied to replan.
	TriggerLabel string
}

// RenderEstimateNotice renders an EstimateNotice.
func RenderEstimateNotice(n EstimateNotice) (string, error) {
	return render("intent_notice_estimate.md.tmpl", struct {
		Marker               string
		PlanRevision         int32
		EstimatedMaxTurns    int
		EstimatedTokenBudget int
		GrantMaxTurns        int32
		GrantTokenBudget     int64
		TurnsOver            bool
		TokensOver           bool
		TriggerLabel         string
		Replan               string
	}{
		Marker:               NoticeMarker(n.Namespace, n.Intent, n.Key),
		PlanRevision:         n.PlanRevision,
		EstimatedMaxTurns:    n.EstimatedMaxTurns,
		EstimatedTokenBudget: n.EstimatedTokenBudget,
		GrantMaxTurns:        n.GrantMaxTurns,
		GrantTokenBudget:     n.GrantTokenBudget,
		TurnsOver:            int64(n.EstimatedMaxTurns) > int64(n.GrantMaxTurns),
		TokensOver:           int64(n.EstimatedTokenBudget) > n.GrantTokenBudget,
		TriggerLabel:         oneLine(n.TriggerLabel),
		Replan:               slashCommand(action.VerbReplan),
	})
}

// IntentSummaryComment is the comment patchy posts on the intent issue when
// every pull request has merged, before it closes the issue as completed.
type IntentSummaryComment struct {
	// Namespace and Intent name the Intent, for the marker (NoticeMarker,
	// keyed "summary").
	Namespace string
	Intent    string
	// PullRequests are the pull requests patchy opened, all merged.
	PullRequests []IntentPullRequest
	// Revisions counts the revision rounds.
	Revisions int32
	// CostMicroUSD is the reported spend.
	CostMicroUSD int64
}

// SummaryKey is the notice key of an intent's summary comment.
const SummaryKey = "summary"

// RenderIntentSummaryComment renders an IntentSummaryComment.
func RenderIntentSummaryComment(c IntentSummaryComment) (string, error) {
	prs := make([]statusPR, len(c.PullRequests))
	for i, pr := range c.PullRequests {
		prs[i] = statusPR{
			Ref: fmt.Sprintf("%s#%d", oneLine(pr.Repository), pr.Number),
			URL: oneLine(pr.URL),
		}
	}
	return render("intent_summary.md.tmpl", struct {
		Marker       string
		PullRequests []statusPR
		Revisions    int32
		Cost         string
	}{
		Marker:       NoticeMarker(c.Namespace, c.Intent, SummaryKey),
		PullRequests: prs,
		Revisions:    c.Revisions,
		Cost:         usd(c.CostMicroUSD),
	})
}
