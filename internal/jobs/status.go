// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Status reports a Job's state, and enough of its pod's that a collector
// can tell a repository-image Job stuck pulling or refused by the sandbox
// probe from a default one, without a second lookup of its own.
type Status struct {
	Active    int32
	Succeeded int32
	Failed    int32
	// Done means the Job reached a terminal condition (Complete or Failed).
	Done bool
	// Created is when the Job was created: the start of the grace a
	// collector gives a repository-image pod to pull its image.
	Created time.Time
	// Waiting is the agent container's waiting reason from the pod
	// (ImagePullBackOff, ErrImagePull, InvalidImageName,
	// CreateContainerConfigError, PodInitializing, ...), empty once it has
	// started or when no pod exists yet. A pod stuck pulling never mutates
	// the Job, so this is the only signal a Job watch cannot deliver.
	Waiting string
	// AgentStarted means the agent container is running or has terminated.
	// An empty Waiting cannot say so on its own: it is also empty when the
	// pod has no container status yet (unscheduled, waiting for a node, not
	// yet reported by the kubelet), and a pull that fails after that never
	// mutates the Job either.
	AgentStarted bool
	// WaitingMessage is the kubelet's message beside Waiting (the registry's
	// "manifest unknown", for one), empty when Waiting is.
	WaitingMessage string
	// InitExitCode is the prepare init container's exit code once it has
	// terminated, nil before; ExitSandboxUnenforced means the sandbox probe
	// refused to hand over to the agent.
	InitExitCode *int32
	// RunnerImageSource is the Job's runner-image-source annotation:
	// v1alpha1.RunnerImageSourceRepository when the Job ran a
	// repository-declared image, else RunnerImageSourceDefault (the
	// annotation is absent on a default Job, whose shape predates it).
	RunnerImageSource string
	// ResourceClass is the Job's resource-class annotation, empty on a Job
	// that ran on the controller's default resources; MemoryLimit is the
	// agent container's memory limit as the Job asked for it, empty when it
	// set none. A collector names both when the agent was OOM-killed.
	ResourceClass string
	MemoryLimit   string
	// Unschedulable is the scheduler's message while the pod's PodScheduled
	// condition is False with reason Unschedulable (no node fits it, and
	// none has been added that does yet), empty otherwise;
	// UnschedulableSince is when it became so. Like a pull failure, a pod
	// that cannot be placed never mutates the Job.
	Unschedulable      string
	UnschedulableSince time.Time
	// AgentTerminated is the agent container's terminated reason
	// (OOMKilled, Error, Completed, ...), empty while it has not
	// terminated, and AgentExitCode its exit code then.
	AgentTerminated string
	AgentExitCode   *int32
	// PodReason is the pod's own status reason (Evicted, DeadlineExceeded,
	// ...), with PodMessage beside it; JobFailure is the reason on the
	// Job's Failed condition (DeadlineExceeded, BackoffLimitExceeded, ...).
	// Each is empty when there is none.
	PodReason  string
	PodMessage string
	JobFailure string
}

// Termination says why the agent stopped without finishing on its own, for a
// collector to put in a run's failure detail when the agent reported nothing:
// its container OOM-killed (naming the memory limit and the resource class it
// came from), the pod evicted, the Job past its deadline, or the container
// exiting with an error. Empty when none of these is known: the Job is still
// running, or the agent finished on its own.
func (s Status) Termination() string {
	exit := ""
	if s.AgentExitCode != nil {
		exit = fmt.Sprintf(" (exit %d)", *s.AgentExitCode)
	}
	switch {
	case s.AgentTerminated == "OOMKilled":
		limit := "with no memory limit set, so the node itself ran out of memory"
		if s.MemoryLimit != "" {
			limit = "at its memory limit of " + s.MemoryLimit
			if s.ResourceClass != "" {
				limit += " (resource class " + s.ResourceClass + ")"
			}
		}
		return "the agent container was OOM-killed" + exit + " " + limit
	case s.PodReason == "Evicted":
		msg := "the agent pod was evicted"
		if s.PodMessage != "" {
			msg += ": " + s.PodMessage
		}
		return msg
	case s.JobFailure == "DeadlineExceeded":
		return "the agent Job ran past its deadline (activeDeadlineSeconds) and was stopped"
	case s.AgentTerminated != "" && s.AgentTerminated != "Completed":
		return "the agent container terminated: " + s.AgentTerminated + exit
	}
	return ""
}

