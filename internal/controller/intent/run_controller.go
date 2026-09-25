// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/changeset"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/schedule"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
)

// runSchedulerRequest is the singleton request every run and Job event also
// maps to: one serialized decision point over the slot pool.
const runSchedulerRequest = "\x00intent-scheduler"

// KindIntent is the jobs.Spec kind, and so the run-kind label value, of every
// intent agent Job; the Finding job controllers' watches filter it out.
const KindIntent = "intent"

// Stage priorities within the slot pool: a build ahead of a plan (a revise
// round, slice 1b, ahead of both), each FIFO.
var stagePriority = map[v1alpha1.IntentStage]int32{
	v1alpha1.IntentStagePlan:   1,
	v1alpha1.IntentStageBuild:  2,
	v1alpha1.IntentStageRevise: 3,
}

// jobPhase is the agentrun phase each stage's Job runs.
var jobPhase = map[v1alpha1.IntentStage]string{
	v1alpha1.IntentStagePlan:   "plan",
	v1alpha1.IntentStageBuild:  "build",
	v1alpha1.IntentStageRevise: "build",
}

// irunGVK is the owner TypeMeta a run's transcript carries.
var irunGVK = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "IntentRun"}

// RunReconciler schedules IntentRuns into agent Jobs from its own slot pool,
// separate from remediation's, collects their results, and pushes a build's
// changeset: see the package doc.
type RunReconciler struct {
	client.Client
	// APIReader reads the run, uncached, at the start of every pass.
	APIReader client.Reader
	Jobs      JobRunner
	GitHub    GitHub
	Settings  Settings
	// MaxConcurrent bounds Running runs (default 1).
	MaxConcurrent int
	// Harness runs every intent Job (brokered claude in production, the
	// fake harness in dev); PlanModel and BuildModel are the canonical
	// models of the two stages.
	Harness    string
	PlanModel  string
	BuildModel string
	// Images decides whether a build runs the Repository's pinned image
	// (PinFor), with this controller's own breaker.
	Images runnerguard.Guard
	// MaxChangesetEntries caps a build changeset's entries; <= 0 means
	// changeset.DefaultMaxEntries.
	MaxChangesetEntries int
	Now                 func() time.Time
	Log                 *slog.Logger
}

func (r *RunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *RunReconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Reconcile handles the singleton scheduling request and each run.
func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name == runSchedulerRequest {
		return r.schedule(ctx)
	}
	return r.run(ctx, req)
}

// schedule grants free slots to pending runs that can launch now, FIFO with
// a build ahead of a plan. Slots are counted from the cluster, never memory.
func (r *RunReconciler) schedule(ctx context.Context) (ctrl.Result, error) {
	settings := r.Settings.withDefaults()
	var list v1alpha1.IntentRunList
	if err := r.List(ctx, &list, client.InNamespace(settings.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	running := 0
	var pending []schedule.Candidate
	for i := range list.Items {
		run := &list.Items[i]
		switch run.Status.Phase {
		case v1alpha1.RunRunning:
			if holdsSlot(run) {
				running++
			}
		case v1alpha1.RunPending, "":
			if run.DeletionTimestamp.IsZero() && r.launchable(ctx, run) {
				pending = append(pending, schedule.Candidate{
					Name:     run.Name,
					Priority: stagePriority[run.Spec.Stage],
					QueuedAt: run.CreationTimestamp.Time,
				})
			}
		}
	}
	maxConcurrent := r.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	for _, name := range schedule.Pick(pending, maxConcurrent-running, r.now(), schedule.AgingPolicy{}) {
		granted, err := r.grant(ctx, name, maxConcurrent)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !granted {
			break
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// holdsSlot reports a Running run that occupies a slot of the pool: every
// one but a build whose Job finished while its Intent is suspended
// (PushHeld). The pool bounds the agents running at once, and that one's
// agent is done; what it still owes is its push.
func holdsSlot(run *v1alpha1.IntentRun) bool {
	return !pushHeld(run)
}

// pushHeld reports a build run marked held: its Job finished while its
// Intent was suspended.
func pushHeld(run *v1alpha1.IntentRun) bool {
	return meta.IsStatusConditionTrue(run.Status.Conditions, v1alpha1.ConditionPushHeld)
}

// launchable reports a pending run that could launch now: its input and
// its Repository's artifact ready, its Intent active and not suspended, its
// Project not suspended. A run waiting on any of them holds no slot.
func (r *RunReconciler) launchable(ctx context.Context, run *v1alpha1.IntentRun) bool {
	var in v1alpha1.Intent
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in); err != nil ||
		in.UID != run.Spec.IntentRef.UID || in.Spec.Suspend || terminal(in.Status.Phase) {
		return false
	}
	var proj v1alpha1.Project
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: in.Spec.Project}, &proj); err != nil ||
		proj.Spec.Suspend {
		return false
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, runInputKey(run), &cm); err != nil {
		return false
	}
	var repo v1alpha1.Repository
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name,
	}, &repo); err != nil {
		return false
	}
	if repo.Status.Artifact == nil {
		return false
	}
	return meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) ||
		imageStallIgnored(run, &repo, requireRepositoryImage(&proj))
}

