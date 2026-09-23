// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package imagecheck is the engine behind `patchy check image`: it tells a
// repository owner, from a workstation and with no cluster access, whether
// patchy will run an agent image they mean to declare in
// .patchy/agent.yaml or .devcontainer/devcontainer.json.
//
// Static runs what source-controller runs, through the same code rather
// than a copy of it: runnerimage.ParseDeclared canonicalises the reference,
// an optional runnerimage.Policy stands in for the operator's registry
// allowlist, and resolve.Resolver.Inspect pins the reference to a digest
// and judges platforms, compressed size, VOLUME, reserved ENV (with
// jobs.ReservedEnvNames as the Job's own reserved set), PATH and, given the
// operator's cosign key, the signature. Inspect reports every verdict where
// the controller stops at the first, so the owner fixes the image in one
// pass, and its first failure is by construction the controller's.
//
// Sandbox emulates the agent pod on a local docker, once for each platform
// the image serves (the host's natively, any other under docker's
// emulation, and a platform the host cannot emulate reported as a SKIP):
// it copies agent-runner and the claude CLI out of the trusted runner
// image, as the Job's prepare init does, then runs the image under test as
// uid 65532 with a read-only root filesystem, no network, no capabilities,
// no privilege escalation, bounded processes, memory and CPU, sized
// executable tmpfs mounts for /tmp and /workspace, the binaries mounted
// read-only at /patchy/bin and the pod's environment (jobs.InjectedEnv), in
// a named container it removes however the run ends, and executes
// agent-runner's own preflight (`agent-runner preflight`, the check a stage
// runs before its first model call) plus `bash -c true` and `git --version`
// on their own. docker is reached only through Commander, so the package
// builds for every target the CLI ships on and tests need no docker at all.
//
// ChooseRunner picks that runner image before the sandbox runs, and reports
// it as a check line of its own, reference and digest: --runner-image as
// given, otherwise the image released with this CLI's version or, for a
// development build, the newest vX.Y.Z release in the registry (never
// latest), each pinned to the digest its tag names there, so a stale local
// copy of the tag never stands in for it. The registry is reached only
// through Registry, so tests fake it too.
//
// Each check is one Check line, PASS, FAIL or SKIP with a reason, and a
// sandbox check also names the platform it ran as.
package imagecheck
