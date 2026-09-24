// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// maintainer is the commenter the command tests grant write access to.
const maintainer = "maintainer"

// commentPayload is the issue_comment.created delivery of comment id on the
// tracking issue (#7), written by maintainer, a User.
func commentPayload(t *testing.T, id int64, body string) string {
	t.Helper()
	return commentPayloadAs(t, "created", id, body, maintainer, "User")
}

// commentPayloadAs is commentPayload with the delivery's action and the
// author's account type chosen.
func commentPayloadAs(t *testing.T, act string, id int64, body, login, typ string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"action": act,
		"issue":  map[string]any{"number": 7, "html_url": "https://github.com/acme/orders/issues/7"},
		"comment": map[string]any{
			"id": id, "body": body, "author_association": "MEMBER",
			"user": map[string]any{"login": login, "id": accountID(login), "type": typ},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// accountID is the GitHub id the test deliveries give login: one account,
// one id, whichever comment it writes.
func accountID(login string) int64 {
	return 5000 + int64(crc32.ChecksumIEEE([]byte(login))%100000)
}

// commentBy is the issue_comment.created delivery of comment id, written by
// login, a User.
func commentBy(t *testing.T, id int64, body, login string) string {
	t.Helper()
	return commentPayloadAs(t, "created", id, body, login, "User")
}

// reconcileOnce runs one projection pass over the finding.
func reconcileOnce(t *testing.T, r *FindingReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"},
	})
}

// settleAll reconciles until no command is pending, failing on any error.
func settleAll(t *testing.T, r *FindingReconciler, c client.Client) {
	t.Helper()
	for range 20 {
		if _, err := reconcileOnce(t, r); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if cmds := get(t, c, "finding-aa-1").Status.Commands; cmds == nil || len(cmds.Pending) == 0 {
			return
		}
	}
	t.Fatalf("commands still pending after 20 passes: %+v", get(t, c, "finding-aa-1").Status.Commands)
}

// replies are the tracking issue's replies to the command in comment id.
func replies(tracker *fakeTracker, id int64) []string {
	var out []string
	for _, cm := range tracker.issueComments[7] {
		if strings.HasPrefix(cm.Body, commandMarker(id)+"\n") {
			out = append(out, cm.Body)
		}
	}
	return out
}

// assertAnswered checks the one answer every command gets, a repeated
// refusal aside: an eyes reaction, exactly one reply holding want, and the
// command no longer pending, its id consumed. The reply is never recorded
// in status.tracking.comments: that machinery lists the thread.
func assertAnswered(t *testing.T, tracker *fakeTracker, f *v1alpha1.Finding, id int64, want ...string) {
	t.Helper()
	assertReplied(t, tracker, f, id, want...)
	if cmds := f.Status.Commands; cmds == nil || !slices.Contains(cmds.Consumed, id) {
		t.Errorf("commands = %+v, want comment %d consumed", cmds, id)
	}
}

// assertRefused checks a refusal's answer: the reaction and exactly one
// reply holding want, the command no longer pending, its id NOT consumed
// (commenting must not push a maintainer's command out of consumed), and
// its author remembered as refused.
func assertRefused(
	t *testing.T, tracker *fakeTracker, f *v1alpha1.Finding, id int64, login string, want ...string,
) {
	t.Helper()
	assertReplied(t, tracker, f, id, want...)
	cmds := f.Status.Commands
	if slices.Contains(cmds.Consumed, id) {
		t.Errorf("consumed = %v, want refused comment %d left out", cmds.Consumed, id)
	}
	if !slices.Contains(cmds.RefusedActors, accountID(login)) {
		t.Errorf("refusedActors = %v, want %s's id", cmds.RefusedActors, login)
	}
}

// assertReplied checks the reaction, exactly one reply holding want, and
// nothing pending for comment id.
func assertReplied(t *testing.T, tracker *fakeTracker, f *v1alpha1.Finding, id int64, want ...string) {
	t.Helper()
	if got := tracker.reactions[id]; !slices.Equal(got, []string{ghclient.ReactionEyes}) {
		t.Errorf("reactions on comment %d = %v, want [eyes]", id, got)
	}
	rs := replies(tracker, id)
	if len(rs) != 1 {
		t.Fatalf("replies to comment %d = %d, want exactly 1: %q", id, len(rs), rs)
	}
	for _, w := range want {
		if !strings.Contains(rs[0], w) {
			t.Errorf("reply lacks %q:\n%s", w, rs[0])
		}
	}
	cmds := f.Status.Commands
	if cmds == nil || pendingCommand(f, id) != nil {
		t.Errorf("commands = %+v, want comment %d no longer pending", cmds, id)
	}
	if trackedComment(f.Status.Tracking, commandMarker(id)) != nil {
		t.Errorf("reply to comment %d recorded on status.tracking.comments", id)
	}
}

// failedFinding is trackedFinding Failed out of remediation, so a retry
// recovers it to Queued.
func failedFinding() *v1alpha1.Finding {
	fnd := trackedFinding(v1alpha1.PhaseFailed)
	done := metav1.NewTime(testClock.Add(-time.Hour))
	fnd.Status.PhaseTimes = []v1alpha1.PhaseTime{
		{Phase: v1alpha1.PhaseRemediating, At: metav1.NewTime(testClock.Add(-2 * time.Hour))},
		{Phase: v1alpha1.PhaseFailed, At: done},
	}
	fnd.Status.CompletedAt = &done
	return fnd
}

