// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// Guard is one job controller's repository-image launch policy.
type Guard struct {
	// Enabled is the controller's --repository-images kill switch: while
	// false no launch copies a pinned image, so a flip takes effect at the
	// next Job without touching any CR.
	Enabled bool
	// Breaker refuses repository images once a sandbox probe has found
	// NetworkPolicy unenforced; nil never trips.
	Breaker *Breaker
}

// Reasons Pin reports for not using an image the Repository pinned.
const (
	SkipDisabled = "repository images are disabled"
	SkipBreaker  = "the sandbox breaker is tripped"
	SkipRevived  = "a human revived the finding, so it runs on the default image"
)

// Pin copies the Repository's pinned runner image into spec when this launch
// may run it. It reports why it did not when an image was pinned but is not
// used, and "" otherwise (copied, or nothing pinned: no declaration, a
// not-applicable devcontainer.json, or a rejected declaration — the last
// carries no image, which is how a Repository rejected under onReject
// default runs the default image without a human step).
//
// What Pin copies is a request, not a record: jobs.Client.Create decides
// whether injection actually happens and returns what the Job runs, and that
// return value is the only thing a launch may record.
func (g Guard) Pin(spec *jobs.Spec, repo *v1alpha1.Repository, fnd *v1alpha1.Finding) string {
	ri := repo.Status.RunnerImage
	if ri == nil || ri.Image == "" {
		return ""
	}
	switch {
	case !g.Enabled:
		return SkipDisabled
	case g.Breaker.Tripped():
		return SkipBreaker
	case Revived(fnd):
		return SkipRevived
	}
	spec.RunnerImage = ri.Image
	spec.RunnerSearchPath = ri.SearchPath
	spec.RunnerImageManifest = ri.Manifest
	return ""
}

// Further reasons PinFor reports: a launch that requires the repository's
// image has none to run.
const (
	SkipNoImage  = "the repository declares no runner image patchy can use"
	SkipRejected = "the repository's runner-image declaration was rejected"
)

// PinFor is Pin for a launch that requires the repository's image: an
// intent's build or revise run, which has no default-image fallback (the
// default image carries no toolchain, so a build there would open an
// untested pull request). It copies the Repository's pinned image into spec
// only when source-controller accepted one — Image set and not Rejected,
// since a Repository rejected under onReject default is Ready all the same
// — and the guard allows it, and returns "" exactly then. Otherwise it
// returns why not (SkipRejected, SkipNoImage, SkipDisabled or SkipBreaker,
// in that order) and leaves spec untouched, so the caller blocks instead of
// launching.
//
// It has no counterpart to Pin's revival rule, which moves a revived
// Finding to the default image: a launch that requires the repository's
// image has no default to move to, and an intent brought back by its
// trigger label is a new plan and a new approval, a fresh human decision,
// not a revival.
//
// Like Pin, what PinFor copies is a request: jobs.Client.Create decides and
// returns what the Job runs, and a caller requiring the image must still
// refuse a Job whose returned stamp is not the repository's.
func (g Guard) PinFor(spec *jobs.Spec, repo *v1alpha1.Repository) string {
	ri := repo.Status.RunnerImage
	switch {
	case ri != nil && ri.Rejected != "":
		return SkipRejected
	case ri == nil || ri.Image == "":
		return SkipNoImage
	case !g.Enabled:
		return SkipDisabled
	case g.Breaker.Tripped():
		return SkipBreaker
	}
	spec.RunnerImage = ri.Image
	spec.RunnerSearchPath = ri.SearchPath
	spec.RunnerImageManifest = ri.Manifest
	return ""
}

// Revived reports whether a human brought the finding back from a terminal
// phase — approve on a HandedOff finding, retry on a Failed one — which the
// append-only phase log records as an earlier HandedOff or Failed entry.
// Such a finding runs on the default image from then on: whatever sent it
// to a human (an incompatible image, an ignore verdict held because the run
// used the repository's image, exhausted attempts) would otherwise be
// retried in the same environment.
func Revived(fnd *v1alpha1.Finding) bool {
	for _, pt := range fnd.Status.PhaseTimes {
		if pt.Phase == v1alpha1.PhaseHandedOff || pt.Phase == v1alpha1.PhaseFailed {
			return true
		}
	}
	return false
}

