// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package scaffold is the engine behind `patchy init app`: it renders the
// files an application repository needs for patchy's intents and previews,
// from templates embedded in the CLI, and writes them without overwriting
// anything unless asked.
//
// The files are the app publishing contract: .patchy/agent.yaml (the
// agent image the repository declares, an immutable toolchain-v<N> tag) and
// .patchy/Dockerfile (that image's recipe, FROM patchy's agent base pinned
// by digest), the uncredentialed build workflows, the split trusted
// publishers (publish-images.yml dispatching publish-runtime.yml and
// publish-agent.yml, gated separately on PREVIEW_PUBLISH_ENABLED and
// AGENT_PUBLISH_ENABLED) and their guard scripts and tests under
// .github/actions/publish, which patchy's changeset rules keep agents out
// of; that directory's README.md lists the repository variables to set and
// the two trusted workflow paths the AWS roles trust. A new application also
// gets a runtime Dockerfile, a .dockerignore, a .gitignore, a README.md
// carrying the same two tables, and a small service that meets the preview
// runtime contract (uid 65532, a read-only root filesystem, one port, a
// readiness path). Options.Existing
// leaves the application's own files alone and builds its runtime image in
// a workflow of its own, so its CI is untouched.
//
// Nothing generated names an account, role or repository ID: the
// publishers read them from repository variables, and AWS role trust is
// the boundary. .patchy/agent.yaml is the exception, as it must be: it
// holds the agent image's full reference, registry included, because
// source-controller reads the image from the tree.
//
// Plan renders without touching the disk, Write checks every path before
// writing any, and NextSteps is the guidance printed afterwards.
// KeepToolchain takes an existing agent toolchain (.patchy/agent.yaml and
// .patchy/Dockerfile) out of a forced write: once written it is the
// repository's own, its tag bumped with every change, so rendering it again
// would roll it back to an older published image. Sanitize
// derives the image name from a repository name, which GitHub lets carry
// characters an image leaf cannot.
package scaffold
