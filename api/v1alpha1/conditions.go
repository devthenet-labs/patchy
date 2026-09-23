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
	// ConditionComplete marks a finished Investigation/Remediation (reason:
	// the stage outcome), a settled EvaluationUnit (reason: Complete or the
	// unit failure reason), or a settled Evaluation (reason: Complete/Failed).
	ConditionComplete = "Complete"
	// ConditionUnitsCreated marks an Evaluation whose child EvaluationUnits
	// all exist.
	ConditionUnitsCreated = "UnitsCreated"
	// ConditionSandboxRefused marks a failed Investigation/Remediation whose
	// repository-image Job the sandbox probe refused (its prepare init
	// exited 78): the agent never ran, so the attempt does not count toward
	// MaxAttempts. The collector sets it from the init's exit code alone,
	// never from anything the pod printed, so a run cannot claim it.
	ConditionSandboxRefused = "SandboxRefused"

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
)
