// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package resourceclass is the operator's menu of agent Job sizes and the
// rules every size must follow.
//
// The operator configures two things (the patchy chart's agent.resources):
//
//   - a default: the CPU and memory requests and limits every agent Job gets
//     (Finding investigate and remediate Jobs, intent plan Jobs, evaluation
//     Jobs, and intent builds whose repository picks no class). Each of the
//     four quantities is optional, and none set is today's Job exactly: no
//     CPU or memory requests or limits at all.
//   - named classes: whole sizes a Project picks per repository
//     (spec.repositories[].agentResourceClass) for that repository's intent
//     build, revise and check-fix runs. A class needs a CPU request, a
//     memory request and a memory limit (a CPU limit is optional, and
//     usually best left off: it throttles browser and Node test suites),
//     and it replaces the default's CPU and memory as a whole.
//
// The menu is the spend ceiling: a Project can only pick a size the operator
// defined, so the largest class is the most any one agent Job can request.
// Nothing a repository, an issue or an agent writes can pick or change one.
//
// Every quantity is checked here before a controller starts, so a size that
// would make the API server refuse the Job (a request above its limit, a
// negative quantity), or that is almost certainly a typo (3500 CPUs, 7Mi of
// memory), is a startup error naming the class, never a run that fails or
// waits out its deadline: positive, within [MinCPU, MaxCPU] and [MinMemory,
// MaxMemory], and each request at or below its limit. A memory limit equal
// to the request is accepted but not recommended: a pod that touches its
// limit is OOM-killed whole.
//
// Classes travel as JSON (--intent-resource-classes): Parse is strict
// (unknown keys and trailing data refused) and reads a quantity quoted or
// not, so a chart value written cpu: 4 renders through toJson as the number
// 4 and still parses. Encode writes the canonical form Parse reads back.
//
// The package is pure: the standard library and apimachinery's resource
// quantity only (a test pins that), so the CLI can read the same menu
// without linking controller code.
package resourceclass
