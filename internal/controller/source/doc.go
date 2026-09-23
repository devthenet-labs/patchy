// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package source is the source-controller's engine: the
// reconcilers for the Forge and Repository kinds. The Forge reconciler
// validates credentials and stamps readiness; the Repository reconciler
// resolves the covering Forge, pins the head SHA exactly once, downloads the
// forge's tarball archive at that SHA (pure HTTP — controllers carry no git
// binary), publishes it through the artifact store for agent jobs to fetch
// credential-lessly, and, when repository-declared runner images are enabled,
// reads the declaration out of that stored tarball and pins the image to a
// digest exactly once beside the SHA (status.runnerImage, one writer).
package source