// imageStallIgnored allows a pinned, stored tree through an image-declaration
// stall when the run will use the default image: every plan, and a build whose
// Project explicitly opts out of the repository image.
func imageStallIgnored(run *v1alpha1.IntentRun, repo *v1alpha1.Repository, requireImage bool) bool {
	if (run.Spec.Stage != v1alpha1.IntentStagePlan &&
		(run.Spec.Stage != v1alpha1.IntentStageRevise &&
			(run.Spec.Stage != v1alpha1.IntentStageBuild || requireImage))) ||
		repo.Status.Artifact == nil || repo.Status.ResolvedSHA == "" {
		return false
	}
	c := meta.FindStatusCondition(repo.Status.Conditions, v1alpha1.ConditionStalled)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonRunnerImageRejected
}

// grant moves one run to Running, while a slot is still free as the API
// server counts them: the cache the pass counted from can lag a grant made
// just before (a pass re-queued while the last one granted), and the pool is
// a spend bound, never to be exceeded. slot is false when none is free.
func (r *RunReconciler) grant(ctx context.Context, name string, maxConcurrent int) (slot bool, err error) {
	var list v1alpha1.IntentRunList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(r.Settings.Namespace)); err != nil {
		return false, err
	}
	running := 0
	for i := range list.Items {
		if list.Items[i].Status.Phase == v1alpha1.RunRunning && holdsSlot(&list.Items[i]) {
			running++
		}
	}
	if running >= maxConcurrent {
		return false, nil
	}
	var run v1alpha1.IntentRun
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Settings.Namespace, Name: name}, &run); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	if run.Status.Phase != "" && run.Status.Phase != v1alpha1.RunPending {
		return true, nil
	}
	run.Status.Phase = v1alpha1.RunRunning
	run.Status.ObservedGeneration = run.Generation
	if err := r.Status().Update(ctx, &run); err != nil && !kerrors.IsConflict(err) {
		return true, client.IgnoreNotFound(err)
	}
	return true, nil
}

// run drives one run: finalize, abort, launch or collect.
func (r *RunReconciler) run(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run v1alpha1.IntentRun
	if err := r.APIReader.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &run)
	}
	switch run.Status.Phase {
	case v1alpha1.RunPending, "":
		return ctrl.Result{}, r.pending(ctx, &run)
	case v1alpha1.RunRunning:
		if ended, err := r.intentEnded(ctx, &run); ended || err != nil {
			return ctrl.Result{}, err
		}
		if run.Status.JobRef == nil {
			return ctrl.Result{}, r.launch(ctx, &run)
		}
		return r.collect(ctx, &run)
	case v1alpha1.RunComplete, v1alpha1.RunFailed:
		// settle deletes a plan run's Repository after the terminal write;
		// a delete that failed there is retried here, on every event, so
		// no plan Repository outlives its collection.
		return ctrl.Result{}, r.deletePlanRepository(ctx, &run)
	}
	return ctrl.Result{}, nil
}

// pending fails a pending run that can never launch: its Intent gone or
// ended, or its Repository stalled (a rejected runner image under onReject
// handoff blocks a build on its image and does not stop a plan, which never
// runs it; any other stall aborts the run).
func (r *RunReconciler) pending(ctx context.Context, run *v1alpha1.IntentRun) error {
	if ended, err := r.intentEnded(ctx, run); ended || err != nil {
		return err
	}
	var repo v1alpha1.Repository
	err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name}, &repo)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	stalled := meta.FindStatusCondition(repo.Status.Conditions, v1alpha1.ConditionStalled)
	if stalled == nil || stalled.Status != metav1.ConditionTrue ||
		imageStallIgnored(run, &repo, true) {
		return nil
	}
	if stalled.Reason == v1alpha1.ReasonRunnerImageRejected && run.Spec.Stage == v1alpha1.IntentStageBuild {
		var in v1alpha1.Intent
		if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in); err != nil {
			return err
		}
		var proj v1alpha1.Project
		if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: in.Spec.Project}, &proj); err != nil {
			return err
		}
		if imageStallIgnored(run, &repo, requireRepositoryImage(&proj)) {
			return nil
		}
		return r.settle(ctx, run, result{outcome: OutcomeImageRequired,
			detail: runnerguard.SkipRejected + ": " + stalled.Message})
	}
	return r.settle(ctx, run, result{outcome: OutcomeAborted,
		detail: "the repository artifact stalled: " + stalled.Message})
}

// runInputKey locates the run's input ConfigMap.
func runInputKey(run *v1alpha1.IntentRun) types.NamespacedName {
	return types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.Inputs.ConfigMap}
}

// intentEnded aborts an unfinished run whose Intent is gone, is not the one
// that created it, or has ended: its Job is deleted and nothing it produces
// is used.
func (r *RunReconciler) intentEnded(ctx context.Context, run *v1alpha1.IntentRun) (bool, error) {
	var in v1alpha1.Intent
	err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in)
	if err != nil && !kerrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && in.UID == run.Spec.IntentRef.UID && in.DeletionTimestamp.IsZero() && !terminal(in.Status.Phase) {
		return false, nil
	}
	if run.Status.JobRef != nil {
		if err := r.Jobs.Delete(ctx, run.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
			return true, err
		}
	}
	// A build that pushed its commit, or was held for a suspension, already
	// recorded its report, usage and transcript: they are kept.
	return true, r.settle(ctx, run, result{outcome: OutcomeAborted, detail: "the intent ended before the run did",
		keep: run.Status.PushedCommit != "" || pushHeld(run)})
}