// TestCommandSettles drives each command from its delivery to its answer:
// Signals records it, and the projection authorises it against GitHub,
// applies it through action.Apply and replies once. Authority is write
// access to the tracking issue's repository and nothing less: read, which a
// public repository grants every account, none, and a login GitHub has no
// account for are all refused. The phase never moves here: a command writes
// spec only, and the controller owning the edge moves the phase.
func TestCommandSettles(t *testing.T) {
	completed := metav1.NewTime(testClock.Add(-time.Hour))
	approval := func(by string, at time.Time) *v1alpha1.Approval {
		return &v1alpha1.Approval{By: by, At: metav1.NewTime(at)}
	}
	cases := []commandCase{
		{
			name: "write access approves, with the note",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionWrite, body: "/patchy approve ship it",
			wantReply: []string{"@maintainer `/patchy approve` is done"},
			wantNot:   []string{"deprecated"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				a := f.Spec.Approval
				if a == nil || a.By != maintainer || a.Note != "ship it" || !a.At.Time.Equal(testClock) {
					t.Errorf("approval = %+v, want maintainer's with the note at the decision", a)
				}
			},
		},
		{
			name: "admin approves",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionAdmin, body: "/patchy approve",
			wantReply: []string{"is done"},
			check:     wantApprovalBy(maintainer),
		},
		{
			name: "maintain approves",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionMaintain, body: "/PATCHY Approve",
			wantReply: []string{"is done"},
			check:     wantApprovalBy(maintainer),
		},
		{
			name: "read is refused: a public repository grants it to everyone",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			body: "/patchy approve",
			wantReply: []string{
				"@maintainer you may not use `/patchy approve` here", "need write access to acme/orders",
			},
			check:   wantApprovalBy(""),
			refused: true,
		},
		{
			name: "none is refused",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionNone, body: "/patchy approve",
			wantReply: []string{"you may not use"},
			check:     wantApprovalBy(""),
			refused:   true,
		},
		{
			name:    "a login GitHub has no account for is refused",
			fnd:     func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			missing: true, body: "/patchy approve",
			wantReply: []string{"you may not use"},
			check:     wantApprovalBy(""),
			refused:   true,
		},
		{
			name: "an unrecognised permission is refused",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: "triage", body: "/patchy approve",
			wantReply: []string{"you may not use"},
			check:     wantApprovalBy(""),
			refused:   true,
		},
		{
			name: "a verb the phase does not admit lists the ones it does",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionWrite, body: "/patchy retry",
			wantReply: []string{
				"`/patchy retry` is not available for this finding in its current phase",
				"- `/patchy approve [note]`", "- `/patchy expedite`", "- `/patchy suspend`",
			},
			wantNot: []string{"- `/patchy retry`", "- `/patchy resume`"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Retry != nil {
					t.Errorf("spec.retry = %+v, want none", f.Spec.Retry)
				}
			},
		},
		{
			name: "an unknown verb gets the help, and GitHub is not asked who wrote it",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			body: "/patchy frobnicate now",
			wantReply: []string{
				"`/patchy frobnicate` is not a command patchy knows",
				"- `/patchy approve [note]`", "- `/patchy retry`", "- `/patchy expedite`",
				"- `/patchy suspend`", "- `/patchy resume`",
			},
			noPermRead: true,
			check:      wantApprovalBy(""),
			refused:    true,
		},
		{
			name: "a verb that is not a word is not echoed",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			body: "/patchy <b>approve</b>",
			wantReply: []string{
				"@maintainer that is not a command patchy knows.", "- `/patchy approve [note]`",
			},
			wantNot:    []string{"<b>"},
			noPermRead: true,
			refused:    true,
		},
		{
			name: "the legacy approve comment approves as before, with the deprecation noted",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionWrite, body: "/approve lgtm",
			wantReply: []string{"`/patchy approve` is done", "deprecated: comment `/patchy approve`"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if a := f.Spec.Approval; a == nil || a.By != maintainer || a.Note != "lgtm" {
					t.Errorf("approval = %+v, want maintainer's with the note", a)
				}
			},
		},
		{
			name: "a configured approve comment is the alias",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			perm: ghclient.PermissionWrite, alias: "@patchy approve", body: "@patchy approve",
			wantReply: []string{"is done", "deprecated"},
			check:     wantApprovalBy(maintainer),
		},
		{
			name:      "the legacy alias is refused without write access, as every command is",
			fnd:       func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseAwaitingApproval) },
			body:      "/approve",
			wantReply: []string{"you may not use `/patchy approve` here"},
			check:     wantApprovalBy(""),
			refused:   true,
		},
		{
			name: "suspend",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseQueued) },
			perm: ghclient.PermissionWrite, body: "/patchy suspend",
			wantReply: []string{"`/patchy suspend` is done", "paused until `/patchy resume`"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if !f.Spec.Suspend {
					t.Error("spec.suspend = false, want true")
				}
			},
		},
		{
			name: "resume",
			fnd: func() *v1alpha1.Finding {
				fnd := trackedFinding(v1alpha1.PhaseQueued)
				fnd.Spec.Suspend = true
				return fnd
			},
			perm: ghclient.PermissionWrite, body: "/patchy resume",
			wantReply: []string{"`/patchy resume` is done"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Suspend {
					t.Error("spec.suspend = true, want false")
				}
			},
		},
		{
			name: "expedite",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseEnhanced) },
			perm: ghclient.PermissionWrite, body: "/patchy expedite",
			wantReply: []string{"`/patchy expedite` is done"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if e := f.Spec.Expedite; e == nil || e.By != maintainer {
					t.Errorf("spec.expedite = %+v, want maintainer's", e)
				}
			},
		},
		{
			name: "retry",
			fnd:  failedFinding,
			perm: ghclient.PermissionWrite, body: "/patchy retry",
			wantReply: []string{"`/patchy retry` is done"},
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if r := f.Spec.Retry; r == nil || r.By != maintainer || !v1alpha1.RetryRequested(f) {
					t.Errorf("spec.retry = %+v, want maintainer's, actionable", r)
				}
			},
		},
		{
			// The recorded approval predates the hand-off, so it can never
			// revive the finding; a fresh approve replaces it.
			name: "a stale approval on a handed-off finding is replaced",
			fnd: func() *v1alpha1.Finding {
				fnd := trackedFinding(v1alpha1.PhaseHandedOff)
				fnd.Spec.Approval = approval("old-approver", testClock.Add(-2*time.Hour))
				fnd.Status.CompletedAt = &completed
				return fnd
			},
			perm: ghclient.PermissionWrite, body: "/patchy approve",
			wantReply: []string{"is done"},
			check:     wantApprovalBy(maintainer),
		},
		{
			name: "a fresh approval on a handed-off finding is kept",
			fnd: func() *v1alpha1.Finding {
				fnd := trackedFinding(v1alpha1.PhaseHandedOff)
				fnd.Spec.Approval = approval("old-approver", testClock.Add(-30*time.Minute))
				fnd.Status.CompletedAt = &completed
				return fnd
			},
			perm: ghclient.PermissionWrite, body: "/patchy approve",
			wantReply: []string{"is done"},
			check:     wantApprovalBy("old-approver"),
		},
		{
			// action.Apply's gate, as the status page and the CLI apply it:
			// a finding still being investigated has no hold to release.
			name: "approve before the hold is not available",
			fnd:  func() *v1alpha1.Finding { return trackedFinding(v1alpha1.PhaseInvestigating) },
			perm: ghclient.PermissionWrite, body: "/patchy approve",
			wantReply: []string{"`/patchy approve` is not available", "- `/patchy expedite`"},
			check:     wantApprovalBy(""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// commandCase is one command TestCommandSettles drives from its delivery to
// its answer, and what the answer must be.
type commandCase struct {
	name      string
	fnd       func() *v1alpha1.Finding
	perm      string // the commenter's permission; "" reads as read
	missing   bool   // the commenter has no GitHub account
	alias     string // the Integration's approveComment
	body      string
	wantReply []string
	wantNot   []string
	// check inspects the finding once the command is answered.
	check func(t *testing.T, f *v1alpha1.Finding)
	// noPermRead: the command is answered without asking GitHub who the
	// commenter is.
	noPermRead bool
	// refused: the answer is a refusal (NotAllowed or UnknownVerb), whose
	// id is not consumed and whose author is remembered as refused.
	refused bool
}

func (tc commandCase) run(t *testing.T) {
	s, r, tracker, c := newReview(t, tc.fnd())
	if tc.perm != "" {
		tracker.perms[maintainer] = tc.perm
	}
	tracker.missing[maintainer] = tc.missing
	integ := testIntegration()
	integ.Spec.GitHub.Issues.ApproveComment = tc.alias
	before := get(t, c, "finding-aa-1")

	if err := s.Handle(t.Context(), integ, event("issue_comment", commentPayload(t, 41, tc.body))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	pending := get(t, c, "finding-aa-1").Status.Commands
	if pending == nil || len(pending.Pending) != 1 || pending.Pending[0].CommentID != 41 {
		t.Fatalf("pending = %+v, want the command recorded by the delivery", pending)
	}
	if len(tracker.permReads) != 0 || len(tracker.comments) != 0 {
		t.Fatal("the delivery called GitHub; it must only record the command")
	}

	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	if tc.refused {
		assertRefused(t, tracker, f, 41, maintainer, tc.wantReply...)
	} else {
		assertAnswered(t, tracker, f, 41, tc.wantReply...)
	}
	if tracker.lists != 0 || len(tracker.recentLists) != 0 {
		t.Errorf("the thread was listed (%d full, %v since): answering a command must not list it",
			tracker.lists, tracker.recentLists)
	}
	for _, w := range tc.wantNot {
		if reply := replies(tracker, 41)[0]; strings.Contains(reply, w) {
			t.Errorf("reply holds %q:\n%s", w, reply)
		}
	}
	if got := len(tracker.permReads) == 0; got != tc.noPermRead {
		t.Errorf("permission reads = %v, want none: %v", tracker.permReads, tc.noPermRead)
	}
	// One writer per edge: a command writes spec, never the phase.
	if f.Status.Phase != before.Status.Phase ||
		!equality.Semantic.DeepEqual(f.Status.PhaseTimes, before.Status.PhaseTimes) ||
		!equality.Semantic.DeepEqual(f.Status.Conditions, before.Status.Conditions) {
		t.Errorf("phase %s -> %s, phaseTimes %v -> %v: a command moved the phase",
			before.Status.Phase, f.Status.Phase, before.Status.PhaseTimes, f.Status.PhaseTimes)
	}
	if tc.check != nil {
		tc.check(t, f)
	}
}

// wantApprovalBy checks the finding's approval is by, or absent for "".
func wantApprovalBy(by string) func(*testing.T, *v1alpha1.Finding) {
	return func(t *testing.T, f *v1alpha1.Finding) {
		t.Helper()
		got := ""
		if f.Spec.Approval != nil {
			got = f.Spec.Approval.By
		}
		if got != by {
			t.Errorf("approval by %q, want %q", got, by)
		}
	}
}

// TestCommandIgnored: what records nothing. A bot's command is never one
// (the App's own "<slug>[bot]" and any other), and neither is a comment
// that is not a command, an edit, or a comment on an issue that is not a
// finding's.
func TestCommandIgnored(t *testing.T) {
	cases := []struct {
		name    string
		payload func(t *testing.T) string
	}{
		{"a bot account", func(t *testing.T) string {
			return commentPayloadAs(t, "created", 41, "/patchy approve", "dependabot[bot]", "Bot")
		}},
		{"the App's own login", func(t *testing.T) string {
			return commentPayloadAs(t, "created", 41, "/patchy approve", botLogin, "User")
		}},
		{"not a command", func(t *testing.T) string { return commentPayload(t, 41, "looks fine to me") }},
		{"a word that starts like the alias", func(t *testing.T) string {
			return commentPayload(t, 41, "/approved")
		}},
		{"a command below the first line", func(t *testing.T) string {
			return commentPayload(t, 41, "Thanks!\n/patchy approve")
		}},
		{"an edit", func(t *testing.T) string {
			return commentPayloadAs(t, "edited", 41, "/patchy approve", maintainer, "User")
		}},
		{"no comment id", func(t *testing.T) string { return commentPayload(t, 0, "/patchy approve") }},
		{"another issue", func(t *testing.T) string {
			return strings.ReplaceAll(commentPayload(t, 41, "/patchy approve"), "/issues/7", "/issues/8")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
			tracker.perms[maintainer] = ghclient.PermissionWrite
			handle(t, s, "issue_comment", tc.payload(t))
			if cmds := get(t, c, "finding-aa-1").Status.Commands; cmds != nil {
				t.Fatalf("commands = %+v, want nothing recorded", cmds)
			}
			settleAll(t, r, c)
			if len(tracker.comments) != 0 || len(tracker.reactions) != 0 || len(tracker.permReads) != 0 {
				t.Errorf("GitHub touched: comments %q, reactions %v, permission reads %v",
					tracker.comments, tracker.reactions, tracker.permReads)
			}
			if get(t, c, "finding-aa-1").Spec.Approval != nil {
				t.Error("approval recorded")
			}
		})
	}
}

// TestCommandDeliveredTwice: a duplicate delivery, before the command is
// answered or after, and a redelivery or demo replay long after, is one
// command answered once.
func TestCommandDeliveredTwice(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	payload := commentPayload(t, 41, "/patchy approve")

	handle(t, s, "issue_comment", payload)
	handle(t, s, "issue_comment", payload)
	if n := len(get(t, c, "finding-aa-1").Status.Commands.Pending); n != 1 {
		t.Fatalf("pending = %d, want the duplicate recorded once", n)
	}
	settleAll(t, r, c)
	approved := get(t, c, "finding-aa-1").Spec.Approval

	handle(t, s, "issue_comment", payload)
	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	assertAnswered(t, tracker, f, 41, "is done")
	if !equality.Semantic.DeepEqual(f.Spec.Approval, approved) {
		t.Errorf("approval = %+v after the redelivery, want %+v unchanged", f.Spec.Approval, approved)
	}
	if len(tracker.permReads) != 1 {
		t.Errorf("permission reads = %v, want one", tracker.permReads)
	}
}

// TestCommandReplayAfterConsume: a suspend answered, then a resume, then the
// suspend's delivery replayed (a demo replay resends everything): the
// replay records nothing, so the finding stays resumed.
func TestCommandReplayAfterConsume(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	suspend := commentPayload(t, 41, "/patchy suspend")

	handle(t, s, "issue_comment", suspend)
	settleAll(t, r, c)
	handle(t, s, "issue_comment", commentPayload(t, 42, "/patchy resume"))
	settleAll(t, r, c)
	handle(t, s, "issue_comment", suspend)
	settleAll(t, r, c)

	f := get(t, c, "finding-aa-1")
	if f.Spec.Suspend {
		t.Error("spec.suspend = true: the replayed suspend applied again")
	}
	if got := f.Status.Commands.Consumed; !slices.Equal(got, []int64{41, 42}) {
		t.Errorf("consumed = %v, want [41 42]", got)
	}
	if len(replies(tracker, 41)) != 1 || len(replies(tracker, 42)) != 1 {
		t.Errorf("replies = %d and %d, want one each", len(replies(tracker, 41)), len(replies(tracker, 42)))
	}
}

// TestCommandsInCommentOrder: deliveries arrive unordered, but GitHub's
// comment ids grow with creation, so pending commands are answered in id
// order. A suspend and the resume written after it, delivered resume
// first, leave the finding resumed.
func TestCommandsInCommentOrder(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	handle(t, s, "issue_comment", commentPayload(t, 51, "/patchy resume"))
	handle(t, s, "issue_comment", commentPayload(t, 50, "/patchy suspend"))

	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	if f.Spec.Suspend {
		t.Error("spec.suspend = true: the resume was applied before the suspend written ahead of it")
	}
	assertAnswered(t, tracker, f, 50, "`/patchy suspend` is done")
	assertAnswered(t, tracker, f, 51, "`/patchy resume` is done")
}

// TestCommandTransientPermissionFailure: GitHub fails the permission read,
// and the delivery is long answered, so nothing would redeliver it. The
// command stays pending, undecided, until the read succeeds; nothing is
// guessed, and nothing is replied until then.
func TestCommandTransientPermissionFailure(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	tracker.permErrs = []error{badGateway("collaborator permission"), badGateway("collaborator permission")}
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))

	for range 2 {
		if _, err := reconcileOnce(t, r); err == nil {
			t.Fatal("Reconcile = nil, want the permission failure returned for the backoff")
		}
		f := get(t, c, "finding-aa-1")
		if p := f.Status.Commands.Pending; len(p) != 1 || p[0].Outcome != "" {
			t.Fatalf("pending = %+v, want the command kept undecided", p)
		}
		if f.Spec.Approval != nil || len(tracker.comments) != 0 || len(tracker.reactions) != 0 {
			t.Fatal("the command acted on or answered without GitHub's answer")
		}
	}

	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	assertAnswered(t, tracker, f, 41, "is done")
	wantApprovalBy(maintainer)(t, f)
}

// TestCommandTransientAcknowledgementFailure: a suspend is decided and
// applied, then GitHub fails the reaction or the reply, and a human resumes
// the finding from the status page meanwhile. The retry resumes at the
// acknowledgement: the suspend is not applied again over the resume, and
// the reply is posted exactly once.
func TestCommandTransientAcknowledgementFailure(t *testing.T) {
	cases := []struct {
		name string
		fail func(*fakeTracker)
	}{
		{"reaction", func(f *fakeTracker) { f.reactErrs = []error{badGateway("react")} }},
		{"reply", func(f *fakeTracker) { f.commentErrs = []error{badGateway("comment")} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
			tracker.perms[maintainer] = ghclient.PermissionWrite
			tc.fail(tracker)
			handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy suspend"))

			if _, err := reconcileOnce(t, r); err == nil {
				t.Fatal("Reconcile = nil, want the acknowledgement failure returned")
			}
			f := get(t, c, "finding-aa-1")
			p := f.Status.Commands.Pending
			if len(p) != 1 || p[0].Outcome != v1alpha1.CommandDone || !p[0].Applied || !f.Spec.Suspend {
				t.Fatalf("pending = %+v, suspend %v: want the command decided Done and applied", p, f.Spec.Suspend)
			}
			f.Spec.Suspend = false // resumed on the status page
			if err := c.Update(t.Context(), f); err != nil {
				t.Fatal(err)
			}

			settleAll(t, r, c)
			f = get(t, c, "finding-aa-1")
			assertAnswered(t, tracker, f, 41, "`/patchy suspend` is done")
			if f.Spec.Suspend {
				t.Error("spec.suspend = true: the suspend was applied again over the resume")
			}
			if len(tracker.permReads) != 1 {
				t.Errorf("permission reads = %v, want the decision taken once", tracker.permReads)
			}
		})
	}
}

// TestCommandCommentDeleted: the command's comment is deleted before it is
// answered. It is answered anyway (it may already have been applied),
// without the reaction GitHub cannot take.
func TestCommandCommentDeleted(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	tracker.reactErrs = []error{notFound("react")}
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))

	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	if n := len(replies(tracker, 41)); n != 1 {
		t.Errorf("replies = %d, want 1", n)
	}
	if cmds := f.Status.Commands; len(cmds.Pending) != 0 || !slices.Equal(cmds.Consumed, []int64{41}) {
		t.Errorf("commands = %+v, want 41 consumed", cmds)
	}
	wantApprovalBy(maintainer)(t, f)
}