// Status reports the named Job's state, including its newest pod's.
func (c *Client) Status(ctx context.Context, jobName string) (Status, error) {
	job, err := c.cs.BatchV1().Jobs(c.cfg.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return Status{}, fmt.Errorf("jobs: status of %s: %w", jobName, err)
	}
	s := statusOf(job)
	pod, err := c.findPod(ctx, jobName)
	if err != nil {
		return Status{}, err
	}
	if pod != nil {
		podStatus(&s, pod)
	}
	return s, nil
}

// podStatus reads what Status reports of the Job's newest pod.
func podStatus(s *Status, pod *corev1.Pod) {
	s.Waiting, s.WaitingMessage = agentWaiting(pod)
	s.AgentStarted = agentStarted(pod)
	s.InitExitCode = prepareExitCode(pod)
	s.Unschedulable, s.UnschedulableSince = unschedulable(pod)
	if agent := agentStatus(pod); agent != nil && agent.State.Terminated != nil {
		s.AgentTerminated = agent.State.Terminated.Reason
		s.AgentExitCode = new(agent.State.Terminated.ExitCode)
	}
	s.PodReason, s.PodMessage = pod.Status.Reason, pod.Status.Message
}

// unschedulable returns the scheduler's message, and since when, while the
// pod cannot be placed on any node; empty once it is scheduled or before
// the scheduler has looked at it. The transition time is when it first
// became unschedulable (the scheduler rewrites the message on every retry,
// never the time); a pod that reports none is timed from its creation.
func unschedulable(pod *corev1.Pod) (string, time.Time) {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse ||
			c.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		since := c.LastTransitionTime.Time
		if since.IsZero() {
			since = pod.CreationTimestamp.Time
		}
		msg := c.Message
		if msg == "" {
			msg = c.Reason
		}
		return msg, since
	}
	return "", time.Time{}
}

// Delete removes a Job and its pods (propagation: background).
func (c *Client) Delete(ctx context.Context, jobName string) error {
	policy := metav1.DeletePropagationBackground
	err := c.cs.BatchV1().Jobs(c.cfg.Namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		return fmt.Errorf("jobs: delete %s: %w", jobName, err)
	}
	return nil
}

func statusOf(job *batchv1.Job) Status {
	s := Status{
		Active:            job.Status.Active,
		Succeeded:         job.Status.Succeeded,
		Failed:            job.Status.Failed,
		Created:           job.CreationTimestamp.Time,
		RunnerImageSource: v1alpha1.RunnerImageSourceDefault,
	}
	if src := job.Annotations[annotationRunnerImageSource]; src == v1alpha1.RunnerImageSourceRepository {
		s.RunnerImageSource = src
	}
	s.ResourceClass = job.Annotations[AnnotationResourceClass]
	for _, ct := range job.Spec.Template.Spec.Containers {
		if mem, ok := ct.Resources.Limits[corev1.ResourceMemory]; ok && ct.Name == agentContainerName {
			s.MemoryLimit = mem.String()
		}
	}
	for _, cond := range job.Status.Conditions {
		terminal := cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed
		if terminal && cond.Status == corev1.ConditionTrue {
			s.Done = true
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			s.JobFailure = cond.Reason
		}
	}
	return s
}

// agentWaiting returns the agent container's waiting reason and message, or
// empty strings once it has started or before the kubelet reported it.
func agentWaiting(pod *corev1.Pod) (reason, message string) {
	agent := agentStatus(pod)
	if agent == nil || agent.State.Waiting == nil {
		return "", ""
	}
	return agent.State.Waiting.Reason, agent.State.Waiting.Message
}

// agentStarted reports whether the agent container is running or has
// terminated.
func agentStarted(pod *corev1.Pod) bool {
	agent := agentStatus(pod)
	return agent != nil && (agent.State.Running != nil || agent.State.Terminated != nil)
}

// prepareExitCode returns the prepare init container's exit code once it
// has terminated, nil otherwise.
func prepareExitCode(pod *corev1.Pod) *int32 {
	for i := range pod.Status.InitContainerStatuses {
		st := &pod.Status.InitContainerStatuses[i]
		if st.Name == initContainerName && st.State.Terminated != nil {
			return new(st.State.Terminated.ExitCode)
		}
	}
	return nil
}
