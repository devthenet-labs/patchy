// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/priority"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/schedule"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
)

// InvestigationPhaseIndex indexes Investigations by status.phase for the
// scheduler's slot accounting.
const InvestigationPhaseIndex = "status.phase"

// schedulerRequest is the singleton request every watch event maps to — one
// serialized decision point, MaxConcurrentReconciles=1 provides the mutex.
const schedulerRequest = "\x00scheduler"

// defaultCalibrationTimeout bounds the advisory rollup read in calibration.
// Generous for a cache hit, short enough that a cache that will never sync
// costs the launch a moment rather than the controller its only worker.
const defaultCalibrationTimeout = 5 * time.Second

// Runner is the slice of the jobs client this controller needs. Create
// also returns the runner image the Job actually runs, which the launch
// records beside the JobRef and verdict routing later reads.
type Runner interface {
	Create(ctx context.Context, spec jobs.Spec) (string, v1alpha1.RunnerImageRef, error)
	Result(ctx context.Context, jobName string) (jobs.RunOutput, error)
	Status(ctx context.Context, jobName string) (jobs.Status, error)
	Delete(ctx context.Context, jobName string) error
}

// InvestigationReconciler grants bounded slots to pending investigations
// (severity-priority order), launches the agent Job, and applies the result
// when the Job completes.
type InvestigationReconciler struct {
	client.Client
	// Runner creates and observes agent Jobs (in the agents namespace).
	Runner Runner
	// Namespace the CRs live in.
	Namespace string
	// MaxConcurrent bounds simultaneously running investigations.
	MaxConcurrent int
	// MaxAttempts bounds analysis attempts per finding before Failed.
	MaxAttempts int32
	// ConfidenceThreshold gates automated remediation queueing.
	ConfidenceThreshold float64
	// Aging lifts long-waiting investigations.
	Aging schedule.AgingPolicy
	// InvestigateHarness/InvestigateModel are the harness id and canonical
	// model the analysis stage runs on, resolved at startup; the Job carries
	// them so its pod runs the harness its runner image was built for.
	InvestigateHarness string
	InvestigateModel   string
	// Images decides whether a launch runs the Repository's pinned
	// repository-declared image (the --repository-images kill switch and
	// the sandbox breaker); the zero value never does.
	Images runnerguard.Guard
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// calibrationTimeout is the deadline seam for the advisory rollup read;
	// zero means defaultCalibrationTimeout.
	calibrationTimeout time.Duration
	// Log receives diagnostics; nil discards.
	Log *slog.Logger
}

// Reconcile handles both the singleton scheduling request and per-object
// runs.
func (r *InvestigationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name == schedulerRequest {
		return r.schedule(ctx)
	}
	return r.run(ctx, req)
}

// schedule grants free slots to the highest-priority pending
// investigations. Slot accounting comes from the cluster, never memory.
func (r *InvestigationReconciler) schedule(ctx context.Context) (ctrl.Result, error) {
	var list v1alpha1.InvestigationList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	running := 0
	var pending []schedule.Candidate
	for i := range list.Items {
		inv := &list.Items[i]
		switch inv.Status.Phase {
		case v1alpha1.RunRunning:
			running++
		case v1alpha1.RunPending, "":
			if !inv.DeletionTimestamp.IsZero() {
				continue
			}
			sev := v1alpha1.Level(inv.Labels[v1alpha1.LabelSeverity])
			pending = append(pending, schedule.Candidate{
				Name:      inv.Name,
				Priority:  priority.Score(sev, "", "", "", priority.DefaultWeights),
				QueuedAt:  inv.CreationTimestamp.Time,
				Expedited: r.expedited(ctx, inv.Namespace, inv.Spec.FindingRef.Name),
			})
		}
	}
	maxConcurrent := r.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 3
	}
	for _, name := range schedule.Pick(pending, maxConcurrent-running, r.now(), r.Aging) {
		if err := r.grant(ctx, name); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Safety tick: re-inspect even if an event is lost.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// expedited reads the parent finding's expedite mark (cached client; a miss
// simply ranks the run normally).
func (r *InvestigationReconciler) expedited(ctx context.Context, namespace, finding string) bool {
	if finding == "" {
		return false
	}
	var fnd v1alpha1.Finding
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: finding}, &fnd); err != nil {
		return false
	}
	return fnd.Spec.Expedite != nil
}

