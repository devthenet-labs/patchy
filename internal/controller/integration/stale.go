// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghas"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

// DefaultStaleRecheck paces the re-read of alerts whose reopen ingest set
// aside as stale, when FindingReconciler.StaleRecheck is unset.
const DefaultStaleRecheck = 15 * time.Minute

// recordStale notes f on fix's status as a stale observation of one of its
// alerts, for the re-check (recheckStale). Idempotent: an observation already
// recorded at the same commit and ref is left as it is, so a re-check that
// finds the alert where it was writes nothing.
func (in *Ingestor) recordStale(ctx context.Context, fix *v1alpha1.Finding, f source.Finding) error {
	obs := v1alpha1.StaleObservation{
		AlertID: alertID(f), Commit: f.Commit, Ref: f.Ref, ObservedAt: metav1.NewTime(in.now()),
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := in.Get(ctx, client.ObjectKeyFromObject(fix), &cur); err != nil {
			return err
		}
		i := slices.IndexFunc(cur.Status.StaleObservations,
			func(o v1alpha1.StaleObservation) bool { return o.AlertID == obs.AlertID })
		switch {
		case i < 0:
			cur.Status.StaleObservations = append(cur.Status.StaleObservations, obs)
		case cur.Status.StaleObservations[i].Commit == obs.Commit && cur.Status.StaleObservations[i].Ref == obs.Ref:
			return nil
		default:
			cur.Status.StaleObservations[i] = obs
		}
		return in.Status().Update(ctx, &cur)
	})
}

// noteAncestry keeps the Integration's CommitAncestry condition honest after
// a lookup: False when GitHub refused the credential (the compare API needs
// Contents: read, which an Integration App split from its Forge App may
// lack), back to True on the next lookup that succeeds. Other failures say
// nothing about the credential and leave it alone. The condition is absent
// until the first refusal and written only when it changes, so a healthy
// Integration never pays a status write for it — deciding costs a cache
// read, not an API call.
func (in *Ingestor) noteAncestry(ctx context.Context, integ *v1alpha1.Integration, err error) {
	denied := ghclient.IsForbidden(err)
	if err != nil && !denied {
		return
	}
	werr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Integration
		if err := in.Get(ctx, client.ObjectKeyFromObject(integ), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		prev := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionCommitAncestry)
		cond := metav1.Condition{
			Type:               v1alpha1.ConditionCommitAncestry,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonAncestryReadable,
			Message:            "commit ancestry lookups succeed",
			ObservedGeneration: cur.Generation,
		}
		switch {
		case denied && prev != nil && prev.Status == metav1.ConditionFalse:
			return nil
		case denied:
			cond.Status = metav1.ConditionFalse
			cond.Reason = v1alpha1.ReasonContentsReadDenied
			cond.Message = "GitHub refused this Integration's credential the compare API, so reopens of " +
				"remediated alerts at commits their fix superseded open duplicate findings; grant it the " +
				"Contents (read) repository permission: " + err.Error()
		case prev == nil || prev.Status == metav1.ConditionTrue:
			return nil
		}
		meta.SetStatusCondition(&cur.Status.Conditions, cond)
		return in.Status().Update(ctx, &cur)
	})
	if werr != nil {
		in.log().LogAttrs(ctx, slog.LevelWarn, "commit ancestry condition not recorded",
			slog.String("integration", integ.Name), slog.Any("error", werr))
	}
}

// recheckStale re-reads each alert whose reopen ingest set aside on fnd as
// stale (status.staleObservations) and settles the ones that no longer need
// watching: the alert closed or went away, a newer generation carries it, or
// it is now seen at a commit the fix does not supersede — a regression the
// scanner will never announce, because the alert was already open — which
// ingest then turns into the successor generation.
//
// It returns when to look again, zero once nothing is pending. It never
// fails the reconcile: an observation that cannot be settled now stays
// recorded for the next pass.
func (r *FindingReconciler) recheckStale(ctx context.Context, fnd *v1alpha1.Finding) time.Duration {
	pending := fnd.Status.StaleObservations
	if r.Ingest == nil || len(pending) == 0 {
		return 0
	}
	settled, err := r.settleStale(ctx, fnd)
	if err != nil {
		r.log().LogAttrs(ctx, slog.LevelWarn, "stale alert re-check failed",
			slog.String("finding", fnd.Name), slog.Any("error", err))
	}
	if len(settled) > 0 {
		if err := r.dropStale(ctx, fnd, settled); err != nil {
			r.log().LogAttrs(ctx, slog.LevelWarn, "settled stale observations not cleared",
				slog.String("finding", fnd.Name), slog.Any("error", err))
			return r.staleRecheck()
		}
	}
	if len(settled) == len(pending) {
		return 0
	}
	return r.staleRecheck()
}