// TestCommandAppliedBeforeCrash: a pass wrote the approval and stopped
// before marking the command applied, and remediation-controller then
// admitted the finding on that approval. The retry finds the verb
// unavailable in the new phase, but the approval on the spec is this
// command's own, so the answer stays Done. The same position with the
// approval not this command's (the finding moved on for another reason)
// is revised to Unavailable before any reply says otherwise.
func TestCommandAppliedBeforeCrash(t *testing.T) {
	decided := metav1.NewTime(testClock.Add(-time.Minute))
	cases := []struct {
		name      string
		approval  *v1alpha1.Approval
		wantReply string
	}{
		{"its own approval", &v1alpha1.Approval{By: maintainer, At: decided}, "`/patchy approve` is done"},
		{"another's approval", &v1alpha1.Approval{By: "someone-else", At: decided},
			"`/patchy approve` is not available"},
		{"no approval", nil, "`/patchy approve` is not available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fnd := trackedFinding(v1alpha1.PhaseQueued)
			fnd.Spec.Approval = tc.approval
			fnd.Status.Commands = &v1alpha1.FindingCommands{Pending: []v1alpha1.FindingCommand{{
				CommentID: 41, Actor: v1alpha1.CommandActor{Login: maintainer, Type: "User"},
				Verb: "approve", ReceivedAt: decided, Outcome: v1alpha1.CommandDone, DecidedAt: &decided,
			}}}
			_, r, tracker, c := newReview(t, fnd)

			settleAll(t, r, c)
			f := get(t, c, "finding-aa-1")
			assertAnswered(t, tracker, f, 41, tc.wantReply)
			if !equality.Semantic.DeepEqual(f.Spec.Approval, tc.approval) {
				t.Errorf("approval = %+v, want %+v untouched", f.Spec.Approval, tc.approval)
			}
			if len(tracker.permReads) != 0 {
				t.Errorf("permission reads = %v: a decided command was decided again", tracker.permReads)
			}
		})
	}
}