// grant moves one investigation to Running (idempotent).
func (r *InvestigationReconciler) grant(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var inv v1alpha1.Investigation
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &inv); err != nil {
			return client.IgnoreNotFound(err)
		}
		if inv.Status.Phase != "" && inv.Status.Phase != v1alpha1.RunPending {
			return nil
		}
		inv.Status.Phase = v1alpha1.RunRunning
		return r.Status().Update(ctx, &inv)
	})
}

// run drives one investigation: launch its Job when granted, apply its
// result when the Job finishes, clean up on deletion.
func (r *InvestigationReconciler) run(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var inv v1alpha1.Investigation
	if err := r.Get(ctx, req.NamespacedName, &inv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !inv.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &inv)
	}

	switch inv.Status.Phase {
	case v1alpha1.RunRunning:
		if inv.Status.JobRef == nil {
			return ctrl.Result{}, r.launch(ctx, &inv)
		}
		return r.collect(ctx, &inv)
	default:
		return ctrl.Result{}, nil // Pending waits for a grant; terminal is done
	}
}

// launch creates the agent Job for a granted investigation.
func (r *InvestigationReconciler) launch(ctx context.Context, inv *v1alpha1.Investigation) error {
	var fnd v1alpha1.Finding
	fndKey := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.FindingRef.Name}
	if err := r.Get(ctx, fndKey, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}
	var repo v1alpha1.Repository
	if inv.Spec.RepositoryRef == nil {
		return r.fail(ctx, inv, &fnd, "aborted", "investigation has no repository artifact", nil, nil)
	}
	repoKey := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.RepositoryRef.Name}
	if err := r.Get(ctx, repoKey, &repo); err != nil {
		return err
	}
	if repo.Status.Artifact == nil {
		return fmt.Errorf("repository %s has no artifact yet", repo.Name)
	}
	handoff, err := templates.RenderFindingIssue(&fnd)
	if err != nil {
		return err
	}

	repoName := ""
	if fnd.Spec.Repository != nil {
		repoName = fnd.Spec.Repository.Name
	}
	spec := jobs.Spec{
		Repo:           repoName,
		Attempt:        int(inv.Spec.Attempt),
		Phase:          "investigate",
		Harness:        r.InvestigateHarness,
		Model:          r.InvestigateModel,
		BaseSHA:        repo.Status.ResolvedSHA,
		IssueMarkdown:  handoff,
		Kind:           string(v1alpha1.RunKindInvestigation),
		Owner:          inv.Name,
		Finding:        fnd.Name,
		ArtifactURL:    repo.Status.Artifact.URL,
		ArtifactDigest: repo.Status.Artifact.Digest,
		Calibration:    r.calibration(ctx, repoName),
		// What the attempt this one retries failed with, from its own spec.
		PreviousAttempt: agentresult.EncodePreviousAttempt(inv.Spec.PreviousAttempt),
	}
	if skipped := r.Images.Pin(&spec, &repo, &fnd); skipped != "" {
		r.log().LogAttrs(ctx, slog.LevelInfo, "not running the repository-declared runner image",
			slog.String("investigation", inv.Name), slog.String("reason", skipped))
	}
	jobName, image, err := r.Runner.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("launch investigation job: %w", err)
	}

	// The image is recorded from what Create returned, never from the
	// Repository's pin: it is the stamp verdict routing keys on.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Investigation
		if err := r.Get(ctx, client.ObjectKeyFromObject(inv), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		cur.Status.JobRef = &v1alpha1.JobReference{Namespace: jobNamespace(jobName), Name: jobName}
		cur.Status.RunnerImage = &image
		return r.Status().Update(ctx, &cur)
	})
}

