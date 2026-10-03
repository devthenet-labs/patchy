// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// UntrackedPullRequests reasons.
const (
	// ReasonUntrackedNoticed: every pull request left open carries the
	// notice.
	ReasonUntrackedNoticed = "Noticed"
	// ReasonUntrackedNoticeRefused: GitHub refused the notice on at least
	// one of them (a locked conversation, a permission the App lost).
	ReasonUntrackedNoticeRefused = "NoticeRefused"
)

// endedFrom is the phase an ended Intent ended from: the phase before its
// terminal one, a Blocked one read as the phase it was blocked from.
func endedFrom(in *v1alpha1.Intent) v1alpha1.IntentPhase {
	pts := in.Status.PhaseTimes
	for i := len(pts) - 2; i >= 0; i-- {
		if pts[i].Phase != v1alpha1.IntentBlocked {
			return pts[i].Phase
		}
	}
	return ""
}

// untrackedPullRequests are the pull requests an ended Intent left open while
// its pull requests were still being opened: it ended Closed (a cancel, the
// issue closed) or Failed (an approved repository left the Project) from
// Building, Blocked from it included, where the pull requests are recorded
// one per pass and the last record moves it to InReview. Nothing reviews,
// revises or merges them any more. None for any other Intent, so a
// one-repository intent, whose one record is written with that move, never
// has any.
func (p *pass) untrackedPullRequests() []v1alpha1.IntentPullRequest {
	if phase := p.in.Status.Phase; phase != v1alpha1.IntentClosed && phase != v1alpha1.IntentFailed {
		return nil
	}
	if endedFrom(p.in) != v1alpha1.IntentBuilding {
		return nil
	}
	var open []v1alpha1.IntentPullRequest
	for _, pr := range p.in.Status.PullRequests {
		if pr.State == prOpen {
			open = append(open, pr)
		}
	}
	return open
}

// noticeUntracked posts, once, on each pull request an ended Intent left open
// (untrackedPullRequests) the notice that it is not part of a completed change
// and that patchy no longer tracks it (templates.IntentUntrackedNotice),
// adopting one the App's bot already posted by its marker; the
// UntrackedPullRequests condition then records it done, so nothing is listed
// for it again. Under a pull request repository's rate floor it posts nothing
// and waits (wait). A refusal is recorded, never retried: the notice is advice,
// and the Intent has ended. changed reports a status write.
func (p *pass) noticeUntracked(ctx context.Context) (changed, wait bool, err error) {
	prs := p.untrackedPullRequests()
	if len(prs) == 0 || meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionUntrackedPullRequests) != nil {
		return false, false, nil
	}
	for _, pr := range prs {
		ok, err := p.rateOK(ctx, pr.Repository)
		if err != nil || !ok {
			return false, err == nil, err
		}
	}
	body, err := templates.RenderIntentUntrackedNotice(p.untrackedNotice())
	if err != nil {
		return false, false, err
	}
	var noticed, refused []string
	for _, pr := range prs {
		ref := fmt.Sprintf("%s#%d", repoSlug(pr.Repository), pr.Number)
		switch err := p.postOnceOnPullRequest(ctx, pr, templates.UntrackedKey, body); {
		case err == nil:
			noticed = append(noticed, ref)
		case ghclient.IsRefused(err):
			refused = append(refused, fmt.Sprintf("%s: %v", ref, err))
		default:
			return false, false, err
		}
	}
	reason := ReasonUntrackedNoticed
	msg := "the intent ended before every one of its pull requests was opened; patchy no longer tracks the ones " +
		"it opened"
	switch len(noticed) {
	case 0:
	case 1:
		msg += ", and " + noticed[0] + " says so"
	default:
		msg += ", and " + strings.Join(noticed, ", ") + " say so"
	}
	if len(refused) > 0 {
		reason = ReasonUntrackedNoticeRefused
		msg += "; GitHub refused the notice on " + strings.Join(refused, "; ")
	}
	return true, false, p.update(ctx, func(cur *v1alpha1.Intent) error {
		setCondition(cur, v1alpha1.ConditionUntrackedPullRequests, metav1.ConditionTrue, reason, msg)
		return nil
	})
}

// untrackedNotice is the notice noticeUntracked posts: every pull request
// recorded, and each repository of the approved plan that has none, in the
// Project's spelling where it still lists the repository.
func (p *pass) untrackedNotice() templates.IntentUntrackedNotice {
	n := templates.IntentUntrackedNotice{Namespace: p.in.Namespace, Intent: p.in.Name,
		Failed: p.in.Status.Phase == v1alpha1.IntentFailed}
	for _, pr := range p.in.Status.PullRequests {
		n.Opened = append(n.Opened, templates.IntentPullRequest{Repository: repoSlug(pr.Repository),
			Number: pr.Number, URL: pr.URL, State: pr.State})
	}
	if pl := p.in.Status.Plan; pl != nil {
		for _, named := range pl.Repositories {
			if p.pullRequest(named) != nil {
				continue
			}
			if r, ok := p.projectRepository(named); ok {
				named = r.URL
			}
			n.NeverOpened = append(n.NeverOpened, repoSlug(named))
		}
	}
	return n
}

// postOnceOnPullRequest posts body, which opens with the notice marker of key,
// on pr unless the App's bot already did. It can only have been posted after
// pr was opened, which was after its repository's build finished: the look
// for it starts there, never at the start of the thread.
func (p *pass) postOnceOnPullRequest(ctx context.Context, pr v1alpha1.IntentPullRequest, key, body string) error {
	var since time.Time
	if ap := p.in.Status.Approval; ap != nil {
		if build := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, pr.Repository).latest(); build != nil &&
			build.Status.FinishedAt != nil {
			since = build.Status.FinishedAt.Add(-clockSkew)
		}
	}
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, since)
	if err != nil {
		return err
	}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	if err != nil {
		return err
	}
	marker := templates.NoticeMarker(p.in.Namespace, p.in.Name, key)
	for _, c := range comments {
		if markerOf(c.Body) == marker && (bot == "" || strings.EqualFold(c.UserLogin, bot)) {
			return nil
		}
	}
	_, err = p.r.GitHub.CreatePullRequestComment(ctx, pr.Repository, pr.Number, body)
	return err
}
