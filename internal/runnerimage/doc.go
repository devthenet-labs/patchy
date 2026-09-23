// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package runnerimage is the pure core of repository-declared agent runner
// images: everything source-controller decides about the image a repository
// asks its agent to run in, with no registry, Kubernetes or filesystem access.
//
// The pieces, in the order resolution applies them:
//
//   - ReadFiles walks a pinned tree's tar.gz stream and returns the two
//     declaration files it honours, .patchy/agent.yaml and
//     .devcontainer/devcontainer.json, each capped at MaxDeclarationBytes.
//   - Declare applies precedence. Its result type keeps the outcomes apart:
//     a Declaration with OutcomeDeclared names the image to resolve,
//     OutcomeNotApplicable means a devcontainer.json was present but not
//     written for patchy (build-based, features, no usable image, unparseable)
//     so the default image is used and the reason is informational, and
//     OutcomeNone means neither file exists. Only an explicit .patchy/agent.yaml
//     that fails validation is a *Rejection error, the one outcome operator
//     policy (onReject) acts on.
//   - ParseDeclared normalizes a declared reference and enforces the strict
//     digest form; Policy validates the operator's registry allowlist entries
//     at construction and Allow matches on path-segment boundaries.
//   - SanitizePath, CheckEnv and CheckVolumes judge an image config: the pod
//     search path derived from the image PATH (the runc default when absent,
//     never an empty or relative component), the ENV names an image may not
//     set, and the VOLUME instructions it may not carry.
//   - Resolver is the seam the registry-backed implementation fills in and
//     reconciler tests fake.
//
// Every deterministic refusal is a *Rejection carrying a fixed message; that
// message is what the Repository condition and the tracking issue show, so
// tests pin the exact text. Any other error is transient.
package runnerimage
