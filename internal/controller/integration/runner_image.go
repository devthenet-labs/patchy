// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// AnnotationProjectedRunnerImage is the hash of the last projected
// runner-image sticky comment ("" while there is none to post).
const AnnotationProjectedRunnerImage = "patchy.bitwisemedia.uk/projected-runner-image"

// projectRunnerImage keeps the runner-image sticky comment current: one
// marker-headed comment per tracking issue, posted once the finding's
// Repository records a declaration and edited in place as runs add to it.
// A repository that declares nothing gets no comment at all.
func (r *FindingReconciler) projectRunnerImage(
	ctx context.Context, fnd *v1alpha1.Finding, tracker trackerClient, repo ghclient.Repo, number int,
) error {
	if !r.RunnerImages || fnd.Spec.Repository == nil {
		return nil
	}
	body, err := r.runnerImageBody(ctx, fnd)
	if err != nil {
		return err
	}
	var state string
	if body != "" {
		state = hashOf(body)
	}
	if fnd.GetAnnotations()[AnnotationProjectedRunnerImage] == state {
		return nil
	}
	if body != "" {
		existing, err := tracker.ListComments(ctx, repo, number)
		if err != nil {
			return err
		}
		switch sticky := findSticky(existing, templates.RunnerImageMarker); {
		case sticky == nil:
			if err := tracker.Comment(ctx, repo, number, body); err != nil {
				return err
			}
		case sticky.Body != body:
			if err := tracker.EditComment(ctx, repo, sticky.ID, body); err != nil {
				return err
			}
		}
	}
	return r.markProjected(ctx, fnd, map[string]string{AnnotationProjectedRunnerImage: state})
}

// runnerImageBody renders the comment from the finding's Repository and its
// runs, or "" when the Repository records no declaration (nothing declared,
// not yet resolved, or the feature off in source-controller).
func (r *FindingReconciler) runnerImageBody(ctx context.Context, fnd *v1alpha1.Finding) (string, error) {
	own := client.MatchingLabels{v1alpha1.LabelFinding: fnd.Name}
	var repos v1alpha1.RepositoryList
	if err := r.List(ctx, &repos, client.InNamespace(fnd.Namespace), own); err != nil {
		return "", fmt.Errorf("list repositories: %w", err)
	}
	src := ownedRepository(repos.Items, fnd)
	if src == nil || src.Status.RunnerImage == nil {
		return "", nil
	}
	var invs v1alpha1.InvestigationList
	if err := r.List(ctx, &invs, client.InNamespace(fnd.Namespace), own); err != nil {
		return "", fmt.Errorf("list investigations: %w", err)
	}
	var rems v1alpha1.RemediationList
	if err := r.List(ctx, &rems, client.InNamespace(fnd.Namespace), own); err != nil {
		return "", fmt.Errorf("list remediations: %w", err)
	}
	c := runnerImageComment(src, imageRuns(fnd, invs.Items, rems.Items))
	c.ApproveCommand = r.approveCommand(ctx, fnd)
	return templates.RenderRunnerImageComment(c)
}

// ownedRepository picks the finding's own Repository out of those carrying
// its label: the one it controls, so a Repository left over from an earlier
// finding of the same name is never quoted.
func ownedRepository(repos []v1alpha1.Repository, fnd *v1alpha1.Finding) *v1alpha1.Repository {
	for i := range repos {
		if owner := metav1.GetControllerOf(&repos[i]); owner != nil && owner.UID == fnd.UID {
			return &repos[i]
		}
	}
	return nil
}

// imageRun is one agent run as the runner-image comment sees it.
type imageRun struct {
	stage   string
	attempt int32
	image   *v1alpha1.RunnerImageRef
	result  *v1alpha1.StageResult
	refused bool
}

// imageRuns flattens the finding's runs, investigations before remediations
// and each in attempt order, which is the order they ran in.
func imageRuns(fnd *v1alpha1.Finding, invs []v1alpha1.Investigation, rems []v1alpha1.Remediation) []imageRun {
	var runs []imageRun
	for i := range invs {
		inv := &invs[i]
		if inv.Spec.FindingRef.UID != "" && inv.Spec.FindingRef.UID != fnd.UID {
			continue
		}
		runs = append(runs, imageRun{
			stage: "investigation", attempt: inv.Spec.Attempt, image: inv.Status.RunnerImage,
			result: inv.Status.Stage, refused: sandboxRefused(inv.Status.Conditions),
		})
	}
	n := len(runs)
	for i := range rems {
		rem := &rems[i]
		if rem.Spec.FindingRef.UID != "" && rem.Spec.FindingRef.UID != fnd.UID {
			continue
		}
		runs = append(runs, imageRun{
			stage: "remediation", attempt: rem.Spec.Attempt, image: rem.Status.RunnerImage,
			result: rem.Status.Stage, refused: sandboxRefused(rem.Status.Conditions),
		})
	}
	byAttempt := func(a, b imageRun) int { return int(a.attempt - b.attempt) }
	slices.SortStableFunc(runs[:n], byAttempt)
	slices.SortStableFunc(runs[n:], byAttempt)
	return runs
}

// sandboxRefused reports a run the sandbox probe refused.
func sandboxRefused(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, v1alpha1.ConditionSandboxRefused)
}

// runnerImageComment maps the Repository's record and the runs onto the
// comment. The Repository says what was declared and what policy made of
// it; the runs say what happened when a pod tried the accepted image, and
// only a run that actually launched on the repository's image counts.
func runnerImageComment(src *v1alpha1.Repository, runs []imageRun) templates.RunnerImageComment {
	ri := src.Status.RunnerImage
	c := templates.RunnerImageComment{
		Manifest: ri.Manifest,
		Declared: ri.Declared,
		Image:    ri.Image,
		Verified: ri.Verified,
		Rejected: ri.Rejected,
		Reason:   ri.Message,
	}
	switch {
	case ri.Rejected != "":
		stalled := meta.FindStatusCondition(src.Status.Conditions, v1alpha1.ConditionStalled)
		c.Parked = stalled != nil && stalled.Status == metav1.ConditionTrue &&
			stalled.Reason == v1alpha1.ReasonRunnerImageRejected
		return c
	case ri.Image == "":
		c.NotApplicable = true
		return c
	}
	for _, run := range runs {
		if run.image == nil || run.image.Source != v1alpha1.RunnerImageSourceRepository {
			continue
		}
		switch {
		case run.refused:
			c.SandboxRefused = &templates.RunnerImageRun{Stage: run.stage, Attempt: run.attempt}
		case run.result != nil && run.result.Outcome == string(envelope.OutcomeImageIncompatible):
			c.Incompatible = &templates.RunnerImageRun{
				Stage: run.stage, Attempt: run.attempt, Detail: run.result.Detail,
			}
		}
	}
	return c
}
