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
	// OutcomeHoldExpired: the build finished while its Intent was suspended,
	// and its Job expired (its TTL) before the suspension was lifted, taking
	// the unpushed changeset with it. The loss is the suspension's, not the
	// agent's, so the attempt does not count.
	OutcomeHoldExpired = "hold_expired"
	// OutcomePushRefused: GitHub refused the build's commit or branch for
	// itself (a ruleset restricting ref creation, a permission the App lost,
	// a repository gone): the same push would be refused again, so the run
	// ends rather than hold its slot retrying it. It counts as an attempt.
	OutcomePushRefused = "push_refused"
	// OutcomeLaunchRefused: the API server refused the run's agent Job for
	// itself (an admission policy or webhook denying it, an invalid Job, a
	// missing namespace). It counts as an attempt.
	OutcomeLaunchRefused = "launch_refused"
	// OutcomeHeadMoved: the PR branch changed after the revise run pinned it.
	// The stale result is never pushed and may be retried on the new head.
	OutcomeHeadMoved = "head_moved"
	// OutcomeInputUnavailable: the feedback named by an immutable round
	// vanished or ceased to be authorised before the agent could read it.
	OutcomeInputUnavailable = "input_unavailable"
	// OutcomeNoUsableFeedback: the review or command supplied no unedited,
	// authorised, substantive feedback. No new agent runs on this outcome;
	// legacy already-launched rounds may be classified here at collection.
	// The round does not spend the Project's maxRevisions allowance.
	OutcomeNoUsableFeedback = "no_usable_feedback"
)

// roundRuns are one stage's runs of one round on one repository, by attempt.
type roundRuns []*v1alpha1.IntentRun

// anyRepository selects a round's runs whatever repository they work on. A
// plan round has one run per attempt, planned from the Project's first
// repository, and a revise round's number is the Intent's own ordinal
// (status.rounds), which never repeats across repositories; a build round
// has one run per approved repository and attempt, so it is always read for
// one repository.
const anyRepository = ""