// TestCommandWaitsForIntegration: with no issues-enabled Integration to read
// GitHub through, a command waits, re-checked, rather than being decided
// without GitHub.
func TestCommandWaitsForIntegration(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))
	var integ v1alpha1.Integration
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "gh"}, &integ); err != nil {
		t.Fatal(err)
	}
	integ.Spec.Suspend = true
	if err := c.Update(t.Context(), &integ); err != nil {
		t.Fatal(err)
	}

	res, err := reconcileOnce(t, r)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != commandRecheck {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, commandRecheck)
	}
	if p := get(t, c, "finding-aa-1").Status.Commands.Pending; len(p) != 1 || p[0].Outcome != "" {
		t.Errorf("pending = %+v, want the command waiting undecided", p)
	}
	if len(tracker.permReads) != 0 {
		t.Errorf("permission reads = %v, want none", tracker.permReads)
	}
}

// TestCommandIssueGone: the tracking issue is deleted before the reply. The
// dead link is dropped (the projection opens a fresh issue) and the command
// stays pending, applied, to be answered there.
func TestCommandIssueGone(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))
	delete(tracker.issues, 7)

	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Tracking != nil {
		t.Errorf("tracking = %+v, want the dead link dropped", f.Status.Tracking)
	}
	if p := f.Status.Commands.Pending; len(p) != 1 || !p[0].Applied {
		t.Errorf("pending = %+v, want the command kept, applied, for the fresh issue", p)
	}
	wantApprovalBy(maintainer)(t, f)
}

