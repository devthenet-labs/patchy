// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IntentStage is the agent stage one IntentRun executes.
// +kubebuilder:validation:Enum=plan;build;revise
type IntentStage string

// Intent stages.
const (
	// IntentStagePlan: read-only planning on the default runner image; the
	// run writes a plan report and changes nothing.
	IntentStagePlan IntentStage = "plan"
	// IntentStageBuild: the initial build of the approved plan, with
	// workspace write, in the repository-declared image; its changeset
	// becomes the branch and pull request.
	IntentStageBuild IntentStage = "build"
	// IntentStageRevise: a revise or check-fix round, pinned at the pull
	// request head and run in the build round's image; its changeset is
	// pushed fast-forward only.
	IntentStageRevise IntentStage = "revise"
)

// IntentRunTrigger names what started a revise round (slice 1b).
// +kubebuilder:validation:Enum=review;command;checks
type IntentRunTrigger string

// Revise-round triggers.
const (
	// IntentRunTriggerReview: a "Request changes" review from an approver.
	IntentRunTriggerReview IntentRunTrigger = "review"
	// IntentRunTriggerCommand: an approver's /patchy revise.
	IntentRunTriggerCommand IntentRunTrigger = "command"
	// IntentRunTriggerChecks: a named check failed on patchy's own head.
	IntentRunTriggerChecks IntentRunTrigger = "checks"
)

// IntentRunRepository is the tree one run works on.
type IntentRunRepository struct {
	// URL is the https URL of the app repository (one of the Project's).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	URL string `json:"url"`
	// RepositoryRef names the Repository artifact this run owns and works
	// from: the default branch head for plan and build runs, the pull
	// request head (spec.ref.branch = the intent branch) for revise runs.
	RepositoryRef LocalObjectReference `json:"repositoryRef"`
}

// IntentRunInputs pins exactly what the run was given, so the record says
// what the agent saw and a restart or a repeated poll can never consume the
// same feedback twice.
type IntentRunInputs struct {
	// ConfigMap names the run's own input ConfigMap (owned by this run):
	// the issue.md and investigation.md handed to the Job — the intent
	// snapshot, and for build and revise runs the approved plan plus, for a
	// revise run, that round's feedback and compare patch.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// InputDigest is the digest of the Intent's issue snapshot the run was
	// given (Intent status.input.digest).
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	InputDigest string `json:"inputDigest"`
	// PlanRevision is the approved plan revision a build or revise run
	// executes; zero for a plan run.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PlanRevision int32 `json:"planRevision,omitempty"`
	// PlanDigest is the approved plan's digest (Intent
	// status.approval.planDigest); the plan bytes are re-hashed at launch
	// and a mismatch stops it. Empty for a plan run.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest,omitempty"`
	// ReviewIDs are the GitHub review ids a revise round consumed (slice
	// 1b): recorded here, on the immutable spec, so no review ever starts
	// a second round.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	ReviewIDs []int64 `json:"reviewIDs,omitempty"`
	// CheckRunIDs are the check-run ids whose failure a check-fix round
	// consumed (slice 1b), so a check failure is consumed exactly once.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	CheckRunIDs []int64 `json:"checkRunIDs,omitempty"`
}

// IntentRunGrant is what the run was granted: the Project's per-stage limits
// clamped by the controller's per-stage ceilings.
type IntentRunGrant struct {
	// MaxTurns granted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxTurns int32 `json:"maxTurns,omitempty"`
	// TokenBudget granted (output tokens; the runner's kill switch).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000000
	TokenBudget int64 `json:"tokenBudget,omitempty"`
	// TimeoutMilliseconds bounds the stage's wall clock, inside the Job
	// deadline (itself inside the broker caller token's lifetime), at most
	// 24h.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400000
	TimeoutMilliseconds int64 `json:"timeoutMilliseconds,omitempty"`
}

