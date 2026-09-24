// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IntentName returns the name of the Intent for issue `issue` of the Project
// named `project`: <project>-<issue>. The schema holds every Intent to it, and
// inside the name budget it is at most 33 characters.
func IntentName(project string, issue int64) string {
	return project + "-" + strconv.FormatInt(issue, 10)
}

// IntentBranchPrefix prefixes every branch intent-controller creates in an
// app repository (patchy-intent/<intent>). It deliberately never matches the
// Finding flow's "patchy/" branch check, so the Finding pull-request handler
// can never act on an intent pull request.
const IntentBranchPrefix = "patchy-intent/"

// MaxIntentPhaseTimes bounds status.phaseTimes. Replans and review rounds
// are human-driven and so unbounded in number; SetIntentPhase keeps the most
// recent entries so the log can never outgrow the schema's MaxItems.
const MaxIntentPhaseTimes = 64

// IntentPhase is the lifecycle of one intent. It is a local enum, not part of
// the Finding phase taxonomy in transitions.go, and its edge table
// (intentTransitions below) is separate from Finding's: intent-controller is
// the single writer of every edge.
// +kubebuilder:validation:Enum=Pending;Planning;AwaitingApproval;Building;InReview;Revising;Blocked;Merged;Closed;Failed
type IntentPhase string

// Intent phases.
const (
	// IntentPending: discovered, the trigger's authority not yet decided.
	IntentPending IntentPhase = "Pending"
	// IntentPlanning: a plan run is pending, running, or being written back.
	IntentPlanning IntentPhase = "Planning"
	// IntentAwaitingApproval: the plan is posted on the issue; waiting for an
	// approver's approval (or a replan request).
	IntentAwaitingApproval IntentPhase = "AwaitingApproval"
	// IntentBuilding: the approved plan is being built into a changeset,
	// pushed, and opened as pull requests.
	IntentBuilding IntentPhase = "Building"
	// IntentInReview: every pull request is open; waiting on reviews, checks
	// and merge.
	IntentInReview IntentPhase = "InReview"
	// IntentRevising: a revise or check-fix round is running against the
	// pull request head.
	IntentRevising IntentPhase = "Revising"
	// IntentBlocked: a limit or a precondition stops progress (revision
	// limit, cost ceiling, missing or rejected repository image, tripped
	// sandbox breaker, repeated check failure); the conditions say which.
	// Re-evaluated when the Project changes, so raising a limit resumes the
	// intent in the phase it was blocked from (IntentBlockedFrom).
	IntentBlocked IntentPhase = "Blocked"
	// IntentMerged: every pull request merged — from InReview, or by a
	// human mid-round or while Blocked; the issue is closed as completed.
	// Terminal.
	IntentMerged IntentPhase = "Merged"
	// IntentClosed: a human closed the issue or ran /patchy cancel, every
	// pull request was closed unmerged, or the trigger was not an
	// approver's. Terminal.
	IntentClosed IntentPhase = "Closed"
	// IntentFailed: plan or build attempts exhausted, or the plan was
	// invalid twice. Stamps completedAt, but an approver re-applying the
	// trigger label revives it to Planning.
	IntentFailed IntentPhase = "Failed"
)

