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
//   - ProjectReconciler validates each Project (exactly one repository
//     unless the controller runs with --intent-multi-repo; every repository
//     resolves to one Forge; the App is installed on the intent repository
//     and on every app repository with the permissions intents use, the
//     internal/intentperm table, each proven by minting a token with it; no
//     other Project shares the intent repository and trigger
//     label; the trigger and approve labels exist, created when missing),
//     then polls the intent repository for open issues carrying the trigger
//     label with a conditional (ETag) listing, and creates the Intent
//     <project>-<issue> for each new one whose trigger was applied since the
//     issue last closed. An issue with more than one Project trigger waits;
//     a second Project cannot claim an issue with an Intent already on it.
//     A name held by another repository's issue is reported as
//     IntentNameConflict, never skipped silently. An issue whose
//     ended Intent still exists is handed to that Intent (Nudger).
//   - IntentReconciler runs each Intent's phase machine: it decides human
//     authority from GitHub API facts, snapshots the request, creates the
//     plan and build IntentRuns, writes the plan back for approval, accepts
//     an approval bound to the plan and input digests, opens the pull
//     requests, and settles merge, close, cancel, failure and revival.
//
// # Several repositories
//
// With --intent-multi-repo a Project may list up to eight repositories. One
// plan run covers every one: it plans from the first and reads each other
// one read-only as a tree (spec.trees), from a Repository of its own the run
// owns; it launches only once every tree is stored, a tree stalled for any
// reason but its declared image aborts it, and what each tree was at launch
// is kept on its status.trees after the tree Repositories are deleted with
// it. The approval then builds every repository the approved plan names
// (approvedRepositories, in plan order), one build run each, all created in
// one pass, each in its own repository's accepted image, its rounds, attempts
// and branch checks its repository's own (round takes the repository). A
// repository whose attempts are spent fails the whole Intent; one with no
// accepted image blocks it, naming the repository. activeRun is sticky
// while Building: it names one in-flight build until that one settles. The
// pull requests open only once every build has pushed, recorded one per pass,
// the last record moving the Intent to InReview; an Intent that ends while
// they are being opened tells each one already opened, once, that it is no
// longer tracked (UntrackedPullRequests), and a revival forgets them. A build
// name another repository's run holds (keys swapped) is never adopted: it
// blocks (UnsupportedRepositories, RepositoryKeyChanged). In review a comment
// cross-linking the pull requests is posted on each, best effort, reported by
// SiblingsLinked, never holding a phase back, and after a refusal tried again
// only once the Project or a pull request's head changes. In review, rounds
// stay serialised per Intent, each on the pull request of one repository: a
// review on A revises A, a /patchy revise on B revises B, a failed named check
// on C's patchy head fixes C. A round already leased is adopted before any
// other is considered, whatever its repository (read live before a round is
// leased, so a cache that lags a lease cannot give its number twice), and the
// open pull requests are then served in turn; a round needs only its own
// repository to stay in the Project. A
// round's review cutoff, feedback window, compare base, image, pushed head,
// observed checks and repeated-failure signature are its own repository's;
// its counters and limits, and the blocks they raise (naming the
// repository), stay the Intent's. A round whose pull request is merged or
// closed under it ends unpushed and is not retried. The Intent is Merged only
// when every pull request has merged; once every one has settled with any
// closed unmerged it is Closed, the issue closed as not planned after a
// notice naming what merged and what did not. patchy never closes one pull
// request because another closed. With the preview projection on, the first
// review pass records the default-branch head of each previewed repository
// without a pull request (status.previewBases), once: its preview component
// runs that commit's image. Without the flag, an Intent of a Project listing
// more than one repository is held Blocked (UnsupportedRepositories): no run
// is launched or created and no push made for it (a Job already running
// finishes, and its push waits), so turning the flag off is a real rollback.
// A revise round blocked mid-flight is the same round when the flag is on
// again: its held push waits for the resume (errRoundBlocked), and the resume
// follows its run. A one-repository Project takes the same path either way.
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
// Closed on a human close, a cancel, or every pull request settled with at
// least one closed unmerged,
// and Planning or Building → Blocked on the cost ceiling, a missing,
// rejected or unusable repository image, or an intent branch or pull request
// that is not patchy's own (BranchConflict), resuming to the phase it was
// blocked from once the block no longer holds. Any phase but a terminal one
// → Blocked while multi-repository intents are off for a Project that lists
// several repositories (UnsupportedRepositories), and Building → Blocked when
// a build's name is another repository's run (UnsupportedRepositories,
// RepositoryKeyChanged).
//
// # Authority
//
// A trigger, an approval, a replan and a cancel count only when GitHub's
// API says who acted: the actor of a labeled issue event, or a comment's
// author. The actor must be in the Project's approvers, have write access
// to the intent repository (ghclient.CanWrite: a public repository answers
// "read" for everyone), and not be a bot; actions by the App's own bot are
// never answered. A comment edited after it was posted is never a command:
// GitHub lets anyone with write access edit anyone's comment and still
// names the original author. REST updated_at is not an edit verdict: it may
// move when a pending review is submitted, and a same-second edit may leave
// it unchanged. Before a command is acted on, GitHub's own edit record
// (GraphQL lastEditedAt/includesCreatedEdit) is read. For the same reason an
// edited comment never reaches a replan's snapshot. An approval is accepted
// only if it is newer than the plan comment, the plan comment re-fetched
// still hashes to the digest recorded when it was posted and was never
// edited, the issue re-read still renders to the input snapshot's digest,
// and, for the label, the label is still on the issue.
//
// A command from someone refused without asking GitHub (not an approver, or
// a bot) is refused whatever it says, and its author gets that refusal once
// per intent (status.commands.refusedActors); later ones get neither
// reaction nor reply, so commenting cannot make patchy write to GitHub once
// per comment.
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
//     status.approval. Every comment is consumed by status.commands.seen, the
//     newest comment the poll has settled, written as soon as a command's
//     reply is posted and for the whole listing once every command in it is
//     answered; no later poll reads a comment at or before it, so a command
//     is never answered twice, even after patchy's reply is deleted, and the
//     thread is listed only from there. Until then, a command's reply marker
//     is found in the same listing that carries the command. An approver's
//     /patchy approve made while the plan is recorded but its posting not
//     yet waits for that record, as the approve label does: the listing is
//     settled only up to it, and a later poll answers it. A refused label
//     is recorded by its notice and the label's removal. A refusal is
//     replied to before it is recorded, so a record without a reply means
//     the action was accepted, and the retry replies "done".
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
