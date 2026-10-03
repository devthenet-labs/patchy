// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// appUntrackedNotices are patchy's untracked notices on app's pull request
// (1), the one these intents open before they end.
func (e *env) appUntrackedNotices() []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[1] {
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
	notices := e.appUntrackedNotices()
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
	if notices := e.appUntrackedNotices(); len(notices) != 1 || !strings.Contains(notices[0], "failed before patchy") {
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
	if n := len(e.appUntrackedNotices()); n != 0 {
		t.Errorf("%d untracked notices on a one-repository intent's pull request, want none", n)
	}
	if c := meta.FindStatusCondition(e.get(name).Status.Conditions,
		v1alpha1.ConditionUntrackedPullRequests); c != nil {
		t.Errorf("UntrackedPullRequests = %+v on a one-repository intent", c)
	}
}

// dropRepository removes the repository at url from the test Project.
func (e *env) dropRepository(url string) {
	e.t.Helper()
	p := e.getProject()
	var kept []v1alpha1.ProjectRepository
	for _, r := range p.Spec.Repositories {
		if !sameRepo(r.URL, url) {
			kept = append(kept, r)
		}
	}
	p.Spec.Repositories = kept
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

// setRepoErrs makes the fake answer each "<method> <owner/name>" of errs
// with its error, on every call.
func (e *env) setRepoErrs(errs map[string]error) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	for k, err := range errs {
		e.gh.repoErrs[k] = err
	}
}

// TestUnreachableUntrackedNoticeNeverHoldsARevival is the round-2 regression
// of the untracked notice: an intent fails while its pull requests are being
// opened, and the one already opened is in a repository patchy can no longer
// reach, or no longer may write to. Its notice is recorded as not posted,
// never retried, and so neither the status comment nor the approver
// re-applying the trigger label waits on it: the intent is revived. Before
// the fix every ended pass returned the repository's error, and the intent
// stayed Failed, its status comment at Building, until the TTL deleted it.
func TestUnreachableUntrackedNoticeNeverHoldsARevival(t *testing.T) {
	tests := []struct {
		name string
		// drop is the repository removed from the Project, which fails the
		// intent; errs answer app's calls.
		drop      string
		errs      map[string]error
		wantInMsg []string
	}{
		{
			name: "the opened pull request's repository left the project",
			drop: appRepoURL,
			// Any call to app would fail: none is made.
			errs: map[string]error{
				"RateRemaining acme/app":           fmt.Errorf("x: %w", forge.ErrNoMatch),
				"ListPullRequestComments acme/app": fmt.Errorf("x: %w", forge.ErrNoMatch),
			},
			wantInMsg: []string{"acme/app#1 was not told, its repository having left the project"},
		},
		{
			name:      "no forge covers the opened pull request's repository",
			drop:      webRepoURL,
			errs:      map[string]error{"RateRemaining acme/app": fmt.Errorf("x: %w", forge.ErrNoMatch)},
			wantInMsg: []string{"could not be posted on acme/app#1", "no forge matches repository"},
		},
		{
			name: "the installation refuses the opened pull request's repository",
			drop: webRepoURL,
			errs: map[string]error{
				"ListPullRequestComments acme/app": ghError(http.StatusUnprocessableEntity, "not installed"),
			},
			wantInMsg: []string{"could not be posted on acme/app#1", "not installed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMultiEnv(t)
			name := e.blockedOpening()
			e.setRepoErrs(tt.errs)
			e.dropRepository(tt.drop)
			e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentFailed })
			e.settleActions(name)

			in := e.get(name)
			c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionUntrackedPullRequests)
			if c == nil || c.Reason != ReasonUntrackedNoticeRefused {
				t.Fatalf("UntrackedPullRequests = %+v, want NoticeRefused", c)
			}
			for _, want := range tt.wantInMsg {
				if !strings.Contains(c.Message, want) {
					t.Errorf("UntrackedPullRequests message %q lacks %q", c.Message, want)
				}
			}
			if n := len(e.appUntrackedNotices()); n != 0 {
				t.Errorf("%d untracked notices on app's pull request, want none", n)
			}
			if body := e.statusBody(); !strings.Contains(body, "`Failed`") {
				t.Errorf("status comment = %q, want it to say Failed", body)
			}

			e.clock.Advance(time.Minute)
			e.gh.label(1, "patchy:target", approver)
			e.reconcileProject()
			e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentPlanning })
		})
	}
}

// TestTransientUntrackedNoticeFailureHoldsOnlyTheHandOff: a failure that may
// pass (a 502 reading app's rate budget) retries the notice. Meanwhile the
// status comment still says the intent failed, and the trigger label waits,
// unanswered, since a revival forgets the pull requests the notice is owed
// on. Once app answers again, the notice is posted and the intent revived.
func TestTransientUntrackedNoticeFailureHoldsOnlyTheHandOff(t *testing.T) {
	e := newMultiEnv(t)
	name := e.blockedOpening()
	e.setRepoErrs(map[string]error{"RateRemaining acme/app": ghError(http.StatusBadGateway, "Bad Gateway")})
	e.dropRepository(webRepoURL)
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentFailed })
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:target", approver)
	e.reconcileProject()
	for range 3 {
		if err := e.reconcileIntent(name); err == nil {
			t.Fatal("an ended pass succeeded while app's rate budget could not be read")
		}
		e.clock.Advance(time.Minute)
	}
	if body := e.statusBody(); !strings.Contains(body, "`Failed`") {
		t.Errorf("status comment = %q, want it to say Failed while the notice waits", body)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentFailed || len(in.Status.PullRequests) != 1 {
		t.Fatalf("phase %s with pull requests %+v, want Failed with app's kept for its notice",
			in.Status.Phase, in.Status.PullRequests)
	}

	e.gh.mu.Lock()
	delete(e.gh.repoErrs, "RateRemaining acme/app")
	e.gh.mu.Unlock()
	e.passUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentPlanning })
	if notices := e.appUntrackedNotices(); len(notices) != 1 {
		t.Errorf("untracked notices on app's pull request = %d, want one", len(notices))
	}
}