// launch creates the run's agent Job. A plan runs read-only on the default
// image, handed the input snapshot. A build is handed the approved plan
// alone, re-hashed here against the digest the approval bound (a mismatch
// launches nothing), with an empty request, and runs only on an accepted
// repository-declared image when its Project requires one: if none is
// usable, or the Job reports it ran the default image, the Job is deleted
// and the run fails image_required, which blocks its Intent.
func (r *RunReconciler) launch(ctx context.Context, run *v1alpha1.IntentRun) error {
	var in v1alpha1.Intent
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in); err != nil {
		return err
	}
	if in.Spec.Suspend {
		return r.requeuePending(ctx, run)
	}
	var proj v1alpha1.Project
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: in.Spec.Project}, &proj); err != nil {
		return err
	}
	var repo v1alpha1.Repository
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name,
	}, &repo); err != nil {
		return err
	}
	if !controlledBy(repo.OwnerReferences, run.UID) || !sameRepo(repo.Spec.URL, run.Spec.Repository.URL) {
		// Someone else's object under the run's derived name: its SHA,
		// tarball and image are not this run's to use.
		return r.settle(ctx, run, result{outcome: OutcomeAborted,
			detail: fmt.Sprintf("Repository %s is not this run's own (for %s); nothing was launched", repo.Name,
				run.Spec.Repository.URL)})
	}
	if repo.Status.Artifact == nil || repo.Status.ResolvedSHA == "" {
		return r.requeuePending(ctx, run)
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, runInputKey(run), &cm); err != nil {
		return err
	}
	if !controlledBy(cm.OwnerReferences, run.UID) {
		return r.settle(ctx, run, result{outcome: OutcomeAborted,
			detail: fmt.Sprintf("input ConfigMap %s is not this run's own; nothing was launched", cm.Name)})
	}
	imageRepo := &repo
	if stage := run.Spec.Stage; stage == v1alpha1.IntentStageRevise {
		source, refusal, sourceErr := r.reviseSource(ctx, run, &repo)
		if sourceErr != nil {
			return sourceErr
		}
		if refusal != nil {
			return r.settle(ctx, run, *refusal)
		}
		imageRepo = source
	}

	stage := run.Spec.Stage
	spec := jobs.Spec{
		Repo:            repoSlug(run.Spec.Repository.URL),
		Attempt:         int(run.Spec.Attempt),
		Phase:           jobPhase[stage],
		Harness:         r.Harness,
		BaseSHA:         repo.Status.ResolvedSHA,
		IssueMarkdown:   cm.Data[keyIssue],
		Kind:            KindIntent,
		Owner:           run.Name,
		Finding:         run.Name,
		ArtifactURL:     repo.Status.Artifact.URL,
		ArtifactDigest:  repo.Status.Artifact.Digest,
		MaxTurns:        run.Spec.Grant.MaxTurns,
		TokenBudget:     run.Spec.Grant.TokenBudget,
		PreviousAttempt: agentresult.EncodePreviousAttempt(run.Spec.PreviousAttempt),
	}
	requireImage, refusal := r.stageSpec(run, &spec, &cm, &proj, imageRepo)
	if refusal != nil {
		return r.settle(ctx, run, *refusal)
	}
	return r.launchJob(ctx, run, &proj, spec, requireImage)
}

func (r *RunReconciler) launchJob(ctx context.Context, run *v1alpha1.IntentRun,
	proj *v1alpha1.Project, spec jobs.Spec, requireImage bool) error {
	settings := r.Settings.withDefaults()
	stage := run.Spec.Stage
	build := settings.grant(proj, v1alpha1.IntentStageBuild)
	jobName, image, err := r.Jobs.Create(ctx, spec, stageEnv(stage, settings, build))
	switch {
	case launchRefused(err):
		// Retrying would keep a granted slot forever: the run ends, its
		// attempt counted, and the Intent reads why.
		return r.settle(ctx, run, result{outcome: OutcomeLaunchRefused,
			detail: "the API server refused the agent Job: " + err.Error()})
	case err != nil:
		return fmt.Errorf("launch run %s: %w", run.Name, err)
	}
	if requireImage && image.Source != v1alpha1.RunnerImageSourceRepository {
		if err := r.Jobs.Delete(ctx, jobName); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s: %w", jobName, err)
		}
		return r.settle(ctx, run, result{outcome: OutcomeImageRequired,
			detail: "the Job ran the default image, " + image.Image + ", and a build requires the repository's own"})
	}
	return r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		now := metav1.NewTime(r.now())
		cur.Status.JobRef = &v1alpha1.JobReference{Namespace: settings.AgentNamespace, Name: jobName}
		cur.Status.RunnerImage = &image
		cur.Status.BaseSHA = spec.BaseSHA
		cur.Status.StartedAt = &now
	})
}

