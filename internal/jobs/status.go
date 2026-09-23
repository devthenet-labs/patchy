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
		s.Waiting, s.WaitingMessage = agentWaiting(pod)
		s.InitExitCode = prepareExitCode(pod)
	}
	return s, nil
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
	for _, cond := range job.Status.Conditions {
		terminal := cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed
		if terminal && cond.Status == corev1.ConditionTrue {
			s.Done = true
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
