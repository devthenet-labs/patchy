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
func (p *pass) syncPRRoundNotices(ctx context.Context) (bool, error) {
	upto := p.in.Status.Rounds
	if p.in.Status.ActiveRun != nil && !terminal(p.in.Status.Phase) {
		upto-- // the current round can still retry; do not report an attempt
	}
	next := p.in.Status.RoundNoticesThrough + 1
	if next > upto {
		return false, nil
	}
	run := p.round(v1alpha1.IntentStageRevise, next).latest()
	if run == nil || len(p.in.Status.PullRequests) != 1 {
		return false, fmt.Errorf("round %d notice lacks its run or recorded PR", next)
	}
	if err := p.finishPRRound(ctx, run); err != nil {
		return false, fmt.Errorf("deliver round %d notice: %w", next, err)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.RoundNoticesThrough = next
		return nil
	})
}