// reviseSource rechecks the exact PR branch pin and the build round's image
// before any agent Job is launched. It never falls back to the default image.
func (r *RunReconciler) reviseSource(ctx context.Context, run *v1alpha1.IntentRun,
	repo *v1alpha1.Repository) (*v1alpha1.Repository, *result, error) {
	branch := branchName(run.Spec.IntentRef.Name)
	if repo.Spec.Ref.Branch != branch {
		return nil, &result{outcome: OutcomeAborted,
			detail: "revise Repository does not point at this intent's PR branch; nothing was launched"}, nil
	}
	head, err := r.GitHub.HeadSHA(ctx, run.Spec.Repository.URL, branch)
	if ghclient.IsNotFound(err) {
		return nil, &result{outcome: OutcomeHeadMoved,
			detail: "intent PR branch was deleted before the revise launch"}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read intent PR head before revise launch: %w", err)
	}
	if head != repo.Status.ResolvedSHA {
		return nil, &result{outcome: OutcomeHeadMoved,
			detail: fmt.Sprintf("intent PR head moved from pinned %s to %s before launch", repo.Status.ResolvedSHA, head)}, nil
	}
	if run.Spec.ImageFrom == nil {
		return nil, &result{outcome: OutcomeImageRequired,
			detail: "revise run has no build-round image source"}, nil
	}
	var pinned v1alpha1.Repository
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace,
		Name: run.Spec.ImageFrom.Name}, &pinned); err != nil {
		return nil, nil, fmt.Errorf("read revise run's build-round image source: %w", err)
	}
	if pinned.UID != run.Spec.ImageFrom.UID || pinned.Status.RunnerImage == nil {
		return nil, &result{outcome: OutcomeImageRequired,
			detail: "revise run's build-round image pin is absent or changed"}, nil
	}
	return &pinned, nil, nil
}

// launchRefused reports a Job create the API server refused for itself: a
// 4xx other than 401, 408, 409 and 429 (an admission policy or webhook
// denying the Job, an invalid Job, a missing namespace), which repeating
// unchanged gets again. A conflict, throttling, a timeout, a server error and
// a failure that never reached the API server are retried.
func launchRefused(err error) bool {
	var status kerrors.APIStatus
	if err == nil || !errors.As(err, &status) {
		return false
	}
	switch code := status.Status().Code; code {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	default:
		return code >= 400 && code < 500
	}
}

// stageSpec completes spec for the run's stage from its input. A plan's
// request is re-hashed against the snapshot the run was created for; a
// build's plan against the approved digest, its request must be empty, and
// it runs the repository's pinned image, which its Project may require.
// refusal is how the run ends without launching, nil to launch.
func (r *RunReconciler) stageSpec(run *v1alpha1.IntentRun, spec *jobs.Spec, cm *corev1.ConfigMap,
	proj *v1alpha1.Project, repo *v1alpha1.Repository) (requireImage bool, refusal *result) {
	switch run.Spec.Stage {
	case v1alpha1.IntentStagePlan:
		if got := digest([]byte(spec.IssueMarkdown)); got != run.Spec.Inputs.InputDigest {
			return false, &result{outcome: OutcomeAborted,
				detail: fmt.Sprintf("the request's bytes hash to %s, not the snapshot's %s; nothing was launched",
					got, run.Spec.Inputs.InputDigest)}
		}
		spec.Model = r.PlanModel
		return false, nil
	case v1alpha1.IntentStageBuild:
		spec.Model = r.BuildModel
		plan := cm.Data[keyInvestigation]
		if got := digest([]byte(plan)); got != run.Spec.Inputs.PlanDigest {
			return false, &result{outcome: OutcomeAborted,
				detail: fmt.Sprintf("the approved plan's bytes hash to %s, not the approved %s; nothing was launched",
					got, run.Spec.Inputs.PlanDigest)}
		}
		if spec.IssueMarkdown != "" {
			return false, &result{outcome: OutcomeAborted,
				detail: "a build is handed the approved plan alone, and its request was not empty"}
		}
		spec.InvestigationMarkdown = plan
		requireImage = requireRepositoryImage(proj)
		if skipped := r.Images.PinFor(spec, repo); skipped != "" && requireImage {
			return true, &result{outcome: OutcomeImageRequired, detail: skipped}
		}
		return requireImage, nil
	case v1alpha1.IntentStageRevise:
		spec.Model = r.BuildModel
		if reason := cm.Data[keyInputRefusal]; reason != "" {
			return true, &result{outcome: OutcomeInputUnavailable, detail: reason}
		}
		approved := cm.Data[keyApprovedPlan]
		if got := digest([]byte(approved)); got != run.Spec.Inputs.PlanDigest ||
			!strings.HasPrefix(cm.Data[keyInvestigation], approved) || spec.IssueMarkdown != "" {
			return true, &result{outcome: OutcomeAborted,
				detail: "revise handoff does not begin with the exact approved plan, or request is not empty"}
		}
		if _, err := report.ParsePlanInput([]byte(cm.Data[keyInvestigation])); err != nil {
			return true, &result{outcome: OutcomeAborted,
				detail: "revise handoff holds text that could not be shown: " + err.Error()}
		}
		spec.InvestigationMarkdown = cm.Data[keyInvestigation]
		if skipped := r.Images.PinFor(spec, repo); skipped != "" {
			return true, &result{outcome: OutcomeImageRequired, detail: skipped}
		}
		return true, nil
	}
	return false, &result{outcome: OutcomeAborted,
		detail: fmt.Sprintf("stage %q is not run by this controller", run.Spec.Stage)}
}

