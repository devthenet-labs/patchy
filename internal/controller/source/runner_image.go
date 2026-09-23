// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// The onReject policies: what a rejected declaration does to the Repository.
const (
	// OnRejectHandoff stalls the Repository, so the gate parks the finding
	// for a human; the default.
	OnRejectHandoff = "handoff"
	// OnRejectDefault records the rejection and leaves the Repository Ready,
	// so the finding runs on the default image with no human step.
	OnRejectDefault = "default"
)

// maxMessageBytes is the CRD's cap on status.runnerImage.message. Condition
// messages are held to it too: they quote the committer-controlled
// declaration, and the CRD refuses a condition message over 32768 bytes by
// failing the whole status write.
const maxMessageBytes = 4096

// DefaultResolveTimeout bounds one resolution's registry traffic. The
// Repository controller runs one worker, so a registry that accepts the
// connection and never answers would otherwise block every Repository.
const DefaultResolveTimeout = 3 * time.Minute

// RunnerImages is the repository-declared runner image configuration. A nil
// RepositoryReconciler.Images is the kill switch: no declaration is read
// and status.runnerImage is never written.
type RunnerImages struct {
	// Policy is the operator's registry allowlist.
	Policy runnerimage.Policy
	// Resolver pins a declared reference to a digest and checks it.
	Resolver runnerimage.Resolver
	// OnReject is OnRejectHandoff or OnRejectDefault; empty means handoff.
	OnReject string
	// ResolveTimeout bounds one resolution's registry traffic; <= 0 means
	// DefaultResolveTimeout.
	ResolveTimeout time.Duration
}

// handoff reports whether a rejection stalls the Repository.
func (i *RunnerImages) handoff() bool { return i.OnReject != OnRejectDefault }

// resolveTimeout is ResolveTimeout with its default applied.
func (i *RunnerImages) resolveTimeout() time.Duration {
	if i.ResolveTimeout <= 0 {
		return DefaultResolveTimeout
	}
	return i.ResolveTimeout
}

// imageRejection is a deterministic refusal of the declaration, carrying
// what status.runnerImage records about it.
type imageRejection struct {
	declared string
	manifest string
	reason   string
	cause    *runnerimage.Rejection
}

func (e *imageRejection) Error() string { return e.cause.Message }
func (e *imageRejection) Unwrap() error { return e.cause }

// status is the record a rejection leaves: no image, the reason and the
// message the Stalled condition also carries. The declared reference is
// bounded like the message, since a rejected one can be anything the
// declaration file held.
func (e *imageRejection) status(now metav1.Time) *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared:   truncate(e.declared),
		Manifest:   e.manifest,
		Rejected:   e.reason,
		Message:    truncate(e.cause.Message),
		ResolvedAt: &now,
	}
}

// rejected wraps a *runnerimage.Rejection from stage, labelling it with the
// Rejection's own reason when the check set one and stage otherwise; any
// other error passes through.
func rejected(stage, declared, manifest string, err error) error {
	var rej *runnerimage.Rejection
	if !errors.As(err, &rej) {
		return err
	}
	reason := rej.Reason
	if reason == "" {
		reason = stage
	}
	return &imageRejection{declared: declared, manifest: manifest, reason: reason, cause: rej}
}

// pinRunnerImage runs the runner-image half of the reconcile once the
// artifact is stored: resolve exactly once, then keep the record. It returns
// done=true when it wrote a terminal status itself (a stall, a failure or a
// conflict re-queue) and the caller must return its result; otherwise the
// caller finishes the reconcile with the Ready=True write.
func (r *RepositoryReconciler) pinRunnerImage(
	ctx context.Context, repo *v1alpha1.Repository, sha, key string,
) (ctrl.Result, bool, error) {
	if repo.Status.RunnerImage != nil {
		// Pinned, accepted or rejected: never re-resolved, so a moved tag or
		// a changed policy cannot alter the environment of a finding in
		// flight. A restart re-fetches the artifact but a rejection stays
		// stalled.
		if repo.Status.RunnerImage.Rejected != "" && r.Images.handoff() {
			return ctrl.Result{}, true, r.stall(ctx, repo, sha, v1alpha1.ReasonRunnerImageRejected,
				errors.New(repo.Status.RunnerImage.Message))
		}
		return ctrl.Result{}, false, nil
	}
	ri, err := r.resolveRunnerImage(ctx, repo, key)
	// Stamped once resolution has finished, not when the reconcile began.
	resolvedAt := metav1.NewTime(r.now())
	var rej *imageRejection
	switch {
	case errors.As(err, &rej):
		repo.Status.RunnerImage = rej.status(resolvedAt)
		r.log().LogAttrs(ctx, slog.LevelWarn, "runner image rejected",
			slog.String("repository", repo.Name), slog.String("manifest", rej.manifest),
			slog.String("declared", rej.declared), slog.String("reason", rej.reason),
			slog.String("message", rej.cause.Message), slog.Bool("handoff", r.Images.handoff()))
		if r.Images.handoff() {
			return ctrl.Result{}, true, r.stall(ctx, repo, sha, v1alpha1.ReasonRunnerImageRejected, rej)
		}
		return ctrl.Result{}, false, nil
	case kerrors.IsConflict(err):
		return ctrl.Result{Requeue: true}, true, nil
	case err != nil:
		return ctrl.Result{}, true, r.fail(ctx, repo, v1alpha1.ReasonRunnerImageResolveFailed, err)
	}
	if ri != nil {
		ri.ResolvedAt = &resolvedAt
	}
	repo.Status.RunnerImage = ri
	return ctrl.Result{}, false, nil
}

