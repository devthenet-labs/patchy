// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

// Condition types. Every kind carries Ready; the rest are noted per kind.
const (
	// ConditionReady is the summary condition on every kind.
	ConditionReady = "Ready"
	// ConditionStalled marks an object that cannot progress without operator
	// action (ambiguous forge match, oversized artifact, ambiguous tracking).
	ConditionStalled = "Stalled"
	// ConditionAccumulationComplete marks a Finding whose 1-hour accumulation
	// window has closed (accumulation is a condition, not a phase — alerts
	// fold in concurrently with enhancement).
	ConditionAccumulationComplete = "AccumulationComplete"
	// ConditionContextEnhanced marks a Finding whose enhancer chain ran.
	ConditionContextEnhanced = "ContextEnhanced"
	// ConditionInvestigated marks a Finding with a completed investigation;
	// the reason carries the recommendation.
	ConditionInvestigated = "Investigated"
	// ConditionApproved marks a Finding whose spec.approval was accepted.
	ConditionApproved = "Approved"
	// ConditionRetried marks a Finding whose spec.retry was consumed — a
	// human recovered it from Failed; the message carries the requester.
	ConditionRetried = "Retried"
	// ConditionForgeResolved reports whether the Finding's repository resolved
	// to exactly one Forge (False reasons: NoRepository, NoForgeMatch,
	// Ambiguous). Parked findings are re-queued when Forges change.
	ConditionForgeResolved = "ForgeResolved"
	// ConditionComplete marks a finished Investigation/Remediation/IntentRun
	// (reason: the stage outcome), a settled EvaluationUnit (reason: Complete
	// or the unit failure reason), or a settled Evaluation (reason:
	// Complete/Failed).
	ConditionComplete = "Complete"
	// ConditionUnitsCreated marks an Evaluation whose child EvaluationUnits
	// all exist.
	ConditionUnitsCreated = "UnitsCreated"
	// ConditionSandboxRefused marks a failed Investigation/Remediation/
	// IntentRun whose repository-image Job the sandbox probe refused (its
	// prepare init exited 78): the agent never ran, so the attempt does not
	// count toward MaxAttempts. The collector sets it from the init's exit code alone,
	// never from anything the pod printed, so a run cannot claim it.
	ConditionSandboxRefused = "SandboxRefused"
	// ConditionCommitAncestry reports, on a github Integration, whether its
	// credential could read commit ancestry the last time ingest needed it:
	// GitHub's compare API, which requires the Contents (read) permission.
	// False (ReasonContentsReadDenied) means the stale-reopen check fails
	// open — every reopen of a remediated alert opens a successor finding.
	// Absent until the first lookup is denied.
	ConditionCommitAncestry = "CommitAncestry"
	// ConditionReviewClosePending marks a Finding in review whose review
	// may have ended in a way the delivery alone cannot settle: its tracking
	// issue closed (ReasonTrackingIssueClosed), or a PR close named it from
	// a repository other than the recorded one (ReasonUnrecordedRepository).
	// The recorded PR's own state decides, and the integration-controller
	// reads it until GitHub answers — a webhook delivery is never redelivered
	// once answered, so the condition is what keeps the close. While no
	// issues-enabled Integration can read it (suspended, issues turned off,
	// deleted), the close waits. Removed as the finding leaves review.
	ConditionReviewClosePending = "ReviewClosePending"

	// Intent conditions, all set by intent-controller. The first three
	// explain a Blocked intent; raising the limit or fixing the image
	// clears them and resumes it.

	// ConditionBudgetExhausted marks an Intent whose spend reached its
	// Project's maxCostMicroUSD ceiling.
	ConditionBudgetExhausted = "BudgetExhausted"
	// ConditionRevisionLimitReached marks an Intent whose revise rounds
	// reached its Project's maxRevisions.
	ConditionRevisionLimitReached = "RevisionLimitReached"
	// ConditionImageRequired marks an Intent whose build or revise run
	// could not launch on an accepted repository-declared runner image (none
	// declared, rejected, or refused by runnerguard) while the Project
	// requires one.
	ConditionImageRequired = "ImageRequired"
	// ConditionApprovalRejected marks an Intent whose approval was refused
	// because the plan comment or the issue body changed after the plan was
	// posted; a replan is requested.
	ConditionApprovalRejected = "ApprovalRejected"
	// ConditionChecksFailing marks an Intent whose named check failed again
	// after a check-fix round with the same failure signature (slice 1b).
	ConditionChecksFailing = "ChecksFailing"

	// ConditionPushHeld marks a Running build IntentRun whose Job has
	// finished while its Intent is suspended: the push waits for the
	// suspension to be lifted. Its agent no longer runs, so it holds no slot
	// of the run pool, and while it waits its Job is not read again. Set by
	// intent-controller's run reconciler, and False once the run settles.
	ConditionPushHeld = "PushHeld"

	// ConditionIntentNameConflict marks a Project, True while one of its
	// trigger-labelled issues cannot become an Intent because the name
	// <project>-<issue> is held by an Intent for an issue of another
	// repository: a Project deleted and recreated, under the same name, on a
	// different intent repository (spec.intentRepository is immutable, so
	// only a recreate can do it). The message names the issue and the Intent;
	// the issue is skipped, never silently, until that Intent is deleted or
	// expires. Set by intent-controller's project reconciler.
	ConditionIntentNameConflict = "IntentNameConflict"

	// Per-scope rollup markers. A scope's finalizer is removed only when its
	// condition is True and deletion is underway — remaining finalizers show
	// exactly which scopes still owe aggregation.
	ConditionRolledUpTotal      = "RolledUpTotal"
	ConditionRolledUpRepository = "RolledUpRepository"
	ConditionRolledUpHarness    = "RolledUpHarness"
	ConditionRolledUpModel      = "RolledUpModel"
)

