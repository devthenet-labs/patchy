// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

const (
	// resourceClassAnnotation names, on a Job and its pod, the resource
	// class the Job runs on; a Job on the default carries none.
	resourceClassAnnotation = "patchy.bitwisemedia.uk/resource-class"
	// doNotDisruptAnnotation keeps Karpenter from evicting an agent pod to
	// consolidate its node; every agent pod carries it.
	doNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
)

// TestIntentResourceClass runs one intent through the shipped
// intent-controller started as the chart renders agent.resources: a small
// default and one class, large, written with an unquoted cpu as toJson
// leaves it. The Project picks large for its one repository. The plan Job
// runs on the default, whatever the repository picks; the build Job runs on
// large, on both of its containers, and records the class; neither pod may
// be evicted to consolidate its node; the Project reports the class it
// picks as defined, and stays Ready.
func TestIntentResourceClass(t *testing.T) {
	cl, gh, _ := startSourceStack(t, appRepo)
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login}},
		Repositories: []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL,
			AgentResourceClass: "large"}},
	}, "--agent-cpu-request", "100m", "--agent-memory-request", "256Mi",
		"--intent-resource-classes", `{"large":{"limits":{"memory":"10Gi"},"requests":{"cpu":4,"memory":"8Gi"}}}`)

	eventually(t, "the project to report its class defined", func() bool {
		return meta.IsStatusConditionTrue(e.project(t).Status.Conditions, v1alpha1.ConditionResourceClassesResolved)
	})

	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	plan := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	checkResources(t, plan, "", corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: mustQuantity("100m"), corev1.ResourceMemory: mustQuantity("256Mi")}})

	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	e.waitPhase(t, name, v1alpha1.IntentInReview)
	build := e.kubelet.waitRun(t, "the build job to run", phaseIs("build"))
	checkResources(t, build, "large", corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity("4"), corev1.ResourceMemory: mustQuantity("8Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: mustQuantity("10Gi")},
	})
	if !meta.IsStatusConditionTrue(e.project(t).Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("the project is not Ready: %+v", e.project(t).Status.Conditions)
	}
}

// checkResources asserts that r's Job ran on want, CPU and memory alike on
// its prepare init and its agent container, with ephemeral storage left to
// the controller's own wall; that it records class (none for "") on the Job
// and its pod; and that its pod may not be disrupted.
func checkResources(t *testing.T, r agentRun, class string, want corev1.ResourceRequirements) {
	t.Helper()
	pod := r.Job.Spec.Template
	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	if len(containers) < 2 {
		t.Fatalf("job %s has %d containers, want the prepare init and the agent", r.Job.Name, len(containers))
	}
	for _, c := range containers {
		for _, side := range []struct {
			name      string
			got, want corev1.ResourceList
		}{{"requests", c.Resources.Requests, want.Requests}, {"limits", c.Resources.Limits, want.Limits}} {
			for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				got, gotOK := side.got[res]
				w, wantOK := side.want[res]
				if gotOK != wantOK || (gotOK && got.Cmp(w) != 0) {
					t.Errorf("job %s container %s %s.%s = %s (set %v), want %s (set %v)", r.Job.Name, c.Name,
						side.name, res, got.String(), gotOK, w.String(), wantOK)
				}
			}
		}
		if _, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]; !ok {
			t.Errorf("job %s container %s lost its ephemeral-storage wall", r.Job.Name, c.Name)
		}
	}
	if got := r.Job.Annotations[resourceClassAnnotation]; got != class {
		t.Errorf("job %s resource class = %q, want %q", r.Job.Name, got, class)
	}
	if got := pod.Annotations[resourceClassAnnotation]; got != class {
		t.Errorf("job %s pod resource class = %q, want %q", r.Job.Name, got, class)
	}
	if got := pod.Annotations[doNotDisruptAnnotation]; got != "true" {
		t.Errorf("job %s pod %s = %q, want \"true\"", r.Job.Name, doNotDisruptAnnotation, got)
	}
}

// mustQuantity is the quantity s spells.
func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }
