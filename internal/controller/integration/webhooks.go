// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/command"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/webhook"
)

// TrackingURLIndex is the field-indexer key mapping a tracking item's html
// URL to its Finding.
const TrackingURLIndex = "status.tracking.url"

// BranchPrefix prefixes every remediation branch; the finding name follows,
// so pull-request webhooks resolve their Finding from the head ref (and
// settle it only when the PR is the one recorded for it: isRecordedPR).
const BranchPrefix = "patchy/"

// Signals applies human actions on tracking items to Findings: the writer of
// edges 16/17 (the recorded PR merged/closed), 19 (issue reopened after
// dismissal) and 20 (issue closed by a human). A close during review that
// the delivery alone cannot settle is kept on the finding instead
// (ConditionReviewClosePending), for the projection to settle against the
// recorded PR (FindingReconciler.settleReview); a command comment is kept
// likewise (status.commands), for the projection to authorise, apply and
// answer (FindingReconciler.settleCommands). Signals itself never calls
// GitHub: a delivery is answered before it is handled, so nothing retries a
// handler that fails.
type Signals struct {
	client.Client
	// Namespace the Findings live in.
	Namespace string
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics; nil discards.
	Log *slog.Logger
}

// Handle applies one tracking-system delivery.
func (s *Signals) Handle(ctx context.Context, integ *v1alpha1.Integration, e webhook.Event) error {
	switch e.Type {
	case "issues":
		return s.issues(ctx, e.Payload)
	case "issue_comment":
		return s.comment(ctx, integ, e.Payload)
	case "pull_request":
		return s.pullRequest(ctx, e.Payload)
	default:
		return nil
	}
}

type issueRef struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
}

