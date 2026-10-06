// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package intentview is the pure projection of intents for human readers: the
// board column an Intent belongs in, the public wording of a run's outcome and
// of what blocks an Intent, the Project limits with the schema defaults
// applied, the rounds an Intent's runs count against those limits, cost
// parsing, and the plain-text rule for any string shown.
//
// Public wording means fixed sentences keyed by controller vocabulary
// (outcomes, condition types and reasons), never a run's detail or a
// condition's message: those can quote a scheduler or kubelet message naming
// private nodes, or an image reference carrying a private registry host and
// account ID, which the issue status comment already keeps off GitHub. The
// wording that overlaps the controller's own (unschedulable, evicted) is
// copied here rather than moved, as are the rules the controller counts
// rounds and attempts by, and tests in internal/controller/intent pin the
// copies together, so the GitHub goldens never move for this package's sake.
//
// It imports the standard library, api/v1alpha1, internal/templates and
// apimachinery types, and nothing else (a test pins that): no client, no
// controller.
package intentview