// jobNamespace is stamped for observability; the jobs client owns the real
// namespace, so this is cosmetic here (empty means "the agents namespace").
func jobNamespace(string) string { return "" }

// collect waits for the Job to finish and applies its result.
func (r *InvestigationReconciler) collect(ctx context.Context, inv *v1alpha1.Investigation) (ctrl.Result, error) {
	st, err := r.Runner.Status(ctx, inv.Status.JobRef.Name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// The Job vanished (TTL, manual delete) without a result.
			var fnd v1alpha1.Finding
			key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.FindingRef.Name}
			if err := r.Get(ctx, key, &fnd); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{}, r.fail(ctx, inv, &fnd, "aborted", "agent job vanished before reporting", nil, nil)
		}
		return ctrl.Result{}, err
	}
	// A repository-image Job's prepare init refused to hand over: there is
	// no log to read, and the cluster cannot sandbox a repository image.
	if runnerguard.SandboxRefused(st) {
		return ctrl.Result{}, r.sandboxRefused(ctx, inv)
	}
	if !st.Done {
		// A default-image Job waits for the Job watch, as it always has; a
		// repository-image one is looked at again while its pod may be stuck
		// pulling, which never mutates the Job.
		requeue, pullFailure := runnerguard.Pending(st, r.now())
		if pullFailure != "" {
			return ctrl.Result{}, r.pullFailed(ctx, inv, pullFailure)
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	out, err := r.Runner.Result(ctx, inv.Status.JobRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Persist before applying: this is the only moment the conversation is
	// still readable, since the Job's TTL takes the pod and its log with it.
	// A transcript that cannot be stored is logged and dropped — losing the
	// explanation must never cost the result it explains.
	ref, err := transcriptstore.Persist(ctx, r.Client, inv.Namespace,
		transcriptLabels(inv), inv, invGVK, out.Turns)
	if err != nil {
		log.FromContext(ctx).Error(err, "persist transcript", "investigation", inv.Name)
	}
	return ctrl.Result{}, r.apply(ctx, inv, out.Events, ref)
}

// sandboxRefused ends a run whose sandbox probe found NetworkPolicy
// unenforced: the breaker trips, so every later launch in this process runs
// the default image, and the finding goes back for another attempt without
// this one counting against MaxAttempts — the run is marked
// SandboxRefused, which fail() subtracts, and the next one runs on the
// default image, so the retry cannot loop.
func (r *InvestigationReconciler) sandboxRefused(ctx context.Context, inv *v1alpha1.Investigation) error {
	r.Images.Breaker.Trip(ctx, inv.Status.JobRef.Name, inv.Spec.FindingRef.Name)
	var fnd v1alpha1.Finding
	key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.FindingRef.Name}
	if err := r.Get(ctx, key, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}
	result := &envelope.Investigation{Stage: agentresult.FailedStage(nil, "aborted", runnerguard.SandboxReason)}
	if err := r.stampChild(ctx, inv, result, v1alpha1.RunFailed, nil, true); err != nil {
		return err
	}
	return r.release(ctx, &fnd, v1alpha1.PhaseEnhanced, runnerguard.SandboxReason)
}

// pullFailed ends a run whose repository-declared image cannot be pulled
// and never will be, then deletes the Job so its pod stops retrying the
// registry until the deadline.
func (r *InvestigationReconciler) pullFailed(ctx context.Context, inv *v1alpha1.Investigation, detail string) error {
	var fnd v1alpha1.Finding
	key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.FindingRef.Name}
	if err := r.Get(ctx, key, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := r.fail(ctx, inv, &fnd, "aborted", detail, nil, nil); err != nil {
		return err
	}
	if err := r.Runner.Delete(ctx, inv.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
		r.log().LogAttrs(ctx, slog.LevelWarn, "delete agent job after pull failure",
			slog.String("job", inv.Status.JobRef.Name), slog.Any("error", err))
	}
	return nil
}

// invGVK identifies the owner reference a transcript carries.
var invGVK = metav1.TypeMeta{
	APIVersion: v1alpha1.GroupVersion.String(),
	Kind:       "Investigation",
}

// transcriptLabels key a transcript to its run, so an operator can find one
// from the finding without walking owner references.
func transcriptLabels(inv *v1alpha1.Investigation) map[string]string {
	return map[string]string{
		v1alpha1.LabelFinding: inv.Spec.FindingRef.Name,
		v1alpha1.LabelOwner:   inv.Name,
		v1alpha1.LabelRunKind: string(v1alpha1.RunKindInvestigation),
		v1alpha1.LabelAttempt: strconv.Itoa(int(inv.Spec.Attempt)),
	}
}

// apply routes the Job's investigation event onto the child and the Finding.
func (r *InvestigationReconciler) apply(
	ctx context.Context, inv *v1alpha1.Investigation, events []envelope.Event,
	transcript *v1alpha1.TranscriptRef,
) error {
	var fnd v1alpha1.Finding
	key := types.NamespacedName{Namespace: inv.Namespace, Name: inv.Spec.FindingRef.Name}
	if err := r.Get(ctx, key, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}

	var result *envelope.Investigation
	for _, e := range events {
		switch e.Type {
		case envelope.TypeInvestigation:
			result = e.Investigation
		case envelope.TypeFatal:
			return r.fail(ctx, inv, &fnd, "aborted", e.Error, stageOf(result), transcript)
		}
	}
	if result == nil {
		return r.fail(ctx, inv, &fnd, "aborted", "agent job produced no investigation event", nil, transcript)
	}
	if result.Outcome != envelope.OutcomeOK {
		return r.fail(ctx, inv, &fnd, string(result.Outcome), result.Detail, &result.Stage, transcript)
	}

	// Stamp the child (single writer: this controller).
	if err := r.stampChild(ctx, inv, result, v1alpha1.RunComplete, transcript, false); err != nil {
		return err
	}

	// Route the finding, on the run's own launch-time image stamp.
	to, priorityLevel, holds := r.route(&fnd, inv, result)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, key, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		summary := &v1alpha1.InvestigationSummary{
			Name:           inv.Name,
			Attempt:        inv.Spec.Attempt,
			Outcome:        string(result.Outcome),
			Recommendation: v1alpha1.Recommendation(result.Recommendation),
			Confidence:     agentresult.FormatConfidence(result.Confidence),
			Exploitability: v1alpha1.Rating(result.Exploitability.Rating),
			Likelihood:     v1alpha1.Rating(result.Likelihood.Rating),
			Impact:         v1alpha1.Rating(result.Impact.Rating),
			AwaitApproval:  len(holds) > 0,
			HoldReasons:    holds,
			Estimate:       estimateOf(result),
			RunnerImage:    inv.Status.RunnerImage.DeepCopy(),
			CompletedAt:    timePtr(r.now()),
		}
		cur.Status.Investigation = summary
		cur.Status.Priority = priorityLevel
		cur.Status.ActiveRun = nil
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionInvestigated,
			Status:             metav1.ConditionTrue,
			Reason:             nonEmpty(result.Recommendation, "Unknown"),
			ObservedGeneration: cur.Generation,
		})
		if cur.Status.Phase == v1alpha1.PhaseInvestigating {
			if err := v1alpha1.SetPhase(&cur, to, r.now()); err != nil {
				return err
			}
		}
		return r.Status().Update(ctx, &cur)
	})
}

