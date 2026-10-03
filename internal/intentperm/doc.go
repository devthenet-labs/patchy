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
//     find the failed checks and read their annotations and job log tails.
//
// Per feature an App is registered for (ForApp), the permissions and webhook
// events it must hold, and nothing more: the findings pipeline (code
// scanning alerts, issues, contents and pull requests write, and the four
// events the integration-controller consumes), intents (the merged intent
// rows above, no event: intent-controller polls), and check fixes (the
// check-fix reads), plus the metadata read GitHub requires of every App.
// The findings row lives here too, so an App manifest is built from one
// table.
//
// Every reader takes it from here: intent-controller proves each grant For
// lists by minting a token with it before a Project is Ready, and an App
// manifest is built from ForApp, so the App holds exactly what a Project
// will be checked for. Permissions and events are plain strings in GitHub's
// own spelling, so the package stays free of any GitHub client and the CLI
// can read it too. It is pure: the standard library and api/v1alpha1 only (a
// test pins that).
package intentperm
