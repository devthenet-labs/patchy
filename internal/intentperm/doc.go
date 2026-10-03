// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package intentperm is the one table of the GitHub App permissions intents
// need, per repository of a Project:
//
//   - the intent repository: issues write (read the issues, comment, label,
//     close);
//   - every application repository: contents write (push the intent branch)
//     and pull requests write (open the pull request, read its reviews and
//     conversation);
//   - every application repository, when spec.checks.fix names a check:
//     checks, statuses and actions read, which a check-fix round needs to
//     find the failed checks and read their annotations and job log tails.
//
// Every reader takes it from here: intent-controller proves each grant by
// minting a token with it before a Project is Ready. Permissions are plain
// strings in GitHub's own spelling, so the package stays free of any GitHub
// client and the CLI can read it too. It is pure: the standard library and
// api/v1alpha1 only (a test pins that).
package intentperm
