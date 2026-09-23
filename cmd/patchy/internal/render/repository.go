// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package render

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
)

// RepositoryDetail renders everything known about one repository snapshot:
// the pinned commit and artifact, its readiness, and the agent runner image
// its tree declared.
func RepositoryDetail(d *printer.Doc, repo *v1alpha1.Repository, now time.Time) {
	d.Section(fmt.Sprintf("Repository %s", repo.Name)).
		Field("URL", repo.Spec.URL).
		Field("Branch", repo.Spec.Ref.Branch).
		Field("Finding", repo.Labels[v1alpha1.LabelFinding]).
		Field("Ready", conditionLine(repo.Status.Conditions, v1alpha1.ConditionReady)).
		Field("Stalled", conditionLine(repo.Status.Conditions, v1alpha1.ConditionStalled)).
		Field("Commit", repo.Status.ResolvedSHA).
		Field("Age", Age(repo.CreationTimestamp.Time, now))
	if f := repo.Status.Forge; f != nil {
		d.Field("Forge", f.Name)
	}
	if a := repo.Status.Artifact; a != nil {
		d.Section("Artifact").
			Field("Digest", a.Digest).
			Fieldf("Size", "%d bytes", a.SizeBytes).
			Field("Fetched", Timestamp(a.LastFetchedAt, now))
	}
	RunnerImage(d, repo.Status.RunnerImage, nil, now)
}

// RunnerImage renders the agent runner image a Repository's tree declared
// and what became of it: the declaring file, the declared and the pinned
// reference, whether the signature was verified, and the rejection or
// not-applicable reason. ran, when given, is what a run actually launched
// on; its source is then the one shown, since a pinned image may still have
// been skipped (repository images off in the job controllers, the sandbox
// breaker tripped, a human revival). Without it the source is the one a
// launch would pick. Nothing is rendered when nothing was declared and
// nothing ran.
func RunnerImage(d *printer.Doc, ri *v1alpha1.RunnerImage, ran *v1alpha1.RunnerImageRef, now time.Time) {
	d.Section("Runner image")
	if ri != nil {
		d.Field("Declared in", ri.Manifest).
			Field("Declared", ri.Declared).
			Field("Pinned", ri.Image)
		switch {
		case ri.Rejected != "":
			d.Fieldf("Rejected", "%s — %s", ri.Rejected, ri.Message)
		case ri.Image == "":
			d.Field("Not applicable", ri.Message)
		case ri.Verified:
			d.Field("Signature", "verified")
		default:
			d.Field("Signature", "not verified (the operator allows unsigned images)")
		}
		d.Field("Resolved", Timestamp(ri.ResolvedAt, now))
	}
	switch {
	case ran != nil:
		d.Field("Source", ran.Source).Field("Ran on", ran.Image)
	case ri != nil && ri.Image != "":
		d.Field("Source", v1alpha1.RunnerImageSourceRepository)
	case ri != nil:
		d.Field("Source", v1alpha1.RunnerImageSourceDefault)
	}
}

// RunnerImageRef renders the image one run launched on, on one line.
func RunnerImageRef(ref *v1alpha1.RunnerImageRef) string {
	if ref == nil {
		return ""
	}
	if ref.Manifest != "" {
		return fmt.Sprintf("%s (%s, declared in %s)", ref.Image, ref.Source, ref.Manifest)
	}
	return fmt.Sprintf("%s (%s)", ref.Image, ref.Source)
}

// conditionLine renders one condition as its status, reason and message.
func conditionLine(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return ""
	}
	s := string(c.Status)
	if c.Reason != "" {
		s += " " + c.Reason
	}
	if c.Message != "" {
		s += " — " + c.Message
	}
	return s
}
