// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
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
	if rem := getRem(t, c); rem.Status.Phase != v1alpha1.RunFailed ||
		!meta.IsStatusConditionTrue(rem.Status.Conditions, v1alpha1.ConditionSandboxRefused) {
		t.Errorf("run = %q conditions %+v, want Failed and SandboxRefused", rem.Status.Phase, rem.Status.Conditions)
	}
	f := findingNow(t, c)
	if f.Status.Phase != v1alpha1.PhaseQueued {
		t.Errorf("phase = %q, want Queued (attempt not consumed)", f.Status.Phase)
	}
	if !strings.HasPrefix(f.Status.LastFailureReason, runnerguard.SandboxUnenforced+": ") {
		t.Errorf("lastFailureReason = %q, want it led by SandboxUnenforced", f.Status.LastFailureReason)
	}
}

// TestRemediationChangesetRejected: a changeset reaching outside the tree
// is refused on any run; one over the entry cap or touching CI definitions
// is refused on a repository-image run only. Every refusal fails the
// attempt changeset_rejected with zero forge calls.
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
		{"over the entry cap on a repository image", v1alpha1.RunnerImageSourceRepository,
			[]envelope.FileChange{file("a.go"), file("b.go"), file("c.go")}, []string{"d.go"},
			"4 entries (upserts plus deletes), over the 3-entry limit"},
		{"over the entry cap on a default image is pushed as before", v1alpha1.RunnerImageSourceDefault,
			[]envelope.FileChange{file("a.go"), file("b.go"), file("c.go")}, []string{"d.go"}, ""},
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

// rejectedOnce runs one collect of a successful remediation carrying cs and
// reports the forge calls it made and where the run and finding ended.
func rejectedOnce(t *testing.T, objs []client.Object, cs func(*envelope.Changeset)) (
	*fakeForge, *v1alpha1.Remediation, *v1alpha1.Finding,
) {
	t.Helper()
	events := crdRemediationEvent(true)
	cs(events[0].Remediation.Changeset)
	runner := &fakeCRRunner{done: true, events: events}
	fw := &fakeForge{}
	r, c := newRemediation(t, runner, fw, objs...)
	remReconcile(t, r)
	return fw, getRem(t, c), findingNow(t, c)
}

// wantRejected asserts a changeset_rejected run with zero forge calls whose
// failure reason contains want.
func wantRejected(t *testing.T, fw *fakeForge, rem *v1alpha1.Remediation, f *v1alpha1.Finding, want string) {
	t.Helper()
	if len(fw.pushed) != 0 || fw.prCalls != 0 {
		t.Fatalf("pushed/prs = %v/%d, want zero forge calls", fw.pushed, fw.prCalls)
	}
	if rem.Status.Phase != v1alpha1.RunFailed || rem.Status.Stage == nil ||
		rem.Status.Stage.Outcome != string(envelope.OutcomeChangesetRejected) {
		t.Errorf("run = %q stage %+v, want Failed / changeset_rejected", rem.Status.Phase, rem.Status.Stage)
	}
	if !strings.HasPrefix(f.Status.LastFailureReason, "changeset_rejected: ") ||
		!strings.Contains(f.Status.LastFailureReason, want) {
		t.Errorf("lastFailureReason = %q, want changeset_rejected naming %q", f.Status.LastFailureReason, want)
	}
}

// TestRemediationChangesetForgedBase: the changeset's base is the pod's
// word, and the push parents it and builds the tree on it. A base other
// than the Repository's pinned commit is refused before any forge call on
// every run: a repository image could otherwise point patchy's branch at a
// fork's head carrying its own workflows, and a changeset diffed against
// one tree and pushed onto another would silently revert whatever differs.
func TestRemediationChangesetForgedBase(t *testing.T) {
	forged := "f00df00df00df00df00df00df00df00df00df00d"
	for _, source := range []string{v1alpha1.RunnerImageSourceRepository, v1alpha1.RunnerImageSourceDefault} {
		t.Run(source, func(t *testing.T) {
			fw, rem, f := rejectedOnce(t, withImage(acceptedImage(), source), func(cs *envelope.Changeset) {
				cs.BaseSHA = forged
			})
			wantRejected(t, fw, rem, f, "is not the repository's pinned commit")
		})
	}
}

