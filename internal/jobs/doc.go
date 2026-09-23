// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package jobs creates and observes the ephemeral Kubernetes Jobs that run
// agent-runner: one Job per Investigation/Remediation attempt,
// deterministically named, labelled by kind/owner/finding (the two job
// controllers share the agents namespace and filter on the kind label), and
// garbage collected via TTL plus an owner-referenced per-Job Secret.
//
// The Job shape carries the isolation model: no credential of any kind
// reaches the pod except the model API key. The init container fetches the
// repository as a digest-verified tarball from source-controller's artifact
// server and synthesizes the local git base; the per-Job Secret holds only
// the handoff markdown. Results come back on the agent container's stdout
// as envelope events, read once at Job completion. Brokered claude pods also
// carry a fixed, non-secret placeholder ANTHROPIC_AUTH_TOKEN (the CLI will not
// start without one); the egress broker strips it and injects the real key.
//
// A Job may run a repository-declared image instead of the harness's runner
// image (Spec.RunnerImage, honoured only under Config.AllowRepositoryImages
// and for a Runner with binaries to Inject). The isolation model does not
// bend for it. Create refuses such a Job outright for a runner that would
// carry a model credential into the pod, for a reference not pinned to a
// sha256 digest, and without an ephemeral-storage limit. Otherwise the
// trusted prepare init still runs the runner image and, after the fetch,
// copies patchy's static agent-runner and the harness CLI into a read-only
// emptyDir the agent container starts from by absolute path, then runs the
// sandbox probe (`agent-runner sandbox-probe`) and refuses to hand over —
// exit ExitSandboxUnenforced — while the pod can still reach the internet,
// the API server or the metadata service. The agent container gets a
// controller-owned PATH, git told to ignore the image's config, and an
// explicit empty value for the known shell, loader, interpreter, git,
// proxy, gateway and PATCHY_* redirection points the Job does not set. That
// is a backstop against configuration an image carries by accident, not a
// boundary: the image supplies git, bash and the libc the CLI runs on, so
// its ENV is exactly as untrusted as its binaries, and the trust boundaries
// are agent-runner and the broker. Names an empty value cannot neutralise
// (git's repository redirections) are reserved instead, so the resolver
// refuses an image that sets them. Such a Job carries the runner-image
// audit annotations and label; a default Job carries none and is
// byte-for-byte what it was before the feature existed. Create returns the
// image and source that actually apply, and that value is what the
// launching controller records.
package jobs
