// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
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

// acceptedImage is a declaration source-controller resolved and pinned.
func acceptedImage() *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".patchy/agent.yaml",
		Image: pinnedImage, SearchPath: "/usr/local/go/bin:/usr/bin:/bin", Verified: true,
	}
}

// rejectedImage is a declaration source-controller refused.
func rejectedImage() *v1alpha1.RunnerImage {
	return &v1alpha1.RunnerImage{
		Declared: "docker.io/evil/env:1", Manifest: ".patchy/agent.yaml",
		Rejected: "NotAllowlisted", Message: "`docker.io/evil/env:1` is not under an allowlisted registry",
	}
}

// srcRepo is the finding's Repository with an artifact, the runner image
// record and the given conditions.
func srcRepo(ri *v1alpha1.RunnerImage, conds ...metav1.Condition) *v1alpha1.Repository {
	return &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{
			Name: fndName + "-src", Namespace: "patchy",
			Labels: map[string]string{v1alpha1.LabelFinding: fndName},
		},
		Spec: v1alpha1.RepositorySpec{URL: "https://github.com/acme/orders"},
		Status: v1alpha1.RepositoryStatus{
			Conditions:  conds,
			ResolvedSHA: "abc123",
			Artifact:    &v1alpha1.Artifact{URL: "http://arts/x.tar.gz", Digest: "deadbeef"},
			RunnerImage: ri,
		},
	}
}

func readyCond() metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "ArtifactReady"}
}

func stalledCond(reason, message string) metav1.Condition {
	return metav1.Condition{
		Type: v1alpha1.ConditionStalled, Status: metav1.ConditionTrue, Reason: reason, Message: message,
	}
}

// TestGateForwardsRunnerImageRejection: under onReject handoff the stalled
// Repository parks the finding with source-controller's own explanation;
// under onReject default the rejection is only recorded, the Repository is
// Ready, and the finding is investigated — never parked.
func TestGateForwardsRunnerImageRejection(t *testing.T) {
	rej := rejectedImage()
	tests := []struct {
		name       string
		repo       *v1alpha1.Repository
		wantPhase  v1alpha1.Phase
		wantReason string
		wantFailed string
	}{
		{
			"onReject handoff parks with the stall message",
			srcRepo(rej, metav1.Condition{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonRunnerImageRejected,
			}, stalledCond(v1alpha1.ReasonRunnerImageRejected, rej.Message)),
			v1alpha1.PhaseHandedOff, v1alpha1.ReasonRunnerImageRejected, rej.Message,
		},
		{
			"onReject default is not parked",
			srcRepo(rej, readyCond()),
			v1alpha1.PhaseInvestigating, "", "",
		},
		{
			"an oversized artifact parks as before",
			srcRepo(nil, stalledCond("ArtifactTooLarge", "tarball exceeds the 1073741824-byte artifact cap")),
			v1alpha1.PhaseHandedOff, "ArtifactStalled", "repository artifact exceeds the size cap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := newGate(t, enhancedFinding(), testForge(), tt.repo)
			gateOnce(t, r)
			f := getF(t, c)
			if f.Status.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", f.Status.Phase, tt.wantPhase)
			}
			if f.Status.LastFailureReason != tt.wantFailed {
				t.Errorf("lastFailureReason = %q, want %q", f.Status.LastFailureReason, tt.wantFailed)
			}
			if tt.wantReason == "" {
				return
			}
			cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionForgeResolved)
			if cond == nil || cond.Reason != tt.wantReason || cond.Message != tt.wantFailed {
				t.Errorf("ForgeResolved = %+v, want reason %s with the message", cond, tt.wantReason)
			}
		})
	}
}

// launchable is a granted, not yet launched Investigation, its finding
// (with the given phase history) and the Repository.
func launchable(repo *v1alpha1.Repository, history ...v1alpha1.Phase) []client.Object {
	objs := investigationFixture()
	fnd := objs[0].(*v1alpha1.Finding)
	for _, p := range history {
		fnd.Status.PhaseTimes = append(fnd.Status.PhaseTimes, v1alpha1.PhaseTime{Phase: p, At: metav1.NewTime(clock)})
	}
	objs[1].(*v1alpha1.Investigation).Status.JobRef = nil
	return append(objs, repo)
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "patchy", Name: name}}
}

func getInv(t *testing.T, c client.Client) *v1alpha1.Investigation {
	t.Helper()
	var inv v1alpha1.Investigation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: fndName + "-inv-1"}, &inv); err != nil {
		t.Fatalf("Get investigation: %v", err)
	}
	return &inv
}

