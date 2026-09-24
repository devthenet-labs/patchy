// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// syncStatusComment keeps the one status comment on the issue up to date. It
// exists once the trigger was accepted (an Intent refused at Pending gets
// its notice alone). It is created exactly once: before creating it patchy
// looks for its own among the comments since the trigger, so a restart
// between posting and recording adopts it. It is edited only when what it
// would say changed (its digest), and a status comment someone deleted is
// posted again.
func (p *pass) syncStatusComment(ctx context.Context) error {
	if p.in.Status.Input == nil {
		return nil
	}
	body, err := templates.RenderIntentStatusComment(p.statusComment())
	if err != nil {
		return err
	}
	d := digest([]byte(body))
	tr := p.in.Status.Tracking
	if tr != nil && tr.StatusCommentID != 0 && tr.StatusDigest == d {
		return nil
	}
	id := int64(0)
	if tr != nil {
		id = tr.StatusCommentID
	}
	if id != 0 {
		err := p.r.GitHub.EditIssueComment(ctx, p.repo(), id, body)
		switch {
		case ghclient.IsNotFound(err):
			id = 0 // deleted: post it again
		case err != nil:
			return fmt.Errorf("edit the status comment: %w", err)
		}
	}
	if id == 0 {
		marker := templates.IntentStatusMarker(p.in.Namespace, p.in.Name)
		c, err := p.findOwn(ctx, marker, p.in.Spec.RequestedBy.At.Time)
		if err != nil {
			return err
		}
		switch {
		case c == nil:
			if c, err = p.r.GitHub.CreateIssueComment(ctx, p.repo(), p.number(), body); err != nil {
				return fmt.Errorf("post the status comment: %w", err)
			}
		case c.Body != body:
			if err := p.r.GitHub.EditIssueComment(ctx, p.repo(), c.ID, body); err != nil {
				return fmt.Errorf("edit the status comment: %w", err)
			}
		}
		id = c.ID
	}
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Tracking = &v1alpha1.IntentTracking{StatusCommentID: id, StatusDigest: d}
		return nil
	})
}

// statusComment is what the status comment says about the Intent now.
func (p *pass) statusComment() templates.IntentStatusComment {
	st := p.in.Status
	c := templates.IntentStatusComment{
		Namespace: p.in.Namespace, Intent: p.in.Name, Phase: string(st.Phase),
		CostMicroUSD: st.Usage.CostMicroUSD, MaxCostMicroUSD: maxCostMicroUSD(p.proj),
		Commands: verbs(st.Phase),
	}
	if pl := st.Plan; pl != nil && pl.CommentID != 0 {
		c.PlanRevision, c.Summary = pl.Revision, pl.Summary
		if p.in.Spec.Issue.URL != "" {
			c.PlanURL = fmt.Sprintf("%s#issuecomment-%d", p.in.Spec.Issue.URL, pl.CommentID)
		}
	}
	if ap := st.Approval; ap != nil {
		c.ApprovedBy, c.ApprovedRevision = ap.By, ap.PlanRevision
	}
	for _, pr := range st.PullRequests {
		c.PullRequests = append(c.PullRequests, templates.IntentPullRequest{
			Repository: repoSlug(pr.Repository), Number: pr.Number, URL: pr.URL, State: pr.State,
		})
	}
	switch st.Phase {
	case v1alpha1.IntentBlocked:
		var reasons []string
		for _, typ := range blockingConditions {
			if cond := meta.FindStatusCondition(st.Conditions, typ); cond != nil && cond.Status == metav1.ConditionTrue {
				reasons = append(reasons, cond.Message)
			}
		}
		c.Reason = strings.Join(reasons, "\n")
	case v1alpha1.IntentFailed:
		c.Reason = p.failureReason()
	}
	return c
}

// failureReason is the latest failed run's outcome and detail, as the
// Failed status explains itself.
func (p *pass) failureReason() string {
	for i := len(p.runs) - 1; i >= 0; i-- {
		run := p.runs[i]
		switch {
		case run.Status.Phase == v1alpha1.RunFailed:
			if run.Status.Detail == "" {
				return run.Status.Outcome
			}
			return run.Status.Outcome + ": " + run.Status.Detail
		case p.planRefused(run):
			reason, _ := p.planRefusal(run)
			return "the plan could not be offered for approval: " + reason
		}
	}
	return ""
}
