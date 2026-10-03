// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// syncPRRoundNotices repairs one settled round per pass, oldest first. The
// durable cursor is written only after posting or finding the bot's marker.
// A lost response or status conflict therefore adopts the comment on retry.
// InReview also covers legacy rounds whose refusal was incorrectly discarded.
// Callers enforce the repository rate floor and still observe merge/cancel
// before attempting delivery, so a failing projection cannot strand the work.
// Each round's notice goes on the pull request of the round's own repository
// (finishPRRound); round numbers never repeat across repositories, so one
// cursor covers every pull request. The current round is never delivered
// while it is in flight, whatever activeRun says: a Revising intent (or one
// Blocked from Revising, which block leaves without an activeRun) posts its
// round's notice when the round ends.
func (p *pass) syncPRRoundNotices(ctx context.Context) (bool, error) {
	next, run, err := p.owedRoundNotice()
	if run == nil || err != nil {
		return false, err
	}
	return true, p.deliverRoundNotice(ctx, next, run)
}

// syncEndedRoundNotices is syncPRRoundNotices for an ended Intent, which no
// longer polls its pull requests: it asks the rate floor of the one
// repository the owed notice goes to, never its siblings', so a sibling patchy
// can no longer reach (one that left the Project and lost its Forge or App
// installation) holds nothing. A repository that has left the Project is
// asked nothing at all: finishPRRound writes nothing there, and the cursor
// moves past its notice. A notice owed in a repository the Project still
// holds is never discarded, refused or not: it is retried until delivered.
// wait reports that repository under its floor; changed a status write.
func (p *pass) syncEndedRoundNotices(ctx context.Context) (changed, wait bool, err error) {
	next, run, err := p.owedRoundNotice()
	if run == nil || err != nil {
		return false, false, err
	}
	if repo := run.Spec.Repository.URL; !p.leftProject(repo) {
		if ok, err := p.rateOK(ctx, repo); err != nil || !ok {
			return false, err == nil, err
		}
	}
	if err := p.deliverRoundNotice(ctx, next, run); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// owedRoundNotice is the next round whose notice is owed and its latest run;
// no run while none is owed yet.
func (p *pass) owedRoundNotice() (next int32, run *v1alpha1.IntentRun, err error) {
	upto := p.in.Status.Rounds
	if p.in.Status.ActiveRun != nil && !terminal(p.in.Status.Phase) || p.roundInFlight() {
		upto-- // the current round can still retry; do not report an attempt
	}
	next = p.in.Status.RoundNoticesThrough + 1
	if next > upto {
		return next, nil, nil
	}
	run = p.round(v1alpha1.IntentStageRevise, next, anyRepository).latest()
	if run == nil || p.pullRequest(run.Spec.Repository.URL) == nil {
		return next, nil, fmt.Errorf("round %d notice lacks its run or recorded PR", next)
	}
	return next, run, nil
}

// deliverRoundNotice posts round next's notice (finishPRRound) and then
// moves the cursor past it.
func (p *pass) deliverRoundNotice(ctx context.Context, next int32, run *v1alpha1.IntentRun) error {
	if err := p.finishPRRound(ctx, run); err != nil {
		return fmt.Errorf("deliver round %d notice: %w", next, err)
	}
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.RoundNoticesThrough = next
		return nil
	})
}