// route maps a completed analysis onto the finding's next phase (edges 5–8),
// the display priority level, and every reason the remediation must wait for
// a human. A non-empty reason set is exactly what sends the finding to
// AwaitingApproval.
//
// An ignore verdict from a run on a repository-declared image is held for a
// human (HandedOff) instead of dismissing the finding: that image controls
// the process the verdict came out of, and dismissal writes back to the
// scanner of record with no human gate. The decision reads the
// Investigation's own launch-time stamp — what the Job client said the pod
// ran — never controller configuration now, so a kill-switch flip between
// launch and collect cannot change it.
func (r *InvestigationReconciler) route(
	fnd *v1alpha1.Finding, inv *v1alpha1.Investigation, result *envelope.Investigation,
) (v1alpha1.Phase, v1alpha1.Level, []v1alpha1.HoldReason) {
	level := v1alpha1.Level(result.Priority)
	switch v1alpha1.Recommendation(result.Recommendation) {
	case v1alpha1.RecommendationIgnore:
		if ranRepositoryImage(inv) {
			return v1alpha1.PhaseHandedOff, level, nil
		}
		return v1alpha1.PhaseDismissed, level, nil
	case v1alpha1.RecommendationManual:
		return v1alpha1.PhaseHandedOff, level, nil
	case v1alpha1.RecommendationRemediate:
		if fnd.Spec.Repository == nil {
			return v1alpha1.PhaseHandedOff, level, nil
		}
		holds := holdReasons(result)
		// Confidence is the controller's threshold, so it joins the set here
		// rather than in the pod.
		if result.Confidence < r.ConfidenceThreshold {
			holds = append(holds, v1alpha1.HoldLowConfidence)
		}
		if len(holds) > 0 {
			return v1alpha1.PhaseAwaitingApproval, level, holds
		}
		return v1alpha1.PhaseQueued, level, nil
	default:
		return v1alpha1.PhaseHandedOff, level, nil
	}
}