// requeuePending hands a granted run's slot back when it cannot launch yet.
func (r *RunReconciler) requeuePending(ctx context.Context, run *v1alpha1.IntentRun) error {
	return r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) { cur.Status.Phase = v1alpha1.RunPending })
}

// collect settles a launched run once its Job has finished.
func (r *RunReconciler) collect(ctx context.Context, run *v1alpha1.IntentRun) (ctrl.Result, error) {
	if run.Status.PushedCommit != "" {
		// The commit was created and recorded; only the branch may be owed.
		if run.Spec.Stage == v1alpha1.IntentStageRevise {
			return r.heldOr(r.advanceBranch(ctx, run))
		}
		return r.heldOr(r.createBranch(ctx, run))
	}
	if pushHeld(run) {
		// Held for a suspension: its Job is read again only once the
		// suspension is lifted, so a held build costs one uncached read per
		// interval rather than its whole log and transcript.
		switch err := r.pushGate(ctx, run); {
		case errors.Is(err, errHeld):
			return r.heldOr(err)
		case errors.Is(err, errIntentEnded):
			return ctrl.Result{}, r.endedBeforePush(ctx, run, result{keep: true})
		case err != nil:
			return ctrl.Result{}, err
		}
	}
	st, err := r.Jobs.Status(ctx, run.Status.JobRef.Name)
	if kerrors.IsNotFound(err) {
		if pushHeld(run) {
			return ctrl.Result{}, r.settle(ctx, run, result{outcome: OutcomeHoldExpired, keep: true,
				detail: "the build finished while its intent was suspended, and its Job expired before the " +
					"suspension was lifted, taking the unpushed changeset with it; the attempt does not count"})
		}
		return ctrl.Result{}, r.settle(ctx, run, result{outcome: OutcomeAborted,
			detail: "agent job vanished before reporting"})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if runnerguard.SandboxRefused(st) {
		r.Images.Breaker.Trip(ctx, run.Status.JobRef.Name, run.Name)
		return ctrl.Result{}, r.settle(ctx, run, result{outcome: OutcomeAborted, detail: runnerguard.SandboxReason,
			sandboxRefused: true})
	}
	if !st.Done {
		requeue, pullFailure := runnerguard.Pending(st, r.now())
		if pullFailure != "" {
			if err := r.Jobs.Delete(ctx, run.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
				r.log().LogAttrs(ctx, slog.LevelWarn, "delete agent job after pull failure",
					slog.String("job", run.Status.JobRef.Name), slog.Any("error", err))
			}
			return ctrl.Result{}, r.settle(ctx, run, result{outcome: OutcomeAborted, detail: pullFailure})
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	out, err := r.Jobs.Result(ctx, run.Status.JobRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Persisted before anything else: the Job's TTL takes the log with it,
	// and losing the explanation must never cost the result it explains.
	transcript, err := transcriptstore.Persist(ctx, r.Client, run.Namespace, map[string]string{
		v1alpha1.LabelIntent:  run.Spec.IntentRef.Name,
		v1alpha1.LabelOwner:   run.Name,
		v1alpha1.LabelRunKind: KindIntent,
		v1alpha1.LabelAttempt: strconv.Itoa(int(run.Spec.Attempt)),
	}, run, irunGVK, out.Turns)
	if err != nil {
		r.log().LogAttrs(ctx, slog.LevelError, "persist transcript",
			slog.String("run", run.Name), slog.Any("error", err))
	}
	if run.Spec.Stage == v1alpha1.IntentStagePlan {
		return ctrl.Result{}, r.collectPlan(ctx, run, out.Events, transcript)
	}
	return r.heldOr(r.collectBuild(ctx, run, out.Events, transcript))
}

// heldOr is the result of a collect that ended with err: a build held for a
// suspension is looked at again after a poll interval (and at once when the
// suspension is lifted, through the Intent watch).
func (r *RunReconciler) heldOr(err error) (ctrl.Result, error) {
	if errors.Is(err, errHeld) {
		return ctrl.Result{RequeueAfter: r.Settings.withDefaults().PollInterval}, nil
	}
	return ctrl.Result{}, err
}

// errHeld: the run's Intent is suspended, and nothing is written to GitHub
// for a suspended intent. The finished build waits, its push not made, and
// is collected again when the suspension is cleared.
var errHeld = errors.New("the intent is suspended; its push waits")

// errIntentEnded: the run's Intent, read uncached, is gone, is another
// Intent under its name, is being deleted, or has ended (a cancel, a human
// close). Nothing more of the build reaches GitHub.
var errIntentEnded = errors.New("the intent ended before the build's push")

// pushGate reads the run's Intent uncached before each write the push makes
// to GitHub (the commit, then the branch), which are the only writes the run
// reconciler makes: the cache it decided to collect from can lag a cancel or
// a suspension written a moment ago. It is errIntentEnded when the Intent no
// longer wants the build, errHeld while it is suspended, and nil to push.
func (r *RunReconciler) pushGate(ctx context.Context, run *v1alpha1.IntentRun) error {
	var in v1alpha1.Intent
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in)
	switch {
	case kerrors.IsNotFound(err):
		return errIntentEnded
	case err != nil:
		return err
	case in.UID != run.Spec.IntentRef.UID || !in.DeletionTimestamp.IsZero() || terminal(in.Status.Phase):
		return errIntentEnded
	case in.Spec.Suspend:
		return errHeld
	}
	if run.Spec.Stage == v1alpha1.IntentStageRevise {
		if in.Status.Phase != v1alpha1.IntentRevising || len(in.Status.PullRequests) != 1 {
			return errIntentEnded
		}
		pr := in.Status.PullRequests[0]
		live, err := r.GitHub.GetPullRequest(ctx, pr.Repository, pr.Number)
		if err != nil {
			return fmt.Errorf("verify revise PR before push: %w", err)
		}
		if live.State != prOpen || live.Merged || pr.NodeID != "" && live.NodeID != pr.NodeID {
			return errIntentEnded
		}
	}
	return nil
}

// endedBeforePush settles a build whose Intent ended before its push was
// complete: aborted, keeping what the build reported, with its Job deleted
// and nothing (more) written to GitHub. A commit already created is left
// dangling, with no branch at it.
func (r *RunReconciler) endedBeforePush(ctx context.Context, run *v1alpha1.IntentRun, res result) error {
	if run.Status.JobRef != nil {
		if err := r.Jobs.Delete(ctx, run.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s: %w", run.Status.JobRef.Name, err)
		}
	}
	res.outcome, res.complete = OutcomeAborted, false
	res.detail = "the intent ended before the build's push; nothing was pushed"
	if run.Status.PushedCommit != "" {
		res.detail = "the intent ended before the build's branch was created; commit " + run.Status.PushedCommit +
			" was made, and no branch points at it"
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "intent ended; the build's push is abandoned", slog.String("run", run.Name))
	return r.settle(ctx, run, res)
}

// hold marks a build whose push waits on a suspension PushHeld, once,
// recording what its push would record (the report, usage and transcript;
// res is nil once the commit recorded them), and returns errHeld. The mark
// frees its slot and keeps later passes from reading its Job until the
// suspension is lifted; the run stays Running. A suspension that outlasts the
// Job's TTL loses the Job, and with it the unpushed changeset: the run then
// ends hold_expired, which does not count as an attempt.
func (r *RunReconciler) hold(ctx context.Context, run *v1alpha1.IntentRun, res *result) error {
	if pushHeld(run) {
		return errHeld
	}
	if err := r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionPushHeld, Status: metav1.ConditionTrue, Reason: "IntentSuspended",
			Message: "the build finished while its intent is suspended; " +
				"its push waits for the suspension to be lifted",
			ObservedGeneration: cur.Generation,
		})
		if res != nil {
			cur.Status.Report = agentresult.TruncateReport(res.report)
			if res.transcript != nil {
				cur.Status.Transcript = res.transcript
			}
			if res.stage != nil {
				cur.Status.Usage = podUsage(res.stage)
			}
		}
	}); err != nil {
		return err
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "intent suspended; the build's push waits", slog.String("run", run.Name))
	return errHeld
}