// TestLaunchPinsRunnerImage: the launch copies the Repository's pin only
// under the kill switch, with the breaker closed, for a finding no human
// revived — and records what Create returned, not the pin.
func TestLaunchPinsRunnerImage(t *testing.T) {
	fresh := []v1alpha1.Phase{v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating}
	retried := []v1alpha1.Phase{
		v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating,
		v1alpha1.PhaseFailed, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating,
	}
	tripped := runnerguard.NewBreaker("test", nil)
	tripped.Trip(t.Context(), "job-0", "other")
	tests := []struct {
		name       string
		guard      runnerguard.Guard
		repo       *v1alpha1.Repository
		history    []v1alpha1.Phase
		wantImage  string
		wantSource string
	}{
		{"pinned and enabled", runnerguard.Guard{Enabled: true}, srcRepo(acceptedImage(), readyCond()), fresh,
			pinnedImage, v1alpha1.RunnerImageSourceRepository},
		{"kill switch off", runnerguard.Guard{}, srcRepo(acceptedImage(), readyCond()), fresh,
			"", v1alpha1.RunnerImageSourceDefault},
		{"breaker tripped", runnerguard.Guard{Enabled: true, Breaker: tripped}, srcRepo(acceptedImage(), readyCond()), fresh,
			"", v1alpha1.RunnerImageSourceDefault},
		{"rejected under onReject default", runnerguard.Guard{Enabled: true}, srcRepo(rejectedImage(), readyCond()), fresh,
			"", v1alpha1.RunnerImageSourceDefault},
		{"nothing declared", runnerguard.Guard{Enabled: true}, srcRepo(nil, readyCond()), fresh,
			"", v1alpha1.RunnerImageSourceDefault},
		{"retried out of Failed runs the default image", runnerguard.Guard{Enabled: true},
			srcRepo(acceptedImage(), readyCond()), retried,
			"", v1alpha1.RunnerImageSourceDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			r, c := newInvestigation(t, runner, launchable(tt.repo, tt.history...)...)
			r.Images = tt.guard
			applyOnce(t, r)

			if len(runner.created) != 1 {
				t.Fatalf("jobs created = %d, want 1", len(runner.created))
			}
			if got := runner.created[0].RunnerImage; got != tt.wantImage {
				t.Errorf("spec.RunnerImage = %q, want %q", got, tt.wantImage)
			}
			inv := getInv(t, c)
			if inv.Status.JobRef == nil || inv.Status.RunnerImage == nil {
				t.Fatalf("jobRef/runnerImage = %+v/%+v, want both recorded", inv.Status.JobRef, inv.Status.RunnerImage)
			}
			if got := inv.Status.RunnerImage.Source; got != tt.wantSource {
				t.Errorf("status.runnerImage.source = %q, want %q", got, tt.wantSource)
			}
			if tt.wantSource == v1alpha1.RunnerImageSourceRepository &&
				(inv.Status.RunnerImage.Image != pinnedImage || inv.Status.RunnerImage.Manifest != ".patchy/agent.yaml") {
				t.Errorf("status.runnerImage = %+v, want the pinned image from .patchy/agent.yaml", inv.Status.RunnerImage)
			}
		})
	}
}

// stamped is the running investigationFixture with its launch stamp.
func stamped(source string) []client.Object {
	objs := investigationFixture()
	img := "claude-agent-runner:1"
	if source == v1alpha1.RunnerImageSourceRepository {
		img = pinnedImage
	}
	objs[1].(*v1alpha1.Investigation).Status.RunnerImage = &v1alpha1.RunnerImageRef{Image: img, Source: source}
	return objs
}