// pendingIDs are the comment ids pending on the finding, ascending.
func pendingIDs(t *testing.T, c client.Client) []int64 {
	t.Helper()
	var ids []int64
	if cmds := get(t, c, "finding-aa-1").Status.Commands; cmds != nil {
		for _, p := range cmds.Pending {
			ids = append(ids, p.CommentID)
		}
	}
	slices.Sort(ids)
	return ids
}

// TestCommandSlotsSharedByAccount: whether a commenter may command the
// finding is not known until GitHub is asked, so the pending slots are
// shared out by account. One account holds at most
// MaxPendingCommandsPerActor, however many it writes; and when every slot
// is held, a new account's command takes the slot of the newest undecided
// command of an account holding more than one, so a flood from a few
// accounts cannot keep a maintainer's command out.
func TestCommandSlotsSharedByAccount(t *testing.T) {
	t.Run("one account holds at most its share", func(t *testing.T) {
		s, _, _, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
		for i := range 5 {
			handle(t, s, "issue_comment", commentBy(t, int64(100+i), "/patchy approve", "flooder"))
		}
		handle(t, s, "issue_comment", commentPayload(t, 200, "/patchy suspend"))
		if got := pendingIDs(t, c); !slices.Equal(got, []int64{100, 101, 200}) {
			t.Errorf("pending = %v, want the flooder's first two and the maintainer's", got)
		}
	})

	t.Run("a full list gives a doubled-up account's newer slot to a new account", func(t *testing.T) {
		s, _, _, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
		// Six accounts hold one slot each, one holds two: every slot held.
		for i := range 6 {
			handle(t, s, "issue_comment", commentBy(t, int64(100+i), "/patchy approve", fmt.Sprintf("drive-by-%d", i)))
		}
		handle(t, s, "issue_comment", commentBy(t, 110, "/patchy approve", "flooder"))
		handle(t, s, "issue_comment", commentBy(t, 111, "/patchy approve", "flooder"))
		if n := len(pendingIDs(t, c)); n != v1alpha1.MaxPendingCommands {
			t.Fatalf("pending = %d, want every slot held", n)
		}

		handle(t, s, "issue_comment", commentPayload(t, 200, "/patchy suspend"))
		got := pendingIDs(t, c)
		if !slices.Contains(got, 200) || slices.Contains(got, 111) || !slices.Contains(got, 110) {
			t.Errorf("pending = %v, want the maintainer's 200 in the flooder's newer slot (111)", got)
		}

		// Now every account holds one: a ninth account is not recorded.
		handle(t, s, "issue_comment", commentBy(t, 300, "/patchy approve", "latecomer"))
		if got := pendingIDs(t, c); slices.Contains(got, 300) || len(got) != v1alpha1.MaxPendingCommands {
			t.Errorf("pending = %v, want the latecomer not recorded", got)
		}
	})

	t.Run("a decided command keeps its slot", func(t *testing.T) {
		fnd := trackedFinding(v1alpha1.PhaseAwaitingApproval)
		decided := metav1.NewTime(testClock)
		fnd.Status.Commands = &v1alpha1.FindingCommands{}
		for i := range v1alpha1.MaxPendingCommands {
			login := fmt.Sprintf("drive-by-%d", i/2) // four accounts, two each
			fnd.Status.Commands.Pending = append(fnd.Status.Commands.Pending, v1alpha1.FindingCommand{
				CommentID: int64(100 + i), Verb: "approve", ReceivedAt: decided,
				Actor:   v1alpha1.CommandActor{Login: login, ID: accountID(login), Type: "User"},
				Outcome: v1alpha1.CommandNotAllowed, DecidedAt: &decided,
			})
		}
		s, _, _, c := newReview(t, fnd)
		handle(t, s, "issue_comment", commentPayload(t, 200, "/patchy suspend"))
		if got := pendingIDs(t, c); slices.Contains(got, 200) || len(got) != v1alpha1.MaxPendingCommands {
			t.Errorf("pending = %v, want every decided command kept and 200 not recorded", got)
		}
	})
}