// issues handles close (any non-terminal phase → HandedOff) and reopen
// (Dismissed → HandedOff). A close during review hands nothing off here: the
// remediation PR's body says "Fixes #N", so its merge closes the issue too,
// and deliveries are handled unordered — the issue's close can land before
// the PR's. The close is kept pending instead, and the PR's own state
// decides (settleReview): merged or closed, the PR's outcome; still open or
// unreadable, a human took the finding over — if the issue is still closed
// when read, since a quick reopen's delivery can land before this one.
func (s *Signals) issues(ctx context.Context, payload []byte) error {
	var ev struct {
		Action string   `json:"action"`
		Issue  issueRef `json:"issue"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("decode issues event: %w", err)
	}
	if ev.Action != "closed" && ev.Action != "reopened" {
		return nil
	}
	fnd, err := s.findByIssueURL(ctx, ev.Issue.HTMLURL)
	if err != nil || fnd == "" {
		return err
	}
	return s.updateFinding(ctx, fnd, func(cur *v1alpha1.Finding) error {
		if cur.Status.Tracking != nil {
			cur.Status.Tracking.State = map[string]string{"closed": "closed", "reopened": "open"}[ev.Action]
		}
		switch {
		case ev.Action == "closed" && cur.Status.Phase == v1alpha1.PhaseInReview:
			pendClose(cur, v1alpha1.ReasonTrackingIssueClosed,
				"the tracking issue closed during review; reading the remediation pull request "+
					"to tell its merge from a hand-off", s.now())
			return nil
		case ev.Action == "closed" && !v1alpha1.Terminal(cur.Status.Phase):
			return v1alpha1.SetPhase(cur, v1alpha1.PhaseHandedOff, s.now())
		case ev.Action == "reopened" && cur.Status.Phase == v1alpha1.PhaseDismissed:
			return v1alpha1.SetPhase(cur, v1alpha1.PhaseHandedOff, s.now())
		default:
			return nil
		}
	})
}

// comment records a human command made on a tracking issue: a comment that
// opens with "/patchy <verb>" (internal/command's grammar), or the
// deprecated approve comment. It records the command on the finding's
// status (status.commands.pending) and does nothing more: whether the
// commenter may issue it is for GitHub to say, and a delivery is answered
// before it is handled, so a GitHub call that failed here would lose the
// command for good. The projection settles it instead
// (FindingReconciler.settleCommands), retrying while GitHub fails.
//
// A command from a bot is ignored, the App's own "<slug>[bot]" among them,
// and so is one already recorded or answered: a duplicate or redelivered
// delivery, or a demo replay.
//
// Anyone who can comment on the issue reaches this point, and whether they
// may command the finding is not known until GitHub is asked, so the
// pending slots are shared out by account (recordCommand) rather than first
// come, first served: no one account can take them all.
func (s *Signals) comment(ctx context.Context, integ *v1alpha1.Integration, payload []byte) error {
	var ev struct {
		Action  string   `json:"action"`
		Issue   issueRef `json:"issue"`
		Comment struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			} `json:"user"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("decode issue_comment event: %w", err)
	}
	if ev.Action != "created" {
		return nil
	}
	parser := command.Parser{Surface: command.FindingIssue, ApproveAlias: approveAlias(integ)}
	cmd, ok := parser.Parse(ev.Comment.Body)
	if !ok {
		return nil
	}
	actor := v1alpha1.CommandActor{Login: ev.Comment.User.Login, ID: ev.Comment.User.ID, Type: ev.Comment.User.Type}
	attrs := make([]slog.Attr, 0, 4) // the finding joins them once resolved
	attrs = append(attrs,
		slog.Int64("comment", ev.Comment.ID), slog.String("login", actor.Login), slog.String("verb", cmd.Verb))
	switch {
	case isBot(actor):
		s.log().LogAttrs(ctx, slog.LevelInfo, "command from a bot ignored", attrs...)
		return nil
	case ev.Comment.ID <= 0 || actor.Login == "":
		s.log().LogAttrs(ctx, slog.LevelWarn, "command delivery names no comment or author; ignored", attrs...)
		return nil
	}
	fnd, err := s.findByIssueURL(ctx, ev.Issue.HTMLURL)
	if err != nil || fnd == "" {
		return err
	}
	attrs = append(attrs, slog.String("finding", fnd))
	pending := v1alpha1.FindingCommand{
		CommentID:  ev.Comment.ID,
		Actor:      actor,
		Verb:       cmd.Verb,
		Note:       cmd.Note,
		Legacy:     cmd.Alias != "",
		ReceivedAt: metav1.NewTime(s.now()),
	}
	return s.updateFinding(ctx, fnd, func(cur *v1alpha1.Finding) error {
		if commandSeen(cur.Status.Commands, pending.CommentID) {
			return nil // a duplicate or redelivered delivery, or a replay
		}
		if cur.Status.Commands == nil {
			cur.Status.Commands = &v1alpha1.FindingCommands{}
		}
		recorded, evicted := recordCommand(cur.Status.Commands, pending)
		switch {
		case !recorded:
			s.log().LogAttrs(ctx, slog.LevelWarn,
				"command not recorded: the account already has its share pending, or every slot is held", attrs...)
		case evicted != nil:
			s.log().LogAttrs(ctx, slog.LevelWarn, "command recorded in the slot of another account's newer command",
				append(attrs, slog.Int64("dropped_comment", evicted.CommentID),
					slog.String("dropped_login", evicted.Actor.Login))...)
		default:
			s.log().LogAttrs(ctx, slog.LevelInfo, "command recorded", attrs...)
		}
		return nil
	})
}

// recordCommand adds c to cmds.Pending, sharing the slots out by account,
// and reports whether it did. An account already holding
// MaxPendingCommandsPerActor gets no more. When every slot is held, c takes
// the slot of the newest undecided command of an account holding more than
// one, which is returned; with no such command, c is not recorded. A
// decided command is never dropped: it may already have taken effect.
func recordCommand(
	cmds *v1alpha1.FindingCommands, c v1alpha1.FindingCommand,
) (recorded bool, evicted *v1alpha1.FindingCommand) {
	if pendingBy(cmds.Pending, c.Actor) >= v1alpha1.MaxPendingCommandsPerActor {
		return false, nil
	}
	if len(cmds.Pending) >= v1alpha1.MaxPendingCommands {
		victim := -1
		for i, p := range cmds.Pending {
			if p.Outcome == "" && pendingBy(cmds.Pending, p.Actor) > 1 &&
				(victim < 0 || p.CommentID > cmds.Pending[victim].CommentID) {
				victim = i
			}
		}
		if victim < 0 {
			return false, nil
		}
		dropped := cmds.Pending[victim]
		evicted = &dropped
		cmds.Pending = slices.Delete(cmds.Pending, victim, victim+1)
	}
	cmds.Pending = append(cmds.Pending, c)
	return true, evicted
}

// pendingBy counts the commands in pending that actor wrote.
func pendingBy(pending []v1alpha1.FindingCommand, actor v1alpha1.CommandActor) int {
	n := 0
	for _, p := range pending {
		if sameActor(p.Actor, actor) {
			n++
		}
	}
	return n
}