// TestRouteHoldsIgnoreOnStamp: an ignore verdict is held for a human only
// when the Investigation's launch stamp says the pod ran a repository
// image; every other verdict routes as it always has.
func TestRouteHoldsIgnoreOnStamp(t *testing.T) {
	r := &InvestigationReconciler{ConfidenceThreshold: 0.75}
	stamp := func(source string) *v1alpha1.Investigation {
		inv := &v1alpha1.Investigation{}
		if source != "" {
			inv.Status.RunnerImage = &v1alpha1.RunnerImageRef{Image: "img", Source: source}
		}
		return inv
	}
	tests := []struct {
		rec    string
		source string
		want   v1alpha1.Phase
	}{
		{"ignore", "", v1alpha1.PhaseDismissed},
		{"ignore", v1alpha1.RunnerImageSourceDefault, v1alpha1.PhaseDismissed},
		{"ignore", v1alpha1.RunnerImageSourceRepository, v1alpha1.PhaseHandedOff},
		{"manual", "", v1alpha1.PhaseHandedOff},
		{"manual", v1alpha1.RunnerImageSourceRepository, v1alpha1.PhaseHandedOff},
		{"remediate", "", v1alpha1.PhaseQueued},
		{"remediate", v1alpha1.RunnerImageSourceDefault, v1alpha1.PhaseQueued},
		{"remediate", v1alpha1.RunnerImageSourceRepository, v1alpha1.PhaseQueued},
		{"bogus", v1alpha1.RunnerImageSourceRepository, v1alpha1.PhaseHandedOff},
	}
	for _, tt := range tests {
		t.Run(tt.rec+"/"+nonEmpty(tt.source, "unstamped"), func(t *testing.T) {
			result := investigationEvent(tt.rec, 0.9, false)[0].Investigation
			got, _, _ := r.route(enhancedFinding(), stamp(tt.source), result)
			if got != tt.want {
				t.Errorf("route = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyHoldsRepositoryImageIgnore: a repository-image ignore lands in
// HandedOff, and the Finding's summary mirrors the stamp for the
// projection; the same verdict from a default image still dismisses.
func TestApplyHoldsRepositoryImageIgnore(t *testing.T) {
	for _, tt := range []struct {
		source string
		want   v1alpha1.Phase
	}{
		{v1alpha1.RunnerImageSourceRepository, v1alpha1.PhaseHandedOff},
		{v1alpha1.RunnerImageSourceDefault, v1alpha1.PhaseDismissed},
	} {
		t.Run(tt.source, func(t *testing.T) {
			runner := &fakeRunner{done: true, events: investigationEvent("ignore", 0.95, false)}
			r, c := newInvestigation(t, runner, stamped(tt.source)...)
			// The kill switch is off at collect time: routing must not care.
			r.Images = runnerguard.Guard{}
			applyOnce(t, r)
			f := getF(t, c)
			if f.Status.Phase != tt.want {
				t.Errorf("phase = %q, want %q", f.Status.Phase, tt.want)
			}
			if f.Status.Investigation == nil || f.Status.Investigation.RunnerImage == nil ||
				f.Status.Investigation.RunnerImage.Source != tt.source {
				t.Errorf("summary.runnerImage = %+v, want source %s mirrored", f.Status.Investigation, tt.source)
			}
		})
	}
}

// TestCollectPullFailFast: a default-image Job stuck pulling is left to the
// Job watch exactly as before; a repository-image one is requeued through
// the grace and failed once the pull cannot heal.
func TestCollectPullFailFast(t *testing.T) {
	manifestUnknown := "failed to pull and unpack image: manifest unknown"
	tests := []struct {
		name        string
		source      string
		st          jobs.Status
		wantRequeue time.Duration
		wantFailed  bool
	}{
		{"default image in ImagePullBackOff is not failed", v1alpha1.RunnerImageSourceDefault, jobs.Status{
			Active: 1, Created: clock.Add(-time.Hour), Waiting: "ImagePullBackOff", WaitingMessage: manifestUnknown,
			RunnerImageSource: v1alpha1.RunnerImageSourceDefault,
		}, 0, false},
		{"repository image within the grace is requeued", v1alpha1.RunnerImageSourceRepository, jobs.Status{
			Active: 1, Created: clock.Add(-30 * time.Second), Waiting: "ErrImagePull", WaitingMessage: manifestUnknown,
			RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}, runnerguard.PullGrace - 30*time.Second, false},
		{"repository image with manifest unknown after the grace is failed", v1alpha1.RunnerImageSourceRepository,
			jobs.Status{
				Active: 1, Created: clock.Add(-5 * time.Minute), Waiting: "ErrImagePull", WaitingMessage: manifestUnknown,
				RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
			}, 0, true},
		// Still unscheduled past the grace: a pull failure once it lands on a
		// node never mutates the Job, so the collector keeps looking.
		{"repository image with no pod status after the grace keeps polling", v1alpha1.RunnerImageSourceRepository,
			jobs.Status{
				Active: 1, Created: clock.Add(-5 * time.Minute), RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
			}, runnerguard.PullPoll, false},
		{"repository image whose agent started is left to the Job watch", v1alpha1.RunnerImageSourceRepository,
			jobs.Status{
				Active: 1, Created: clock.Add(-5 * time.Minute), AgentStarted: true,
				RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
			}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := tt.st
			runner := &fakeRunner{status: &st}
			r, c := newInvestigation(t, runner, stamped(tt.source)...)
			res, err := r.Reconcile(t.Context(), reqFor(fndName+"-inv-1"))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.RequeueAfter != tt.wantRequeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}
			inv := getInv(t, c)
			f := getF(t, c)
			if !tt.wantFailed {
				if inv.Status.Phase != v1alpha1.RunRunning || f.Status.Phase != v1alpha1.PhaseInvestigating {
					t.Errorf("run/finding = %q/%q, want Running/Investigating", inv.Status.Phase, f.Status.Phase)
				}
				if len(runner.deleted) != 0 || runner.results != 0 {
					t.Errorf("deleted/results = %v/%d, want the Job left alone", runner.deleted, runner.results)
				}
				return
			}
			if inv.Status.Phase != v1alpha1.RunFailed || inv.Status.Stage == nil || inv.Status.Stage.Outcome != "aborted" {
				t.Errorf("run = %q stage %+v, want Failed / aborted", inv.Status.Phase, inv.Status.Stage)
			}
			if f.Status.Phase != v1alpha1.PhaseEnhanced ||
				!strings.HasPrefix(f.Status.LastFailureReason, "aborted: agent image pull failed: ErrImagePull") {
				t.Errorf("finding = %q %q, want Enhanced with the pull failure", f.Status.Phase, f.Status.LastFailureReason)
			}
			if len(runner.deleted) != 1 || runner.deleted[0] != "job-1" {
				t.Errorf("deleted = %v, want the stuck Job deleted", runner.deleted)
			}
		})
	}
}

// TestCollectSandboxRefused: a repository-image Job whose prepare init
// exited 78 fails the run SandboxUnenforced without consuming the attempt
// (even the last one), trips the breaker, and the next launch runs the
// default image.
func TestCollectSandboxRefused(t *testing.T) {
	objs := stamped(v1alpha1.RunnerImageSourceRepository)
	// The final attempt: a consumed failure here would exhaust the finding.
	objs[0].(*v1alpha1.Finding).Status.Attempts.Investigation = 2
	objs[1].(*v1alpha1.Investigation).Spec.Attempt = 2
	objs = append(objs, srcRepo(acceptedImage(), readyCond()))
	exit := int32(jobs.ExitSandboxUnenforced)
	runner := &fakeRunner{status: &jobs.Status{
		Failed: 1, Done: true, Waiting: "PodInitializing", InitExitCode: &exit,
		RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}}
	breaker := runnerguard.NewBreaker("test", nil)
	r, c := newInvestigation(t, runner, objs...)
	r.Images = runnerguard.Guard{Enabled: true, Breaker: breaker}
	applyOnce(t, r)

	if runner.results != 0 {
		t.Errorf("Result read %d times, want 0 (the agent never started)", runner.results)
	}
	if !breaker.Tripped() {
		t.Error("breaker not tripped")
	}
	inv := getInv(t, c)
	if inv.Status.Phase != v1alpha1.RunFailed || inv.Status.Stage == nil ||
		!strings.HasPrefix(inv.Status.Stage.Detail, runnerguard.SandboxUnenforced) {
		t.Errorf("run = %q stage %+v, want Failed with SandboxUnenforced", inv.Status.Phase, inv.Status.Stage)
	}
	f := getF(t, c)
	if f.Status.Phase != v1alpha1.PhaseEnhanced {
		t.Errorf("phase = %q, want Enhanced (attempt not consumed)", f.Status.Phase)
	}
	if !strings.HasPrefix(f.Status.LastFailureReason, runnerguard.SandboxUnenforced+": ") {
		t.Errorf("lastFailureReason = %q, want it led by SandboxUnenforced", f.Status.LastFailureReason)
	}

	// The next attempt runs on the default image.
	next := launchable(srcRepo(acceptedImage(), readyCond()), v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating)
	runner2 := &fakeRunner{}
	r2, _ := newInvestigation(t, runner2, next...)
	r2.Images = runnerguard.Guard{Enabled: true, Breaker: breaker}
	applyOnce(t, r2)
	if len(runner2.created) != 1 || runner2.created[0].RunnerImage != "" {
		t.Errorf("next launch = %+v, want the default image while the breaker is tripped", runner2.created)
	}
}

// TestCollectDefaultInitExit78NotJudged: a default Job never runs the
// probe, so its init's exit status is not read as a sandbox refusal.
func TestCollectDefaultInitExit78NotJudged(t *testing.T) {
	exit := int32(jobs.ExitSandboxUnenforced)
	runner := &fakeRunner{
		status: &jobs.Status{
			Failed: 1, Done: true, InitExitCode: &exit, RunnerImageSource: v1alpha1.RunnerImageSourceDefault,
		},
		events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeFatal, Error: "init failed"}},
	}
	breaker := runnerguard.NewBreaker("test", nil)
	r, c := newInvestigation(t, runner, stamped(v1alpha1.RunnerImageSourceDefault)...)
	r.Images = runnerguard.Guard{Enabled: true, Breaker: breaker}
	applyOnce(t, r)
	if breaker.Tripped() {
		t.Error("breaker tripped by a default Job")
	}
	if runner.results != 1 {
		t.Errorf("Result read %d times, want the usual one read", runner.results)
	}
	if got := getF(t, c).Status.LastFailureReason; strings.Contains(got, runnerguard.SandboxUnenforced) {
		t.Errorf("lastFailureReason = %q, want the ordinary failure", got)
	}
}
