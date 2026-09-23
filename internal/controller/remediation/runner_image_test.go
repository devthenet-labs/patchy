// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
)

const pinnedImage = "ghcr.io/acme/go-env@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const remName = "finding-aa-1-rem-1"

// acceptedImage is a declaration source-controller resolved and pinned.
func acceptedImage() *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".patchy/agent.yaml",
		Image: pinnedImage, SearchPath: "/usr/local/go/bin:/usr/bin:/bin", Verified: true,
	}
}

// withImage is runningRemediation with the Repository's runner image
// record, the Remediation's launch stamp (empty source: none) and the
// finding's phase history.
func withImage(ri *v1alpha1.RunnerImage, stampSource string, history ...v1alpha1.Phase) []client.Object {
	objs := runningRemediation()
	fnd := objs[0].(*v1alpha1.Finding)
	for _, p := range history {
		fnd.Status.PhaseTimes = append(fnd.Status.PhaseTimes, v1alpha1.PhaseTime{Phase: p, At: metav1.NewTime(crdClock)})
	}
	if stampSource != "" {
		img := "claude-agent-runner:1"
		if stampSource == v1alpha1.RunnerImageSourceRepository {
			img = pinnedImage
		}
		objs[1].(*v1alpha1.Remediation).Status.RunnerImage = &v1alpha1.RunnerImageRef{Image: img, Source: stampSource}
	}
	objs[3].(*v1alpha1.Repository).Status.RunnerImage = ri
	return objs
}

func getRem(t *testing.T, c client.Client) *v1alpha1.Remediation {
	t.Helper()
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: remName}, &rem); err != nil {
		t.Fatalf("Get remediation: %v", err)
	}
	return &rem
}

func remReconcile(t *testing.T, r *RemediationReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: remName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// TestRemediationLaunchPinsRunnerImage: the launch copies the Repository's
// pin only under the kill switch for a finding no human revived — releasing
// an approval hold is not a revival — and records what Create returned.
func TestRemediationLaunchPinsRunnerImage(t *testing.T) {
	investigated := []v1alpha1.Phase{v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating}
	then := func(more ...v1alpha1.Phase) []v1alpha1.Phase {
		return append(append([]v1alpha1.Phase{}, investigated...), more...)
	}
	tests := []struct {
		name       string
		guard      runnerguard.Guard
		history    []v1alpha1.Phase
		wantSource string
	}{
		{"queued straight from the verdict", runnerguard.Guard{Enabled: true},
			then(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating), v1alpha1.RunnerImageSourceRepository},
		{"approval hold released", runnerguard.Guard{Enabled: true},
			then(v1alpha1.PhaseAwaitingApproval, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating),
			v1alpha1.RunnerImageSourceRepository},
		{"kill switch off", runnerguard.Guard{},
			then(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating), v1alpha1.RunnerImageSourceDefault},
		{"approved out of HandedOff runs the default image", runnerguard.Guard{Enabled: true},
			then(v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating),
			v1alpha1.RunnerImageSourceDefault},
		{"retried out of Failed runs the default image", runnerguard.Guard{Enabled: true},
			then(v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating, v1alpha1.PhaseFailed, v1alpha1.PhaseQueued,
				v1alpha1.PhaseRemediating),
			v1alpha1.RunnerImageSourceDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(acceptedImage(), "", tt.history...)
			objs[1].(*v1alpha1.Remediation).Status.JobRef = nil // granted, not launched
			runner := &fakeCRRunner{}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			r.Images = tt.guard
			remReconcile(t, r)

			if len(runner.created) != 1 {
				t.Fatalf("jobs = %d, want 1", len(runner.created))
			}
			wantImage := ""
			if tt.wantSource == v1alpha1.RunnerImageSourceRepository {
				wantImage = pinnedImage
			}
			if got := runner.created[0].RunnerImage; got != wantImage {
				t.Errorf("spec.RunnerImage = %q, want %q", got, wantImage)
			}
			rem := getRem(t, c)
			if rem.Status.JobRef == nil || rem.Status.RunnerImage == nil || rem.Status.RunnerImage.Source != tt.wantSource {
				t.Errorf("jobRef/runnerImage = %+v/%+v, want source %s recorded", rem.Status.JobRef,
					rem.Status.RunnerImage, tt.wantSource)
			}
		})
	}
}