// sameActor reports whether a and b are one GitHub account: by its numeric
// id when both carry one (a login can be renamed), else by login, which
// GitHub compares without case.
func sameActor(a, b v1alpha1.CommandActor) bool {
	if a.ID > 0 && b.ID > 0 {
		return a.ID == b.ID
	}
	return strings.EqualFold(a.Login, b.Login)
}

// approveAlias is the Integration's configured approve comment; empty means
// the parser's default, "/approve".
func approveAlias(integ *v1alpha1.Integration) string {
	if integ == nil || integ.Spec.GitHub == nil || integ.Spec.GitHub.Issues == nil {
		return ""
	}
	return integ.Spec.GitHub.Issues.ApproveComment
}

// isBot reports a bot account: GitHub's Bot type, which every App's
// "<slug>[bot]" user carries, or a login of that form.
func isBot(a v1alpha1.CommandActor) bool {
	return a.Type == "Bot" || strings.HasSuffix(a.Login, "[bot]")
}

// commandSeen reports whether the command in comment id is recorded on the
// finding, or remembered as answered: in consumed, or the last suspend or
// resume applied. The check is exact. Nothing is inferred from an id's
// place among those remembered: a delivery can arrive long after later
// commands are answered (the redelivery sweep resends one the webhook
// queue turned away), and it must still be answered.
func commandSeen(cmds *v1alpha1.FindingCommands, id int64) bool {
	if cmds == nil {
		return false
	}
	return slices.ContainsFunc(cmds.Pending, func(c v1alpha1.FindingCommand) bool { return c.CommentID == id }) ||
		slices.Contains(cmds.Consumed, id) || (cmds.LastToggle > 0 && id == cmds.LastToggle)
}

// repoRef is a delivery's reference to a repository.
type repoRef struct {
	FullName string `json:"full_name"`
}

// pullRequest handles merge/close of a remediation PR, resolved to its
// Finding by the branch name and settled only when it is the PR recorded
// for that Finding (isRecordedPR). One that differs only in its repository
// (unrecordedRepoPR) is kept pending, for the recorded PR's own state to
// settle (settleReview).
func (s *Signals) pullRequest(ctx context.Context, payload []byte) error {
	var ev struct {
		Action      string `json:"action"`
		PullRequest struct {
			Number         int64  `json:"number"`
			HTMLURL        string `json:"html_url"`
			Merged         bool   `json:"merged"`
			MergedAt       string `json:"merged_at"`
			MergeCommitSHA string `json:"merge_commit_sha"`
			Head           struct {
				Ref  string  `json:"ref"`
				Repo repoRef `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository repoRef `json:"repository"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("decode pull_request event: %w", err)
	}
	if ev.Action != "closed" || !strings.HasPrefix(ev.PullRequest.Head.Ref, BranchPrefix) {
		return nil
	}
	fnd := strings.TrimPrefix(ev.PullRequest.Head.Ref, BranchPrefix)
	closed := prClose{
		merged:         ev.PullRequest.Merged,
		mergeCommitSHA: ev.PullRequest.MergeCommitSHA,
	}
	if at, err := time.Parse(time.RFC3339, ev.PullRequest.MergedAt); err == nil {
		closed.mergedAt = at
	}
	repo, headRepo, number := ev.Repository.FullName, ev.PullRequest.Head.Repo.FullName, ev.PullRequest.Number
	return s.updateFinding(ctx, fnd, func(cur *v1alpha1.Finding) error {
		if cur.Status.Phase != v1alpha1.PhaseInReview {
			// Stale or duplicate — or the second of a merge's two
			// deliveries, after its tracking issue's close settled it.
			return nil
		}
		attrs := []slog.Attr{
			slog.String("finding", cur.Name), slog.String("repository", repo),
			slog.Int64("number", number), slog.String("head_repository", headRepo),
		}
		switch {
		case isRecordedPR(cur, repo, headRepo, number):
			return closed.settle(cur, s.now())
		case unrecordedRepoPR(cur, repo, headRepo, number):
			s.log().LogAttrs(ctx, slog.LevelInfo,
				"closed pull request names an unrecorded repository; confirming against the recorded one", attrs...)
			// An issue close already pending says more: while the PR is
			// open, it is a hand-off.
			if !meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionReviewClosePending) {
				pendClose(cur, v1alpha1.ReasonUnrecordedRepository, fmt.Sprintf(
					"pull request #%d closed in %s, not the recorded repository; reading the recorded pull request "+
						"to tell a rename or transfer from another repository's pull request", number, repo), s.now())
			}
			return nil
		default:
			s.log().LogAttrs(ctx, slog.LevelInfo, "closed pull request is not the finding's recorded one; ignored",
				attrs...)
			return nil
		}
	})
}

// prClose is the close of a finding's remediation PR, as the PR's own
// delivery or, when a close is pending (settleReview), the API reports it.
type prClose struct {
	merged         bool
	mergedAt       time.Time // zero when unknown
	mergeCommitSHA string
}

// settle applies the close to an InReview finding: merged → Remediated
// (edge 16), recording when and the commit the merge put on the base branch;
// closed unmerged → Failed (edge 17). Any close pending goes with it.
func (c prClose) settle(cur *v1alpha1.Finding, now time.Time) error {
	meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionReviewClosePending)
	to := v1alpha1.PhaseFailed
	state := "closed"
	if c.merged {
		to = v1alpha1.PhaseRemediated
		state = "merged"
	}
	if cur.Status.PullRequest != nil {
		cur.Status.PullRequest.State = state
		if c.merged {
			if !c.mergedAt.IsZero() {
				t := metav1.NewTime(c.mergedAt)
				cur.Status.PullRequest.MergedAt = &t
			}
			// A commit id longer than a SHA-256 hex digest is not one;
			// record nothing rather than a truncated id.
			if sha := c.mergeCommitSHA; len(sha) <= 64 {
				cur.Status.PullRequest.MergeCommitSHA = sha
			}
		}
	}
	return v1alpha1.SetPhase(cur, to, now)
}

