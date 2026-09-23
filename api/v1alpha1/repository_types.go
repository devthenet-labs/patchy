// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepositoryRef pins what to clone.
type RepositoryRef struct {
	// Branch to resolve; source-controller resolves it to a SHA exactly once
	// and pins status.resolvedSHA.
	// +optional
	Branch string `json:"branch,omitempty"`
}

// RepositorySpec asks source-controller to produce a SHA-pinned clone
// artifact of one repository.
type RepositorySpec struct {
	// URL is the normalized https URL of the repository to clone.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.url is immutable"
	URL string `json:"url"`
	// Ref selects what to clone.
	// +optional
	Ref RepositoryRef `json:"ref,omitempty"`
}

// Artifact is the served source tarball: the forge's archive of the working
// tree at ResolvedSHA, under one top-level directory and with no .git
// directory (source-controller downloads it over plain HTTP and never runs
// git). The agent Job's prepare init creates the synthetic base commit the
// agent's commit and changeset flow diffs against.
type Artifact struct {
	// URL the artifact is served at (cluster-internal; the path embeds an
	// unguessable random id).
	URL string `json:"url"`
	// Digest is the sha256 of the tarball, verified by the fetching init
	// container.
	Digest string `json:"digest"`
	// SizeBytes of the tarball.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// LastFetchedAt is when the clone was produced.
	// +optional
	LastFetchedAt *metav1.Time `json:"lastFetchedAt,omitempty"`
}

// RunnerImage records the agent runner image the repository declared and
// the digest-pinned reference every policy check ran against. It has one
// writer, source-controller, which resolves it exactly once per Repository
// beside ResolvedSHA (the declaration is read from the stored tarball, so it
// is bound to the pinned tree): a moved tag can never change the environment
// between the investigation and the remediation of one finding. Nil when the
// tree declares no image or the feature is disabled; the job controllers
// then run the per-harness runner image.
type RunnerImage struct {
	// Declared is the reference exactly as the declaring file names it, in
	// tag or digest form.
	// +optional
	Declared string `json:"declared,omitempty"`
	// Manifest is the repository-relative path of the declaring file:
	// ".patchy/agent.yaml" or ".devcontainer/devcontainer.json". It is the
	// one record of which file won the precedence, so no separate source
	// field exists; empty when neither file exists.
	// +optional
	Manifest string `json:"manifest,omitempty"`
	// Image is the digest-pinned reference (name@sha256:...) that was
	// checked, verified and recorded; a launching Job runs exactly this.
	// Empty when the declaration was rejected.
	// +optional
	Image string `json:"image,omitempty"`
	// SearchPath is the image's sanitized PATH: absolute entries only, no
	// empty component, the runc default when the image config carries none.
	// The Job prepends its own binary directory to it.
	// +optional
	SearchPath string `json:"searchPath,omitempty"`
	// Verified reports that a signature by the operator's cosign key was
	// checked against Image; false when the operator allows unsigned images.
	// +optional
	Verified bool `json:"verified,omitempty"`
	// Rejected is the short reason the declaration failed policy (the
	// RunnerImageRejected condition's detail, e.g. not allowlisted, unsigned,
	// oversized); empty when the image was accepted. A rejected Repository
	// keeps its artifact, so a human-revived finding runs on the default
	// image with this recorded.
	// +optional
	Rejected string `json:"rejected,omitempty"`
	// Message is the human-readable explanation behind Rejected, mirrored
	// from the Stalled condition so the tracking issue can quote it.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Message string `json:"message,omitempty"`
	// ResolvedAt is when resolution finished, accepted or rejected.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// RepositoryStatus is the repository artifact's observed state.
type RepositoryStatus struct {
	// Conditions of the repository (Ready: artifact available and, when the
	// tree declares a runner image, that image resolved; Stalled: cannot
	// clone, artifact over the size cap, or runner image rejected).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the last spec generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ResolvedSHA is the commit the artifact is pinned to; investigation and
	// remediation both work this exact tree, and it is the push base.
	// +optional
	ResolvedSHA string `json:"resolvedSHA,omitempty"`
	// Forge names the Forge whose credentials cloned the repository.
	// +optional
	Forge *LocalObjectReference `json:"forge,omitempty"`
	// Artifact is the served tarball.
	// +optional
	Artifact *Artifact `json:"artifact,omitempty"`
	// RunnerImage is the repository-declared agent image, pinned to a digest
	// exactly once beside ResolvedSHA (source-controller). Nil when the tree
	// declares none or the feature is disabled.
	// +optional
	RunnerImage *RunnerImage `json:"runnerImage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=repo,categories=patchy
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="SHA",type=string,JSONPath=`.status.resolvedSHA`,priority=1
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.runnerImage.image`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Repository is a SHA-pinned clone artifact: source-controller clones the
// repository with Forge credentials and serves it as a tarball that agent
// jobs fetch credential-lessly. Owned by a Finding and garbage-collected
// with it.
type Repository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RepositorySpec `json:"spec"`
	// +optional
	Status RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository.
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}
