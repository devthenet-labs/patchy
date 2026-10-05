// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// noResourcesConfig is testConfig with no CPU or memory: what every
// controller builds when agent.resources.default is empty, today's Jobs.
func noResourcesConfig() Config {
	cfg := testConfig()
	cfg.CPURequest, cfg.MemoryRequest, cfg.CPULimit, cfg.MemoryLimit = "", "", "", ""
	return cfg
}

// largeClass is the docs' example class: requests cpu 4 and memory 8Gi, a
// memory limit above the request, and no CPU limit.
func largeClass() *Resources {
	return &Resources{Class: "large", CPURequest: "4", MemoryRequest: "8Gi", MemoryLimit: "10Gi"}
}

// TestGoldenNoResourcesJob proves the shape of a Job built with no CPU or
// memory set anywhere: no resources on either container, and no
// resource-class annotation. Every existing golden is built from testConfig,
// which sets all four, so none of them can show this.
func TestGoldenNoResourcesJob(t *testing.T) {
	job := buildJobForTest(t, noResourcesConfig(), testSpec())
	for _, ct := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		if len(ct.Resources.Requests) != 0 || len(ct.Resources.Limits) != 0 {
			t.Errorf("%s resources = %+v, want none", ct.Name, ct.Resources)
		}
	}
	if _, ok := job.Annotations[AnnotationResourceClass]; ok {
		t.Errorf("a Job with no class carries %s", AnnotationResourceClass)
	}
	goldenJob(t, "job_no_resources", job)
}

// TestGoldenResourceClassJob pins an intent build on a class: the class's
// CPU and memory on both containers, no CPU limit, and the class recorded on
// the Job and its pod.
func TestGoldenResourceClassJob(t *testing.T) {
	spec := testSpec()
	spec.Phase, spec.Kind, spec.IssueMarkdown = "build", "intent", ""
	spec.Finding, spec.Owner = "target-1-b1-web-a1", "target-1-b1-web-a1"
	spec.InvestigationMarkdown = "# Plan\n"
	spec.MaxTurns, spec.TokenBudget = 150, 800000
	spec.Resources = largeClass()
	goldenJob(t, "job_resource_class", buildJobForTest(t, noResourcesConfig(), spec))
}

// TestResourcesOverride: a Spec's Resources replaces Config's CPU and memory
// as a whole (a quantity the class leaves unset is unset, never Config's),
// identically on both containers, while ephemeral storage always stays
// Config's; nil leaves Config's in place.
func TestResourcesOverride(t *testing.T) {
	cfg := testConfig() // 500m / 1Gi / 2 / 4Gi
	cfg.EphemeralStorage = "8Gi"
	tests := []struct {
		name       string
		over       *Resources
		wantReq    map[corev1.ResourceName]string
		wantLim    map[corev1.ResourceName]string
		wantAnnot  string
		annotFound bool
	}{
		{
			name:    "nil keeps Config's",
			wantReq: map[corev1.ResourceName]string{"cpu": "500m", "memory": "1Gi", "ephemeral-storage": "8Gi"},
			wantLim: map[corev1.ResourceName]string{"cpu": "2", "memory": "4Gi", "ephemeral-storage": "8Gi"},
		},
		{
			name:       "a class replaces all four, its unset CPU limit included",
			over:       largeClass(),
			wantReq:    map[corev1.ResourceName]string{"cpu": "4", "memory": "8Gi", "ephemeral-storage": "8Gi"},
			wantLim:    map[corev1.ResourceName]string{"memory": "10Gi", "ephemeral-storage": "8Gi"},
			wantAnnot:  "large",
			annotFound: true,
		},
		{
			name: "a class with a CPU limit",
			over: &Resources{Class: "capped", CPURequest: "1", MemoryRequest: "2Gi", CPULimit: "2",
				MemoryLimit: "3Gi"},
			wantReq:    map[corev1.ResourceName]string{"cpu": "1", "memory": "2Gi", "ephemeral-storage": "8Gi"},
			wantLim:    map[corev1.ResourceName]string{"cpu": "2", "memory": "3Gi", "ephemeral-storage": "8Gi"},
			wantAnnot:  "capped",
			annotFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			spec.Resources = tt.over
			job := buildJobForTest(t, cfg, spec)
			pod := job.Spec.Template
			for _, ct := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
				checkList(t, ct.Name+" requests", ct.Resources.Requests, tt.wantReq)
				checkList(t, ct.Name+" limits", ct.Resources.Limits, tt.wantLim)
			}
			for where, ann := range map[string]map[string]string{"job": job.Annotations, "pod": pod.Annotations} {
				got, ok := ann[AnnotationResourceClass]
				if ok != tt.annotFound || got != tt.wantAnnot {
					t.Errorf("%s %s = %q (present %v), want %q (present %v)", where, AnnotationResourceClass,
						got, ok, tt.wantAnnot, tt.annotFound)
				}
			}
		})
	}
}