// intentTransitions is the Intent's legal edge table. Every edge has one
// writer component, intent-controller; the reconciler and the reason are
// noted per edge:
//
//   - ""→Pending: the intent reconciler's first status write on an Intent the
//     project reconciler created (the create itself is spec-only).
//   - Pending→Planning: the trigger label was applied by an approver.
//   - Pending→Closed: the trigger was not an approver's (or was a Bot's); one
//     notice is posted.
//   - Planning→AwaitingApproval: a valid plan was posted on the issue.
//   - Planning→Failed: plan attempts exhausted, or the plan invalid twice.
//   - AwaitingApproval→Building: an approval was accepted (approve label or
//     /patchy approve, bound to the plan and input digests).
//   - AwaitingApproval→Planning: an approver asked for a replan (re-applied
//     the trigger label or ran /patchy replan).
//   - Building→InReview: every pull request is open.
//   - Building→Failed: build attempts exhausted.
//   - InReview→Revising: a review round, /patchy revise, or a check-fix round
//     started (slice 1b).
//   - Revising→InReview: the round pushed, or failed (a condition is set and
//     a notice posted; a failed round never fails the intent).
//   - InReview→Merged: every pull request merged.
//   - Revising→Merged, Blocked→Merged: every pull request merged by a human
//     during a round or while the intent was blocked (pull request state is
//     polled in both). A merge is the human's final word, so the intent
//     completes directly: the running round's Job is deleted through the
//     finalizer, and no false resume through InReview is recorded while a
//     block still holds. These mirror the edges to Closed.
//   - every non-terminal phase→Blocked: a limit or precondition stopped it.
//   - Blocked→the phase it was blocked from: the Project changed and the
//     block no longer holds (IntentBlockedFrom names the target).
//   - every non-terminal phase→Closed: a human closed the issue or ran
//     /patchy cancel, or every pull request was closed unmerged.
//   - Failed→Planning: revival — an approver re-applied the trigger label.
var intentTransitions = map[IntentPhase][]IntentPhase{
	"":                     {IntentPending},
	IntentPending:          {IntentPlanning, IntentBlocked, IntentClosed},
	IntentPlanning:         {IntentAwaitingApproval, IntentBlocked, IntentClosed, IntentFailed},
	IntentAwaitingApproval: {IntentBuilding, IntentPlanning, IntentBlocked, IntentClosed},
	IntentBuilding:         {IntentInReview, IntentBlocked, IntentClosed, IntentFailed},
	IntentInReview:         {IntentRevising, IntentMerged, IntentBlocked, IntentClosed},
	IntentRevising:         {IntentInReview, IntentMerged, IntentBlocked, IntentClosed},
	IntentBlocked: {
		IntentPending, IntentPlanning, IntentAwaitingApproval,
		IntentBuilding, IntentInReview, IntentRevising, IntentMerged, IntentClosed,
	},
	IntentMerged: nil,
	IntentClosed: nil,
	IntentFailed: {IntentPlanning}, // revival by an approver's trigger label
}

// intentTerminal is the set of phases that complete an Intent for TTL
// purposes (status.completedAt is set on entry). Merged and Closed have no
// outgoing edges; Failed is revivable, and revival clears completedAt.
// Blocked is NOT terminal: a blocked intent waits for a human and must not
// expire while it does.
var intentTerminal = map[IntentPhase]bool{
	IntentMerged: true,
	IntentClosed: true,
	IntentFailed: true,
}

// CanTransitionIntent reports whether moving an Intent from phase `from` to
// `to` is legal. The empty phase means a new Intent. Self-transitions are
// always legal no-ops.
func CanTransitionIntent(from, to IntentPhase) bool {
	if from == to {
		return true
	}
	return slices.Contains(intentTransitions[from], to)
}

// IntentTerminal reports whether the phase completes an Intent (starts its
// TTL). Failed is terminal but revivable.
func IntentTerminal(p IntentPhase) bool {
	return intentTerminal[p]
}

// IntentBlockedFrom returns the phase a Blocked intent resumes to once its
// block no longer holds — the phase it was blocked from — or "" when the
// Intent is not Blocked or its history holds no earlier phase. Blocked has
// no self-entries in phaseTimes (self-transitions do not append), so the
// latest non-Blocked entry is always the one immediately before it.
func IntentBlockedFrom(i *Intent) IntentPhase {
	if i.Status.Phase != IntentBlocked {
		return ""
	}
	prior := IntentPhase("")
	for _, pt := range i.Status.PhaseTimes {
		if pt.Phase != IntentBlocked {
			prior = pt.Phase
		}
	}
	return prior
}

