// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// errNotOwned: an object exists under a derived name but belongs to someone
// else (a same-named earlier Intent's, still terminating); it is left alone
// and the create retried after a backoff.
var errNotOwned = errors.New("exists and belongs to another owner")

// pending decides the trigger that created the Intent: an approver's starts
// planning; anyone else's closes the Intent with one notice, the label
// removed first.
func (p *pass) pending(ctx context.Context) (bool, error) {
	rb := p.in.Spec.RequestedBy
	if err := p.readBot(ctx); err != nil {
		return false, err
	}
	actor := ghclient.Actor{Login: rb.Login}
	if p.isOwnLogin(rb.Login) {
		actor.Type = "Bot"
	}
	ok, bot, err := p.authorize(ctx, actor)
	if err != nil {
		return false, err
	}
	if ok {
		return true, p.startPlanning(ctx, nil, nil)
	}
	if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), p.trigger()); err != nil {
		return false, fmt.Errorf("remove the trigger label: %w", err)
	}
	a := humanAction{source: v1alpha1.IntentActionLabel, id: rb.EventID, at: rb.At.Time, actor: actor,
		label: p.trigger()}
	if err := p.notAllowed(ctx, a, bot, true, true); err != nil {
		return false, err
	}
	return true, p.setPhase(ctx, v1alpha1.IntentClosed, nil)
}

// startPlanning snapshots the request at the next input revision and moves
// the Intent to Planning, consuming trigger (a replan or revival) in the same
// status write. issue is the issue as this pass read it, nil to read it now.
// A replan's and a revival's snapshot carries the approvers' comments since
// the last plan.
func (p *pass) startPlanning(ctx context.Context, issue *ghclient.Issue, trigger *v1alpha1.IntentAction) error {
	if issue == nil {
		var err error
		if issue, err = p.r.GitHub.GetIssue(ctx, p.repo(), p.number()); err != nil {
			return fmt.Errorf("read the issue: %w", err)
		}
	}
	snap := snapshot{Title: issue.Title, Body: issue.Body}
	for _, r := range p.proj.Spec.Repositories {
		snap.Repositories = append(snap.Repositories, r.URL)
	}
	if trigger != nil {
		since := p.in.Spec.RequestedBy.At.Time
		if pl := p.in.Status.Plan; pl != nil && pl.PostedAt != nil {
			since = pl.PostedAt.Time
		}
		feedback, err := p.feedbackSince(ctx, since)
		if err != nil {
			return err
		}
		snap.Comments = renderFeedback(feedback)
	}
	revision := int32(1)
	if in := p.in.Status.Input; in != nil {
		revision = in.Revision + 1
	}
	if revision > v1alpha1.MaxIntentRound {
		return fmt.Errorf("input revision %d is past the limit of %d", revision, v1alpha1.MaxIntentRound)
	}
	cm, err := p.ensureInputConfigMap(ctx, revision, snap)
	if err != nil {
		return err
	}
	input := v1alpha1.IntentInput{Revision: revision, Digest: digest([]byte(cm.Data[keyIssue])), ConfigMap: cm.Name}
	return p.setPhase(ctx, v1alpha1.IntentPlanning, func(cur *v1alpha1.Intent) {
		cur.Status.Input = &input
		cur.Status.Plan = nil
		cur.Status.Approval = nil
		cur.Status.ActiveRun = nil
		if trigger != nil {
			cur.Status.LastTrigger = trigger
		}
	})
}

// feedbackSince is the approvers' comments since since, oldest first:
// patchy's own and anyone else's left out (non-approvers' comments never
// reach a prompt), and so is any comment edited after it was posted. GitHub
// lets anyone with write access edit an approver's comment and still names
// the approver as its author, so an edited comment is not certainly an
// approver's words.
func (p *pass) feedbackSince(ctx context.Context, since time.Time) ([]*ghclient.Comment, error) {
	if err := p.listComments(ctx, since); err != nil {
		return nil, err
	}
	var out []*ghclient.Comment
	for _, c := range p.comments {
		if !c.CreatedAt.After(since) || p.isOwn(c) || p.isOwnLogin(c.UserLogin) || c.Author().IsBot() ||
			!isApprover(p.proj, c.UserLogin) || edited(c) {
			continue
		}
		out = append(out, c)
	}
	// The renderer can include only the newest maxFeedbackItems. Ask GitHub
	// about those comments' edit history too: REST timestamps cannot reveal
	// an edit made in the second a comment was posted.
	if len(out) > maxFeedbackItems {
		out = out[len(out)-maxFeedbackItems:]
	}
	kept := out[:0]
	for _, c := range out {
		e, err := p.everEdited(ctx, c)
		if err != nil {
			return nil, err
		}
		if !e {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// ensureInputConfigMap creates the immutable snapshot ConfigMap of revision,
// or adopts the one a failed pass created (owned by this Intent): its bytes,
// not a fresh snapshot's, are what the input digest covers.
func (p *pass) ensureInputConfigMap(ctx context.Context, revision int32, snap snapshot) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: inputConfigMapName(p.in.Name, revision), Namespace: p.in.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
			OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
		},
		Immutable: new(true),
		Data: map[string]string{
			keyIssue:        string(snap.render()),
			keyTitle:        snap.Title,
			keyBody:         snap.Body,
			keyRepositories: strings.Join(snap.Repositories, "\n"),
			keyComments:     snap.Comments,
		},
	}
	return p.ensureConfigMap(ctx, cm, p.in.UID)
}