// resolveRunnerImage reads the declaration out of the stored tarball (so it
// is bound to the pinned tree) and, when it names an image, allowlists,
// pins and checks it. The registry is consulted only after the first status
// write has persisted the artifact with Ready=False / RunnerImageResolving,
// so a transient failure or a restart resumes here without a re-download.
// A *imageRejection is deterministic; any other error is transient. The
// returned record's ResolvedAt is the caller's to stamp.
func (r *RepositoryReconciler) resolveRunnerImage(
	ctx context.Context, repo *v1alpha1.Repository, key string,
) (*v1alpha1.RunnerImage, error) {
	digest := ""
	if repo.Status.Artifact != nil {
		digest = repo.Status.Artifact.Digest
	}
	if r.knownUndeclared(key, digest) {
		return nil, nil
	}
	decl, err := r.declaration(key)
	if err != nil {
		if !runnerimage.IsRejection(err) {
			// The stored archive cannot be read: the artifact, not the
			// declaration, is at fault. Drop it so the retry downloads it
			// again instead of re-reading the same bytes forever.
			r.Artifacts.Delete(key)
			return nil, err
		}
		return nil, rejected("InvalidDeclaration", "", runnerimage.AgentYAMLPath, err)
	}
	switch decl.Outcome {
	case runnerimage.OutcomeNone:
		// Nothing is recorded (status.runnerImage stays nil), so the
		// pin-once guard never closes; remember the tree instead, or every
		// reconcile would walk the whole archive again.
		r.rememberUndeclared(key, digest)
		return nil, nil
	case runnerimage.OutcomeNotApplicable:
		// Not a rejection: the file was not written for patchy. The reason
		// is recorded for the owner and the default image runs.
		return &v1alpha1.RunnerImage{Manifest: decl.Manifest, Message: truncate(decl.Reason)}, nil
	}
	ref, err := runnerimage.ParseDeclared(decl.Image)
	if err != nil {
		return nil, rejected("InvalidReference", decl.Image, decl.Manifest, err)
	}
	if err := r.Images.Policy.Allow(ref); err != nil {
		return nil, rejected("NotAllowlisted", decl.Image, decl.Manifest, err)
	}
	if err := r.resolving(ctx, repo, decl); err != nil {
		return nil, err
	}
	timeout := r.Images.resolveTimeout()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	res, err := r.Images.Resolver.Resolve(rctx, ref)
	cancel()
	if err != nil {
		if runnerimage.IsRejection(err) {
			return nil, rejected("Resolve", decl.Image, decl.Manifest, err)
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("resolve runner image %s: the registry did not answer within %s: %w",
				decl.Image, timeout, err)
		}
		return nil, fmt.Errorf("resolve runner image %s: %w", decl.Image, err)
	}
	return &v1alpha1.RunnerImage{
		Declared:   decl.Image,
		Manifest:   decl.Manifest,
		Image:      res.Image,
		SearchPath: strings.Join(res.SearchPath, ":"),
		Verified:   res.Verified,
	}, nil
}

// knownUndeclared reports whether the stored tree under key, at digest, has
// already been read and found to declare no image.
func (r *RepositoryReconciler) knownUndeclared(key, digest string) bool {
	r.undeclaredMu.Lock()
	defer r.undeclaredMu.Unlock()
	d, ok := r.undeclared[key]
	return ok && digest != "" && d == digest
}

// rememberUndeclared records that the tree under key, at digest, declares
// no image.
func (r *RepositoryReconciler) rememberUndeclared(key, digest string) {
	if digest == "" {
		return
	}
	r.undeclaredMu.Lock()
	defer r.undeclaredMu.Unlock()
	if r.undeclared == nil {
		r.undeclared = make(map[string]string)
	}
	r.undeclared[key] = digest
}

// forgetDeclaration drops what is remembered about key's tree.
func (r *RepositoryReconciler) forgetDeclaration(key string) {
	r.undeclaredMu.Lock()
	defer r.undeclaredMu.Unlock()
	delete(r.undeclared, key)
}

// declaration reads the two declaration files out of the stored artifact
// and applies precedence.
func (r *RepositoryReconciler) declaration(key string) (runnerimage.Declaration, error) {
	rc, err := r.Artifacts.Open(key)
	if err != nil {
		return runnerimage.Declaration{}, fmt.Errorf("open artifact: %w", err)
	}
	files, err := runnerimage.ReadFiles(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return runnerimage.Declaration{}, fmt.Errorf("read declaration: %w", err)
	}
	return runnerimage.Declare(files)
}

// resolving is the first of the two status writes: the artifact, SHA and
// forge are persisted with Ready=False / RunnerImageResolving before any
// registry call is made.
func (r *RepositoryReconciler) resolving(
	ctx context.Context, repo *v1alpha1.Repository, decl runnerimage.Declaration,
) error {
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonRunnerImageResolving,
		Message:            truncate(fmt.Sprintf("resolving runner image `%s` declared in `%s`", decl.Image, decl.Manifest)),
		ObservedGeneration: repo.Generation,
	})
	meta.RemoveStatusCondition(&repo.Status.Conditions, v1alpha1.ConditionStalled)
	repo.Status.ObservedGeneration = repo.Generation
	return r.Status().Update(ctx, repo)
}

// truncate bounds a message to maxMessageBytes, on a rune boundary.
func truncate(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	const ellipsis = "…"
	return strings.ToValidUTF8(s[:maxMessageBytes-len(ellipsis)], "") + ellipsis
}