// ranRepositoryImage reports whether the Investigation's launch stamp says
// its pod ran a repository-declared image. A run launched before the stamp
// existed ran the default image.
func ranRepositoryImage(inv *v1alpha1.Investigation) bool {
	ri := inv.Status.RunnerImage
	return ri != nil && ri.Source == v1alpha1.RunnerImageSourceRepository
}

// holdReasons maps the runner's hold vocabulary onto the API's. The runner
// raises the reasons only it can see (the breaking-change flag, the estimate
// against the automated budget it was configured with); AwaitApproval is a
// bare fallback so an older runner that reports no reasons still holds.
func holdReasons(result *envelope.Investigation) []v1alpha1.HoldReason {
	holds := make([]v1alpha1.HoldReason, 0, len(result.HoldReasons))
	for _, h := range result.HoldReasons {
		holds = append(holds, v1alpha1.HoldReason(h))
	}
	if len(holds) == 0 && result.AwaitApproval {
		holds = append(holds, v1alpha1.HoldBreakingChangeAvailable)
	}
	return holds
}

// calibration renders how earlier estimates in this repository compared to
// reality, as JSON for the investigation prompt. It prefers the repository's
// own history and falls back to the estate-wide one when that is too thin;
// with neither it returns empty, and the prompt omits the section rather than
// anchoring the agent on an average of nothing.
//
// This is advisory garnish: every failure path here returns empty rather than
// failing the run, because a missing rollup must never cost an investigation.
// That includes the read never returning: the Get goes through the cached
// client, so a rollup informer that cannot sync (RBAC denied, CRD absent)
// blocks on cache sync until its context is done — and with
// MaxConcurrentReconciles=1 a reconciler parked there deadlocks the whole
// controller. The deadline below is what keeps the garnish from costing the
// meal.
func (r *InvestigationReconciler) calibration(ctx context.Context, repo string) string {
	timeout := r.calibrationTimeout
	if timeout <= 0 {
		timeout = defaultCalibrationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scopes := []struct {
		scope v1alpha1.RollupScope
		label string
	}{
		{v1alpha1.RollupScope{Type: v1alpha1.ScopeRepository, Key: repo}, repo},
		{v1alpha1.RollupScope{Type: v1alpha1.ScopeTotal}, "all repositories"},
	}
	for _, s := range scopes {
		if s.scope.Type == v1alpha1.ScopeRepository && repo == "" {
			continue
		}
		var ru v1alpha1.FindingRollup
		key := types.NamespacedName{Namespace: r.Namespace, Name: stats.ScopeObjectName(s.scope)}
		if err := r.Get(ctx, key, &ru); err != nil {
			// A deadline here means the rollup cache never synced, which is
			// an operator-visible misconfiguration rather than a thin
			// history — say so once instead of silently omitting the section.
			if ctx.Err() != nil {
				if r.Log != nil {
					r.Log.Warn("estimate calibration unavailable", "error", err)
				}
				return ""
			}
			continue
		}
		c := stats.CalibrationFrom(&ru.Status, s.label)
		if c == nil {
			continue
		}
		blob, err := json.Marshal(c)
		if err != nil {
			if r.Log != nil {
				r.Log.Warn("encode estimate calibration", "error", err)
			}
			return ""
		}
		return string(blob)
	}
	return ""
}

// stageOf is the stage of an investigation event that may not have arrived.
func stageOf(result *envelope.Investigation) *envelope.Stage {
	if result == nil {
		return nil
	}
	return &result.Stage
}

// estimateOf lifts the analysis's cost prediction, or nil when it made none.
func estimateOf(result *envelope.Investigation) *v1alpha1.AgentEstimate {
	if result.EstimatedMaxTurns <= 0 && result.EstimatedTokenBudget <= 0 {
		return nil
	}
	return &v1alpha1.AgentEstimate{
		MaxTurns:    int32(result.EstimatedMaxTurns),
		TokenBudget: int64(result.EstimatedTokenBudget),
	}
}

// fail stamps a failed run and either reverts the finding for a retry or
// exhausts it (edges 4 and 9). reported is the agent's own stage when it got
// far enough to emit one; its accounting is preserved rather than discarded,
// so a failed run still lands its harness, model, turns, tokens and cost on
// the child and in the rollups. Nil for a run that produced no event at all.
// Attempts the sandbox probe refused are not counted toward MaxAttempts.
func (r *InvestigationReconciler) fail(
	ctx context.Context, inv *v1alpha1.Investigation, fnd *v1alpha1.Finding,
	outcome, detail string, reported *envelope.Stage, transcript *v1alpha1.TranscriptRef,
) error {
	// Counted before the child is stamped: an error here leaves it Running
	// for the next reconcile instead of Failed with the finding unreleased.
	consumed, err := r.consumedAttempts(ctx, inv)
	if err != nil {
		return err
	}
	result := &envelope.Investigation{Stage: agentresult.FailedStage(reported, outcome, detail)}
	if err := r.stampChild(ctx, inv, result, v1alpha1.RunFailed, transcript, false); err != nil {
		return err
	}
	maxAttempts := r.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}
	to := v1alpha1.PhaseEnhanced
	if consumed >= maxAttempts {
		to = v1alpha1.PhaseFailed
	}
	return r.release(ctx, fnd, to, outcome+": "+detail)
}