// Condition reasons.
const (
	// ReasonContentsReadDenied: GitHub refused the Integration's credential
	// the compare API (403) — it lacks the Contents (read) permission.
	ReasonContentsReadDenied = "ContentsReadDenied"
	// ReasonAncestryReadable: the Integration's credential read commit
	// ancestry after an earlier denial.
	ReasonAncestryReadable = "AncestryReadable"
	// ReasonNoRepository: the finding has no repository (e.g. a cloud
	// finding) — nothing to resolve a Forge against.
	ReasonNoRepository = "NoRepository"
	// ReasonNoForgeMatch: no Forge's filters matched the repository.
	ReasonNoForgeMatch = "NoForgeMatch"
	// ReasonAmbiguous: more than one Forge matched with equal specificity —
	// an operator configuration error.
	ReasonAmbiguous = "Ambiguous"
	// ReasonRepositoryUnresolved: a cloud finding's repository lookup failed
	// in a way worth retrying, so the enhancer chain is holding the finding
	// at Opened rather than advancing it without a repository. Bounded by the
	// accumulation window; past that the finding advances regardless.
	ReasonRepositoryUnresolved = "RepositoryUnresolved"
	// ReasonTrackingIssueClosed: the tracking issue closed during review.
	// The PR body's "Fixes #N" closes it on merge too, so a merged or closed
	// PR settles the finding as its own delivery would. A PR still open, or
	// one GitHub answers 404/403 for, means a human closed the issue to take
	// the finding over (HandedOff) — if GitHub still reports the issue
	// closed: deliveries are unordered, so a quick reopen may land first.
	ReasonTrackingIssueClosed = "TrackingIssueClosed"
	// ReasonUnrecordedRepository: a PR close named the finding with its
	// recorded number, from a branch in the repository it closed in, but not
	// the recorded repository — renamed since (GitHub sends the new name and
	// redirects the old), or another repository's PR of the same number. A
	// recorded PR still open means the latter: nothing moves. A transfer to
	// another owner looks the same, but the recorded PR reads under the old
	// name only while the credential can still read the repository there (a
	// public one, or an App installation on the old owner that still has
	// it); otherwise GitHub answers 404 and nothing moves either.
	ReasonUnrecordedRepository = "UnrecordedRepository"

	// Repository runner-image reasons, all set by source-controller.

	// ReasonRunnerImageResolving: the artifact is stored and pinned, and
	// Ready is False only while the repository-declared runner image is
	// resolved. The status write is split around resolution on purpose: a
	// restart here resumes at resolution from the artifact on disk instead
	// of re-downloading the tree.
	ReasonRunnerImageResolving = "RunnerImageResolving"
	// ReasonRunnerImageRejected: the declared runner image failed policy
	// deterministically (an unusable manifest, a registry outside the
	// allowlist, a 401/403/404, an oversized or wrong-platform image, a
	// reserved ENV, a VOLUME, an empty PATH, a missing or bad signature).
	// Stalled=True with a human message; the artifact is retained so the
	// finding can still run on the default image.
	ReasonRunnerImageRejected = "RunnerImageRejected"
	// ReasonRunnerImageResolveFailed: resolving the declared runner image
	// hit a transient error (registry unreachable). Ready=False and the
	// reconcile backs off and retries, without re-downloading the artifact.
	ReasonRunnerImageResolveFailed = "RunnerImageResolveFailed"

	// Project Ready=False reasons, all set by intent-controller.

	// ReasonForgeUnresolved: the intent repository or an app repository
	// resolves to no Forge, or to more than one.
	ReasonForgeUnresolved = "ForgeUnresolved"
	// ReasonAppNotInstalled: the GitHub App is not installed on the intent
	// repository or on an app repository.
	ReasonAppNotInstalled = "AppNotInstalled"
	// ReasonAmbiguousIntentRepository: another Project shares this intent
	// repository with the same trigger label, so an issue could not be told
	// apart between them.
	ReasonAmbiguousIntentRepository = "AmbiguousIntentRepository"
)
