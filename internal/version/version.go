// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package version

// Version is injected at build time via -ldflags (see tasks.toml and
// .goreleaser.yaml).
//
// Version defaults to dev when no release metadata is supplied.
var Version = "dev"

// Commit is injected at build time via -ldflags (see tasks.toml and
// .goreleaser.yaml).
//
// Commit defaults to none when no release metadata is supplied.
var Commit = "none"

// BuildDate is injected at build time via -ldflags (see tasks.toml and
// .goreleaser.yaml).
//
// BuildDate defaults to unknown when no release metadata is supplied.
var BuildDate = "unknown"

// RunnerImageRepository is the repository the release publishes its claude
// runner image to, the trusted donor of agent-runner and the claude CLI
// that `patchy check image --run` copies them out of. It is injected at
// build time via -ldflags from the release registry (PATCHY_IMAGE_REGISTRY
// in .goreleaser.yaml and hack/build.sh), the same value the release's
// images are pushed under, so a fork's CLI names its own registry and no
// registry is written into the code.
//
// RunnerImageRepository is empty when no release registry is supplied (a
// plain go build): such a CLI knows no runner image of its own.
var RunnerImageRepository string

// AgentBaseRepository is the repository the release publishes its
// agent-base image to, the base a repository-declared agent image builds
// on. It is injected at build time via -ldflags from the release registry,
// as RunnerImageRepository is.
//
// AgentBaseRepository is empty when no release registry is supplied (a
// plain go build).
var AgentBaseRepository string
