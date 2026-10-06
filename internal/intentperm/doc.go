// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package intentperm is the one table of the GitHub App permissions patchy
// needs. It has two views of the same rows.
//
// Per repository of a Project (For), what intents need:
//
//   - the intent repository: issues write (read the issues, comment, label,
//     close);
//   - every application repository: contents write (push the intent branch),
//     pull requests write (open the pull request, read its reviews and
//     conversation), and issues read, the token a reviewer's collaborator
//     permission and the installation's rate budget are read with
//     (RepositoryReads);
//   - every application repository, when spec.checks.fix names a check:
//     checks, statuses and actions read, which a check-fix round needs to
//     find the failed checks and read their annotations and job log tails;
//   - every application repository, when spec.checks.rerunFailed is set as
//     well: actions write, which re-runs the failed jobs of the Actions
//     runs behind a failed named check once before a check-fix round.
//
// Per feature an App is registered for (ForApp), the permissions and webhook
// events it must hold, and nothing more: the findings pipeline (code
// scanning alerts, issues, contents and pull requests write, and the four
// events the integration-controller consumes), intents (the merged intent
// rows above, no event: intent-controller polls), check fixes (the
// check-fix reads), and failed-check re-runs (actions write, a feature of
// its own so check fixes never widen to a write on CI), plus the metadata
// read GitHub requires of every App.
// The findings row lives here too, so an App manifest is built from one
// table.
//
// Every reader takes it from here: intent-controller proves each grant For
// lists by minting a token with it before a Project is Ready, and an App
// manifest is built from ForApp, so the App holds exactly what a Project
// will be checked for. Every token intent-controller mints is a grant of For
// for the repository it is minted on, with one deliberate exception, and it
// is read-only: while an intent is open, the pull request it opened in a
// repository its Project has since stopped listing is still read where the
// installation allows (pull requests read, and issues read for the
// installation's rate budget), so that pull request's merge still counts
// towards the intent's ending. For has no row for that repository any more,
// so Ready proves nothing there: nothing is written there, and once refused
// there the read is not asked again for a while.
//
// Permissions and events are plain strings in GitHub's own spelling, so the
// package stays free of any GitHub client and the CLI can read it too. It is
// pure: the standard library and api/v1alpha1 only (a test pins that).
package intentperm