// TestCommandSeenIsExact: a delivery is skipped only for a command recorded
// or remembered as answered (consumed, or the last suspend or resume
// applied). Nothing is inferred from an id's place among those remembered:
// a delivery the webhook queue turned away comes back through the
// redelivery sweep long after later commands are answered, and must still
// be answered. consumed keeps the largest ids answered, ascending.
func TestCommandSeenIsExact(t *testing.T) {
	var ids []int64
	for i := range v1alpha1.MaxConsumedCommands + 3 {
		ids = consumeID(ids, int64(1000-i)) // answered newest first
	}
	if len(ids) != v1alpha1.MaxConsumedCommands || !slices.IsSorted(ids) {
		t.Fatalf("consumed = %v, want %d ids ascending", ids, v1alpha1.MaxConsumedCommands)
	}
	if ids[0] != int64(1000-v1alpha1.MaxConsumedCommands+1) || ids[len(ids)-1] != 1000 {
		t.Errorf("consumed spans %d..%d, want the largest kept", ids[0], ids[len(ids)-1])
	}
	if got := consumeID(ids, 1000); !slices.Equal(got, ids) {
		t.Errorf("re-consuming an id changed the list: %v", got)
	}
	cmds := &v1alpha1.FindingCommands{
		Consumed:   ids,
		Pending:    []v1alpha1.FindingCommand{{CommentID: 2000}},
		LastToggle: 1500,
	}
	for _, tc := range []struct {
		id   int64
		want bool
	}{
		{1000, true}, // consumed
		{2000, true}, // pending
		{1500, true}, // the last suspend or resume applied
		{int64(1000 - v1alpha1.MaxConsumedCommands), false}, // below every id kept: may never have arrived
		{1499, false}, // unknown, between those remembered
		{3000, false}, // newer than all
	} {
		if got := commandSeen(cmds, tc.id); got != tc.want {
			t.Errorf("commandSeen(%d) = %v, want %v", tc.id, got, tc.want)
		}
	}
	if commandSeen(nil, 10) {
		t.Error("an id counted as seen on a finding with no commands")
	}
}

// TestLateCommandAfterRefusalFlood: accounts without write access comment
// command after command, all refused, and then a maintainer's command
// written before all of them arrives late (the webhook queue turned it
// away, and the redelivery sweep brought it back). The refusals never
// entered consumed, so the late command is recorded and applied; and each
// flooding account got one reply, the rest of its refusals the reaction
// alone.
func TestLateCommandAfterRefusalFlood(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	floods := v1alpha1.MaxConsumedCommands + 8
	for i := range floods {
		login := []string{"flooder-a", "flooder-b"}[i%2]
		handle(t, s, "issue_comment", commentBy(t, int64(1000+i), "/patchy suspend", login))
		settleAll(t, r, c)
	}
	f := get(t, c, "finding-aa-1")
	if len(f.Status.Commands.Consumed) != 0 {
		t.Fatalf("consumed = %v, want no refused command kept", f.Status.Commands.Consumed)
	}
	if f.Spec.Suspend {
		t.Fatal("a refused command suspended the finding")
	}
	var replied int
	for i := range floods {
		replied += len(replies(tracker, int64(1000+i)))
	}
	if replied != 2 {
		t.Errorf("replies to %d refused commands from two accounts = %d, want one each", floods, replied)
	}
	if got := tracker.reactions[int64(1000+floods-1)]; !slices.Equal(got, []string{ghclient.ReactionEyes}) {
		t.Errorf("a quiet refusal's reactions = %v, want [eyes]", got)
	}

	handle(t, s, "issue_comment", commentPayload(t, 900, "/patchy approve"))
	settleAll(t, r, c)
	f = get(t, c, "finding-aa-1")
	assertAnswered(t, tracker, f, 900, "`/patchy approve` is done")
	wantApprovalBy(maintainer)(t, f)
}

