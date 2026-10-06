// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// maxTailLine bounds one log line a live follow holds in memory. A turn line
// is a few KiB at most (the recorder caps every turn's text); the lines past
// it are stage results, a build's carrying its whole changeset (about 7 MiB
// of base64), which a live viewer never wants. Result still reads those
// whole; a follow skips them without buffering them, so a handful of live
// viewers cannot hold tens of MiB each.
const maxTailLine = 256 << 10

// scanTurns delivers each turn line of r to fn, skipping every other line
// and every line longer than maxTailLine without aborting the follow. A
// handler error stops the scan.
func scanTurns(r io.Reader, fn func(transcript.Turn) error) error {
	br := bufio.NewReaderSize(r, maxTailLine)
	skipping := false
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// The rest of this line is dropped too, whatever it holds.
			skipping = true
			continue
		}
		if !skipping && transcript.HasPrefix(line) {
			if t, ok := transcript.Decode(line); ok {
				if ferr := fn(t); ferr != nil {
					return ferr
				}
			}
		}
		skipping = false
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// maxIdleTimeout bounds an idle limit read off a Job: anything longer is
// reported as unknown rather than believed.
const maxIdleTimeout = 48 * time.Hour

// Facts is what a status reader may know of an agent Job: its clock and
// limits, projected out of the Job and nothing else of it. Its environment,
// image, volumes and every other field stay where they are.
type Facts struct {
	// Created is the Job's creation time.
	Created time.Time
	// Started is when the Job controller started it; zero before.
	Started time.Time
	// DeadlineSeconds is the Job's activeDeadlineSeconds, counted from
	// Started; zero when it sets none.
	DeadlineSeconds int64
	// InvestigateIdle and RemediateIdle are the agent container's stage idle
	// limits (the watchdog that ends a run making no progress); a plan run
	// runs under the first, a build or revise run under the second. Zero
	// means disabled or not set on this Job.
	InvestigateIdle time.Duration
	RemediateIdle   time.Duration
	// Done means the Job reached a terminal condition.
	Done bool
}

// The agent container's idle-limit variables Facts reads.
const (
	envInvestigateIdle = "PATCHY_INVESTIGATE_IDLE_TIMEOUT"
	envRemediateIdle   = "PATCHY_REMEDIATE_IDLE_TIMEOUT"
)

// Facts reads the named Job's clock and limits.
func (t *Tailer) Facts(ctx context.Context, jobName string) (Facts, error) {
	job, err := t.cs.BatchV1().Jobs(t.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return Facts{}, fmt.Errorf("jobs: facts of %s: %w", jobName, err)
	}
	f := Facts{Created: job.CreationTimestamp.Time, Done: statusOf(job).Done}
	if s := job.Status.StartTime; s != nil {
		f.Started = s.Time
	}
	if d := job.Spec.ActiveDeadlineSeconds; d != nil && *d > 0 {
		f.DeadlineSeconds = *d
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name != agentContainerName {
			continue
		}
		for _, e := range c.Env {
			switch e.Name {
			case envInvestigateIdle:
				f.InvestigateIdle = idleLimit(e.Value)
			case envRemediateIdle:
				f.RemediateIdle = idleLimit(e.Value)
			}
		}
	}
	return f, nil
}

// idleLimit parses an idle limit, zero for anything unparseable, negative or
// past maxIdleTimeout.
func idleLimit(v string) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d > maxIdleTimeout {
		return 0
	}
	return d
}
