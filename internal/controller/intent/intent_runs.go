// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
)

// The outcomes the controller records on a run beside the envelope's own.
const (
	// OutcomeAborted: the run ended with no stage result (its Job vanished,
	// its image could not be pulled, its Intent ended), or its launch was
	// refused before any Job existed.
	OutcomeAborted = "aborted"
	// OutcomeImageRequired: a build could not run on an accepted
	// repository-declared image. The agent never ran, so the attempt does
	// not count; the Intent goes Blocked with ImageRequired.
	OutcomeImageRequired = "image_required"
	// OutcomeNotBuilt: the build agent reported it could not build the
	// plan.
	OutcomeNotBuilt = "not_built"
	// OutcomeBranchExists: the intent branch exists at a commit other than
	// the one this run created. Nothing is forced.
	OutcomeBranchExists = "branch_exists"
)

// roundRuns are one stage's runs of one round, by attempt.
type roundRuns []*v1alpha1.IntentRun

// round is this Intent's runs of stage and round.
func (p *pass) round(stage v1alpha1.IntentStage, round int32) roundRuns {
	var rs roundRuns
	for _, run := range p.runs {
		if run.Spec.Stage == stage && run.Spec.Round == round {
			rs = append(rs, run)
		}
	}
	slices.SortFunc(rs, func(a, b *v1alpha1.IntentRun) int { return int(a.Spec.Attempt - b.Spec.Attempt) })
	return rs
}

func (rs roundRuns) latest() *v1alpha1.IntentRun {
	if len(rs) == 0 {
		return nil
	}
	return rs[len(rs)-1]
}

// next is the next attempt's ordinal: attempt ordinals never repeat within a
// round, uncounted attempts included.
func (rs roundRuns) next() int32 {
	if l := rs.latest(); l != nil {
		return l.Spec.Attempt + 1
	}
	return 1
}

// counted is how many attempts of the round failed with the agent having
// run: a failed run the sandbox refused or that had no image to run on did
// not run its agent; refused reports a completed run whose result is
// unusable all the same (a plan no comment can show).
func (rs roundRuns) counted(refused func(*v1alpha1.IntentRun) bool) int32 {
	var n int32
	for _, run := range rs {
		switch run.Status.Phase {
		case v1alpha1.RunFailed:
			if !uncounted(run) {
				n++
			}
		case v1alpha1.RunComplete:
			if refused != nil && refused(run) {
				n++
			}
		}
	}
	return n
}

// uncounted reports a failed run whose agent never ran. Its outcome is the
// controller's own: a pod may not report image_required (podOutcome).
func uncounted(run *v1alpha1.IntentRun) bool {
	return run.Status.Outcome == OutcomeImageRequired || runnerguard.Refused(run.Status.Conditions)
}

// imageBlocked reports a failed build run that blocks its Intent on the
// repository image: none usable, the default image ran, or the sandbox
// probe refused it.
func imageBlocked(run *v1alpha1.IntentRun) bool {
	return run.Status.Phase == v1alpha1.RunFailed && uncounted(run)
}

// previousAttempt is what the next attempt is told about latest when its
// agent ran and failed (or produced a plan no comment can show); nil
// otherwise.
func (p *pass) previousAttempt(latest *v1alpha1.IntentRun) *v1alpha1.PreviousAttempt {
	if latest == nil {
		return nil
	}
	switch {
	case latest.Status.Phase == v1alpha1.RunFailed && !uncounted(latest):
		return agentresult.PreviousAttempt(latest.Name, latest.Spec.Attempt,
			&v1alpha1.StageResult{Outcome: latest.Status.Outcome, Detail: latest.Status.Detail})
	case p.planRefused(latest):
		reason, _ := p.planRefusal(latest)
		return agentresult.PreviousAttempt(latest.Name, latest.Spec.Attempt,
			&v1alpha1.StageResult{Outcome: string(envelope.OutcomeReportInvalid),
				Detail: "the plan could not be offered for approval: " + reason})
	}
	return latest.Spec.PreviousAttempt
}