// podOutcomes are the outcomes an agent pod may report for a stage that did
// not succeed. The envelope is the pod's stdout, and a build runs in the
// repository's own image, so what it reports is untrusted: it may not claim
// an outcome the controller decides by (image_required makes an attempt
// uncounted and blocks the Intent; aborted, branch_exists and the rest are
// the controller's own), nor one the run's status would refuse.
var podOutcomes = map[envelope.Outcome]bool{
	envelope.OutcomeRuntimeError:      true,
	envelope.OutcomeTimeout:           true,
	envelope.OutcomeBudgetExceeded:    true,
	envelope.OutcomeReportMissing:     true,
	envelope.OutcomeReportInvalid:     true,
	envelope.OutcomeCommitFailed:      true,
	envelope.OutcomeChangesetTooLarge: true,
	envelope.OutcomeImageIncompatible: true,
}

// podOutcome is the outcome and detail a run records for a stage the pod
// reports as not succeeding: the pod's own when it is one a pod may report,
// else runtime_error, with what the pod claimed quoted in the detail.
func podOutcome(o envelope.Outcome, detail string) (string, string) {
	if podOutcomes[o] {
		return string(o), detail
	}
	return string(envelope.OutcomeRuntimeError),
		fmt.Sprintf("the agent reported the outcome %.64q, which a pod may not report: %s", string(o), detail)
}

// result is how a run ended, as the run records it.
type result struct {
	outcome        string
	detail         string
	report         string
	stage          *envelope.Stage
	transcript     *v1alpha1.TranscriptRef
	sandboxRefused bool
	complete       bool
	// keep leaves the report, usage and transcript a push recorded with its
	// commit.
	keep bool
}