// PullGrace is how long a repository-image pod may sit waiting on its agent
// container before a collector judges the wait; PullPoll paces the looks
// after it until the container has started.
const (
	PullGrace = 2 * time.Minute
	PullPoll  = 30 * time.Second
)

// Pending decides what a collector does with a Job that has not finished.
// For a default-image Job, or a finished one, it returns nothing: the Job
// watch reports completion, as it always has. For a repository-image Job it
// returns a requeue until the pull grace (timed from the Job's creation) has
// passed — whatever the pod reports, since a pod that has not yet reported a
// container status is indistinguishable from one about to fail its pull —
// then a failure detail once the agent container is stuck on a pull failure
// that will not heal (InvalidImageName, CreateContainerConfigError, or a pull
// error saying the manifest is unknown or not found), a slower requeue until
// the agent container has started, and nothing once it has. The slower
// requeue covers a container waiting on anything else and a pod with no
// container status at all (unscheduled, or waiting for a node past the
// grace): a pull that fails once it is scheduled never mutates the Job, so
// only these looks would see it. Any other pull error keeps waiting for the
// Job's activeDeadlineSeconds.
func Pending(st jobs.Status, now time.Time) (time.Duration, string) {
	if st.Done || st.RunnerImageSource != v1alpha1.RunnerImageSourceRepository {
		return 0, ""
	}
	if elapsed := now.Sub(st.Created); elapsed < PullGrace {
		return PullGrace - elapsed, ""
	}
	if deterministicPull(st.Waiting, st.WaitingMessage) {
		detail := st.Waiting
		if st.WaitingMessage != "" {
			detail += ": " + st.WaitingMessage
		}
		return 0, "agent image pull failed: " + detail
	}
	if !st.AgentStarted {
		return PullPoll, ""
	}
	return 0, ""
}

// deterministicPull reports a waiting state no amount of backoff will clear.
// The kubelet alternates ErrImagePull with ImagePullBackOff, and newer ones
// carry the registry's error on both, so the message is read on either.
func deterministicPull(reason, message string) bool {
	switch reason {
	case "InvalidImageName", "CreateContainerConfigError":
		return true
	case "ErrImagePull", "ImagePullBackOff":
		msg := strings.ToLower(message)
		return strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "not found")
	}
	return false
}

// SandboxUnenforced leads the failure reason a collector records when a
// repository-image Job's prepare init refused to hand over; SandboxReason is
// the whole of it.
const (
	SandboxUnenforced = "SandboxUnenforced"
	SandboxReason     = SandboxUnenforced + ": the sandbox probe found the agent pod's egress open " +
		"(prepare exited 78): NetworkPolicy is not enforced, so repository-declared runner images " +
		"are refused until the controller restarts"
)

// RefusedCondition is the condition a collector sets on a run whose Job
// SandboxRefused judged refused; the run's attempt then does not count
// toward MaxAttempts.
func RefusedCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               v1alpha1.ConditionSandboxRefused,
		Status:             metav1.ConditionTrue,
		Reason:             SandboxUnenforced,
		Message:            SandboxReason,
		ObservedGeneration: generation,
	}
}

// Refused reports whether a run's conditions record that the sandbox probe
// refused it (RefusedCondition), so its attempt is not counted.
func Refused(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, v1alpha1.ConditionSandboxRefused)
}

// SandboxRefused reports whether a repository-image Job's prepare init
// exited with jobs.ExitSandboxUnenforced: the probe found egress open, the
// agent container never started, and there is no log to read. A default
// Job never runs the probe, so its init's exit status is not judged here.
func SandboxRefused(st jobs.Status) bool {
	return st.RunnerImageSource == v1alpha1.RunnerImageSourceRepository &&
		st.InitExitCode != nil && *st.InitExitCode == jobs.ExitSandboxUnenforced
}