// ensureConfigMap creates cm, or adopts the existing one of its name when
// its controller owner is owner.
func (p *pass) ensureConfigMap(ctx context.Context, cm *corev1.ConfigMap, owner types.UID) (*corev1.ConfigMap, error) {
	err := p.r.Create(ctx, cm)
	if err == nil {
		return cm, nil
	}
	if !kerrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create configmap %s: %w", cm.Name, err)
	}
	var existing corev1.ConfigMap
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}, &existing); err != nil {
		return nil, fmt.Errorf("read configmap %s: %w", cm.Name, err)
	}
	if !controlledBy(existing.OwnerReferences, owner) {
		return nil, fmt.Errorf("configmap %s: %w", cm.Name, errNotOwned)
	}
	return &existing, nil
}

// inputSnapshot reads the parts of the input snapshot back; nil when its
// ConfigMap is gone.
func (p *pass) inputSnapshot(ctx context.Context, input *v1alpha1.IntentInput) (*snapshot, error) {
	var cm corev1.ConfigMap
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: input.ConfigMap}, &cm)
	if kerrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the input snapshot: %w", err)
	}
	s := &snapshot{Title: cm.Data[keyTitle], Body: cm.Data[keyBody], Comments: cm.Data[keyComments]}
	if r := cm.Data[keyRepositories]; r != "" {
		s.Repositories = strings.Split(r, "\n")
	}
	return s, nil
}

// planning runs the current input revision's plan round: a first attempt,
// the write-back of a plan that ran, or the next attempt after a failed or
// unshowable one, until the attempts are spent and the Intent fails.
func (p *pass) planning(ctx context.Context) (bool, error) {
	round := p.in.Status.Input.Revision
	rs := p.round(v1alpha1.IntentStagePlan, round)
	latest := rs.latest()
	if latest == nil {
		return p.launch(ctx, v1alpha1.IntentStagePlan, round, 1, nil)
	}
	switch latest.Status.Phase {
	case v1alpha1.RunComplete:
		refusal, notice := p.planRefusal(latest)
		if refusal == "" {
			return p.writeBack(ctx, latest)
		}
		since := latest.CreationTimestamp.Add(-clockSkew)
		if notice != "" {
			if err := p.notice(ctx, fmt.Sprintf("plan-r%d", round), since, notice, nil); err != nil {
				return false, err
			}
		}
	case v1alpha1.RunFailed:
	default:
		return p.ensureActive(ctx, latest)
	}
	if rs.counted(p.planRefused) >= p.set.MaxAttempts {
		return true, p.fail(ctx)
	}
	return p.launch(ctx, v1alpha1.IntentStagePlan, round, rs.next(), p.previousAttempt(latest))
}

// planComment is the approval comment's input for a plan report.
func (p *pass) planComment(revision int32, raw []byte) (templates.PlanComment, error) {
	parsed, err := report.ParsePlan(raw)
	if err != nil {
		return templates.PlanComment{}, err
	}
	return templates.PlanComment{
		Namespace: p.in.Namespace, Intent: p.in.Name, Revision: revision, Report: raw,
		Summary: parsed.Summary, NewDependencies: parsed.NewDependencies, Questions: parsed.Questions,
		ApproveLabel: approveLabel(p.proj), TriggerLabel: p.trigger(),
	}, nil
}

// planRefusal says why a completed plan run's plan cannot be offered for
// approval ("" when it can), and the notice to post in its place: no comment
// can show it in full (templates.ErrPlanRefused), or it no longer parses.
// The render is deterministic, so every pass reaches the same answer, and an
// unshowable plan counts as a failed attempt without a record of its own.
func (p *pass) planRefusal(run *v1alpha1.IntentRun) (reason, notice string) {
	pc, err := p.planComment(run.Spec.Round, []byte(run.Status.Report))
	if err != nil {
		return fmt.Sprintf("the plan report does not parse: %v", err), ""
	}
	notice, err = templates.RenderPlanComment(pc)
	switch {
	case errors.Is(err, templates.ErrPlanRefused):
		return err.Error(), notice
	case err != nil:
		return err.Error(), ""
	}
	return "", ""
}

// planRefused reports a completed plan run whose plan cannot be offered.
func (p *pass) planRefused(run *v1alpha1.IntentRun) bool {
	if run.Spec.Stage != v1alpha1.IntentStagePlan || run.Status.Phase != v1alpha1.RunComplete {
		return false
	}
	reason, _ := p.planRefusal(run)
	return reason != ""
}