// launch creates the next run of stage's round, unless the Project is
// suspended (no run is launched until it is resumed), the Intent's spend has
// reached the Project's ceiling (Blocked), or the round has used every
// attempt ordinal (Failed).
func (p *pass) launch(ctx context.Context, stage v1alpha1.IntentStage, round, attempt int32,
	prev *v1alpha1.PreviousAttempt) (bool, error) {
	if p.proj.Spec.Suspend {
		return false, nil
	}
	if spent, ceiling := p.in.Status.Usage.CostMicroUSD, maxCostMicroUSD(p.proj); spent >= ceiling {
		return true, p.block(ctx, v1alpha1.ConditionBudgetExhausted, "CostCeilingReached",
			fmt.Sprintf("the intent has spent %d micro-USD, at or over the project's ceiling of %d; "+
				"raise limits.maxCostMicroUSD to continue", spent, ceiling))
	}
	if attempt > v1alpha1.MaxIntentRunAttempt {
		return true, p.fail(ctx)
	}
	run, err := p.createRun(ctx, stage, round, attempt, prev)
	if errors.Is(err, errRepositoryGone) {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "the approved plan's repository left the project; the intent fails",
			slog.String("intent", p.in.Name))
		return true, p.fail(ctx)
	}
	if err != nil {
		return false, err
	}
	return p.ensureActive(ctx, run)
}

// createRun creates the run under its deterministic name (the lease), or
// adopts the run a failed pass created: only one whose intentRef UID is this
// Intent's, since a same-named earlier Intent's may still be terminating.
func (p *pass) createRun(ctx context.Context, stage v1alpha1.IntentStage, round, attempt int32,
	prev *v1alpha1.PreviousAttempt) (*v1alpha1.IntentRun, error) {
	repo, ok := p.runRepository(stage)
	if !ok {
		return nil, errRepositoryGone
	}
	name := v1alpha1.IntentRunName(p.in.Name, stage, round, repo.Name, attempt)
	inputs := v1alpha1.IntentRunInputs{ConfigMap: runInputName(name), InputDigest: p.in.Status.Input.Digest}
	if stage == v1alpha1.IntentStageBuild {
		ap := p.in.Status.Approval
		inputs.InputDigest, inputs.PlanRevision, inputs.PlanDigest = ap.InputDigest, ap.PlanRevision, ap.PlanDigest
	}
	run := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: p.in.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
			Finalizers:      []string{v1alpha1.FinalizerJobs},
			OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
		},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: p.in.Name, UID: p.in.UID},
			Stage:     stage,
			Repository: v1alpha1.IntentRunRepository{
				URL: repo.URL, RepositoryRef: v1alpha1.LocalObjectReference{Name: runRepositoryName(name)},
			},
			Round:           round,
			Attempt:         attempt,
			Inputs:          inputs,
			Grant:           p.set.grant(p.proj, stage),
			PreviousAttempt: prev,
		},
	}
	err := p.r.Create(ctx, run)
	if err == nil {
		p.runs = append(p.runs, run)
		return run, nil
	}
	if !kerrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create run %s: %w", name, err)
	}
	var existing v1alpha1.IntentRun
	if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: name}, &existing); err != nil {
		return nil, fmt.Errorf("read run %s: %w", name, err)
	}
	if existing.Spec.IntentRef.UID != p.in.UID {
		return nil, fmt.Errorf("run %s: %w", name, errNotOwned)
	}
	return &existing, nil
}

// errRepositoryGone: the approved plan names a repository the Project no
// longer holds. Nothing is built anywhere else than what was approved.
var errRepositoryGone = errors.New("the approved plan's repository is no longer one of the project's")

// runRepository is the repository a run of stage works on: a plan plans from
// the Project's first repository; a build builds in the repository the
// approved plan names, while the Project still holds it, never one the
// Project has been changed to since the approval.
func (p *pass) runRepository(stage v1alpha1.IntentStage) (v1alpha1.ProjectRepository, bool) {
	if stage == v1alpha1.IntentStagePlan {
		return p.proj.Spec.Repositories[0], true
	}
	if pl := p.in.Status.Plan; pl != nil {
		for _, named := range pl.Repositories {
			for _, r := range p.proj.Spec.Repositories {
				if sameRepo(named, r.URL) {
					return r, true
				}
			}
		}
	}
	return v1alpha1.ProjectRepository{}, false
}

// ensureActive makes sure an unfinished run has its input ConfigMap and
// Repository, and is recorded as the Intent's active run.
func (p *pass) ensureActive(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	if run.Status.Phase == v1alpha1.RunComplete || run.Status.Phase == v1alpha1.RunFailed {
		return false, nil
	}
	if err := p.ensureRunChildren(ctx, run); err != nil {
		if errors.Is(err, errPlanChanged) {
			return true, p.fail(ctx)
		}
		return false, err
	}
	if ar := p.in.Status.ActiveRun; ar != nil && ar.Name == run.Name && ar.UID == run.UID {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.ActiveRun = &v1alpha1.ObjectReference{Name: run.Name, UID: run.UID}
		return nil
	})
}