// consumedAttempts is inv's attempt number less the finding's earlier
// attempts the sandbox probe refused: their agent never ran.
func (r *InvestigationReconciler) consumedAttempts(ctx context.Context, inv *v1alpha1.Investigation) (int32, error) {
	var siblings v1alpha1.InvestigationList
	if err := r.List(ctx, &siblings, client.InNamespace(inv.Namespace),
		client.MatchingLabels{v1alpha1.LabelFinding: inv.Spec.FindingRef.Name}); err != nil {
		return 0, fmt.Errorf("count refused attempts: %w", err)
	}
	consumed := inv.Spec.Attempt
	for i := range siblings.Items {
		sib := &siblings.Items[i]
		if sib.Spec.FindingRef.UID == inv.Spec.FindingRef.UID && sib.Spec.Attempt < inv.Spec.Attempt &&
			runnerguard.Refused(sib.Status.Conditions) {
			consumed--
		}
	}
	return consumed, nil
}

// release moves a finding whose run failed out of Investigating to phase
// to, recording why.
func (r *InvestigationReconciler) release(ctx context.Context, fnd *v1alpha1.Finding, to v1alpha1.Phase,
	reason string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if cur.Status.Phase != v1alpha1.PhaseInvestigating {
			return nil
		}
		if err := v1alpha1.SetPhase(&cur, to, r.now()); err != nil {
			return err
		}
		cur.Status.ActiveRun = nil
		cur.Status.LastFailureReason = agentresult.TruncateDetail(reason)
		return r.Status().Update(ctx, &cur)
	})
}