// TestRemediationChangesetUnpushable: a mode the envelope does not allow,
// or content that is not base64, fails at the forge on every retry — after
// a write token is minted and, for the mode, after every blob is created.
// Both are refused as changeset_rejected with zero forge calls instead of
// being retried as a transient push error.
func TestRemediationChangesetUnpushable(t *testing.T) {
	tests := []struct {
		name string
		fc   envelope.FileChange
		want string
	}{
		{"unknown mode", envelope.FileChange{Path: "b.go", Mode: "999999", ContentB64: "eA=="},
			`"b.go" has mode "999999"`},
		{"tree mode", envelope.FileChange{Path: "b", Mode: "040000", ContentB64: "eA=="},
			`"b" has mode "040000"`},
		{"undecodable content", envelope.FileChange{Path: "b.go", Mode: "100644", ContentB64: "not base64!"},
			`"b.go" content is not base64`},
	}
	for _, tt := range tests {
		for _, source := range []string{v1alpha1.RunnerImageSourceRepository, v1alpha1.RunnerImageSourceDefault} {
			t.Run(tt.name+"/"+source, func(t *testing.T) {
				fw, rem, f := rejectedOnce(t, withImage(acceptedImage(), source), func(cs *envelope.Changeset) {
					cs.Upserts = append(cs.Upserts, tt.fc)
				})
				wantRejected(t, fw, rem, f, tt.want)
			})
		}
	}
}

// TestRemediationChangesetEntryCapDefaultImage pins what the entry cap
// means with repository images off: a default-image changeset over the
// default cap (a vendored dependency bump rewrites hundreds of files) is
// pushed exactly as before this feature, while the same changeset from a
// repository-image run is refused before any forge call.
func TestRemediationChangesetEntryCapDefaultImage(t *testing.T) {
	many := func(cs *envelope.Changeset) {
		cs.Upserts = nil
		for i := range DefaultChangesetMaxEntries + 1 {
			cs.Upserts = append(cs.Upserts, envelope.FileChange{
				Path: "vendor/example.com/m/f" + strconv.Itoa(i) + ".go", Mode: "100644", ContentB64: "eA==",
			})
		}
	}
	t.Run("default image is pushed as before", func(t *testing.T) {
		fw, _, f := rejectedOnce(t, withImage(acceptedImage(), v1alpha1.RunnerImageSourceDefault), many)
		if len(fw.pushed) != 1 || fw.prCalls != 1 || f.Status.Phase != v1alpha1.PhaseInReview {
			t.Errorf("pushed/prs/phase = %v/%d/%q, want pushed and InReview", fw.pushed, fw.prCalls, f.Status.Phase)
		}
	})
	t.Run("repository image is refused", func(t *testing.T) {
		fw, rem, f := rejectedOnce(t, withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository), many)
		wantRejected(t, fw, rem, f, "501 entries (upserts plus deletes), over the 500-entry limit")
	})
}

// TestRemediationChangesetInvestigationImage: the remediation agent acts on
// the investigation's report and parameters, so a default-image run whose
// investigation ran a repository image — or whose investigation cannot be
// found to say otherwise — is held to the repository-image rules: a
// workflow change is refused with zero forge calls. One whose
// investigation ran the default image pushes as before.
func TestRemediationChangesetInvestigationImage(t *testing.T) {
	workflow := func(cs *envelope.Changeset) {
		cs.Upserts = append(cs.Upserts, envelope.FileChange{
			Path: ".github/workflows/ci.yml", Mode: "100644", ContentB64: "eA==",
		})
	}
	revived := []v1alpha1.Phase{v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating}
	t.Run("investigation ran a repository image", func(t *testing.T) {
		objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceDefault, revived...)
		objs[2].(*v1alpha1.Investigation).Status.RunnerImage = &v1alpha1.RunnerImageRef{
			Image: pinnedImage, Source: v1alpha1.RunnerImageSourceRepository,
		}
		fw, rem, f := rejectedOnce(t, objs, workflow)
		wantRejected(t, fw, rem, f, `".github/workflows/ci.yml" is a CI definition`)
	})
	t.Run("investigation not found", func(t *testing.T) {
		objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceDefault, revived...)
		objs = append(objs[:2], objs[3:]...) // no Investigation
		fw, rem, f := rejectedOnce(t, objs, workflow)
		wantRejected(t, fw, rem, f, `".github/workflows/ci.yml" is a CI definition`)
	})
	t.Run("investigation ran the default image", func(t *testing.T) {
		objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceDefault, revived...)
		objs[2].(*v1alpha1.Investigation).Status.RunnerImage = &v1alpha1.RunnerImageRef{
			Image: "claude-agent-runner:1", Source: v1alpha1.RunnerImageSourceDefault,
		}
		fw, _, f := rejectedOnce(t, objs, workflow)
		if len(fw.pushed) != 1 || fw.prCalls != 1 || f.Status.Phase != v1alpha1.PhaseInReview {
			t.Errorf("pushed/prs/phase = %v/%d/%q, want pushed as before", fw.pushed, fw.prCalls, f.Status.Phase)
		}
	})
}