// TestRemediationCollectPullFailFast: a default-image Job stuck pulling is
// left to the Job watch as before; a repository-image one with a manifest
// the registry does not have is failed after the grace, before any forge
// call, and its Job deleted.
func TestRemediationCollectPullFailFast(t *testing.T) {
	manifestUnknown := "failed to pull and unpack image: manifest unknown"
	for _, tt := range []struct {
		name       string
		source     string
		wantFailed bool
	}{
		{"default image in ImagePullBackOff is not failed", v1alpha1.RunnerImageSourceDefault, false},
		{"repository image with manifest unknown is failed", v1alpha1.RunnerImageSourceRepository, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			waiting := "ImagePullBackOff"
			if tt.wantFailed {
				waiting = "ErrImagePull"
			}
			runner := &fakeCRRunner{status: &jobs.Status{
				Active: 1, Created: crdClock.Add(-10 * time.Minute), Waiting: waiting, WaitingMessage: manifestUnknown,
				RunnerImageSource: tt.source,
			}}
			fw := &fakeForge{}
			r, c := newRemediation(t, runner, fw, withImage(acceptedImage(), tt.source)...)
			res := remReconcile(t, r)

			if len(fw.pushed) != 0 || fw.prCalls != 0 || runner.results != 0 {
				t.Errorf("pushed/prs/results = %v/%d/%d, want no forge call and no log read",
					fw.pushed, fw.prCalls, runner.results)
			}
			rem := getRem(t, c)
			f := findingNow(t, c)
			if !tt.wantFailed {
				if res.RequeueAfter != 0 || rem.Status.Phase != v1alpha1.RunRunning ||
					f.Status.Phase != v1alpha1.PhaseRemediating || len(runner.deleted) != 0 {
					t.Errorf("requeue %v, run %q, finding %q, deleted %v: want the default Job left to its watch",
						res.RequeueAfter, rem.Status.Phase, f.Status.Phase, runner.deleted)
				}
				return
			}
			if rem.Status.Phase != v1alpha1.RunFailed || f.Status.Phase != v1alpha1.PhaseQueued ||
				!strings.HasPrefix(f.Status.LastFailureReason, "aborted: agent image pull failed: ErrImagePull") {
				t.Errorf("run %q, finding %q %q: want Failed, Queued, the pull failure",
					rem.Status.Phase, f.Status.Phase, f.Status.LastFailureReason)
			}
			if len(runner.deleted) != 1 {
				t.Errorf("deleted = %v, want the stuck Job deleted", runner.deleted)
			}
		})
	}
}

// TestRemediationSandboxRefused: a repository-image Job refused by the
// sandbox probe trips the breaker and re-queues the finding even on its
// last attempt, with no log read and no forge call.
func TestRemediationSandboxRefused(t *testing.T) {
	objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
	objs[0].(*v1alpha1.Finding).Status.Attempts.Remediation = 2
	objs[1].(*v1alpha1.Remediation).Spec.Attempt = 2 // MaxAttempts is 2
	exit := int32(jobs.ExitSandboxUnenforced)
	runner := &fakeCRRunner{status: &jobs.Status{
		Failed: 1, Done: true, Waiting: "PodInitializing", InitExitCode: &exit,
		RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}}
	fw := &fakeForge{}
	breaker := runnerguard.NewBreaker("test", nil)
	r, c := newRemediation(t, runner, fw, objs...)
	r.Images = runnerguard.Guard{Enabled: true, Breaker: breaker}
	remReconcile(t, r)

	if !breaker.Tripped() {
		t.Error("breaker not tripped")
	}
	if runner.results != 0 || len(fw.pushed) != 0 || fw.prCalls != 0 {
		t.Errorf("results/pushed/prs = %d/%v/%d, want none", runner.results, fw.pushed, fw.prCalls)
	}
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("run = %q, want Failed", rem.Status.Phase)
	}
	f := findingNow(t, c)
	if f.Status.Phase != v1alpha1.PhaseQueued {
		t.Errorf("phase = %q, want Queued (attempt not consumed)", f.Status.Phase)
	}
	if !strings.HasPrefix(f.Status.LastFailureReason, runnerguard.SandboxUnenforced+": ") {
		t.Errorf("lastFailureReason = %q, want it led by SandboxUnenforced", f.Status.LastFailureReason)
	}
}