// writeBack offers a completed plan for approval, in two durable steps. The
// first stores the plan's bytes in their immutable ConfigMap and records the
// plan (its digest is those bytes'). The second, unless a retry finds the plan
// comment already posted, removes any approve label (none may predate the
// plan it approves), posts the estimate notice when the plan expects more
// than its build is granted, and posts the plan comment; then it records the
// comment GitHub stored: its id, the
// digest of its body as GitHub returned it, and GitHub's created_at, which an
// approval must postdate.
func (p *pass) writeBack(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	round := run.Spec.Round
	pl := p.in.Status.Plan
	if pl == nil || pl.Revision != round {
		raw := []byte(run.Status.Report)
		parsed, err := report.ParsePlan(raw)
		if err != nil {
			return false, fmt.Errorf("parse plan r%d: %w", round, err)
		}
		cm, err := p.ensureConfigMap(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: planConfigMapName(p.in.Name, round), Namespace: p.in.Namespace,
				Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
				OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
			},
			Immutable: new(true),
			Data:      map[string]string{keyPlan: string(raw)},
		}, p.in.UID)
		if err != nil {
			return false, err
		}
		plan := v1alpha1.IntentPlan{
			Revision: round, Digest: digest([]byte(cm.Data[keyPlan])), ConfigMap: cm.Name,
			Summary: parsed.Summary, Repositories: parsed.Repositories,
		}
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Plan = &plan
			return nil
		})
	}
	raw, err := p.planBytes(ctx, pl)
	if err != nil {
		return false, err
	}
	pc, err := p.planComment(round, raw)
	if err != nil {
		return false, err
	}
	body, err := templates.RenderPlanComment(pc)
	if err != nil {
		return false, fmt.Errorf("render plan r%d: %w", round, err)
	}
	since := p.enteredAt().Add(-clockSkew)
	marker := templates.PlanMarker(p.in.Namespace, p.in.Name, round, pl.Digest)
	c, err := p.findOwn(ctx, marker, since)
	if err != nil {
		return false, err
	}
	if c == nil || c.Body != body || edited(c) {
		// Not found, or found edited before it was recorded (even back to
		// its original bytes, which an approval would refuse): post the plan
		// afresh, so what is recorded is what patchy posted, unedited. No
		// approve label may predate the plan it approves, so any is removed
		// first. Once the plan is posted the label is left alone: a retry
		// that finds the plan must not take away an approver's label added
		// since, which is an approval to answer.
		if err := p.r.GitHub.RemoveLabel(ctx, p.repo(), p.number(), approveLabel(p.proj)); err != nil {
			return false, fmt.Errorf("remove the approve label: %w", err)
		}
		if err := p.estimateNotice(ctx, round, raw, since); err != nil {
			return false, err
		}
		if c, err = p.r.GitHub.CreateIssueComment(ctx, p.repo(), p.number(), body); err != nil {
			return false, fmt.Errorf("post plan r%d: %w", round, err)
		}
	}
	posted := metav1.NewTime(c.CreatedAt)
	return true, p.setPhase(ctx, v1alpha1.IntentAwaitingApproval, func(cur *v1alpha1.Intent) {
		cur.Status.Plan.CommentID = c.ID
		cur.Status.Plan.CommentDigest = digest([]byte(c.Body))
		cur.Status.Plan.PostedAt = &posted
		cur.Status.ActiveRun = nil
	})
}

// planBytes reads the recorded plan's bytes from its ConfigMap and checks
// they still hash to the recorded digest.
func (p *pass) planBytes(ctx context.Context, pl *v1alpha1.IntentPlan) ([]byte, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: p.in.Namespace, Name: pl.ConfigMap}
	if err := p.r.APIReader.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("read plan r%d: %w", pl.Revision, err)
	}
	raw := []byte(cm.Data[keyPlan])
	if got := digest(raw); got != pl.Digest {
		return nil, fmt.Errorf("plan r%d: %w: %s holds %s, not %s", pl.Revision, errPlanChanged, cm.Name, got, pl.Digest)
	}
	return raw, nil
}

// errPlanChanged: a plan's stored bytes no longer hash to its recorded
// digest. Nothing is built from them.
var errPlanChanged = errors.New("the plan's bytes changed")

// estimateNotice tells the approver, once and before the plan, when the
// plan's own estimate of its build exceeds the build's grant.
func (p *pass) estimateNotice(ctx context.Context, round int32, raw []byte, since time.Time) error {
	parsed, err := report.ParsePlan(raw)
	if err != nil {
		return err
	}
	grant := p.set.grant(p.proj, v1alpha1.IntentStageBuild)
	if int64(parsed.EstimatedMaxTurns) <= int64(grant.MaxTurns) &&
		int64(parsed.EstimatedTokenBudget) <= grant.TokenBudget {
		return nil
	}
	key := fmt.Sprintf("estimate-r%d", round)
	body, err := templates.RenderEstimateNotice(templates.EstimateNotice{
		Namespace: p.in.Namespace, Intent: p.in.Name, Key: key, PlanRevision: round,
		EstimatedMaxTurns: parsed.EstimatedMaxTurns, EstimatedTokenBudget: parsed.EstimatedTokenBudget,
		GrantMaxTurns: grant.MaxTurns, GrantTokenBudget: grant.TokenBudget, TriggerLabel: p.trigger(),
	})
	return p.notice(ctx, key, since, body, err)
}
