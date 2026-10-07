// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package ledger is the preview sign-in relay's previewauth.CodeLedger over
// one chart-created Lease: every redeemed code's key, until the code would
// have expired anyway, kept in one annotation and updated with the Lease's
// resourceVersion as a compare-and-swap. Two relay replicas therefore agree
// on which codes are spent without the relay holding create or delete rights
// in the release namespace: its RBAC is get and update on this one Lease by
// name. Logins are rare, so one object is enough; the ledger is capped, and a
// full ledger or a conflict storm is a transient failure (503), never a
// silently reusable code.
package ledger
