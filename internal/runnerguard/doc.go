// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package runnerguard is the controller side of repository-declared runner
// images, shared by the two job controllers (investigation, remediation) so
// their launch and collect paths cannot drift apart.
//
// It decides three things. At launch, whether a Job may run the image the
// Repository pinned (Guard.Pin): only with the controller's kill switch on,
// the sandbox breaker untripped and the finding not revived by a human, and
// only an image source-controller actually pinned — a rejected declaration
// carries no image, so its finding runs the default one. While a
// repository-image Job has not finished, whether its pod is still within the
// pull grace, stuck on a deterministic pull failure, or worth another look
// (Pending): a pod in ImagePullBackOff never mutates its Job, so the Job watch
// alone would never report it. And whether the trusted prepare init refused
// to hand over because NetworkPolicy is not enforced (SandboxRefused), which
// trips the Breaker for the rest of the process's life.
//
// Every collect-side decision reads the Job's own runner-image-source
// annotation (jobs.Status.RunnerImageSource), never controller configuration,
// so a default-image Job behaves exactly as it did before the feature
// existed, and a kill-switch flip never changes how an already-launched Job
// is judged.
package runnerguard
