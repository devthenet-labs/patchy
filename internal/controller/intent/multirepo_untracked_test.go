// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// untrackedNotices are patchy's untracked notices on pull request n.
func (e *env) untrackedNotices(n int64) []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[n] {
		if strings.Contains(markerOf(c.Body), " "+templates.UntrackedKey+" ") {
			out = append(out, c.Body)
		}
	}
	return out
}

// blockedOpening drives a multi-repository intent until app's pull request is
// recorded, then deletes web's pushed branch, so web's pull request cannot
// open: the intent is Blocked from Building (BranchMissing) with one pull
// request open.
func (e *env) blockedOpening() string {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.driveUntil(name, func(in *v1alpha1.Intent) bool { return len(in.Status.PullRequests) == 1 })
	e.gh.mu.Lock()
	delete(e.gh.branches, fakeRef(webRepoURL, branchName(name)))
	e.gh.mu.Unlock()
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionBranchConflict); c == nil ||
		c.Reason != ReasonBranchMissing || len(in.Status.PullRequests) != 1 {
		e.t.Fatalf("blocked on %+v with pull requests %+v, want BranchMissing beside app's", c,
			in.Status.PullRequests)
	}
	return name
}

// TestCancelWhileOpeningNoticesTheOpenedPullRequest: an approver cancels an
// intent blocked while its pull requests are being opened, app's opened and
// web's not. app's pull request, whose body says the intent completes when
// every one merges, is told once that the intent ended and it is no longer
// tracked, naming web as never opened; the condition records it; later
// passes post nothing more; web gets no pull request.
func TestCancelWhileOpeningNoticesTheOpenedPullRequest(t *testing.T) {
	e := newMultiEnv(t)
	name := e.blockedOpening()
	e.gh.comment(approver, "/patchy cancel")
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentClosed })
	e.settleActions(name)
	e.settleActions(name)
	notices := e.untrackedNotices(1)
	if len(notices) != 1 || !strings.Contains(notices[0], "was closed before patchy had opened") ||
		!strings.Contains(notices[0], "`"+webSlug+"`") || !strings.Contains(notices[0], "no longer reviews") {
		t.Fatalf("untracked notices on app's pull request = %q, want one naming web as never opened", notices)
	}
	in := e.get(name)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUntrackedPullRequests)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonUntrackedNoticed ||
		!strings.Contains(c.Message, "acme/app#1") {
		t.Errorf("UntrackedPullRequests = %+v, want Noticed naming acme/app#1", c)
	}
	if len(e.gh.prs) != 1 {
		t.Errorf("pull requests = %d, want app's alone", len(e.gh.prs))
	}
}

// TestFailWhileOpeningNoticesAndRevivalForgets: an approved repository leaves
// the Project while the intent is blocked opening its pull requests, which
// fails it. The pull request already opened is told so, saying the intent
// failed; reviving the intent forgets it, so the new round's pull requests
// are its own.
func TestFailWhileOpeningNoticesAndRevivalForgets(t *testing.T) {
	e := newMultiEnv(t)
	name := e.blockedOpening()
	p := e.getProject()
	p.Spec.Repositories = p.Spec.Repositories[:1]
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentFailed })
	e.settleActions(name)
	if notices := e.untrackedNotices(1); len(notices) != 1 || !strings.Contains(notices[0], "failed before patchy") {
		t.Fatalf("untracked notices on app's pull request = %q, want one saying the intent failed", notices)
	}

	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:target", approver)
	e.reconcileProject()
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentPlanning })
	in := e.get(name)
	if len(in.Status.PullRequests) != 0 || in.Status.Branch != "" ||
		meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUntrackedPullRequests) != nil {
		t.Errorf("revived with pull requests %+v, branch %q, conditions %+v; want them forgotten",
			in.Status.PullRequests, in.Status.Branch, in.Status.Conditions)
	}
}

// TestOneRepositoryEndingPostsNoUntrackedNotice: a one-repository intent
// cancelled in review leaves its pull request as it always has, with no
// untracked notice: its one record moved it to InReview.
func TestOneRepositoryEndingPostsNoUntrackedNotice(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.comment(approver, "/patchy cancel")
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentClosed })
	e.settleActions(name)
	if n := len(e.untrackedNotices(1)); n != 0 {
		t.Errorf("%d untracked notices on a one-repository intent's pull request, want none", n)
	}
	if c := meta.FindStatusCondition(e.get(name).Status.Conditions,
		v1alpha1.ConditionUntrackedPullRequests); c != nil {
		t.Errorf("UntrackedPullRequests = %+v on a one-repository intent", c)
	}
}