// TestRemediationChangesetRejected: a changeset over the entry cap, or one
// reaching outside the tree, is refused on any run; one touching CI
// definitions is refused on a repository-image run only. Every refusal
// fails the attempt changeset_rejected with zero forge calls.
func TestRemediationChangesetRejected(t *testing.T) {
	file := func(p string) envelope.FileChange {
		return envelope.FileChange{Path: p, Mode: "100644", ContentB64: "eA=="}
	}
	tests := []struct {
		name       string
		source     string
		upserts    []envelope.FileChange
		deletes    []string
		wantReject string
	}{
		{"over the entry cap", v1alpha1.RunnerImageSourceDefault,
			[]envelope.FileChange{file("a.go"), file("b.go"), file("c.go")}, []string{"d.go"},
			"4 entries (upserts plus deletes), over the 3-entry limit"},
		{"workflow on a repository image", v1alpha1.RunnerImageSourceRepository,
			[]envelope.FileChange{file("a.go"), file(".github/workflows/release.yml")}, nil,
			`".github/workflows/release.yml" is a CI definition`},
		{"workflow deleted on a repository image", v1alpha1.RunnerImageSourceRepository,
			[]envelope.FileChange{file("a.go")}, []string{".github/workflows/scan.yml"},
			`".github/workflows/scan.yml" is a CI definition`},
		{"composite action on a repository image", v1alpha1.RunnerImageSourceRepository,
			[]envelope.FileChange{file(".github/actions/setup/action.yml")}, nil,
			`".github/actions/setup/action.yml" is a CI definition`},
		{"escaping the tree on a default image", v1alpha1.RunnerImageSourceDefault,
			[]envelope.FileChange{file("../../etc/passwd")}, nil, `has a ".." component`},
		{"workflow on a default image is pushed as before", v1alpha1.RunnerImageSourceDefault,
			[]envelope.FileChange{file("a.go"), file(".github/workflows/release.yml")}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := crdRemediationEvent(true)
			events[0].Remediation.Changeset.Upserts = tt.upserts
			events[0].Remediation.Changeset.Deletes = tt.deletes
			runner := &fakeCRRunner{done: true, events: events}
			fw := &fakeForge{}
			r, c := newRemediation(t, runner, fw, withImage(acceptedImage(), tt.source)...)
			r.MaxChangesetEntries = 3
			remReconcile(t, r)

			f := findingNow(t, c)
			rem := getRem(t, c)
			if tt.wantReject == "" {
				if len(fw.pushed) != 1 || fw.prCalls != 1 || f.Status.Phase != v1alpha1.PhaseInReview {
					t.Errorf("pushed/prs/phase = %v/%d/%q, want pushed and InReview", fw.pushed, fw.prCalls, f.Status.Phase)
				}
				return
			}
			if len(fw.pushed) != 0 || fw.prCalls != 0 {
				t.Fatalf("pushed/prs = %v/%d, want zero forge calls", fw.pushed, fw.prCalls)
			}
			if rem.Status.Phase != v1alpha1.RunFailed || rem.Status.Stage == nil ||
				rem.Status.Stage.Outcome != string(envelope.OutcomeChangesetRejected) {
				t.Errorf("run = %q stage %+v, want Failed / changeset_rejected", rem.Status.Phase, rem.Status.Stage)
			}
			// The rejected run's accounting survives the rejection.
			if rem.Status.Stage != nil && rem.Status.Stage.Usage.CostUSD != "3.500000" {
				t.Errorf("stage cost = %q, want the run's 3.500000 kept", rem.Status.Stage.Usage.CostUSD)
			}
			if !strings.HasPrefix(f.Status.LastFailureReason, "changeset_rejected: ") ||
				!strings.Contains(f.Status.LastFailureReason, tt.wantReject) {
				t.Errorf("lastFailureReason = %q, want changeset_rejected naming %q", f.Status.LastFailureReason, tt.wantReject)
			}
		})
	}
}
