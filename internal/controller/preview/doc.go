// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package preview deploys an intent's immutable runtime images, one to four
// components behind one host, into a fixed, chart-guarded namespace slot. It
// has no GitHub, ECR, cloud, namespace, RBAC, NetworkPolicy or Secret client.
// Every rendered object is deterministic, a single-component Preview renders
// exactly what it did before multi-component previews existed, and every slot
// is released only after finalizer cleanup sees no workload left.
package preview
