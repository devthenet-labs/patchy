// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Project defaults. The schema applies each one server-side (the
// +kubebuilder:default markers below carry the same literals, and the schema
// envtest pins the two together); they are exported for code that reads a
// Project the API server never defaulted, such as a fake client's.
const (
	// DefaultApproveLabel is the label an approver adds to an intent issue
	// to approve its current plan, when a Project names none.
	DefaultApproveLabel = "patchy:approved"
	// DefaultMaxActiveIntents is limits.maxActiveIntents when unset.
	DefaultMaxActiveIntents int32 = 2
	// DefaultMaxRevisions is limits.maxRevisions when unset.
	DefaultMaxRevisions int32 = 3
	// DefaultMaxCheckFixes is limits.maxCheckFixes when unset.
	DefaultMaxCheckFixes int32 = 2
	// DefaultMaxCostMicroUSD is limits.maxCostMicroUSD when unset ($10).
	DefaultMaxCostMicroUSD int64 = 10000000
)

// ProjectLabels names the GitHub labels a Project reacts to on its intent
// repository. GitHub compares label names case-insensitively, and so does
// every check below.
// +kubebuilder:validation:XValidation:rule="!has(self.trigger) || !has(self.approve) || self.trigger.lowerAscii() != self.approve.lowerAscii()",message="labels.trigger and labels.approve must differ"
type ProjectLabels struct {
	// Trigger is the label that both starts an intent and names this
	// Project: an issue carrying it becomes the Intent <project>-<issue>,
	// and an approver re-applying it asks for a replan. Empty means
	// "patchy:<project name>", which intent-controller derives (a schema
	// default cannot see the object's name) — so a Project named "approved"
	// must set it explicitly or its derived trigger equals the default
	// approve label, which the controller reports as not Ready. Two
	// Projects sharing an intent repository must use different triggers.
	// 50 characters is GitHub's own label-name limit.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50
	Trigger string `json:"trigger,omitempty"`
	// Approve is the label an approver adds to approve the plan posted on
	// the intent issue — the alias of `/patchy approve`. The approval is
	// accepted only from an approver, after the plan was posted, and while
	// both the plan comment and the issue body still hash to what was
	// planned.
	// +optional
	// +kubebuilder:default="patchy:approved"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=50
	Approve string `json:"approve,omitempty"`
}

// ProjectApprovers is the allowlist of humans whose actions on this Project's
// intents carry authority.
type ProjectApprovers struct {
	// Logins are the GitHub logins whose trigger, approval, commands and
	// review feedback count; everyone else's are ignored (a trigger from
	// anyone else closes the intent with one notice). Compared
	// case-insensitively. A Bot never counts, so the pattern admits no
	// "[bot]" suffix. The operator owns this list; it is never derived from
	// author_association.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Logins []string `json:"logins"`
}

// ProjectRepository is one application repository the Project's intents may
// build in.
type ProjectRepository struct {
	// Name is the repository's short key within the Project, used in the
	// names of the IntentRuns and Repositories created for it
	// (<intent>-bld-<name>-a<n>), hence a DNS label of at most 32
	// characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// URL is the repository's https URL (https://github.com/<owner>/<name>).
	// It must resolve to exactly one Forge, and the App must be installed on
	// it, or the Project is not Ready. No credentials in the URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	URL string `json:"url"`
}

// StageLimits bound one agent stage of an intent. Zero (or omitted) means the
// controller's per-stage default; a value above the controller's per-stage
// ceiling (its flags) is clamped to it, so a Project can lower spend but never
// raise it past what the operator runs the controller with.
type StageLimits struct {
	// MaxTurns the agent may take in one run of the stage.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MaxTurns int32 `json:"maxTurns,omitempty"`
	// TokenBudget is the output-token budget of one run of the stage (the
	// runner's kill switch).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000000
	TokenBudget int64 `json:"tokenBudget,omitempty"`
}

// ProjectLimits bound the spend of a Project's intents before it starts.
// The global bound is intent-controller's --max-concurrent-runs slot pool.
type ProjectLimits struct {
	// MaxActiveIntents is how many of this Project's intents may be
	// non-terminal at once; a newly triggered issue past it waits.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	MaxActiveIntents int32 `json:"maxActiveIntents,omitempty"`
	// MaxRevisions is how many review-driven revise rounds one intent may
	// run; the next is refused and the Intent goes Blocked with
	// RevisionLimitReached until the limit is raised. A head_moved re-run
	// does not count. Zero is meaningful (no revise rounds), so the field
	// is a pointer: nil means the default.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxRevisions *int32 `json:"maxRevisions,omitempty"`
	// MaxCheckFixes is how many automatic check-fix rounds one intent may
	// run, counted separately from MaxRevisions but under the same cost
	// ceiling (slice 1b). Zero is meaningful (no check-fix rounds), so the
	// field is a pointer: nil means the default.
	// +optional
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxCheckFixes *int32 `json:"maxCheckFixes,omitempty"`
	// MaxCostMicroUSD is the per-intent spend ceiling in micro-USD (10000000
	// = $10), checked before every launch; past it the Intent goes Blocked
	// with BudgetExhausted. Advisory for build and revise runs, whose
	// reported usage comes from a repository-declared image and is
	// untrusted — the per-run limits, the Job deadline and MaxRevisions are
	// what is actually enforced there. The ceiling is capped at $1000.
	// +optional
	// +kubebuilder:default=10000000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000000
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD,omitempty"`
	// Plan bounds each planning run (read-only, default runner image).
	// +optional
	Plan StageLimits `json:"plan,omitempty"`
	// Build bounds each initial build run.
	// +optional
	Build StageLimits `json:"build,omitempty"`
	// Revise bounds each revise or check-fix run.
	// +optional
	Revise StageLimits `json:"revise,omitempty"`
}

