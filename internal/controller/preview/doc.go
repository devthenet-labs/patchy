// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package preview deploys one immutable PR-head runtime image into a fixed,
// chart-guarded namespace slot. It has no GitHub, ECR, cloud, namespace, RBAC,
// NetworkPolicy or Secret client. Every rendered object is deterministic and
// every slot is released only after finalizer cleanup sees no workload left.
package preview
