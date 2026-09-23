// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

const pinned = "ghcr.io/acme/go-env@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var clock = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func repoWith(ri *v1alpha1.RunnerImage) *v1alpha1.Repository {
	return &v1alpha1.Repository{Status: v1alpha1.RepositoryStatus{RunnerImage: ri}}
}

func findingThrough(phases ...v1alpha1.Phase) *v1alpha1.Finding {
	f := &v1alpha1.Finding{}
	for _, p := range phases {
		f.Status.PhaseTimes = append(f.Status.PhaseTimes, v1alpha1.PhaseTime{Phase: p, At: metav1.NewTime(clock)})
	}
	return f
}

func trippedBreaker() *Breaker {
	b := NewBreaker("test", nil)
	b.Trip(context.Background(), "job", "finding")
	return b
}

// TestPin: a launch copies the pin only with the kill switch on, the
// breaker closed and the finding never revived — and only an image
// source-controller actually pinned.
func TestPin(t *testing.T) {
	accepted := &v1alpha1.RunnerImage{
		Declared: "ghcr.io/acme/go-env:1.26", Manifest: ".patchy/agent.yaml",
		Image: pinned, SearchPath: "/usr/local/go/bin:/usr/bin:/bin", Verified: true,
	}
	fresh := findingThrough(v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating)
	tests := []struct {
		name     string
		guard    Guard
		repo     *v1alpha1.Repository
		finding  *v1alpha1.Finding
		wantPin  bool
		wantSkip string
	}{
		{"enabled, pinned, fresh finding", Guard{Enabled: true}, repoWith(accepted), fresh, true, ""},
		{"nothing declared", Guard{Enabled: true}, repoWith(nil), fresh, false, ""},
		{"not-applicable devcontainer", Guard{Enabled: true}, repoWith(&v1alpha1.RunnerImage{
			Manifest: ".devcontainer/devcontainer.json", Message: "builds its image",
		}), fresh, false, ""},
		{"rejected under onReject default", Guard{Enabled: true}, repoWith(&v1alpha1.RunnerImage{
			Declared: "docker.io/evil/x:1", Manifest: ".patchy/agent.yaml",
			Rejected: "NotAllowlisted", Message: "not allowlisted",
		}), fresh, false, ""},
		{"kill switch off", Guard{}, repoWith(accepted), fresh, false, SkipDisabled},
		{"breaker tripped", Guard{Enabled: true, Breaker: trippedBreaker()}, repoWith(accepted), fresh, false, SkipBreaker},
		{"approved out of HandedOff", Guard{Enabled: true}, repoWith(accepted), findingThrough(
			v1alpha1.PhaseInvestigating, v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating,
		), false, SkipRevived},
		{"retried out of Failed", Guard{Enabled: true}, repoWith(accepted), findingThrough(
			v1alpha1.PhaseInvestigating, v1alpha1.PhaseFailed, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating,
		), false, SkipRevived},
		{"approval hold released is not a revival", Guard{Enabled: true}, repoWith(accepted), findingThrough(
			v1alpha1.PhaseInvestigating, v1alpha1.PhaseAwaitingApproval, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating,
		), true, ""},
		{"automatic retry is not a revival", Guard{Enabled: true}, repoWith(accepted), findingThrough(
			v1alpha1.PhaseInvestigating, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating,
		), true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := jobs.Spec{Repo: "acme/orders"}
			skip := tt.guard.Pin(&spec, tt.repo, tt.finding)
			if skip != tt.wantSkip {
				t.Errorf("Pin skip = %q, want %q", skip, tt.wantSkip)
			}
			want := jobs.Spec{Repo: "acme/orders"}
			if tt.wantPin {
				want.RunnerImage = pinned
				want.RunnerSearchPath = accepted.SearchPath
				want.RunnerImageManifest = accepted.Manifest
			}
			if spec != want {
				t.Errorf("spec = %+v, want %+v", spec, want)
			}
		})
	}
}

func repoStatus(created time.Time, waiting, message string) jobs.Status {
	return jobs.Status{
		Active: 1, Created: created, Waiting: waiting, WaitingMessage: message,
		RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
	}
}

// started marks st's agent container as started.
func started(st jobs.Status) jobs.Status {
	st.AgentStarted = true
	return st
}