// ensureRunChildren creates the run's input ConfigMap and Repository, both
// owned by the run, when missing. The input is re-hashed as it is written:
// a plan run's is the input snapshot, a build run's the approved plan alone,
// with an empty request. An existing object under either name is used only
// when the run is its controller owner, however it was found: anything else
// (a same-named earlier Intent's remains, or someone else's object) is left
// alone and the pass retried after a backoff.
func (p *pass) ensureRunChildren(ctx context.Context, run *v1alpha1.IntentRun) error {
	var cm corev1.ConfigMap
	err := p.r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Inputs.ConfigMap}, &cm)
	switch {
	case err == nil:
		if !controlledBy(cm.OwnerReferences, run.UID) {
			return fmt.Errorf("configmap %s: %w", cm.Name, errNotOwned)
		}
	case kerrors.IsNotFound(err):
		data, err := p.runInput(ctx, run)
		if err != nil {
			return err
		}
		if _, err := p.ensureConfigMap(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: run.Spec.Inputs.ConfigMap, Namespace: run.Namespace,
				Labels: map[string]string{
					v1alpha1.LabelIntent: p.in.Name, v1alpha1.LabelIntentRun: run.Name,
				},
				OwnerReferences: []metav1.OwnerReference{runOwner(run)},
			},
			Immutable: new(true),
			Data:      data,
		}, run.UID); err != nil {
			return err
		}
	default:
		return err
	}

	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name}
	err = p.r.Get(ctx, key, &repo)
	switch {
	case err == nil:
		if !controlledBy(repo.OwnerReferences, run.UID) {
			return fmt.Errorf("repository %s: %w", key.Name, errNotOwned)
		}
		return nil
	case !kerrors.IsNotFound(err):
		return err
	}
	repo = v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			// Never LabelFinding: the Finding flow's mappers ignore these.
			Labels: map[string]string{
				v1alpha1.LabelIntent: p.in.Name, v1alpha1.LabelIntentRun: run.Name,
			},
			OwnerReferences: []metav1.OwnerReference{runOwner(run)},
		},
		// No branch: source-controller pins the default branch's head.
		Spec: v1alpha1.RepositorySpec{URL: run.Spec.Repository.URL},
	}
	err = p.r.Create(ctx, &repo)
	if err == nil {
		return nil
	}
	if !kerrors.IsAlreadyExists(err) {
		return fmt.Errorf("create repository %s: %w", key.Name, err)
	}
	var existing v1alpha1.Repository
	if err := p.r.APIReader.Get(ctx, key, &existing); err != nil {
		return fmt.Errorf("read repository %s: %w", key.Name, err)
	}
	if !controlledBy(existing.OwnerReferences, run.UID) {
		return fmt.Errorf("repository %s: %w", key.Name, errNotOwned)
	}
	return nil
}

// runInput is the run's handoff: issue.md and, for a build, investigation.md.
func (p *pass) runInput(ctx context.Context, run *v1alpha1.IntentRun) (map[string]string, error) {
	if run.Spec.Stage == v1alpha1.IntentStagePlan {
		var cm corev1.ConfigMap
		name := p.in.Status.Input.ConfigMap
		if err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: name}, &cm); err != nil {
			return nil, fmt.Errorf("read the input snapshot: %w", err)
		}
		issue := cm.Data[keyIssue]
		if got := digest([]byte(issue)); got != run.Spec.Inputs.InputDigest {
			return nil, fmt.Errorf("input snapshot %s holds %s, not %s", name, got, run.Spec.Inputs.InputDigest)
		}
		return map[string]string{keyIssue: issue}, nil
	}
	pl := p.in.Status.Plan
	if pl == nil || pl.Revision != run.Spec.Inputs.PlanRevision || pl.Digest != run.Spec.Inputs.PlanDigest {
		return nil, fmt.Errorf("run %s: %w: the recorded plan is not the approved one", run.Name, errPlanChanged)
	}
	raw, err := p.planBytes(ctx, pl)
	if err != nil {
		return nil, err
	}
	// The build reads the approved plan and nothing else of the request:
	// its issue.md is empty, which agent-runner requires.
	return map[string]string{keyIssue: "", keyInvestigation: string(raw)}, nil
}

// intentOwner is the controller owner reference to the Intent.
func intentOwner(in *v1alpha1.Intent) metav1.OwnerReference {
	return *metav1.NewControllerRef(in, v1alpha1.GroupVersion.WithKind("Intent"))
}

// runOwner is the controller owner reference to the run.
func runOwner(run *v1alpha1.IntentRun) metav1.OwnerReference {
	return *metav1.NewControllerRef(run, v1alpha1.GroupVersion.WithKind("IntentRun"))
}

// controlledBy reports a controller owner reference to uid.
func controlledBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}
