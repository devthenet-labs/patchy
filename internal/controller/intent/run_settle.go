// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
)

// updateRun writes the run's status as mutate leaves it, over the version in
// hand; on success run is the written version.
func (r *RunReconciler) updateRun(ctx context.Context, run *v1alpha1.IntentRun,
	mutate func(*v1alpha1.IntentRun)) error {
	cur := run.DeepCopy()
	mutate(cur)
	cur.Status.ObservedGeneration = cur.Generation
	if err := r.Status().Update(ctx, cur); err != nil {
		return fmt.Errorf("update run %s: %w", run.Name, err)
	}
	*run = *cur
	return nil
}

// settle stamps a run's terminal status (Complete, or Failed with the
// outcome and detail), then deletes a plan run's Repository: only a build's
// is kept, as the intent's runner-image anchor.
func (r *RunReconciler) settle(ctx context.Context, run *v1alpha1.IntentRun, res result) error {
	if err := r.updateRun(ctx, run, func(cur *v1alpha1.IntentRun) {
		now := metav1.NewTime(r.now())
		cur.Status.Phase = v1alpha1.RunFailed
		condStatus := metav1.ConditionFalse
		if res.complete {
			cur.Status.Phase = v1alpha1.RunComplete
			condStatus = metav1.ConditionTrue
		}
		cur.Status.Outcome = res.outcome
		cur.Status.Detail = agentresult.TruncateDetail(res.detail)
		if !res.keep {
			cur.Status.Report = agentresult.TruncateReport(res.report)
			if res.transcript != nil {
				cur.Status.Transcript = res.transcript
			}
			if res.stage != nil {
				cur.Status.Usage = agentresult.FromStage(res.stage).Usage
			}
		}
		cur.Status.FinishedAt = &now
		reason := res.outcome
		if reason == "" {
			reason = "Unknown"
		}
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionComplete, Status: condStatus, Reason: reason,
			Message: cur.Status.Detail, ObservedGeneration: cur.Generation,
		})
		if res.sandboxRefused {
			meta.SetStatusCondition(&cur.Status.Conditions, runnerguard.RefusedCondition(cur.Generation))
		}
	}); err != nil {
		return err
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "intent run finished",
		slog.String("run", run.Name), slog.String("phase", string(run.Status.Phase)),
		slog.String("outcome", run.Status.Outcome))
	if run.Spec.Stage != v1alpha1.IntentStagePlan {
		return nil
	}
	repo := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{
		Namespace: run.Namespace, Name: run.Spec.Repository.RepositoryRef.Name,
	}}
	if err := r.Delete(ctx, repo); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete the plan repository %s: %w", repo.Name, err)
	}
	return nil
}

// finalize deletes the run's Job (its Secret goes with it) and releases the
// jobs finalizer: an Intent's foreground deletion waits for this.
func (r *RunReconciler) finalize(ctx context.Context, run *v1alpha1.IntentRun) error {
	job := jobs.NameFor(run.Name, KindIntent, run.Spec.Attempt)
	if run.Status.JobRef != nil {
		job = run.Status.JobRef.Name
	}
	if err := r.Jobs.Delete(ctx, job); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete job %s: %w", job, err)
	}
	if !slices.Contains(run.Finalizers, v1alpha1.FinalizerJobs) {
		return nil
	}
	cur := run.DeepCopy()
	cur.Finalizers = slices.DeleteFunc(cur.Finalizers, func(f string) bool { return f == v1alpha1.FinalizerJobs })
	return client.IgnoreNotFound(r.Update(ctx, cur))
}

// SetupWithManager registers the run reconciler. Every run, intent Job,
// intent Repository and run-input ConfigMap event also reaches the singleton
// scheduler request, since each can make a pending run launchable; an
// Intent's change reaches its runs, which abort when it has ended.
func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheduler := func(ns string) ctrl.Request {
		return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: runSchedulerRequest}}
	}
	ns := r.Settings.Namespace
	mapSelf := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{
			{NamespacedName: client.ObjectKeyFromObject(obj)},
			scheduler(obj.GetNamespace()),
		}
	})
	mapJob := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		if obj.GetLabels()[v1alpha1.LabelRunKind] != KindIntent {
			return nil
		}
		owner := obj.GetLabels()[v1alpha1.LabelOwner]
		if owner == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: ns, Name: owner}},
			scheduler(ns),
		}
	})
	mapRunChild := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		run := obj.GetLabels()[v1alpha1.LabelIntentRun]
		if run == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: run}},
			scheduler(obj.GetNamespace()),
		}
	})
	mapIntent := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		var runs v1alpha1.IntentRunList
		if err := mgr.GetClient().List(ctx, &runs, client.InNamespace(obj.GetNamespace()),
			client.MatchingLabels{v1alpha1.LabelIntent: obj.GetName()}); err != nil {
			return nil
		}
		out := []ctrl.Request{scheduler(obj.GetNamespace())}
		for i := range runs.Items {
			if p := runs.Items[i].Status.Phase; p != v1alpha1.RunComplete && p != v1alpha1.RunFailed {
				out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&runs.Items[i])})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1alpha1.IntentRun{}, mapSelf).
		Watches(&batchv1.Job{}, mapJob).
		Watches(&v1alpha1.Repository{}, mapRunChild).
		Watches(&corev1.ConfigMap{}, mapRunChild).
		Watches(&v1alpha1.Intent{}, mapIntent).
		Named("intent-run").
		Complete(r)
}
