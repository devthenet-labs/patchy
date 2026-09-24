// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package intent is intent-controller's engine: intent-driven development,
// slice 1a (plan, approve, build, pull request, merge; replan and cancel).
// It polls GitHub rather than taking webhooks, so every reconcile is a
// function of what GitHub's API reports now and what the custom resources
// hold, and a lost, repeated or reordered delivery cannot matter.
//
// # Reconcilers
//
//   - ProjectReconciler validates each Project (exactly one repository in
//     slice 1; every repository resolves to one Forge; the App is installed
//     on the intent repository and the app repository with the permissions
//     intents use; no other Project shares the intent repository and trigger
//     label; the trigger and approve labels exist, created when missing),
//     then polls the intent repository for open issues carrying the trigger
//     label with a conditional (ETag) listing, and creates the Intent
//     <project>-<issue> for each new one whose trigger was applied since the
//     issue last closed. A name held by another repository's issue is
//     reported as IntentNameConflict, never skipped silently. An issue whose
//     ended Intent still exists is handed to that Intent (Nudger).
//   - IntentReconciler runs each Intent's phase machine: it decides human
//     authority from GitHub API facts, snapshots the request, creates the
//     plan and build IntentRuns, writes the plan back for approval, accepts
//     an approval bound to the plan and input digests, opens the pull
//     request, and settles merge, close, cancel, failure and revival.
//   - RunReconciler is the run scheduler: its own slot pool, granted FIFO
//     with build before plan (schedule.Pick), launches each IntentRun's
//     agent Job (jobs.Create, kind intent), collects its result, persists the
//     transcript, and for a build validates the changeset and pushes it.
//   - TTLReconciler deletes an Intent its TTL after completedAt, with
//     foreground propagation, so the name stays taken until everything the
//     Intent owns is gone.
//
// # Single writer
//
// intent-controller is the only writer of Intent, IntentRun and Project
// status, and within it each has one reconciler writing it:
//
//	Intent spec          ProjectReconciler, once, at creation
//	Intent status        IntentReconciler (every phase edge)
//	IntentRun spec       IntentReconciler, once, at creation (the lease)
//	IntentRun status     RunReconciler
//	Project status       ProjectReconciler
//
// Repositories (spec, at creation) and the input, plan and run-input
// ConfigMaps are created by the IntentReconciler and never changed; the
// transcript ConfigMap by the RunReconciler. source-controller alone writes
// Repository status. Humans write spec.suspend only.
//
// # The phase machine
//
// v1alpha1.SetIntentPhase validates every edge against the Intent's own
// table (intent_types.go), separate from Finding's. Slice 1a walks
// Pending → Planning → AwaitingApproval → Building → InReview → Merged, with
// AwaitingApproval → Planning on a replan, Planning and Building → Failed
// when attempts are exhausted (two per stage; a plan the approval comment
// cannot show counts as an invalid attempt), Failed → Planning when an
// approver applies the trigger label again, any non-terminal phase →
// Closed on a human close, a cancel, or every pull request closed unmerged,
// and Planning or Building → Blocked on the cost ceiling or a missing,
// rejected or unusable repository image, resuming to the phase it was
// blocked from once the block no longer holds.
//
// # Authority
//
// A trigger, an approval, a replan and a cancel count only when GitHub's
// API says who acted: the actor of a labeled issue event, or a comment's
// author. The actor must be in the Project's approvers, have write access
// to the intent repository (ghclient.CanWrite: a public repository answers
// "read" for everyone), and not be a bot; actions by the App's own bot are
// never answered. An approval is accepted only if it is newer than the plan
// comment, the plan comment re-fetched still hashes to the digest recorded
// when it was posted, the issue re-read still renders to the input
// snapshot's digest, and, for the label, the label is still on the issue.
//
// # Durable settle
//
// Every GitHub side effect is idempotent, and every effect a retry could
// repeat is recorded before or found after it:
//
//   - A comment patchy posts is headed by a marker. Before posting, patchy
//     looks for its own comment with that marker among the comments since
//     the moment the thing it answers happened (never the whole thread),
//     so a restart between posting and recording never posts twice. The
//     status comment is created exactly once and edited only when its
//     digest changes.
//   - A human action is consumed exactly once. Trigger actions (the trigger
//     label re-applied, /patchy replan) are consumed by status.lastTrigger,
//     written in the same status write as their effect, and only actions
//     GitHub dates after it are considered. An accepted approval is
//     status.approval. Every other answered command, and every refused
//     label, is recorded by patchy's reply marker, which the next poll finds
//     in the same listing that carries the command. A refusal is replied to
//     before it is recorded, so a record without a reply means the action
//     was accepted, and the retry replies "done".
//   - Terminal effects come before the terminal status write: the trigger
//     label is removed before Failed (and before Closed on a refused
//     trigger), the issue is closed before Merged (after the summary) and
//     before Closed on a cancel or on every pull request closed. A restart in
//     between repeats the effects, which are idempotent, and completedAt is
//     never set while the trigger is still in place.
//   - The build push is two-phase: the commit is created, its SHA recorded
//     on the run, and only then the branch patchy-intent/<intent> created,
//     create-only; an existing branch is adopted only if it already points
//     at the recorded commit, and is otherwise branch_exists. Nothing is ever
//     forced, and the default branch is never touched.
//
// A transient GitHub failure returns the error, and the reconcile retries
// with backoff: nothing is ever decided without GitHub's answer.
package intent