// TestRefusalRepliedOncePerAccount: an account refused once on a finding is
// not replied to again there, whatever the refusal (not allowed, or a verb
// patchy does not know): later refusals get the reaction alone, so
// commenting cannot make patchy post a reply per comment. Another account
// gets its own first reply, and a write-access commenter's answers are never
// quietened.
func TestRefusalRepliedOncePerAccount(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	for _, d := range []struct {
		id          int64
		body, login string
	}{
		{41, "/patchy approve", "stranger"},
		{42, "/patchy frobnicate", "stranger"},
		{43, "/patchy approve", "stranger"},
		{44, "/patchy approve", "passer-by"},
		{45, "/patchy frobnicate", maintainer},
		{46, "/patchy frobnicate", maintainer},
		{47, "/patchy retry", maintainer},
		{48, "/patchy retry", maintainer},
	} {
		handle(t, s, "issue_comment", commentBy(t, d.id, d.body, d.login))
		settleAll(t, r, c)
	}
	f := get(t, c, "finding-aa-1")
	assertRefused(t, tracker, f, 41, "stranger", "@stranger you may not use")
	assertRefused(t, tracker, f, 44, "passer-by", "@passer-by you may not use")
	assertRefused(t, tracker, f, 45, maintainer, "not a command patchy knows")
	for _, id := range []int64{42, 43, 46} {
		if n := len(replies(tracker, id)); n != 0 {
			t.Errorf("replies to repeated refusal %d = %d, want the reaction alone", id, n)
		}
		if got := tracker.reactions[id]; !slices.Equal(got, []string{ghclient.ReactionEyes}) {
			t.Errorf("reactions on repeated refusal %d = %v, want [eyes]", id, got)
		}
		if pendingCommand(f, id) != nil || slices.Contains(f.Status.Commands.Consumed, id) {
			t.Errorf("repeated refusal %d still pending or consumed: %+v", id, f.Status.Commands)
		}
	}
	// Unavailable is not a refusal: each is answered in full.
	assertAnswered(t, tracker, f, 47, "`/patchy retry` is not available")
	assertAnswered(t, tracker, f, 48, "`/patchy retry` is not available")
}

// TestLateToggleSuperseded: a suspend or resume delivered after a later one
// is answered must not undo it. GitHub reorders deliveries, and the
// redelivery sweep resends one the webhook queue turned away, so an older
// toggle can arrive after a newer one has been decided, applied and
// consumed. It is answered Superseded and not applied: the later command
// stands. A newer toggle that was refused supersedes nothing.
func TestLateToggleSuperseded(t *testing.T) {
	cases := []struct {
		name        string
		first, late string // the commands answered, then the late one
		lateID      int64
		wantSuspend bool
	}{
		{"a late suspend does not undo a later resume", "/patchy resume", "/patchy suspend", 50, false},
		{"a late resume does not undo a later suspend", "/patchy suspend", "/patchy resume", 50, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
			tracker.perms[maintainer] = ghclient.PermissionWrite
			opposite := map[string]string{"/patchy resume": "/patchy suspend", "/patchy suspend": "/patchy resume"}
			handle(t, s, "issue_comment", commentPayload(t, 49, opposite[tc.first]))
			settleAll(t, r, c)
			handle(t, s, "issue_comment", commentPayload(t, 51, tc.first))
			settleAll(t, r, c)

			handle(t, s, "issue_comment", commentPayload(t, tc.lateID, tc.late))
			settleAll(t, r, c)
			f := get(t, c, "finding-aa-1")
			if f.Spec.Suspend != tc.wantSuspend {
				t.Errorf("spec.suspend = %v, want %v: the late command undid the later one",
					f.Spec.Suspend, tc.wantSuspend)
			}
			assertAnswered(t, tracker, f, tc.lateID, "was written before the last", "the later command stands")
			if got := f.Status.Commands.Consumed; !slices.Equal(got, []int64{49, 50, 51}) {
				t.Errorf("consumed = %v, want [49 50 51]", got)
			}
			if f.Status.Commands.LastToggle != 51 {
				t.Errorf("lastToggle = %d, want 51", f.Status.Commands.LastToggle)
			}

			// A delivery of the last toggle itself records nothing.
			handle(t, s, "issue_comment", commentPayload(t, 51, tc.first))
			if p := get(t, c, "finding-aa-1").Status.Commands.Pending; len(p) != 0 {
				t.Errorf("pending = %+v after the last toggle was delivered again, want nothing", p)
			}
		})
	}

	t.Run("a refused later toggle supersedes nothing", func(t *testing.T) {
		s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
		tracker.perms[maintainer] = ghclient.PermissionWrite
		handle(t, s, "issue_comment", commentBy(t, 61, "/patchy resume", "stranger"))
		settleAll(t, r, c)
		handle(t, s, "issue_comment", commentPayload(t, 60, "/patchy suspend"))
		settleAll(t, r, c)
		f := get(t, c, "finding-aa-1")
		if !f.Spec.Suspend {
			t.Error("spec.suspend = false: a refused resume superseded the maintainer's suspend")
		}
		assertAnswered(t, tracker, f, 60, "`/patchy suspend` is done")
	})
}

// TestCommandReplyAfterAmbiguousFailure: the reply is posted but the call
// fails anyway, as a timeout does. The retry, finding the answer already
// begun, looks among the comments since it began (and only those) and
// finds the reply, so it is not posted twice.
func TestCommandReplyAfterAmbiguousFailure(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseAwaitingApproval))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	tracker.postedErrs = []error{badGateway("comment")}
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy approve"))

	if _, err := reconcileOnce(t, r); err == nil {
		t.Fatal("Reconcile = nil, want the reply's failure returned")
	}
	settleAll(t, r, c)
	f := get(t, c, "finding-aa-1")
	assertAnswered(t, tracker, f, 41, "is done")
	if want := []time.Time{testClock.Add(-answerSkew)}; !slices.EqualFunc(tracker.recentLists, want, time.Time.Equal) {
		t.Errorf("listings since = %v, want one, since the answer began less the skew (%v)", tracker.recentLists, want)
	}
	if tracker.lists != 0 {
		t.Errorf("full thread listings = %d, want none", tracker.lists)
	}
}

