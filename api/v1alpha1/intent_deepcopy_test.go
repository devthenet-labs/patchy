// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestIntentKindsDeepCopy proves the generated deepcopy isolates every
// reference-typed field of the intent kinds — slices, pointers and nested
// times — so a controller mutating a cached object's copy cannot leak into
// the informer cache. Each case builds the object twice: one is copied and
// the copy mutated through every reference, the other is the pristine
// expectation the original must still equal.
func TestIntentKindsDeepCopy(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	later := now.Add(time.Hour)
	digest := "sha256:" + strings.Repeat("d", 64)

	cases := []struct {
		name   string
		build  func() runtime.Object
		mutate func(runtime.Object)
	}{
		{"project", func() runtime.Object {
			require, revisions, fixes := true, int32(1), int32(0)
			return &Project{Spec: ProjectSpec{
				IntentRepository:       "https://github.com/acme/intents",
				Approvers:              ProjectApprovers{Logins: []string{"octocat"}},
				Repositories:           []ProjectRepository{{Name: "shop", URL: "https://github.com/acme/shop"}},
				Limits:                 ProjectLimits{MaxRevisions: &revisions, MaxCheckFixes: &fixes},
				Checks:                 ProjectChecks{Fix: []string{"test"}, Timeout: &metav1.Duration{Duration: time.Minute}},
				RequireRepositoryImage: &require,
			}, Status: ProjectStatus{LastPolledAt: now.DeepCopy()}}
		}, func(o runtime.Object) {
			p := o.(*Project)
			p.Spec.Approvers.Logins[0] = "mallory"
			*p.Spec.Limits.MaxRevisions = 20
			*p.Spec.Limits.MaxCheckFixes = 20
			p.Spec.Repositories[0].URL = "https://github.com/evil/shop"
			p.Spec.Checks.Fix[0] = "lint"
			p.Spec.Checks.Timeout.Duration = time.Hour
			*p.Spec.RequireRepositoryImage = false
			p.Status.LastPolledAt.Time = later
		}},
		{"intent", func() runtime.Object {
			return &Intent{Status: IntentStatus{
				PhaseTimes: []IntentPhaseTime{{Phase: IntentPending, At: now}},
				Plan: &IntentPlan{
					Revision: 1, Digest: digest, ConfigMap: "p", PostedAt: now.DeepCopy(), Repositories: []string{"u"},
				},
				Approval: &IntentApproval{
					By: "octocat", Source: IntentActionLabel, EventID: 1, At: now, PlanRevision: 1,
					PlanDigest: digest, InputDigest: digest,
				},
				LastTrigger: &IntentAction{Source: IntentActionCommand, EventID: 2, Login: "octocat", At: now},
				PullRequests: []IntentPullRequest{{
					Repository: "https://github.com/acme/shop", Number: 7, MergedAt: now.DeepCopy(),
				}},
				Tracking:    &IntentTracking{StatusCommentID: 1},
				ActiveRun:   &ObjectReference{Name: "run"},
				CompletedAt: now.DeepCopy(),
			}}
		}, func(o runtime.Object) {
			s := &o.(*Intent).Status
			s.PhaseTimes[0].Phase = IntentClosed
			s.Plan.Repositories[0] = "v"
			s.Plan.PostedAt.Time = later
			s.Approval.By = "mallory"
			s.LastTrigger.EventID = 3
			s.LastTrigger.At.Time = later
			s.PullRequests[0].Number = 8
			s.PullRequests[0].MergedAt.Time = later
			s.Tracking.StatusCommentID = 2
			s.ActiveRun.Name = "other"
			s.CompletedAt.Time = later
		}},
		{"intent run", func() runtime.Object {
			return &IntentRun{Spec: IntentRunSpec{
				Inputs:          IntentRunInputs{ReviewIDs: []int64{1}, CheckRunIDs: []int64{2}, StatusIDs: []int64{3}},
				ImageFrom:       &ObjectReference{Name: "r0"},
				PreviousAttempt: &PreviousAttempt{Name: "a1", Attempt: 1, Outcome: "timeout"},
			}, Status: IntentRunStatus{
				JobRef:      &JobReference{Name: "job"},
				RunnerImage: &RunnerImageRef{Image: "img", Source: RunnerImageSourceRepository},
				Transcript:  &TranscriptRef{Name: "t"},
				StartedAt:   now.DeepCopy(),
			}}
		}, func(o runtime.Object) {
			r := o.(*IntentRun)
			r.Spec.Inputs.ReviewIDs[0] = 9
			r.Spec.Inputs.CheckRunIDs[0] = 9
			r.Spec.Inputs.StatusIDs[0] = 9
			r.Spec.ImageFrom.Name = "head"
			r.Spec.PreviousAttempt.Outcome = "ok"
			r.Status.JobRef.Name = "other"
			r.Status.RunnerImage.Source = RunnerImageSourceDefault
			r.Status.Transcript.Name = "u"
			r.Status.StartedAt.Time = later
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := c.build()
			dup := orig.DeepCopyObject()
			if !reflect.DeepEqual(dup, orig) {
				t.Fatalf("DeepCopyObject() = %+v, want %+v", dup, orig)
			}
			c.mutate(dup)
			if !reflect.DeepEqual(orig, c.build()) {
				t.Errorf("mutating the copy changed the original: %+v", orig)
			}
		})
	}
}