// settleStale reports which of fnd's stale observations are settled.
func (r *FindingReconciler) settleStale(
	ctx context.Context, fnd *v1alpha1.Finding,
) ([]v1alpha1.StaleObservation, error) {
	all := fnd.Status.StaleObservations
	var integ v1alpha1.Integration
	key := types.NamespacedName{Namespace: fnd.Namespace, Name: fnd.Spec.IntegrationRef.Name}
	if err := r.Get(ctx, key, &integ); err != nil {
		if kerrors.IsNotFound(err) {
			return all, nil // nothing left to ingest a regression through
		}
		return nil, err
	}
	if !codeScanningEnabled(&integ) {
		return nil, nil // suspended or paused: look again once it ingests
	}
	if fnd.Spec.Repository == nil {
		return all, nil
	}
	repo, err := parseOwnerRepo(fnd.Spec.Repository.Name)
	if err != nil {
		return all, nil
	}
	gh, err := r.clientFor(ctx, &integ, repo)
	if err != nil {
		return nil, err
	}
	var family v1alpha1.FindingList
	if err := r.List(ctx, &family, client.InNamespace(fnd.Namespace),
		client.MatchingFields{KeyHashIndex: fnd.Labels[v1alpha1.LabelKeyHash]}); err != nil {
		return nil, err
	}
	var settled []v1alpha1.StaleObservation
	var errs []error
	for _, obs := range all {
		reason, err := r.settle(ctx, &integ, gh, repo, fnd, family.Items, obs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if reason == "" {
			continue
		}
		r.log().LogAttrs(ctx, slog.LevelInfo, "stale alert observation settled",
			slog.String("finding", fnd.Name), slog.String("alert", obs.AlertID),
			slog.String("commit", obs.Commit), slog.String("reason", reason))
		settled = append(settled, obs)
	}
	return settled, errors.Join(errs...)
}

// settle re-reads one stale observation's alert and says why it is settled,
// or "" while it is still pending.
func (r *FindingReconciler) settle(
	ctx context.Context, integ *v1alpha1.Integration, gh trackerClient, repo ghclient.Repo,
	fnd *v1alpha1.Finding, family []v1alpha1.Finding, obs v1alpha1.StaleObservation,
) (reason string, err error) {
	number, err := strconv.Atoi(obs.AlertID)
	if err != nil {
		return "not a code-scanning alert number", nil
	}
	if latest := latestWithAlert(family, obs.AlertID); latest != nil && latest.Name != fnd.Name {
		return "a newer generation carries the alert", nil
	}
	alert, err := gh.GetAlert(ctx, repo, number)
	if ghclient.IsNotFound(err) {
		return "alert gone", nil
	}
	if err != nil {
		return "", err
	}
	if alert.State != "open" {
		return "alert " + alert.State, nil
	}
	// Only an analysis of the branch the stale observation was made on can
	// move it; the latest instance of a pull-request analysis says nothing
	// about that branch.
	branch, ok := branchOf(alert.MostRecentRef)
	if want, _ := branchOf(obs.Ref); !ok || branch != want || alert.MostRecentSHA == "" {
		return "", nil
	}
	f := ghas.FindingFromAlert(repo, alert)
	stale, err := r.Ingest.ingest(ctx, integ, f, true)
	if err != nil || stale {
		return "", err
	}
	return "observed at a commit the fix does not supersede; ingested", nil
}

// dropStale removes settled observations from fnd's status under conflict
// retry — each only while it is still the one settled, so an observation
// ingest re-recorded at a new commit in the meantime survives.
func (r *FindingReconciler) dropStale(
	ctx context.Context, fnd *v1alpha1.Finding, settled []v1alpha1.StaleObservation,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		kept := slices.DeleteFunc(slices.Clone(cur.Status.StaleObservations), func(o v1alpha1.StaleObservation) bool {
			return slices.ContainsFunc(settled, func(s v1alpha1.StaleObservation) bool {
				return s.AlertID == o.AlertID && s.Commit == o.Commit
			})
		})
		if len(kept) == len(cur.Status.StaleObservations) {
			return nil
		}
		cur.Status.StaleObservations = kept
		return r.Status().Update(ctx, &cur)
	})
}

func (r *FindingReconciler) staleRecheck() time.Duration {
	if r.StaleRecheck <= 0 {
		return DefaultStaleRecheck
	}
	return r.StaleRecheck
}

func (r *FindingReconciler) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}