// round is this Intent's runs of stage and round on repoURL (anyRepository:
// on any), by attempt. A build round's attempts, next attempt and counted
// failures are its repository's own: each repository of a multi-repository
// plan is built, retried and spent separately.
func (p *pass) round(stage v1alpha1.IntentStage, round int32, repoURL string) roundRuns {
	var rs roundRuns
	for _, run := range p.runs {
		if run.Spec.Stage == stage && run.Spec.Round == round &&
			(repoURL == anyRepository || sameRepo(run.Spec.Repository.URL, repoURL)) {
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

// uncounted reports a failed run whose agent never ran, or whose result a
// suspension lost. Its outcome is the controller's own: a pod may not report
// image_required or hold_expired (podOutcome).
func uncounted(run *v1alpha1.IntentRun) bool {
	return run.Status.Outcome == OutcomeImageRequired || run.Status.Outcome == OutcomeHoldExpired ||
		run.Status.Outcome == OutcomeHeadMoved ||
		runnerguard.Refused(run.Status.Conditions)
}

// imageBlocked reports a failed build run that blocks its Intent on the
// repository image: none usable, the default image ran, or the sandbox
// probe refused it.
func imageBlocked(run *v1alpha1.IntentRun) bool {
	return run.Status.Phase == v1alpha1.RunFailed && run.Status.Outcome != OutcomeHoldExpired && uncounted(run)
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

// mayLaunch reports whether runs may be created now. Nothing is launched
// while the Project is suspended (ok and stop both false: the phase waits for
// it to be resumed), and nothing once the Intent's spend has reached the
// Project's ceiling (Blocked; stop). The ceiling is checked once before a
// build round's fan-out, so it stays advisory for builds: one pass may start
// every approved repository's build below it.
func (p *pass) mayLaunch(ctx context.Context) (ok, stop bool, err error) {
	if p.proj.Spec.Suspend {
		return false, false, nil
	}
	if spent, ceiling := p.in.Status.Usage.CostMicroUSD, maxCostMicroUSD(p.proj); spent >= ceiling {
		return false, true, p.block(ctx, v1alpha1.ConditionBudgetExhausted, "CostCeilingReached",
			fmt.Sprintf("the intent has spent %d micro-USD, at or over the project's ceiling of %d; "+
				"raise limits.maxCostMicroUSD to continue", spent, ceiling))
	}
	return true, false, nil
}

// launchPlan creates the next run of the plan round, unless the Project is
// suspended (no run is launched until it is resumed), the Intent's spend has
// reached the Project's ceiling (Blocked), or the round has used every
// attempt ordinal (Failed).
func (p *pass) launchPlan(ctx context.Context, round, attempt int32, prev *v1alpha1.PreviousAttempt) (bool, error) {
	if ok, stop, err := p.mayLaunch(ctx); !ok {
		return stop, err
	}
	if attempt > v1alpha1.MaxIntentRunAttempt {
		return true, p.fail(ctx)
	}
	run, err := p.createRun(ctx, v1alpha1.IntentStagePlan, p.proj.Spec.Repositories[0], round, attempt, prev)
	if err != nil {
		return false, err
	}
	return p.ensureActive(ctx, run)
}

// buildLaunch is one repository's next build attempt.
type buildLaunch struct {
	repo    v1alpha1.ProjectRepository
	attempt int32
	prev    *v1alpha1.PreviousAttempt
}

// launchBuilds creates the next build run of each of launches, in one pass,
// unless the Project is suspended or the spend is at its ceiling (checked
// once, for the whole fan-out), an attempt ordinal is past the last (Failed),
// or a repository's intent branch is not this round's to use (Blocked,
// before any build is spent). Each create is its own lease: a pass that
// stops between two creates adopts the first on the next one.
func (p *pass) launchBuilds(ctx context.Context, round int32, launches []buildLaunch) (
	runs []*v1alpha1.IntentRun, stop bool, err error) {
	if ok, stop, err := p.mayLaunch(ctx); !ok {
		return nil, stop, err
	}
	for _, l := range launches {
		if l.attempt > v1alpha1.MaxIntentRunAttempt {
			return nil, true, p.fail(ctx)
		}
	}
	for _, l := range launches {
		if blocked, err := p.blockOnBranch(ctx, round, l.repo); blocked || err != nil {
			return nil, blocked, err
		}
	}
	for _, l := range launches {
		run, err := p.createRun(ctx, v1alpha1.IntentStageBuild, l.repo, round, l.attempt, l.prev)
		if err != nil {
			return nil, false, err
		}
		runs = append(runs, run)
	}
	return runs, false, nil
}

// blockOnBranch blocks the Intent, before a build of repo is spent, when its
// branch there is not this round's to use (branchConflict): the block lifts
// once the branch is gone.
func (p *pass) blockOnBranch(ctx context.Context, round int32, repo v1alpha1.ProjectRepository) (bool, error) {
	sha, stale, err := p.branchConflict(ctx, repo.URL, round)
	if err != nil || sha == "" {
		return false, err
	}
	branch := branchName(p.in.Name)
	if stale != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "the intent branch is at a commit an earlier round pushed",
			slog.String("intent", p.in.Name), slog.String("repository", repo.URL), slog.String("branch", branch),
			slog.String("commit", sha), slog.String("run", stale.Name))
		return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonStaleRoundBranch,
			fmt.Sprintf("the branch %s already exists in %s at %s, which run %s of an earlier round of this "+
				"intent pushed; patchy never moves it, and builds nothing while it stands. The branch is "+
				"patchy's own, so deleting it is safe. Delete it to resume.", branch, repoSlug(repo.URL), sha,
				stale.Name))
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the intent branch exists at a commit this intent did not push",
		slog.String("intent", p.in.Name), slog.String("repository", repo.URL), slog.String("branch", branch),
		slog.String("commit", sha))
	return true, p.block(ctx, v1alpha1.ConditionBranchConflict, ReasonBranchExists,
		fmt.Sprintf("the branch %s already exists in %s at %s, a commit this intent did not push (an earlier "+
			"intent under this name, or someone else, made it); patchy never moves it, and builds nothing "+
			"while it stands. Delete the branch to resume.", branch, repoSlug(repo.URL), sha))
}

// createRun creates the run of stage on repo under its deterministic name
// (the lease), or adopts the run a failed pass created: only one whose
// intentRef UID is this Intent's, since a same-named earlier Intent's may
// still be terminating. A plan run of a Project with more than one
// repository also reads every other one (spec.trees), each from a
// Repository of its own the run will own.
func (p *pass) createRun(ctx context.Context, stage v1alpha1.IntentStage, repo v1alpha1.ProjectRepository,
	round, attempt int32, prev *v1alpha1.PreviousAttempt) (*v1alpha1.IntentRun, error) {
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
	if stage == v1alpha1.IntentStagePlan {
		for _, tree := range p.proj.Spec.Repositories[1:] {
			run.Spec.Trees = append(run.Spec.Trees, v1alpha1.IntentRunTree{
				Name: tree.Name, URL: tree.URL,
				RepositoryRef: v1alpha1.LocalObjectReference{Name: v1alpha1.IntentRunTreeRepositoryName(name, tree.Name)},
			})
		}
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
	// The name is the lease of one stage, round and attempt, and a build's
	// of one repository; a run under it for anything else is not this one.
	// Rounds are read by URL and names are made from the Project's keys, so
	// a key moved from one repository to another (the Project changed under
	// the intent) can name another repository's run.
	if existing.Spec.Stage != stage || existing.Spec.Round != round || existing.Spec.Attempt != attempt ||
		stage == v1alpha1.IntentStageBuild && !sameRepo(existing.Spec.Repository.URL, repo.URL) {
		return nil, fmt.Errorf("%w: run %s, the name the build of %s takes under the key %q, is the %s run of %s "+
			"(round %d, attempt %d)", errRunNameTaken, name, repoSlug(repo.URL), repo.Name, existing.Spec.Stage,
			repoSlug(existing.Spec.Repository.URL), existing.Spec.Round, existing.Spec.Attempt)
	}
	return &existing, nil
}

// errRunNameTaken: the deterministic name of a run to create is held by
// another of this Intent's runs: the Project's repository keys changed while
// the intent was building, one key now naming another repository than the
// one its runs were created for. The run is never adopted as this one.
var errRunNameTaken = errors.New("the Project's repository keys changed under the intent")

// errRepositoryGone: the approved plan names a repository the Project no
// longer holds. Nothing is built anywhere else than what was approved.
var errRepositoryGone = errors.New("the approved plan's repository is no longer one of the project's")

// projectRepository is the Project's repository at url, compared the way
// forges compare URLs.
func (p *pass) projectRepository(url string) (v1alpha1.ProjectRepository, bool) {
	for _, r := range p.proj.Spec.Repositories {
		if sameRepo(url, r.URL) {
			return r, true
		}
	}
	return v1alpha1.ProjectRepository{}, false
}

// leftProject reports a repository the Project no longer holds. patchy
// writes nothing more to one: no push, no round notice, no untracked notice.
func (p *pass) leftProject(url string) bool {
	_, ok := p.projectRepository(url)
	return !ok
}

// approvedRepositories are the Project repositories the approved plan names,
// in the plan's order: what the build round builds, one run each, and where
// it opens one pull request each. gone is true when the plan names one the
// Project no longer holds (or names none): nothing is built anywhere but
// what was approved, and no part of a plan is built without the rest, so
// the Intent fails. Never a repository the Project was changed to since the
// approval.
func (p *pass) approvedRepositories() (repos []v1alpha1.ProjectRepository, gone bool) {
	pl := p.in.Status.Plan
	if pl == nil {
		return nil, true
	}
	for _, named := range pl.Repositories {
		r, ok := p.projectRepository(named)
		if !ok {
			return nil, true
		}
		if !slices.ContainsFunc(repos, func(o v1alpha1.ProjectRepository) bool { return sameRepo(o.URL, r.URL) }) {
			repos = append(repos, r)
		}
	}
	return repos, len(repos) == 0
}

// approvedRepository is the Project's repository at url when the approved
// plan names it: a revise round works only in a repository the approved
// plan built. Only url itself is looked up in the Project. A round needs its
// own repository and nothing of its siblings', so one that has left the
// Project since (its pull request merged, say) stops no round on the others,
// as approvedRepositories, which builds need whole, would.
func (p *pass) approvedRepository(url string) (v1alpha1.ProjectRepository, bool) {
	pl := p.in.Status.Plan
	if pl == nil || !slices.ContainsFunc(pl.Repositories, func(named string) bool { return sameRepo(named, url) }) {
		return v1alpha1.ProjectRepository{}, false
	}
	return p.projectRepository(url)
}

// repositorySlugs are the "owner/name" of repos, in their order, as the
// Project spells them: how a comment or pull request body names the
// repositories of an intent, never in a planner's spelling.
func repositorySlugs(repos []v1alpha1.ProjectRepository) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = repoSlug(r.URL)
	}
	return out
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
	if run.Spec.Stage == v1alpha1.IntentStageRevise {
		return p.ensureReviseChildren(ctx, run)
	}
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

	own := run.Spec.Repository
	if err := p.ensureRunRepository(ctx, run, own.RepositoryRef.Name, own.URL); err != nil {
		return err
	}
	// A plan run of a multi-repository Project reads every other repository
	// too, each from a Repository of its own, owned by the run and deleted
	// with its collection like the planning repository's.
	for _, tree := range run.Spec.Trees {
		if err := p.ensureRunRepository(ctx, run, tree.RepositoryRef.Name, tree.URL); err != nil {
			return err
		}
	}
	return nil
}

// ensureRunRepository creates the Repository name at the default branch head
// of url, owned by run, when missing. One that exists is used only when run
// is its controller owner.
func (p *pass) ensureRunRepository(ctx context.Context, run *v1alpha1.IntentRun, name, url string) error {
	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: run.Namespace, Name: name}
	err := p.r.Get(ctx, key, &repo)
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
		Spec: v1alpha1.RepositorySpec{URL: url},
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
	if run.Spec.Stage == v1alpha1.IntentStageRevise {
		return p.reviseInput(ctx, run, raw)
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
