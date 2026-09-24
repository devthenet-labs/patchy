// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
	v1alpha1.IntentStagePlan:  "plan",
	v1alpha1.IntentStageBuild: "build",
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
			running++
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
		if err := r.grant(ctx, name); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
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
	return meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) && repo.Status.Artifact != nil
}

// grant moves one run to Running.
func (r *RunReconciler) grant(ctx context.Context, name string) error {
	var run v1alpha1.IntentRun
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Settings.Namespace, Name: name}, &run); err != nil {
		return client.IgnoreNotFound(err)
	}
	if run.Status.Phase != "" && run.Status.Phase != v1alpha1.RunPending {
		return nil
	}
	run.Status.Phase = v1alpha1.RunRunning
	run.Status.ObservedGeneration = run.Generation
	if err := r.Status().Update(ctx, &run); err != nil && !kerrors.IsConflict(err) {
		return client.IgnoreNotFound(err)
	}
	return nil
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
	}
	return ctrl.Result{}, nil
}

// pending fails a pending run that can never launch: its Intent gone or
// ended, or its Repository stalled (a rejected runner image under onReject
// handoff blocks a build on its image; any other stall aborts the run).
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
	if stalled == nil || stalled.Status != metav1.ConditionTrue {
		return nil
	}
	if stalled.Reason == v1alpha1.ReasonRunnerImageRejected && run.Spec.Stage == v1alpha1.IntentStageBuild {
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
	return true, r.settle(ctx, run, result{outcome: OutcomeAborted, detail: "the intent ended before the run did"})
}

// launch creates the run's agent Job. A plan runs read-only on the default
// image, handed the input snapshot. A build is handed the approved plan
// alone, re-hashed here against the digest the approval bound (a mismatch
// launches nothing), with an empty request, and runs only on an accepted
// repository-declared image when its Project requires one: if none is
// usable, or the Job reports it ran the default image, the Job is deleted
// and the run fails image_required, which blocks its Intent.
func (r *RunReconciler) launch(ctx context.Context, run *v1alpha1.IntentRun) error {
	settings := r.Settings.withDefaults()
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
	if repo.Status.Artifact == nil || repo.Status.ResolvedSHA == "" {
		return r.requeuePending(ctx, run)
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, runInputKey(run), &cm); err != nil {
		return err
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
	requireImage := false
	switch stage {
	case v1alpha1.IntentStagePlan:
		spec.Model = r.PlanModel
	case v1alpha1.IntentStageBuild:
		spec.Model = r.BuildModel
		plan := cm.Data[keyInvestigation]
		if got := digest([]byte(plan)); got != run.Spec.Inputs.PlanDigest {
			return r.settle(ctx, run, result{outcome: OutcomeAborted,
				detail: fmt.Sprintf("the approved plan's bytes hash to %s, not the approved %s; nothing was launched",
					got, run.Spec.Inputs.PlanDigest)})
		}
		if spec.IssueMarkdown != "" {
			return r.settle(ctx, run, result{outcome: OutcomeAborted,
				detail: "a build is handed the approved plan alone, and its request was not empty"})
		}
		spec.InvestigationMarkdown = plan
		requireImage = requireRepositoryImage(&proj)
		if skipped := r.Images.PinFor(&spec, &repo); skipped != "" && requireImage {
			return r.settle(ctx, run, result{outcome: OutcomeImageRequired, detail: skipped})
		}
	default:
		return r.settle(ctx, run, result{outcome: OutcomeAborted,
			detail: fmt.Sprintf("stage %q is not run by this controller", stage)})
	}
	build := settings.grant(&proj, v1alpha1.IntentStageBuild)
	jobName, image, err := r.Jobs.Create(ctx, spec, stageEnv(stage, settings, build))
	if err != nil {
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
		cur.Status.BaseSHA = repo.Status.ResolvedSHA
		cur.Status.StartedAt = &now
	})
}

// requeuePending hands a granted run's slot back when it cannot launch yet.
func (r *RunReconciler) requeuePending(ctx context.Context, run *v1alpha1.IntentRun) error {
	return r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) { cur.Status.Phase = v1alpha1.RunPending })
}

// collect settles a launched run once its Job has finished.
func (r *RunReconciler) collect(ctx context.Context, run *v1alpha1.IntentRun) (ctrl.Result, error) {
	if run.Status.PushedCommit != "" {
		// The commit was created and recorded; only the branch may be owed.
		return ctrl.Result{}, r.createBranch(ctx, run)
	}
	st, err := r.Jobs.Status(ctx, run.Status.JobRef.Name)
	if kerrors.IsNotFound(err) {
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
	return ctrl.Result{}, r.collectBuild(ctx, run, out.Events, transcript)
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
		res.outcome, res.detail = string(ev.Outcome), ev.Detail
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
		res.outcome, res.detail = string(ev.Outcome), ev.Detail
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
	if err != nil {
		return fmt.Errorf("create the commit: %w", err)
	}
	if err := r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		cur.Status.PushedCommit = commit
		cur.Status.Report = agentresult.TruncateReport(res.report)
		cur.Status.Transcript = res.transcript
		cur.Status.Usage = agentresult.FromStage(res.stage).Usage
	}); err != nil {
		return err
	}
	return r.createBranch(ctx, run)
}

// createBranch creates patchy-intent/<intent> at the recorded commit,
// create-only: an existing branch is adopted only when it already points at
// that commit (a retry), and is otherwise branch_exists. Nothing is forced.
func (r *RunReconciler) createBranch(ctx context.Context, run *v1alpha1.IntentRun) error {
	branch := branchName(run.Spec.IntentRef.Name)
	err := r.GitHub.CreateBranchRef(ctx, run.Spec.Repository.URL, branch, run.Status.PushedCommit)
	switch {
	case errors.Is(err, ghclient.ErrBranchExists):
		return r.settle(ctx, run, result{outcome: OutcomeBranchExists, detail: err.Error(), keep: true})
	case err != nil:
		return fmt.Errorf("create the branch: %w", err)
	}
	return r.settle(ctx, run, result{outcome: string(envelope.OutcomeOK), complete: true, keep: true})
}
