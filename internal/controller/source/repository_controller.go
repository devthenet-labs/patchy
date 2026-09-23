// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// DefaultMaxArtifactBytes caps a stored tarball; larger repositories stall
// the Repository instead of exhausting artifact storage.
const DefaultMaxArtifactBytes = int64(1) << 30 // 1 GiB

// RepositoryReconciler produces the SHA-pinned tarball artifact for each
// Repository: resolve the covering Forge, pin the head SHA once, download
// the archive, publish it through the artifact store.
type RepositoryReconciler struct {
	client.Client
	// Forges resolves and authenticates forge access.
	Forges *forge.Store
	// Artifacts stores and serves the tarballs.
	Artifacts *artifact.Store
	// MaxArtifactBytes caps a tarball; zero means DefaultMaxArtifactBytes.
	MaxArtifactBytes int64
	// ClientFor returns the forge API client for a resolution; nil uses the
	// Forges store (tests substitute a fake).
	ClientFor func(ctx context.Context, res *forge.Resolved) (forgeClient, error)
	// Images resolves the repository-declared runner image beside the SHA,
	// exactly once per Repository; nil disables the feature (the kill
	// switch) and leaves the Job shape untouched.
	Images *RunnerImages
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives reconcile diagnostics; nil discards.
	Log *slog.Logger

	// undeclared maps an artifact key to the digest of a stored tree already
	// read and found to declare no runner image (see knownUndeclared).
	undeclaredMu sync.Mutex
	undeclared   map[string]string
}

func (r *RepositoryReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// clientFor resolves the API-client seam.
func (r *RepositoryReconciler) clientFor(ctx context.Context, res *forge.Resolved) (forgeClient, error) {
	if r.ClientFor != nil {
		return r.ClientFor(ctx, res)
	}
	return r.Forges.Client(ctx, res)
}

// Reconcile implements the Repository control loop.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var repo v1alpha1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if kerrors.IsNotFound(err) {
			// The object is gone; the artifact is local state keyed by name.
			r.Artifacts.Delete(req.String())
			r.forgetDeclaration(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !repo.DeletionTimestamp.IsZero() {
		r.Artifacts.Delete(req.String())
		r.forgetDeclaration(req.String())
		return ctrl.Result{}, nil
	}

	res, err := r.Forges.Resolve(ctx, repo.Namespace, repo.Spec.URL)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &repo, forgeFailReason(err), err)
	}

	gh, err := r.clientFor(ctx, res)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &repo, "CredentialInvalid", err)
	}

	// Pin the SHA exactly once: investigation and remediation must see the
	// same tree, and it is the eventual push base.
	sha := repo.Status.ResolvedSHA
	if sha == "" {
		branch := repo.Spec.Ref.Branch
		if branch == "" {
			if branch, err = gh.DefaultBranch(ctx, res.Repo); err != nil {
				return ctrl.Result{}, r.fail(ctx, &repo, "ResolveFailed", err)
			}
		}
		if sha, err = gh.HeadSHA(ctx, res.Repo, branch); err != nil {
			return ctrl.Result{}, r.fail(ctx, &repo, "ResolveFailed", err)
		}
	}

	info, ok := r.Artifacts.Get(req.String())
	fetched := false
	if !ok || repo.Status.Artifact == nil || repo.Status.Artifact.Digest != info.Digest {
		if info, err = r.fetch(ctx, gh, res, sha, req.String()); err != nil {
			var tooBig *tooLargeError
			if errors.As(err, &tooBig) {
				return ctrl.Result{}, r.stall(ctx, &repo, sha, "ArtifactTooLarge", tooBig)
			}
			return ctrl.Result{}, r.fail(ctx, &repo, "FetchFailed", err)
		}
		fetched = true
	}

	fetchedAt := r.fetchedAt(&repo, fetched)
	repo.Status.ResolvedSHA = sha
	repo.Status.Forge = &v1alpha1.LocalObjectReference{Name: res.Forge.Name}
	repo.Status.Artifact = &v1alpha1.Artifact{
		URL:           info.URL,
		Digest:        info.Digest,
		SizeBytes:     info.Size,
		LastFetchedAt: &fetchedAt,
	}
	// Pin the runner image exactly once, beside the SHA, with the artifact
	// already in hand so resolution never costs a re-download.
	if r.Images != nil {
		if result, done, err := r.pinRunnerImage(ctx, &repo, sha, req.String()); done {
			return result, err
		}
	}
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "ArtifactReady",
		ObservedGeneration: repo.Generation,
	})
	meta.RemoveStatusCondition(&repo.Status.Conditions, v1alpha1.ConditionStalled)
	repo.Status.ObservedGeneration = repo.Generation
	if err := r.Status().Update(ctx, &repo); err != nil {
		if kerrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	attrs := []slog.Attr{
		slog.String("repository", req.String()),
		slog.String("sha", sha),
		slog.Int64("bytes", info.Size),
	}
	if ri := repo.Status.RunnerImage; ri != nil {
		attrs = append(attrs, slog.String("runner_image", ri.Image), slog.String("runner_image_manifest", ri.Manifest))
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "repository artifact ready", attrs...)
	return ctrl.Result{}, nil
}