// checkList compares a resource list with the quantities wanted, by value.
func checkList(t *testing.T, what string, got corev1.ResourceList, want map[corev1.ResourceName]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for name, w := range want {
		q, ok := got[name]
		if !ok || q.Cmp(resource.MustParse(w)) != 0 {
			t.Errorf("%s %s = %v, want %s", what, name, got[name], w)
		}
	}
}

// TestResourcesOverrideRefusesABadQuantity: the class codec refuses a bad
// quantity at startup, but buildJob still names one rather than build a Job
// the API server would refuse.
func TestResourcesOverrideRefusesABadQuantity(t *testing.T) {
	spec := testSpec()
	spec.Resources = &Resources{Class: "x", CPURequest: "four"}
	if _, err := New(fake.NewClientset(), noResourcesConfig(), nil).buildJob("j", spec); err == nil {
		t.Fatal("buildJob accepted cpu \"four\"")
	}
}

// TestDoNotDisruptOnEveryAgentPod: Karpenter may not evict an agent pod to
// consolidate its node, a Finding's, an intent's or an evaluation's alike.
// The annotation is the pod's, never the Job's: Karpenter reads it there.
func TestDoNotDisruptOnEveryAgentPod(t *testing.T) {
	check := func(t *testing.T, job *batchv1.Job) {
		t.Helper()
		if got := job.Spec.Template.Annotations["karpenter.sh/do-not-disrupt"]; got != "true" {
			t.Errorf("pod annotation karpenter.sh/do-not-disrupt = %q, want \"true\"", got)
		}
		if _, ok := job.Annotations["karpenter.sh/do-not-disrupt"]; ok {
			t.Error("the Job itself carries karpenter.sh/do-not-disrupt; only its pod should")
		}
	}
	t.Run("agent", func(t *testing.T) { check(t, buildJobForTest(t, noResourcesConfig(), testSpec())) })
	t.Run("evaluation", func(t *testing.T) {
		job, err := New(fake.NewClientset(), noResourcesConfig(), nil).buildEvalJob("e", testEvalSpec())
		if err != nil {
			t.Fatal(err)
		}
		check(t, job)
	})
}

// TestEvalJobWithoutResources: evaluation Jobs take the controller's default
// like every other agent Job, and an empty default leaves them with none.
func TestEvalJobWithoutResources(t *testing.T) {
	job, err := New(fake.NewClientset(), noResourcesConfig(), nil).buildEvalJob("e", testEvalSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		if len(ct.Resources.Requests) != 0 || len(ct.Resources.Limits) != 0 {
			t.Errorf("%s resources = %+v, want none", ct.Name, ct.Resources)
		}
	}
}