// TestCommandAnswerGivenUp: GitHub keeps failing the reply (an issue that
// will take no more comments, say). The command is retried on the backoff
// for commandAnswerTimeout from when its answer began, then consumed
// without the reply: it holds its pending slot, and the commands behind it,
// no longer. Its effect is already on the spec.
func TestCommandAnswerGivenUp(t *testing.T) {
	s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
	tracker.perms[maintainer] = ghclient.PermissionWrite
	for range 10 {
		tracker.commentErrs = append(tracker.commentErrs, badGateway("comment"))
	}
	now := testClock
	r.Now = func() time.Time { return now }
	handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy suspend"))

	for range 3 {
		if _, err := reconcileOnce(t, r); err == nil {
			t.Fatal("Reconcile = nil, want the reply's failure returned for the backoff")
		}
	}
	f := get(t, c, "finding-aa-1")
	if p := pendingCommand(f, 41); p == nil || !p.Applied || !f.Spec.Suspend {
		t.Fatalf("pending = %+v, suspend %v: want the command applied and still being answered", p, f.Spec.Suspend)
	}

	now = testClock.Add(commandAnswerTimeout)
	if _, err := reconcileOnce(t, r); err != nil {
		t.Fatalf("Reconcile past the timeout = %v, want the answer given up", err)
	}
	f = get(t, c, "finding-aa-1")
	if pendingCommand(f, 41) != nil || !slices.Contains(f.Status.Commands.Consumed, 41) {
		t.Errorf("commands = %+v, want 41 consumed without its reply", f.Status.Commands)
	}
	if n := len(replies(tracker, 41)); n != 0 {
		t.Errorf("replies = %d, want none", n)
	}
	if !f.Spec.Suspend {
		t.Error("spec.suspend = false: giving up the answer undid the effect")
	}
}

// TestCommandAnswerFailureHoldsUpNothing: an answer GitHub keeps failing
// neither stops the finding's projection (a Remediated finding's issue is
// still closed; the failure is returned after it, for the backoff) nor
// holds up a later command's effect: a resume written after a suspend whose
// answer is failing still takes effect.
func TestCommandAnswerFailureHoldsUpNothing(t *testing.T) {
	t.Run("the projection runs", func(t *testing.T) {
		tracker := newFakeTracker()
		tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open", Author: botLogin}
		tracker.reactErrs = []error{badGateway("react"), badGateway("react")}
		fnd := projectable(v1alpha1.PhaseRemediated)
		fnd.Status.Tracking = &v1alpha1.TrackingStatus{
			Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
		}
		began := metav1.NewTime(testClock.Add(-time.Minute))
		fnd.Status.Commands = &v1alpha1.FindingCommands{Pending: []v1alpha1.FindingCommand{{
			CommentID: 41, Actor: v1alpha1.CommandActor{Login: maintainer, ID: accountID(maintainer), Type: "User"},
			Verb: "suspend", ReceivedAt: began, Outcome: v1alpha1.CommandUnavailable, DecidedAt: &began,
			AnsweringSince: &began,
		}}}
		r, c := newProjector(t, tracker, testIntegration(), fnd)

		if _, err := reconcileOnce(t, r); err == nil {
			t.Fatal("Reconcile = nil, want the answer's failure returned")
		}
		if !slices.Equal(tracker.closed, []int{7}) {
			t.Errorf("closed = %v: the failing answer stopped the projection", tracker.closed)
		}
		if pendingCommand(get(t, c, "finding-aa-1"), 41) == nil {
			t.Error("the command was dropped; want it kept for the retry")
		}
	})

	t.Run("a later command takes effect", func(t *testing.T) {
		s, r, tracker, c := newReview(t, trackedFinding(v1alpha1.PhaseQueued))
		tracker.perms[maintainer] = ghclient.PermissionWrite
		for range 10 {
			tracker.commentErrs = append(tracker.commentErrs, badGateway("comment"))
		}
		handle(t, s, "issue_comment", commentPayload(t, 41, "/patchy suspend"))
		for range 2 {
			_, _ = reconcileOnce(t, r)
		}
		if !get(t, c, "finding-aa-1").Spec.Suspend {
			t.Fatal("spec.suspend = false, want the suspend applied")
		}
		handle(t, s, "issue_comment", commentPayload(t, 42, "/patchy resume"))
		for range 4 {
			_, _ = reconcileOnce(t, r)
		}
		f := get(t, c, "finding-aa-1")
		if f.Spec.Suspend {
			t.Error("spec.suspend = true: the resume waited behind the suspend's failing answer")
		}
		if p := pendingCommand(f, 42); p == nil || !p.Applied {
			t.Errorf("pending 42 = %+v, want it applied while the answers still fail", p)
		}

		tracker.commentErrs = nil
		settleAll(t, r, c)
		f = get(t, c, "finding-aa-1")
		assertAnswered(t, tracker, f, 41, "`/patchy suspend` is done")
		assertAnswered(t, tracker, f, 42, "`/patchy resume` is done")
	})
}

// TestCommandReplyNeverEchoesMarkup pins the reply's shape for each
// outcome, and that the note, which a human wrote, is never echoed.
func TestCommandReplyNeverEchoesMarkup(t *testing.T) {
	repo := ghclient.Repo{Owner: "acme", Name: "orders"}
	base := v1alpha1.FindingCommand{
		CommentID: 9, Actor: v1alpha1.CommandActor{Login: "dev"}, Verb: "approve",
		Note: "<img src=x onerror=alert(1)> @everyone",
	}
	for _, outcome := range []v1alpha1.CommandOutcome{
		v1alpha1.CommandDone, v1alpha1.CommandUnavailable, v1alpha1.CommandNotAllowed, v1alpha1.CommandUnknownVerb,
		v1alpha1.CommandSuperseded,
	} {
		c := base
		c.Outcome = outcome
		reply := commandReply(c, repo)
		if !strings.HasPrefix(reply, commandMarker(9)+"\n@dev ") {
			t.Errorf("%s reply does not open with its marker and the author:\n%s", outcome, reply)
		}
		if strings.Contains(reply, "onerror") || strings.Contains(reply, "@everyone") {
			t.Errorf("%s reply echoes the note:\n%s", outcome, reply)
		}
	}
	c := base
	c.Outcome = v1alpha1.CommandUnavailable
	if got := commandReply(c, repo); !strings.Contains(got, "No command is available for it now.") {
		t.Errorf("unavailable reply with nothing available:\n%s", got)
	}
}