// collectPlan settles a plan run from its plan event. The plan is re-derived
// from its report (agentresult.FromPlan), and one naming a repository
// outside the Project is invalid.
func (r *RunReconciler) collectPlan(ctx context.Context, run *v1alpha1.IntentRun, events []envelope.Event,
	transcript *v1alpha1.TranscriptRef) error {
	var ev *envelope.Plan
	for _, e := range events {
		switch e.Type {
		case envelope.TypePlan:
			ev = e.Plan
		case envelope.TypeFatal:
			return r.settle(ctx, run, result{outcome: OutcomeAborted, detail: e.Error, transcript: transcript})
		}
	}
	if ev == nil {
		return r.settle(ctx, run, result{outcome: OutcomeAborted, detail: "agent job produced no plan event",
			transcript: transcript})
	}
	res := result{stage: &ev.Stage, transcript: transcript}
	plan, err := agentresult.FromPlan(ev)
	switch {
	case err != nil:
		res.outcome, res.detail = string(envelope.OutcomeReportInvalid), err.Error()
	case ev.Outcome != envelope.OutcomeOK:
		res.outcome, res.detail = podOutcome(ev.Outcome, ev.Detail)
	default:
		if bad := r.outsideProject(ctx, run, plan.Repositories); bad != "" {
			res.outcome = string(envelope.OutcomeReportInvalid)
			res.detail = "the plan names a repository outside the project: " + bad
			break
		}
		res.outcome, res.complete, res.report = string(envelope.OutcomeOK), true, plan.Report
	}
	return r.settle(ctx, run, res)
}

// outsideProject is the first of repos not among the run's Project's
// repositories, or "".
func (r *RunReconciler) outsideProject(ctx context.Context, run *v1alpha1.IntentRun, repos []string) string {
	var in v1alpha1.Intent
	var proj v1alpha1.Project
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in); err != nil {
		return "(the intent is gone)"
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: in.Spec.Project}, &proj); err != nil {
		return "(the project is gone)"
	}
	for _, u := range repos {
		found := false
		for _, pr := range proj.Spec.Repositories {
			if sameRepo(u, pr.URL) {
				found = true
			}
		}
		if !found {
			return u
		}
	}
	return ""
}

// collectBuild settles a build run from its remediation event: a build the
// agent reports unbuilt fails; a changeset is validated against the intent
// rules (the pinned base, path shape, and never .github, .patchy or
// .devcontainer) before any forge call, then pushed in two phases.
func (r *RunReconciler) collectBuild(ctx context.Context, run *v1alpha1.IntentRun, events []envelope.Event,
	transcript *v1alpha1.TranscriptRef) error {
	var ev *envelope.Remediation
	for _, e := range events {
		switch e.Type {
		case envelope.TypeRemediation:
			ev = e.Remediation
		case envelope.TypeFatal:
			return r.settle(ctx, run, result{outcome: OutcomeAborted, detail: e.Error, transcript: transcript})
		}
	}
	if ev == nil {
		return r.settle(ctx, run, result{outcome: OutcomeAborted, detail: "agent job produced no build event",
			transcript: transcript})
	}
	res := result{stage: &ev.Stage, transcript: transcript, report: ev.ReportMarkdown}
	switch {
	case ev.Outcome != envelope.OutcomeOK:
		res.outcome, res.detail = podOutcome(ev.Outcome, ev.Detail)
		return r.settle(ctx, run, res)
	case !ev.Success:
		res.outcome, res.detail = OutcomeNotBuilt, "the build agent reported the plan not built"
		if b, err := report.ParseBuild([]byte(ev.ReportMarkdown)); err == nil && b.Reason != "" {
			res.detail += ": " + b.Reason
		}
		return r.settle(ctx, run, res)
	case ev.Changeset == nil:
		res.outcome, res.detail = OutcomeAborted, "the build reported success without a changeset"
		return r.settle(ctx, run, res)
	}
	rules := changeset.IntentRules(run.Status.BaseSHA, r.MaxChangesetEntries)
	if err := changeset.Validate(ev.Changeset, rules); err != nil {
		res.outcome, res.detail = string(envelope.OutcomeChangesetRejected), err.Error()
		return r.settle(ctx, run, res)
	}
	return r.push(ctx, run, ev, res)
}

// push creates the build's commit (the controller composes its message; the
// agent's are dropped), records it on the run before any ref moves, then
// creates the intent branch at it.
func (r *RunReconciler) push(ctx context.Context, run *v1alpha1.IntentRun, ev *envelope.Remediation, res result) error {
	switch err := r.pushGate(ctx, run); {
	case errors.Is(err, errIntentEnded):
		return r.endedBeforePush(ctx, run, res)
	case errors.Is(err, errHeld):
		return r.hold(ctx, run, &res)
	case err != nil:
		return err
	}
	var in v1alpha1.Intent
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.IntentRef.Name}, &in); err != nil {
		return err
	}
	summary := ""
	if in.Status.Plan != nil {
		summary = in.Status.Plan.Summary
	}
	req := ghclient.CommitRequest{
		BaseSHA: ev.Changeset.BaseSHA,
		Message: templates.IntentCommitMessage(templates.IntentCommit{
			Project: in.Spec.Project, Summary: summary, IntentRepository: repoSlug(in.Spec.Issue.Repository),
			IssueNumber: in.Spec.Issue.Number, Round: run.Spec.Round, Namespace: in.Namespace, Intent: in.Name,
			Run: run.Name,
		}),
		Deletes: ev.Changeset.Deletes,
	}
	for _, up := range ev.Changeset.Upserts {
		content, err := base64.StdEncoding.DecodeString(up.ContentB64)
		if err != nil {
			res.outcome, res.detail = string(envelope.OutcomeChangesetRejected),
				fmt.Sprintf("changeset path %q content is not base64: %v", up.Path, err)
			return r.settle(ctx, run, res)
		}
		req.Files = append(req.Files, ghclient.CommitFile{Path: up.Path, Mode: up.Mode, Content: content})
	}
	commit, err := r.GitHub.CreateCommit(ctx, run.Spec.Repository.URL, req)
	switch {
	case ghclient.IsRefused(err):
		// The same commit would be refused again: the run ends rather than
		// hold its slot retrying it.
		res.outcome, res.detail = OutcomePushRefused, "GitHub refused the build's commit: "+err.Error()
		return r.settle(ctx, run, res)
	case err != nil:
		return fmt.Errorf("create the commit: %w", err)
	}
	if err := r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		cur.Status.PushedCommit = commit
		cur.Status.Report = agentresult.TruncateReport(res.report)
		cur.Status.Transcript = res.transcript
		cur.Status.Usage = podUsage(res.stage)
	}); err != nil {
		return err
	}
	if run.Spec.Stage == v1alpha1.IntentStageRevise {
		return r.advanceBranch(ctx, run)
	}
	return r.createBranch(ctx, run)
}