// TestStatusReadsResourcesAndScheduling: what a collector needs to say why
// a run's pod never ran or was killed: the class and memory limit off the
// Job, the scheduler's verdict off the pod, and the Job's failure reason.
func TestStatusReadsResourcesAndScheduling(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	since := created.Add(5 * time.Second)

	spec := testSpec()
	spec.Resources = largeClass()
	job := buildJobForTest(t, noResourcesConfig(), spec)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "DeadlineExceeded"}}
	pod := jobPodInState(job.Name, corev1.PodPending, corev1.ContainerState{})
	pod.CreationTimestamp = metav1.NewTime(created)
	pod.Status.ContainerStatuses = nil
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message:            "0/2 nodes are available: 2 Insufficient cpu.",
		LastTransitionTime: metav1.NewTime(since),
	}}
	cs := fake.NewClientset(job, pod)
	st, err := New(cs, noResourcesConfig(), nil).Status(ctx, job.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.ResourceClass != "large" || st.MemoryLimit != "10Gi" {
		t.Errorf("class, memory limit = %q, %q; want large, 10Gi", st.ResourceClass, st.MemoryLimit)
	}
	if want := "requests cpu 4, memory 8Gi; limits memory 10Gi"; st.Resources != want {
		t.Errorf("resources = %q, want %q", st.Resources, want)
	}
	if st.Scheduled {
		t.Error("an unschedulable pod reads as scheduled")
	}
	if st.Unschedulable != "0/2 nodes are available: 2 Insufficient cpu." || !st.UnschedulableSince.Equal(since) {
		t.Errorf("unschedulable = %q since %v", st.Unschedulable, st.UnschedulableSince)
	}
	if st.JobFailure != "DeadlineExceeded" || !st.Done {
		t.Errorf("job failure = %q, done %v", st.JobFailure, st.Done)
	}

	// No transition time: timed from the pod's creation. Scheduled: none.
	pod.Status.Conditions[0].LastTransitionTime = metav1.Time{}
	if _, since := unschedulable(pod); !since.Equal(created) {
		t.Errorf("unschedulable since %v, want the pod's creation %v", since, created)
	}
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	if msg, _ := unschedulable(pod); msg != "" {
		t.Errorf("a scheduled pod reads unschedulable %q", msg)
	}
	var scheduled Status
	podStatus(&scheduled, pod)
	if !scheduled.Scheduled || scheduled.Unschedulable != "" {
		t.Errorf("a scheduled pod reads scheduled %v, unschedulable %q", scheduled.Scheduled, scheduled.Unschedulable)
	}
	// A default Job: no class, no memory limit.
	def := statusOf(buildJobForTest(t, noResourcesConfig(), testSpec()))
	if def.ResourceClass != "" || def.MemoryLimit != "" || def.Resources != "no requests or limits" {
		t.Errorf("default Job class, memory limit, resources = %q, %q, %q; want none", def.ResourceClass,
			def.MemoryLimit, def.Resources)
	}
}

// TestTermination: each way an agent stops without finishing reads as one
// sentence a run's detail can carry; an OOM kill names the limit and class.
func TestTermination(t *testing.T) {
	code := func(n int32) *int32 { return &n }
	tests := []struct {
		name string
		st   Status
		want string
	}{
		{"running", Status{AgentStarted: true}, ""},
		{"completed", Status{AgentTerminated: "Completed", AgentExitCode: code(0)}, ""},
		{"oom on a class", Status{AgentTerminated: "OOMKilled", AgentExitCode: code(137), MemoryLimit: "10Gi",
			ResourceClass: "large"},
			"the agent container was OOM-killed (exit 137) at its memory limit of 10Gi (resource class large)"},
		{"oom on the default", Status{AgentTerminated: "OOMKilled", AgentExitCode: code(137), MemoryLimit: "4Gi"},
			"the agent container was OOM-killed (exit 137) at its memory limit of 4Gi"},
		{"oom with no limit", Status{AgentTerminated: "OOMKilled"},
			"the agent container was OOM-killed with no memory limit set, so the node itself ran out of memory"},
		{"evicted", Status{PodReason: "Evicted", PodMessage: "The node was low on resource: memory."},
			"the agent pod was evicted: The node was low on resource: memory."},
		{"deadline", Status{JobFailure: "DeadlineExceeded", Done: true},
			"the agent Job ran past its deadline (activeDeadlineSeconds) and was stopped"},
		{"error exit", Status{AgentTerminated: "Error", AgentExitCode: code(2)},
			"the agent container terminated: Error (exit 2)"},
	}
	for _, tt := range tests {
		if got := tt.st.Termination(); got != tt.want {
			t.Errorf("%s: Termination() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