// TestPending: a repository-image pod gets the pull grace whatever it
// reports, then fails fast only on a pull failure that cannot heal and is
// looked at again until its agent container has started; a default-image
// Job is never judged at all.
func TestPending(t *testing.T) {
	early := clock.Add(-30 * time.Second)
	late := clock.Add(-5 * time.Minute)
	manifestUnknown := "failed to pull and unpack image \"" + pinned + "\": manifest unknown"
	backoffNotFound := "Back-off pulling image: ErrImagePull: ghcr.io/acme/go-env: not found"
	tests := []struct {
		name        string
		st          jobs.Status
		wantRequeue time.Duration
		wantFail    string
	}{
		{"default job stuck pulling is not judged", jobs.Status{
			Active: 1, Created: late, Waiting: "ImagePullBackOff", WaitingMessage: manifestUnknown,
			RunnerImageSource: v1alpha1.RunnerImageSourceDefault,
		}, 0, ""},
		{"finished job is not judged", jobs.Status{
			Done: true, Created: late, Waiting: "ErrImagePull", WaitingMessage: manifestUnknown,
			RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}, 0, ""},
		{"within grace, manifest unknown still waits", repoStatus(early, "ErrImagePull", manifestUnknown),
			PullGrace - 30*time.Second, ""},
		{"within grace, no pod status yet", repoStatus(early, "", ""), PullGrace - 30*time.Second, ""},
		{"after grace, manifest unknown fails", repoStatus(late, "ErrImagePull", manifestUnknown),
			0, "agent image pull failed: ErrImagePull: " + manifestUnknown},
		{"after grace, backoff carrying not found fails", repoStatus(late, "ImagePullBackOff", backoffNotFound),
			0, "agent image pull failed: ImagePullBackOff: " + backoffNotFound},
		{"after grace, invalid image name fails", repoStatus(late, "InvalidImageName", ""),
			0, "agent image pull failed: InvalidImageName"},
		{"after grace, container config error fails", repoStatus(late, "CreateContainerConfigError", "secret missing"),
			0, "agent image pull failed: CreateContainerConfigError: secret missing"},
		{"after grace, registry timeout keeps waiting", repoStatus(late, "ErrImagePull", "i/o timeout"), PullPoll, ""},
		{"after grace, bare backoff keeps waiting", repoStatus(late, "ImagePullBackOff", "Back-off pulling image"),
			PullPoll, ""},
		{"after grace, still initializing keeps waiting", repoStatus(late, "PodInitializing", ""), PullPoll, ""},
		{"after grace, running is left to the Job watch", started(repoStatus(late, "", "")), 0, ""},
		// No container status at all (unscheduled, waiting for a node, not
		// yet reported): a later pull failure never mutates the Job, so the
		// collector must keep looking.
		{"after grace, no container status yet keeps polling", repoStatus(late, "", ""), PullPoll, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requeue, fail := Pending(tt.st, clock)
			if requeue != tt.wantRequeue || fail != tt.wantFail {
				t.Errorf("Pending = (%v, %q), want (%v, %q)", requeue, fail, tt.wantRequeue, tt.wantFail)
			}
		})
	}
}

// TestSandboxRefused: only a repository-image Job's init exiting 78 is the
// probe's refusal.
func TestSandboxRefused(t *testing.T) {
	code := func(c int32) *int32 { return &c }
	tests := []struct {
		name string
		st   jobs.Status
		want bool
	}{
		{"repository job, init exited 78", jobs.Status{
			Done: true, InitExitCode: code(jobs.ExitSandboxUnenforced),
			RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}, true},
		{"repository job, init succeeded", jobs.Status{
			InitExitCode: code(0), RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}, false},
		{"repository job, init still running", jobs.Status{
			RunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		}, false},
		{"default job, init exited 78", jobs.Status{
			Done: true, InitExitCode: code(jobs.ExitSandboxUnenforced),
			RunnerImageSource: v1alpha1.RunnerImageSourceDefault,
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SandboxRefused(tt.st); got != tt.want {
				t.Errorf("SandboxRefused = %v, want %v", got, tt.want)
			}
		})
	}
	if !strings.HasPrefix(SandboxReason, SandboxUnenforced+": ") {
		t.Errorf("SandboxReason = %q, want it led by %s", SandboxReason, SandboxUnenforced)
	}
}

// testMetrics installs, once per test binary, a ManualReader-backed global
// MeterProvider; the global provider delegates to it even for instruments
// created before it.
var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// gauge reads the breaker gauge for component, or -1 when it has no point.
func gauge(t *testing.T, component string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "patchy.sandbox.breaker" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("patchy.sandbox.breaker is %T, want an int64 gauge", m.Data)
			}
			for _, dp := range g.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("component")); ok && v.AsString() == component {
					return dp.Value
				}
			}
		}
	}
	return -1
}

// TestBreaker: the breaker starts closed, stays open once tripped, reports
// its state on the gauge, and the nil breaker never trips.
func TestBreaker(t *testing.T) {
	testMetrics()
	b := NewBreaker("breaker-test", nil)
	if b.Tripped() {
		t.Fatal("new breaker is tripped")
	}
	if got := gauge(t, "breaker-test"); got != 0 {
		t.Errorf("gauge before trip = %d, want 0", got)
	}
	b.Trip(context.Background(), "job-1", "finding-1")
	b.Trip(context.Background(), "job-2", "finding-2")
	if !b.Tripped() {
		t.Fatal("breaker not tripped after Trip")
	}
	if got := gauge(t, "breaker-test"); got != 1 {
		t.Errorf("gauge after trip = %d, want 1", got)
	}

	var none *Breaker
	none.Trip(context.Background(), "job", "finding")
	if none.Tripped() {
		t.Error("nil breaker reports tripped")
	}
}