// TestRemediationSandboxRefusalConsumesNoAttempt: a refused attempt does
// not count toward MaxAttempts. Attempt 1 is refused by the sandbox probe;
// attempt 2 (on the default image) fails for an ordinary reason and is —
// with MaxAttempts 2 — the first real attempt, so the finding is queued
// again, not exhausted. A refusal the pod merely claims in its own output
// is an ordinary failure and does count.
func TestRemediationSandboxRefusalConsumesNoAttempt(t *testing.T) {
	exit := int32(jobs.ExitSandboxUnenforced)
	claimed := crdRemediationEvent(false)
	claimed[0].Remediation.Stage = envelope.Stage{
		Outcome: envelope.Outcome(runnerguard.SandboxUnenforced), Detail: runnerguard.SandboxReason,
	}
	tests := []struct {
		name      string
		attempt1  fakeCRRunner
		wantAfter v1alpha1.Phase
	}{
		{"refused by the probe", fakeCRRunner{status: &jobs.Status{
			Failed: 1, Done: true, InitExitCode: &exit, RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}}, v1alpha1.PhaseQueued},
		{"refusal claimed in the pod's output", fakeCRRunner{done: true, events: claimed}, v1alpha1.PhaseFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &tt.attempt1
			r, c := newRemediation(t, runner, &fakeForge{},
				withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)...)
			r.Images = runnerguard.Guard{Enabled: true, Breaker: runnerguard.NewBreaker("test", nil)}
			remReconcile(t, r)
			if got := findingNow(t, c).Status.Phase; got != v1alpha1.PhaseQueued {
				t.Fatalf("after attempt 1 phase = %q, want Queued", got)
			}

			// The spawner opens attempt 2 and the scheduler grants it.
			rem2 := runningRemediation()[1].(*v1alpha1.Remediation)
			rem2.Name, rem2.Spec.Attempt = "finding-aa-1-rem-2", 2
			status := v1alpha1.RemediationStatus{
				Phase: v1alpha1.RunRunning, JobRef: &v1alpha1.JobReference{Name: "job-rem-2"},
				RunnerImage: &v1alpha1.RunnerImageRef{
					Image: "claude-agent-runner:1", Source: v1alpha1.RunnerImageSourceDefault,
				},
			}
			if err := c.Create(t.Context(), rem2); err != nil {
				t.Fatalf("Create attempt 2: %v", err)
			}
			rem2.Status = status
			if err := c.Status().Update(t.Context(), rem2); err != nil {
				t.Fatalf("stamp attempt 2: %v", err)
			}
			f := findingNow(t, c)
			if err := v1alpha1.SetPhase(f, v1alpha1.PhaseRemediating, crdClock); err != nil {
				t.Fatal(err)
			}
			f.Status.Attempts.Remediation = 2
			if err := c.Status().Update(t.Context(), f); err != nil {
				t.Fatalf("grant attempt 2: %v", err)
			}

			runner.status, runner.done = nil, true
			runner.events = []envelope.Event{{V: envelope.Version, Type: envelope.TypeFatal, Error: "claude exited 1"}}
			remOnce(t, r, "finding-aa-1-rem-2")
			if f := findingNow(t, c); f.Status.Phase != tt.wantAfter {
				t.Errorf("after attempt 2 phase = %q (%s), want %q", f.Status.Phase, f.Status.LastFailureReason,
					tt.wantAfter)
			}
		})
	}
}
