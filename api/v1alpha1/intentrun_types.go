// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IntentRunName returns the deterministic name of one IntentRun of the Intent
// named `intent` — creating the run under it is the run lease:
//
//   - plan:   <intent>-plan-r<round>-a<attempt>
//   - build:  <intent>-bld-r<round>-<repoKey>-a<attempt>
//   - revise: <intent>-rev<round>-<repoKey>-a<attempt>
//
// repoKey is the Project repository's key; a plan run plans from the first
// repository and its name carries none. Within one Intent, distinct (stage,
// round, attempt, and for build and revise runs repoKey) give distinct names,
// and inside the name budget
// every name is at most 63 characters, a valid label value. A name can still
// equal a run name of a different Intent: one whose project name happens to
// embed such a suffix, or the same-named Intent recreated for the issue after
// the TTL deleted the first, whose runs may still be terminating. So a
// controller adopting an existing run on AlreadyExists checks its
// spec.intentRef UID, which the schema requires, and never adopts a run whose
// UID is not its Intent's. An unknown stage returns "".
func IntentRunName(intent string, stage IntentStage, round int32, repoKey string, attempt int32) string {
	switch stage {
	case IntentStagePlan:
		return fmt.Sprintf("%s-plan-r%d-a%d", intent, round, attempt)
	case IntentStageBuild:
		return fmt.Sprintf("%s-bld-r%d-%s-a%d", intent, round, repoKey, attempt)
	case IntentStageRevise:
		return fmt.Sprintf("%s-rev%d-%s-a%d", intent, round, repoKey, attempt)
	default:
		return ""
	}
}

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
	// IntentRunTriggerCommand: an approver's /patchy revise (or /patchy
	// retry) comment, whose id the run records in inputs.commandID.
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
// same feedback twice. What a revise round consumed is recorded on the
// immutable spec — that record is the exactly-once guarantee — so the
// schema requires each trigger's own record (reviewIDs for a review round,
// checkRunIDs or statusIDs for a checks round, commandID for a command
// round) and keeps each record to the rounds it belongs to: a command round
// may also consume the approver reviews since the last round as its
// feedback, while a checks round consumes failed checks only — check runs
// and commit statuses, each in its own id space — so it is never counted
// against maxRevisions.
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
	// executes (a build run's round); zero for a plan run.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	PlanRevision int32 `json:"planRevision,omitempty"`
	// PlanDigest is the approved plan's digest (Intent
	// status.approval.planDigest); the plan bytes are re-hashed at launch
	// and a mismatch stops it. Empty for a plan run.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest,omitempty"`
	// ReviewIDs are the GitHub review ids a review or command round
	// consumed (slice 1b): recorded here, on the immutable spec, so no
	// review ever starts a second round. Required on a review round.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	ReviewIDs []int64 `json:"reviewIDs,omitempty"`
	// CheckRunIDs are the check-run ids whose failure a checks round
	// consumed (slice 1b), so a check failure is consumed exactly once. A
	// checks round records at least one of CheckRunIDs and StatusIDs, and
	// no other round records either.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	CheckRunIDs []int64 `json:"checkRunIDs,omitempty"`
	// StatusIDs are the commit-status ids whose failure (state failure or
	// error) a checks round consumed (slice 1b), for a named check that CI
	// reports as a commit status (its context) rather than a check run.
	// Commit statuses and check runs are separate GitHub id spaces, so a
	// status id is never recorded in CheckRunIDs, where it could equal, and
	// mark consumed, an unrelated check run. A checks round records at
	// least one of CheckRunIDs and StatusIDs, and no other round records
	// either.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	StatusIDs []int64 `json:"statusIDs,omitempty"`
	// CommandID is the GitHub id of the issue comment carrying the /patchy
	// command (revise, or retry) a command round consumed (slice 1b), so
	// a command starts at most one round. Required on a command round, and
	// on no other.
	// +optional
	// +kubebuilder:validation:Minimum=1
	CommandID int64 `json:"commandID,omitempty"`
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
// deterministic name (IntentRunName, from the stage, round, repository key
// and attempt) is the run lease. Runs are kept until their Intent expires, so
// every round's number comes from persisted state that never repeats (see
// Round): a new round can never collide with, and adopt, an earlier round's
// run.
//
// Beyond immutability the schema holds the stage invariants the design's
// security posture rests on, so a malformed run record is refused at
// admission rather than launched: every run names its Intent by UID, a build
// or revise run always pins an approved plan, and a revise run always names
// its trigger, records what that trigger consumed (IntentRunInputs), and
// takes its image from the build round's Repository, named by UID.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new IntentRun for a new attempt"
// +kubebuilder:validation:XValidation:rule="has(self.intentRef.uid) && size(self.intentRef.uid) > 0",message="spec.intentRef.uid is required: a run is adopted on AlreadyExists only by its Intent's UID"
// +kubebuilder:validation:XValidation:rule="!has(self.imageFrom) || (has(self.imageFrom.uid) && size(self.imageFrom.uid) > 0)",message="spec.imageFrom.uid is required: a revise run's image source is pinned by UID"
// +kubebuilder:validation:XValidation:rule="self.stage != 'build' || (has(self.inputs.planRevision) && self.round == self.inputs.planRevision)",message="a build run's round is the approved plan revision it builds (inputs.planRevision)"
// +kubebuilder:validation:XValidation:rule="self.stage == 'plan' || (has(self.inputs.planRevision) && self.inputs.planRevision >= 1 && has(self.inputs.planDigest))",message="build and revise runs pin the approved plan (inputs.planRevision and inputs.planDigest)"
// +kubebuilder:validation:XValidation:rule="(self.stage == 'revise') == has(self.trigger)",message="spec.trigger is set on revise runs, and only on them"
// +kubebuilder:validation:XValidation:rule="(self.stage == 'revise') == has(self.imageFrom)",message="spec.imageFrom is set on revise runs, and only on them"
// +kubebuilder:validation:XValidation:rule="!has(self.trigger) || (self.trigger == 'review' ? has(self.inputs.reviewIDs) : self.trigger == 'checks' ? (has(self.inputs.checkRunIDs) || has(self.inputs.statusIDs)) : has(self.inputs.commandID))",message="a revise run records what its trigger consumed: inputs.reviewIDs for a review round, inputs.checkRunIDs or inputs.statusIDs for a checks round, inputs.commandID for a command round"
// +kubebuilder:validation:XValidation:rule="!has(self.inputs.reviewIDs) || (has(self.trigger) && self.trigger != 'checks')",message="inputs.reviewIDs are consumed only by review and command rounds"
// +kubebuilder:validation:XValidation:rule="!has(self.inputs.checkRunIDs) || (has(self.trigger) && self.trigger == 'checks')",message="inputs.checkRunIDs are consumed only by checks rounds"
// +kubebuilder:validation:XValidation:rule="!has(self.inputs.statusIDs) || (has(self.trigger) && self.trigger == 'checks')",message="inputs.statusIDs are consumed only by checks rounds"
// +kubebuilder:validation:XValidation:rule="!has(self.inputs.commandID) || (has(self.trigger) && self.trigger == 'command')",message="inputs.commandID is consumed only by command rounds"
type IntentRunSpec struct {
	// IntentRef is the owning Intent. Its UID is required (CEL-enforced):
	// run names can repeat across Intents (see IntentRunName), so the UID is
	// what a controller adopting an existing run on AlreadyExists compares.
	IntentRef ObjectReference `json:"intentRef"`
	// Stage the run executes.
	Stage IntentStage `json:"stage"`
	// Trigger names what started a revise round (slice 1b); set on every
	// revise run and on no other.
	// +optional
	Trigger IntentRunTrigger `json:"trigger,omitempty"`
	// Repository is the tree the run works on.
	Repository IntentRunRepository `json:"repository"`
	// Round is the round the run belongs to, 1-based and at most
	// MaxIntentRound, taken from a counter that never repeats:
	//
	//   - plan: the Intent's input revision it plans from
	//     (status.input.revision), which every entry to Planning except a
	//     resume from Blocked advances — so a plan after a replan, or after
	//     a revival whose earlier plan failed before any plan was posted, is
	//     a new round.
	//   - build: the approved plan revision (inputs.planRevision,
	//     CEL-enforced), so a build after a Failed→Planning revival and a
	//     new approval is a new round.
	//   - revise: the Intent's revise-round ordinal (status.rounds, once
	//     advanced for this round), one count over every revise-stage round
	//     whatever its trigger or outcome.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Round int32 `json:"round"`
	// Attempt ordinal within the round, 1-based and at most
	// MaxIntentRunAttempt. A retry of a failed attempt, and a revise
	// round's one head_moved re-run (re-pinned at the moved head), are each
	// the next attempt of the same round; a resume from Blocked continues
	// the round it was blocked in.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	Attempt int32 `json:"attempt"`
	// Inputs pin what the run was given.
	Inputs IntentRunInputs `json:"inputs"`
	// ImageFrom is the build round's Repository, whose accepted runner image
	// a revise run launches on — never the image the pull request head
	// declares, since an agent could otherwise choose its own next sandbox.
	// Set on every revise run and on no other, always with its UID
	// (CEL-enforced), so a same-named Repository can never stand in for it.
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
	// own: changeset_rejected, branch_exists, head_moved, aborted for a run
	// killed with no envelope, push_refused and launch_refused for a push or
	// a Job create refused for itself (GitHub's or the API server's 4xx), and
	// hold_expired for a build whose Job expired while its push waited on a
	// suspension (not counted as an attempt).
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
	// Conditions of the run (Complete, SandboxRefused, PushHeld).
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
// transcript; the changeset itself never appears here. Its name is written
// into label values (LabelIntentRun, and the jobs package's LabelOwner and
// LabelFinding) and a Job is mapped back to its run by that exact value, so
// the schema refuses a name over 63 characters — a backstop, since
// IntentRunName inside the name budget never produces one.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="IntentRun names are at most 63 characters: they are written into label values"
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