// stampChild writes the run result onto the Investigation; refused also
// marks it SandboxRefused.
func (r *InvestigationReconciler) stampChild(
	ctx context.Context, inv *v1alpha1.Investigation, result *envelope.Investigation,
	phase v1alpha1.RunPhase, transcript *v1alpha1.TranscriptRef, refused bool,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Investigation
		if err := r.Get(ctx, client.ObjectKeyFromObject(inv), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		cur.Status.Phase = phase
		cur.Status.Stage = agentresult.FromStage(&result.Stage)
		cur.Status.Stage.Transcript = transcript
		cur.Status.Report = agentresult.TruncateReport(result.ReportMarkdown)
		if phase == v1alpha1.RunComplete {
			cur.Status.Exploitability = agentresult.Analysis(result.Exploitability)
			cur.Status.Likelihood = agentresult.Analysis(result.Likelihood)
			cur.Status.Impact = agentresult.Analysis(result.Impact)
			cur.Status.Recommendation = v1alpha1.Recommendation(result.Recommendation)
			cur.Status.Confidence = agentresult.FormatConfidence(result.Confidence)
			cur.Status.Severity = v1alpha1.Level(result.Severity)
			cur.Status.Priority = v1alpha1.Level(result.Priority)
			holds := holdReasons(result)
			cur.Status.AwaitApproval = len(holds) > 0
			cur.Status.HoldReasons = holds
			// Model plus the prediction. The turn/token grant is NOT set
			// here: the spawner resolves it against the automated budget,
			// the manual budget, and whether a human approved the estimate.
			if est := estimateOf(result); result.RemediationModel != "" || est != nil {
				cur.Status.RemediationParameters = &v1alpha1.AgentParameters{
					Model:    result.RemediationModel,
					Estimate: est,
				}
			}
		}
		// The message is capped like the stage detail: the envelope's detail
		// is unbounded (a full git status, a stderr tail), and the CRD
		// refuses a condition message over 32768 bytes by rejecting the
		// whole status write, which would leave the run Running.
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionComplete,
			Status:             metav1.ConditionTrue,
			Reason:             nonEmpty(string(result.Outcome), "Unknown"),
			Message:            agentresult.TruncateDetail(result.Detail),
			ObservedGeneration: cur.Generation,
		})
		if refused {
			meta.SetStatusCondition(&cur.Status.Conditions, runnerguard.RefusedCondition(cur.Generation))
		}
		return r.Status().Update(ctx, &cur)
	})
}

// finalize cleans the child's Job/Secret before letting deletion proceed
// (the FinalizerJobs contract).
func (r *InvestigationReconciler) finalize(ctx context.Context, inv *v1alpha1.Investigation) error {
	if inv.Status.JobRef != nil {
		if err := r.Runner.Delete(ctx, inv.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Investigation
		if err := r.Get(ctx, client.ObjectKeyFromObject(inv), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		fins := cur.GetFinalizers()
		out := fins[:0]
		for _, f := range fins {
			if f != v1alpha1.FinalizerJobs {
				out = append(out, f)
			}
		}
		if len(out) == len(fins) {
			return nil
		}
		cur.SetFinalizers(out)
		return r.Update(ctx, &cur)
	})
}

// SetupWithManager wires the reconciler: Investigations, plus agent-Job
// completions mapped back (by owner label, filtered to this kind), all also
// fanned into the singleton scheduling request.
func (r *InvestigationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapJob := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		if obj.GetLabels()[v1alpha1.LabelRunKind] != string(v1alpha1.RunKindInvestigation) {
			return nil
		}
		owner := obj.GetLabels()[v1alpha1.LabelOwner]
		if owner == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: owner}},
			{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: schedulerRequest}},
		}
	})
	mapSelf := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}},
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: schedulerRequest}},
		}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1alpha1.Investigation{}, mapSelf).
		Watches(&batchv1.Job{}, mapJob).
		Named("investigation").
		Complete(r)
}

func timePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (r *InvestigationReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *InvestigationReconciler) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}