// SetIntentPhase moves the Intent to phase `to` at time `now`: it validates
// the transition, appends to status.phaseTimes (keeping the newest
// MaxIntentPhaseTimes entries), and maintains status.completedAt (set on
// terminal entry, cleared on revival — the TTL contract is completedAt +
// TTL). Callers running under conflict retry must call SetIntentPhase again
// after every re-Get so the transition is re-validated against fresh state;
// an illegal transition returns an error and mutates nothing.
func SetIntentPhase(i *Intent, to IntentPhase, now time.Time) error {
	from := i.Status.Phase
	if !CanTransitionIntent(from, to) {
		return fmt.Errorf("illegal intent transition %q -> %q", from, to)
	}
	if from == to {
		return nil
	}
	t := metav1.NewTime(now)
	i.Status.Phase = to
	times := append(i.Status.PhaseTimes, IntentPhaseTime{Phase: to, At: t})
	if over := len(times) - MaxIntentPhaseTimes; over > 0 {
		times = slices.Clone(times[over:])
	}
	i.Status.PhaseTimes = times
	if IntentTerminal(to) {
		i.Status.CompletedAt = &t
	} else {
		i.Status.CompletedAt = nil
	}
	return nil
}

// IntentIssue locates the intent issue: the human-written task.
type IntentIssue struct {
	// Repository is the https URL of the intent repository, copied from the
	// Project's spec.intentRepository at creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	Repository string `json:"repository"`
	// Number of the issue, at most seven digits (MaxIntentIssueNumber): it
	// is part of every Intent and IntentRun name (see the name budget).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9999999
	Number int64 `json:"number"`
	// URL is the issue's html URL, for display.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url,omitempty"`
}

// IntentRequest records who triggered the intent, as GitHub's issue events
// API reports it — never a handler-time stamp, which is not evidence of
// ordering under webhook redelivery and replay.
type IntentRequest struct {
	// Login of the actor that applied the trigger label. It may be anyone,
	// including a Bot ("<name>[bot]"): the intent reconciler decides
	// authority from it, and a non-approver's trigger closes the Intent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*(\[bot\])?$`
	Login string `json:"login"`
	// At is the labeled event's created_at.
	At metav1.Time `json:"at"`
	// EventID is the GitHub id of the labeled issue event.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
}

// IntentActionSource names where a human action on the intent issue came
// from, and so which GitHub id space its event id is in: a labeled issue
// event's id and an issue comment's id are separate id spaces, so an id alone
// cannot say which kind of action it points at.
// +kubebuilder:validation:Enum=label;command
type IntentActionSource string

// Human action sources.
const (
	// IntentActionLabel: a label applied to the intent issue — the approve
	// label, or the trigger label re-applied. The id is the labeled issue
	// event's, from the issue events API.
	IntentActionLabel IntentActionSource = "label"
	// IntentActionCommand: an issue comment whose first line is a /patchy
	// command (approve, replan). The id is the comment's.
	IntentActionCommand IntentActionSource = "command"
)

// IntentAction identifies one human action on the intent issue as GitHub's
// API reports it — never a handler-time stamp.
type IntentAction struct {
	// Source of the action, and so the id space of EventID.
	Source IntentActionSource `json:"source"`
	// EventID is GitHub's id of the action, in Source's id space.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
	// Login of the actor. It may be anyone, a Bot ("<name>[bot]")
	// included: an action refused for its actor is consumed too.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*(\[bot\])?$`
	Login string `json:"login"`
	// At is the action's created_at, as GitHub reports it.
	At metav1.Time `json:"at"`
}