// IntentRunSpec identifies one attempt of one stage on one repository. It is
// immutable — a new attempt is a new IntentRun (CEL-enforced) — and
// intent-controller writes it once, at creation. Creating it under its
// deterministic name is the run lease:
//
//   - plan:   <intent>-plan-r<round>-a<attempt>
//   - build:  <intent>-bld-<repository key>-a<attempt>
//   - revise: <intent>-rev<round>-<repository key>-a<attempt>
//
// Beyond immutability the schema holds the stage invariants the design's
// security posture rests on, so a malformed run record is refused at
// admission rather than launched: a build or revise run always pins an
// approved plan, and a revise run always names its trigger and takes its
// image from the build round's Repository.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new IntentRun for a new attempt"
// +kubebuilder:validation:XValidation:rule="self.stage == 'build' ? self.round == 0 : self.round >= 1",message="spec.round is 0 for a build run and at least 1 for plan and revise runs"
// +kubebuilder:validation:XValidation:rule="self.stage == 'plan' || (has(self.inputs.planRevision) && self.inputs.planRevision >= 1 && has(self.inputs.planDigest))",message="build and revise runs pin the approved plan (inputs.planRevision and inputs.planDigest)"
// +kubebuilder:validation:XValidation:rule="(self.stage == 'revise') == has(self.trigger)",message="spec.trigger is set on revise runs, and only on them"
// +kubebuilder:validation:XValidation:rule="(self.stage == 'revise') == has(self.imageFrom)",message="spec.imageFrom is set on revise runs, and only on them"
type IntentRunSpec struct {
	// IntentRef is the owning Intent (UID-pinned).
	IntentRef ObjectReference `json:"intentRef"`
	// Stage the run executes.
	Stage IntentStage `json:"stage"`
	// Trigger names what started a revise round (slice 1b); set on every
	// revise run and on no other.
	// +optional
	Trigger IntentRunTrigger `json:"trigger,omitempty"`
	// Repository is the tree the run works on.
	Repository IntentRunRepository `json:"repository"`
	// Round is the plan revision for a plan run, zero for a build run, and
	// the revise round (1-based) for a revise run.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	Round int32 `json:"round"`
	// Attempt ordinal within the round, 1-based.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	Attempt int32 `json:"attempt"`
	// Inputs pin what the run was given.
	Inputs IntentRunInputs `json:"inputs"`
	// ImageFrom is the build round's Repository (UID-pinned), whose
	// accepted runner image a revise run launches on — never the image the
	// pull request head declares, since an agent could otherwise choose its
	// own next sandbox. Set on every revise run and on no other.
	// +optional
	ImageFrom *ObjectReference `json:"imageFrom,omitempty"`
	// Grant bounds the run.
	// +optional
	Grant IntentRunGrant `json:"grant,omitempty"`
	// PreviousAttempt is the failed attempt this one retries, rendered into
	// the prompt so the agent does not repeat the failure. Nil on a first
	// attempt.
	// +optional
	PreviousAttempt *PreviousAttempt `json:"previousAttempt,omitempty"`
}

// IntentRunStatus records how the run ran. Written only by
// intent-controller's run scheduler.
type IntentRunStatus struct {
	// Phase of the run.
	// +optional
	Phase RunPhase `json:"phase,omitempty"`
	// JobRef locates the agent Job in the agents namespace.
	// +optional
	JobRef *JobReference `json:"jobRef,omitempty"`
	// RunnerImage is the image the Job actually launched on, written beside
	// JobRef from what the Job client returned — never from configuration.
	// A build or revise run that reports the default image is refused.
	// +optional
	RunnerImage *RunnerImageRef `json:"runnerImage,omitempty"`
	// BaseSHA is the commit the run worked on (its Repository's pin), which
	// the changeset's base must equal.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	BaseSHA string `json:"baseSHA,omitempty"`
	// PushedCommit is the commit created from the run's changeset, recorded
	// before any ref moves so a restart can adopt its own ref.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	PushedCommit string `json:"pushedCommit,omitempty"`
	// Outcome is the envelope outcome vocabulary, plus the controller's
	// own: changeset_rejected, branch_exists, head_moved, and aborted for a
	// run killed with no envelope.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Outcome string `json:"outcome,omitempty"`
	// Report is the run's report markdown (for a plan run, the plan; the
	// raw bytes also land in the Intent's immutable plan ConfigMap).
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	Report string `json:"report,omitempty"`
	// Detail explains a non-ok outcome for humans. Untrusted: it can quote
	// output from the repository or image the run executed on.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Detail string `json:"detail,omitempty"`
	// Usage accounting for the run, as the harness reported it.
	// +optional
	Usage UsageSummary `json:"usage,omitempty"`
	// Transcript points at the conversation the run produced, in a
	// ConfigMap owned by this run.
	// +optional
	Transcript *TranscriptRef `json:"transcript,omitempty"`
	// StartedAt is when the Job launched.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the run was collected.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Conditions of the run (Complete, SandboxRefused).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the last spec generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=irun,categories=patchy
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.spec.stage`
// +kubebuilder:printcolumn:name="Round",type=integer,JSONPath=`.spec.round`
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.spec.attempt`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.outcome`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository.url`,priority=1
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger`,priority=1
// +kubebuilder:printcolumn:name="Cost",type=string,JSONPath=`.status.usage.costUSD`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IntentRun is one immutable attempt of one agent stage (plan, build or
// revise) on one repository for an Intent, which owns it. It carries the
// jobs finalizer and owns its Repository, its input ConfigMap and its
// transcript; the changeset itself never appears here.
type IntentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentRunSpec `json:"spec"`
	// +optional
	Status IntentRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IntentRunList contains a list of IntentRun.
type IntentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentRun `json:"items"`
}
