// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PreviewPhase is the local state of a disposable runtime preview. It never
// participates in Finding or Intent phase transitions.
// +kubebuilder:validation:Enum=Pending;Queued;Deploying;Ready;Failed;Expired
type PreviewPhase string

// Preview phases.
const (
	PreviewPending   PreviewPhase = "Pending"
	PreviewQueued    PreviewPhase = "Queued"
	PreviewDeploying PreviewPhase = "Deploying"
	PreviewReady     PreviewPhase = "Ready"
	PreviewFailed    PreviewPhase = "Failed"
	PreviewExpired   PreviewPhase = "Expired"
)

// PreviewComponent is the fixed application shape copied from an operator's
// Project, with only Revision taken from the live, recorded PR head.
type PreviewComponent struct {
	// Name identifies the component and its rendered resources.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=16
	Name string `json:"name"`
	// ImageRepository is the operator-configured repository, without a tag.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.:-]*/patchy/previews/[a-z0-9-]+$`
	// +kubebuilder:validation:MaxLength=255
	ImageRepository string `json:"imageRepository"`
	// Revision is the full GitHub pull-request head SHA, never a branch name.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`
	// Port and ReadinessPath are operator configuration, not agent input.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:Pattern=`^/[a-zA-Z0-9/_-]*$`
	// +kubebuilder:validation:MaxLength=128
	ReadinessPath string `json:"readinessPath"`
}

// PreviewSpec is authored by intent-controller in the release namespace.
// A PR head change updates Revision; an operator's Project change may also
// update the component runtime configuration.
type PreviewSpec struct {
	// IntentRef pins the exact Intent; a recycled name cannot adopt a preview.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="intentRef is immutable"
	IntentRef ObjectReference `json:"intentRef"`
	// HostLabel is the single DNS label before the operator's host suffix.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostLabel is immutable"
	HostLabel string `json:"hostLabel"`
	// Slice 2 has one application repository and one runtime component.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	Components []PreviewComponent `json:"components"`
	// TTL is at most 72 hours since the last successful deployment. It is
	// also a cap on queued/deploying previews, so they cannot hold a slot
	// forever if the image never becomes available.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h') && duration(self) <= duration('72h')",message="ttl must be between 1h and 72h"
	TTL metav1.Duration `json:"ttl"`
}

// PreviewComponentStatus records the image the kubelet actually ran.
type PreviewComponentStatus struct {
	Name string `json:"name"`
	// ImageID is the runtime's digest-bearing image ID once a Pod is Ready.
	// +optional
	ImageID string `json:"imageID,omitempty"`
}

// PreviewStatus is written only by preview-controller. Slot is held until
// finalizer cleanup has removed every rendered object from that slot.
type PreviewStatus struct {
	// +optional
	Phase PreviewPhase `json:"phase,omitempty"`
	// ObservedGeneration distinguishes an expired or failed revision from a
	// later PR-head update that may legitimately start a fresh deployment.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Slot *int32 `json:"slot,omitempty"`
	// +optional
	URL string `json:"url,omitempty"`
	// ObservedRevision is the revision this slot currently serves or tried.
	// +optional
	ObservedRevision string `json:"observedRevision,omitempty"`
	// +optional
	// +kubebuilder:validation:Maximum=3
	Retries int32 `json:"retries,omitempty"`
	// +optional
	AttemptStartedAt *metav1.Time `json:"attemptStartedAt,omitempty"`
	// +optional
	LastDeployedAt *metav1.Time `json:"lastDeployedAt,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=1
	Components []PreviewComponentStatus `json:"components,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// Preview deploys one approved PR head into one of the chart's fixed,
// isolated slots. It owns no namespace or infrastructure resource.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Slot",type=integer,JSONPath=`.status.slot`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Preview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PreviewSpec   `json:"spec"`
	Status            PreviewStatus `json:"status,omitempty"`
}

// PreviewList is a list of Previews.
// +kubebuilder:object:root=true
type PreviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Preview `json:"items"`
}