// IntentSpec identifies one intent issue. intent-controller writes it once,
// at creation; it is immutable except spec.suspend, which humans may patch
// with the native verb (CEL-enforced per field, so a mutation of any other
// field is refused at admission).
type IntentSpec struct {
	// Project names the Project this intent belongs to (the Project whose
	// trigger label the issue carries); at most MaxProjectNameLength, like
	// the Project's own name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=25
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.project is immutable"
	Project string `json:"project"`
	// Issue is the intent issue.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.issue is immutable"
	Issue IntentIssue `json:"issue"`
	// RequestedBy is who applied the trigger label, from the issue events.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.requestedBy is immutable"
	RequestedBy IntentRequest `json:"requestedBy"`
	// Suspend pauses the intent (human-written): no run is launched and
	// nothing is written to GitHub for it until cleared. Running Jobs
	// finish.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// IntentPhaseTime records when the intent entered a phase.
type IntentPhaseTime struct {
	// Phase entered.
	Phase IntentPhase `json:"phase"`
	// At is the entry time.
	At metav1.Time `json:"at"`
}

// IntentInput is the immutable snapshot of the issue a plan was made from.
type IntentInput struct {
	// Revision of the snapshot, 1-based. Every entry to Planning except a
	// resume from Blocked — the first, a replan, a revival from Failed —
	// takes a new snapshot at the next revision, so a revision is never
	// reused; it is the round of that Planning's plan runs. At most
	// MaxIntentRound.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Revision int32 `json:"revision"`
	// Digest is the sha256 of the snapshot bytes; an approval is bound to
	// it, and a changed issue body refuses the approval.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ConfigMap names the immutable ConfigMap holding the snapshot
	// (<intent>-input-r<revision>): the issue title and body, plus, on a
	// replan, the approvers' comments since the previous plan.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
}

// IntentPlan is the plan posted for approval.
type IntentPlan struct {
	// Revision of the plan: the input revision it was planned from, so it
	// is never reused. Plan revisions can skip a number, when that
	// Planning's plan runs failed before any plan was posted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Revision int32 `json:"revision"`
	// Digest is the sha256 of the raw plan report bytes stored in
	// ConfigMap; the build run re-hashes them at launch and refuses a
	// mismatch.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ConfigMap names the immutable ConfigMap holding the raw plan report
	// (<intent>-plan-r<revision>).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// CommentID is GitHub's id of the posted plan comment; zero until
	// posted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CommentID int64 `json:"commentID,omitempty"`
	// CommentDigest is the sha256 of the comment body as GitHub returned it
	// when posted. The approval re-fetches the comment and refuses if it no
	// longer hashes to this, so the builder builds what the approver saw.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CommentDigest string `json:"commentDigest,omitempty"`
	// PostedAt is the comment's created_at as GitHub returned it; only an
	// approval event strictly after it counts.
	// +optional
	PostedAt *metav1.Time `json:"postedAt,omitempty"`
	// Summary is the plan's one-line summary from its frontmatter.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Summary string `json:"summary,omitempty"`
	// Repositories are the URLs of the Project repositories the plan
	// touches (a subset of the Project's; a plan naming any other is
	// rejected).
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=256
	Repositories []string `json:"repositories,omitempty"`
}

// IntentApproval records the accepted approval and exactly what it approved.
type IntentApproval struct {
	// By is the approver's login. Only an approver's approval is ever
	// accepted, and a Bot never is, so the pattern admits no "[bot]".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// Source of the approving action — the approve label, or a /patchy
	// approve comment — and so which id space EventID is in.
	Source IntentActionSource `json:"source"`
	// EventID is GitHub's id of the approving action, in Source's id space:
	// the labeled issue event for the approve label, or the comment
	// carrying /patchy approve.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
	// At is the approving action's created_at, as GitHub reports it.
	At metav1.Time `json:"at"`
	// PlanRevision is the plan revision approved; the build runs it starts
	// take it as their round.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	PlanRevision int32 `json:"planRevision"`
	// PlanDigest is the digest of the plan approved.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest"`
	// InputDigest is the digest of the issue snapshot the plan was made
	// from.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	InputDigest string `json:"inputDigest"`
}

// IntentPullRequest is one pull request patchy opened for the intent. Pull
// requests are correlated only by the (repository, number, nodeID) recorded
// here when patchy opened them — never by head ref, and never a fork's.
type IntentPullRequest struct {
	// Repository is the https URL of the app repository the pull request
	// is in (one of the Project's repositories); the list key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Repository string `json:"repository"`
	// Number of the pull request.
	// +kubebuilder:validation:Minimum=1
	Number int64 `json:"number"`
	// URL is the pull request's html URL.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	URL string `json:"url,omitempty"`
	// NodeID is GitHub's global node id of the pull request.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	NodeID string `json:"nodeID,omitempty"`
	// HeadSHA is the head commit last observed (patchy's push, or a human's
	// since).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	HeadSHA string `json:"headSHA,omitempty"`
	// State of the pull request.
	// +optional
	// +kubebuilder:validation:Enum=open;merged;closed
	State string `json:"state,omitempty"`
	// MergedAt is when the pull request merged.
	// +optional
	MergedAt *metav1.Time `json:"mergedAt,omitempty"`
	// MergeCommitSHA is the commit the merge put on the base branch.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	MergeCommitSHA string `json:"mergeCommitSHA,omitempty"`
}

// IntentUsage totals the agent spend of every run of the intent. Money is
// int64 micro-USD (structural schemas forbid floats, and a sum of decimal
// strings would round); zeroes mean nothing was reported.
type IntentUsage struct {
	// CostMicroUSD is the summed harness-reported cost in micro-USD,
	// checked against the Project's maxCostMicroUSD before every launch.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// InputTokens consumed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens produced.
	// +optional
	// +kubebuilder:validation:Minimum=0
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheReadTokens read from prompt cache.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// CacheCreationTokens written to prompt cache.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
}

// IntentTracking locates the one status comment kept on the intent issue.
type IntentTracking struct {
	// StatusCommentID is GitHub's id of the status comment, posted exactly
	// once (marker <!-- patchy:intent <project>/<issue> -->) and edited
	// thereafter.
	// +optional
	// +kubebuilder:validation:Minimum=0
	StatusCommentID int64 `json:"statusCommentID,omitempty"`
	// StatusDigest is the sha256 of the body last written to it, so an
	// unchanged status is never re-written.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	StatusDigest string `json:"statusDigest,omitempty"`
}

// IntentStatus is the intent's observed state. Written only by
// intent-controller's intent reconciler.
type IntentStatus struct {
	// Phase of the lifecycle (see intentTransitions for edges and writers).
	// +optional
	Phase IntentPhase `json:"phase,omitempty"`
	// PhaseTimes is the phase entry log, newest last, bounded to the most
	// recent MaxIntentPhaseTimes entries.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	PhaseTimes []IntentPhaseTime `json:"phaseTimes,omitempty"`
	// Conditions of the intent: BudgetExhausted, RevisionLimitReached,
	// ImageRequired, ApprovalRejected and (slice 1b) ChecksFailing.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the last spec generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Input is the current issue snapshot.
	// +optional
	Input *IntentInput `json:"input,omitempty"`
	// Plan is the current plan.
	// +optional
	Plan *IntentPlan `json:"plan,omitempty"`
	// Approval is the accepted approval of the current plan; nil until one
	// is accepted, and cleared by a replan.
	// +optional
	Approval *IntentApproval `json:"approval,omitempty"`
	// LastTrigger is the newest trigger action consumed after the one that
	// created the intent (spec.requestedBy, which is immutable): the
	// trigger label re-applied, or /patchy replan. It is recorded whether
	// the action was acted on (a replan from AwaitingApproval, a revival
	// from Failed) or refused with its one notice (not an approver's). A
	// later poll considers only trigger actions newer than it (newer than
	// spec.requestedBy while it is nil), comparing GitHub's own created_at
	// and never the controller's phaseTimes or completedAt. So a revival
	// whose plan fails again, posting no plan to anchor on, cannot
	// re-consume the action that revived it.
	// +optional
	LastTrigger *IntentAction `json:"lastTrigger,omitempty"`
	// Branch is the branch every pull request of the intent is opened from:
	// patchy-intent/<intent>, created once and then only fast-forwarded.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^patchy-intent/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Branch string `json:"branch,omitempty"`
	// PullRequests are the pull requests patchy opened, one per app
	// repository.
	// +optional
	// +listType=map
	// +listMapKey=repository
	// +kubebuilder:validation:MaxItems=8
	PullRequests []IntentPullRequest `json:"pullRequests,omitempty"`
	// Rounds is the revise-round ordinal: how many revise-stage rounds have
	// started, whatever their trigger (review, command or checks) and
	// whatever their outcome — a failed round counts. A new round's runs
	// take spec.round = Rounds+1, and the status write that records the
	// round's first run as ActiveRun advances Rounds to it, so a restart
	// between that create and that write recomputes the same name and
	// adopts the run rather than starting the round twice. Retries and the
	// head_moved re-run are further attempts of the same round. Revisions
	// and CheckFixes count subsets of these rounds (slice 1b).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Rounds int32 `json:"rounds,omitempty"`
	// Revisions counts completed review-driven revise rounds, against the
	// Project's maxRevisions.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Revisions int32 `json:"revisions,omitempty"`
	// CheckFixes counts check-fix rounds, against the Project's
	// maxCheckFixes (slice 1b).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	CheckFixes int32 `json:"checkFixes,omitempty"`
	// Usage totals the spend of every run.
	// +optional
	Usage IntentUsage `json:"usage,omitempty"`
	// Tracking locates the status comment on the intent issue.
	// +optional
	Tracking *IntentTracking `json:"tracking,omitempty"`
	// ActiveRun points at the IntentRun currently holding the run lease
	// (the lease itself is the deterministic IntentRun create).
	// +optional
	ActiveRun *ObjectReference `json:"activeRun,omitempty"`
	// CompletedAt is set on terminal-phase entry and cleared on revival;
	// the TTL contract is completedAt + TTL.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=patchy
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Issue",type=string,JSONPath=`.spec.issue.url`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PRs",type=string,JSONPath=`.status.pullRequests[*].url`,description="The first pull request (slice 1 opens exactly one)"
// +kubebuilder:printcolumn:name="Revisions",type=integer,JSONPath=`.status.revisions`
// +kubebuilder:printcolumn:name="Cost",type=integer,JSONPath=`.status.usage.costMicroUSD`,description="Total spend in micro-USD"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Intent is one unit of human-requested development work: an issue in a
// Project's intent repository carried from plan, through a human approval,
// to built pull requests and their merge. It is the state machine for that
// work (a local phase enum, single writer intent-controller), owns one
// immutable IntentRun per agent attempt, and expires on a TTL after
// completion. The issue is the human-facing projection. Its name is always
// IntentName(spec.project, spec.issue.number) — <project>-<issue>, at most 33
// characters — so the project reconciler's create is idempotent per issue and
// every name derived from it fits the name budget. (The rule compares the
// issue number as an integer: concatenating string(int), whose size CEL
// cannot bound, would exceed the rule cost budget.)
// +kubebuilder:validation:XValidation:rule="self.metadata.name.startsWith(self.spec.project + '-') && !self.metadata.name.substring(size(self.spec.project) + 1).startsWith('0') && int(self.metadata.name.substring(size(self.spec.project) + 1)) == self.spec.issue.number",message="an Intent is named <spec.project>-<spec.issue.number>"
type Intent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentSpec `json:"spec"`
	// +optional
	Status IntentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IntentList contains a list of Intent.
type IntentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Intent `json:"items"`
}