// isRecordedPR reports whether a closed pull request — number, in repo, from
// a branch in headRepo (owner/name each) — is the remediation PR recorded
// for the finding: the same number, in the repository the record names, from
// a branch in that same repository. The head ref alone proves nothing:
// anyone who can open a pull request can name a branch patchy/<finding>, and
// a fork's branch carries whatever name its owner chose.
func isRecordedPR(f *v1alpha1.Finding, repo, headRepo string, number int64) bool {
	pr := f.Status.PullRequest
	if pr == nil || pr.Number != number {
		return false
	}
	want, ok := recordedPRRepo(f)
	return ok && strings.EqualFold(repo, want.String()) && strings.EqualFold(headRepo, repo)
}

// unrecordedRepoPR reports a closed pull request that isRecordedPR rejects
// for its repository alone: the recorded number, from a branch in the
// repository it closed in, which is not the recorded one. A rename or
// transfer of the recorded repository looks exactly so — every delivery
// then carries the new name — and so does another repository's PR of the
// same number. Only the recorded PR's own state tells them apart, which is
// why the delivery settles nothing itself.
func unrecordedRepoPR(f *v1alpha1.Finding, repo, headRepo string, number int64) bool {
	pr := f.Status.PullRequest
	return pr != nil && pr.Number == number && repo != "" && strings.EqualFold(headRepo, repo)
}

// recordedPRRepo is the repository the finding's remediation PR lives in:
// the one its recorded URL names, else the finding's own, where
// remediation-controller opens every PR. false when neither parses.
func recordedPRRepo(f *v1alpha1.Finding) (ghclient.Repo, bool) {
	if pr := f.Status.PullRequest; pr != nil && pr.URL != "" {
		if _, repo, err := forge.ParseRepoURL(pr.URL); err == nil {
			return repo, true
		}
	}
	if f.Spec.Repository != nil {
		if repo, err := parseOwnerRepo(f.Spec.Repository.Name); err == nil {
			return repo, true
		}
	}
	return ghclient.Repo{}, false
}

// findByIssueURL resolves a Finding by its projected tracking URL; empty
// when the issue is not one of ours.
func (s *Signals) findByIssueURL(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", nil
	}
	var list v1alpha1.FindingList
	if err := s.List(ctx, &list, client.InNamespace(s.Namespace),
		client.MatchingFields{TrackingURLIndex: url}); err != nil {
		return "", fmt.Errorf("index findings by tracking url: %w", err)
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	return list.Items[0].Name, nil
}

// updateFinding applies mutate under conflict retry; a vanished Finding, or
// a delivery that changes nothing (a duplicate, or the second of a merge's
// two), is a no-op that writes nothing.
func (s *Signals) updateFinding(ctx context.Context, name string, mutate func(*v1alpha1.Finding) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := s.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		before := cur.Status.DeepCopy()
		if err := mutate(&cur); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		return s.Status().Update(ctx, &cur)
	})
}

func (s *Signals) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Signals) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}