// advanceBranch moves an intent PR head only when the current branch still
// points at the SHA the revise run cloned. GitHub's update is non-forcing,
// so a concurrent human push can never be overwritten.
func (r *RunReconciler) advanceBranch(ctx context.Context, run *v1alpha1.IntentRun) error {
	switch err := r.pushGate(ctx, run); {
	case errors.Is(err, errIntentEnded):
		return r.endedBeforePush(ctx, run, result{keep: true})
	case errors.Is(err, errHeld):
		return r.hold(ctx, run, nil)
	case err != nil:
		return err
	}
	branch := branchName(run.Spec.IntentRef.Name)
	head, err := r.GitHub.HeadSHA(ctx, run.Spec.Repository.URL, branch)
	if ghclient.IsNotFound(err) {
		return r.settle(ctx, run, result{outcome: OutcomeHeadMoved, keep: true,
			detail: "intent PR branch was deleted before the revise fast-forward"})
	}
	if ghclient.IsRefused(err) {
		return r.settle(ctx, run, result{outcome: OutcomePushRefused, keep: true,
			detail: "GitHub refused to read the intent PR head before fast-forward: " + err.Error()})
	}
	if err != nil {
		return fmt.Errorf("read intent PR head before advancing: %w", err)
	}
	if head != run.Status.BaseSHA {
		return r.settle(ctx, run, result{outcome: OutcomeHeadMoved, keep: true,
			detail: fmt.Sprintf("intent PR head moved from %s to %s; the stale revise result was not pushed",
				run.Status.BaseSHA, head)})
	}
	err = r.GitHub.FastForwardRef(ctx, run.Spec.Repository.URL, branch, run.Status.PushedCommit)
	switch {
	case errors.Is(err, ghclient.ErrRefNotFound):
		return r.settle(ctx, run, result{outcome: OutcomeHeadMoved, keep: true,
			detail: "intent PR branch was deleted during the fast-forward"})
	case errors.Is(err, ghclient.ErrNotFastForward):
		return r.settle(ctx, run, result{outcome: OutcomeHeadMoved, keep: true,
			detail: "intent PR head moved during the fast-forward; the stale revise result was not pushed"})
	case ghclient.IsRefused(err):
		return r.settle(ctx, run, result{outcome: OutcomePushRefused, keep: true,
			detail: "GitHub refused the intent PR fast-forward: " + err.Error()})
	case err != nil:
		return fmt.Errorf("fast-forward intent PR head: %w", err)
	}
	return r.settle(ctx, run, result{outcome: string(envelope.OutcomeOK), complete: true, keep: true})
}

// createBranch creates patchy-intent/<intent> at the recorded commit,
// create-only: an existing branch is adopted only when it already points at
// that commit (a retry), and is otherwise branch_exists. Nothing is forced.
func (r *RunReconciler) createBranch(ctx context.Context, run *v1alpha1.IntentRun) error {
	switch err := r.pushGate(ctx, run); {
	case errors.Is(err, errIntentEnded):
		return r.endedBeforePush(ctx, run, result{keep: true})
	case errors.Is(err, errHeld):
		return r.hold(ctx, run, nil)
	case err != nil:
		return err
	}
	branch := branchName(run.Spec.IntentRef.Name)
	err := r.GitHub.CreateBranchRef(ctx, run.Spec.Repository.URL, branch, run.Status.PushedCommit)
	switch {
	case errors.Is(err, ghclient.ErrBranchExists):
		return r.settle(ctx, run, result{outcome: OutcomeBranchExists, detail: err.Error(), keep: true})
	case ghclient.IsRefused(err):
		// A ruleset restricting ref creation, a permission the App lost:
		// the same branch would be refused again, and the Job the run could
		// otherwise time out on is no longer read once the commit is made.
		return r.settle(ctx, run, result{outcome: OutcomePushRefused, keep: true,
			detail: "GitHub refused the intent branch: " + err.Error()})
	case err != nil:
		return fmt.Errorf("create the branch: %w", err)
	}
	return r.settle(ctx, run, result{outcome: string(envelope.OutcomeOK), complete: true, keep: true})
}