// fetchedAt is the artifact's LastFetchedAt: now when this pass downloaded
// it, otherwise what the status already records. Re-stamping it on every
// pass would make each status write a real change, whose watch event
// re-queues the Repository.
func (r *RepositoryReconciler) fetchedAt(repo *v1alpha1.Repository, fetched bool) metav1.Time {
	if !fetched && repo.Status.Artifact != nil && repo.Status.Artifact.LastFetchedAt != nil {
		return *repo.Status.Artifact.LastFetchedAt
	}
	return metav1.NewTime(r.now())
}

// tooLargeError marks a tarball over the cap — a stall, not a retry.
type tooLargeError struct{ max int64 }

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("tarball exceeds the %d-byte artifact cap", e.max)
}

// fetch downloads the tarball at sha and stores it under key.
func (r *RepositoryReconciler) fetch(
	ctx context.Context, gh forgeClient, res *forge.Resolved, sha, key string,
) (*artifact.Info, error) {
	rc, err := gh.Tarball(ctx, res.Repo, sha)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	maxBytes := r.MaxArtifactBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxArtifactBytes
	}
	info, err := r.Artifacts.Put(key, io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if info.Size > maxBytes {
		r.Artifacts.Delete(key)
		return nil, &tooLargeError{max: maxBytes}
	}
	return info, nil
}

// forgeClient is the slice of ghclient.Client the reconciler needs — tests
// substitute a fake via ClientFor.
type forgeClient interface {
	DefaultBranch(ctx context.Context, repo ghclient.Repo) (string, error)
	HeadSHA(ctx context.Context, repo ghclient.Repo, branch string) (string, error)
	Tarball(ctx context.Context, repo ghclient.Repo, ref string) (io.ReadCloser, error)
}

// fail records a not-ready condition and gives up until spec or forge
// configuration changes re-queue the object (transient API errors return the
// error for backoff instead).
func (r *RepositoryReconciler) fail(ctx context.Context, repo *v1alpha1.Repository, reason string, cause error) error {
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            truncate(cause.Error()),
		ObservedGeneration: repo.Generation,
	})
	repo.Status.ObservedGeneration = repo.Generation
	if err := r.Status().Update(ctx, repo); err != nil && !kerrors.IsConflict(err) {
		return errors.Join(cause, err)
	}
	// Configuration errors wait for a Forge/spec change; everything else
	// backs off and retries.
	if errors.Is(cause, forge.ErrNoMatch) || errors.Is(cause, forge.ErrAmbiguous) {
		r.log().LogAttrs(ctx, slog.LevelWarn, "repository unresolvable",
			slog.String("repository", repo.Name), slog.Any("error", cause))
		return nil
	}
	return cause
}

// stall records the Stalled condition with the reason (an over-cap artifact,
// a rejected runner image); only a spec change re-queues. Whatever the
// caller has put on the status is carried through, so a rejected runner
// image keeps its artifact and a revived finding still has a tree to run on.
func (r *RepositoryReconciler) stall(
	ctx context.Context, repo *v1alpha1.Repository, sha, reason string, cause error,
) error {
	repo.Status.ResolvedSHA = sha
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionStalled,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            truncate(cause.Error()),
		ObservedGeneration: repo.Generation,
	})
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: repo.Generation,
	})
	repo.Status.ObservedGeneration = repo.Generation
	if err := r.Status().Update(ctx, repo); err != nil && !kerrors.IsConflict(err) {
		return err
	}
	return nil
}

// SetupWithManager wires the reconciler: its own kind, plus a Forge watch
// that re-queues every Repository in the namespace — a Forge created or
// re-scoped after ingest must resolve previously-unresolvable repositories
// without human action.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapForge := func(ctx context.Context, obj client.Object) []ctrl.Request {
		var repos v1alpha1.RepositoryList
		if err := mgr.GetClient().List(ctx, &repos, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		out := make([]ctrl.Request, 0, len(repos.Items))
		for i := range repos.Items {
			out = append(out, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: repos.Items[i].Namespace,
				Name:      repos.Items[i].Name,
			}})
		}
		return out
	}
	// Spec changes only: the Forge reconciler's own status writes (credential
	// validation) would otherwise re-queue every Repository in the namespace
	// on each pass, and resolution reads Forge spec alone.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Repository{}).
		Watches(&v1alpha1.Forge{}, handler.EnqueueRequestsFromMapFunc(mapForge),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("repository").
		Complete(r)
}

func (r *RepositoryReconciler) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

// forgeFailReason maps resolution errors to condition reasons.
func forgeFailReason(err error) string {
	switch {
	case errors.Is(err, forge.ErrAmbiguous):
		return v1alpha1.ReasonAmbiguous
	case errors.Is(err, forge.ErrNoMatch):
		return v1alpha1.ReasonNoForgeMatch
	default:
		return "ResolveFailed"
	}
}