// ProjectChecks configures automatic fix rounds on failed CI checks of the
// pull requests patchy opened (slice 1b).
type ProjectChecks struct {
	// Fix names the checks (check-run or commit-status names, e.g. "test",
	// "lint") whose failure on patchy's own head starts a fix round without
	// a human. Empty means patchy never auto-fixes: a failing check is
	// reported on the intent issue and an approver can run /patchy revise.
	// The list is explicit so a flaky or unrelated check cannot burn
	// budget.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	Fix []string `json:"fix,omitempty"`
	// Timeout is how long after a push the controller waits for the named
	// checks to settle before it stops watching them, between 1m and 6h. A
	// pointer so that an unset value is omitted and the schema default
	// applies (a zero metav1.Duration would serialize as "0s").
	// +optional
	// +kubebuilder:default="30m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('6h')",message="checks.timeout must be between 1m and 6h"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// ProjectSpec is operator configuration: where intents are filed, who may
// authorise them, which repositories they build in, and what they may spend.
// The operator writes it (through the patchy-config chart); intent-controller
// only reads it. Writing projects is admin-only in RBAC, because a Project is
// the power to point agents at repositories.
type ProjectSpec struct {
	// IntentRepository is the https URL of the repository whose issues are
	// this Project's intents (e.g. https://github.com/acme/intents). Several
	// Projects may share one, told apart by their trigger labels. The App
	// must be installed on it and it must resolve to exactly one Forge.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	IntentRepository string `json:"intentRepository"`
	// Labels names the trigger and approve labels.
	// +optional
	// +kubebuilder:default={}
	Labels ProjectLabels `json:"labels,omitempty"`
	// Approvers is the allowlist whose actions count.
	Approvers ProjectApprovers `json:"approvers"`
	// Repositories are the application repositories intents build in; the
	// first entry is also the planning repository. The schema admits up to
	// 8 — the multi-repo shape — but slice 1 builds in exactly one:
	// intent-controller reports a Project with more as not Ready rather
	// than the schema refusing it, so the CRD does not change when
	// multi-repo intents land.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Repositories []ProjectRepository `json:"repositories"`
	// Limits bound each intent's spend.
	// +optional
	// +kubebuilder:default={}
	Limits ProjectLimits `json:"limits,omitempty"`
	// Checks configures automatic check-fix rounds (slice 1b).
	// +optional
	// +kubebuilder:default={}
	Checks ProjectChecks `json:"checks,omitempty"`
	// RequireRepositoryImage, true by default, launches build and revise
	// runs only on an accepted repository-declared runner image (the
	// Repository's pinned, not-rejected image) and blocks the Intent with
	// ImageRequired otherwise. False lets them fall back to the default
	// runner image, which carries no toolchain.
	// +optional
	// +kubebuilder:default=true
	RequireRepositoryImage *bool `json:"requireRepositoryImage,omitempty"`
	// Suspend stops the Project: no new intents are discovered and no run
	// of its intents is launched. Running Jobs finish.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ProjectStatus is the Project's observed state. Written only by
// intent-controller's project reconciler.
type ProjectStatus struct {
	// Conditions of the Project. Ready is True when every repository
	// resolves to exactly one Forge, the App is installed on the intent
	// repository and every app repository, and the labels exist; False
	// reasons include ForgeUnresolved, AppNotInstalled and
	// AmbiguousIntentRepository.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the last spec generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ActiveIntents counts this Project's non-terminal Intents, against
	// spec.limits.maxActiveIntents.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ActiveIntents int32 `json:"activeIntents,omitempty"`
	// LastPolledAt is when the intent repository's trigger-labelled issues
	// were last listed.
	// +optional
	LastPolledAt *metav1.Time `json:"lastPolledAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=proj,categories=patchy
// +kubebuilder:printcolumn:name="IntentRepo",type=string,JSONPath=`.spec.intentRepository`,description="The intent repository"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeIntents`,description="Non-terminal intents"
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`,priority=1
// +kubebuilder:printcolumn:name="Polled",type=date,JSONPath=`.status.lastPolledAt`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Project is operator configuration for intent-driven development: an intent
// repository whose trigger-labelled issues become Intents, the approvers
// whose actions count, the application repositories the work is built in,
// and the limits on what it may spend. Its name is at most 63 characters: it
// prefixes every Intent's name (<project>-<issue>) and Intent.spec.project
// holds it.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="Project names are at most 63 characters"
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectSpec `json:"spec"`
	// +optional
	Status ProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}
