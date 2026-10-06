// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"bufio"
	"bytes"
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
// of base64), which a live viewer never wants, and an output chunk that long
// is skipped the same way. Result still reads those whole; a follow skips
// them without buffering them, so a handful of live viewers cannot hold tens
// of MiB each.
const maxTailLine = 256 << 10

// Sink receives what a live follow reads from an agent's log: each
// transcript turn, and each chunk of a running command's output. Either may
// be nil, and the lines it would have received are skipped. An error from
// either stops the follow.
type Sink struct {
	Turn   func(transcript.Turn) error
	Output func(transcript.Output) error
}

// scanFollow delivers each turn line and each output line of r to sink,
// skipping every other line and every line longer than maxTailLine without
// aborting the follow. A handler error stops the scan.
func scanFollow(r io.Reader, sink Sink) error {
	br := bufio.NewReaderSize(r, maxTailLine)
	skipping := false
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// The rest of this line is dropped too, whatever it holds.
			skipping = true
			continue
		}
		if !skipping {
			if ferr := sink.deliver(line); ferr != nil {
				return ferr
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

// deliver hands one log line to the handler for its kind. A line is the kind
// of the first prefix it carries, the line's own, whatever its text quotes
// after it, and is offered to that decoder alone. Testing one kind first, as
// Result does turns before events, would drop the output of a command that
// merely prints the turn prefix (a grep through patchy's own source): the
// line would be read as a turn, fail to decode and never reach the output
// decoder.
func (s Sink) deliver(line []byte) error {
	turnAt := bytes.Index(line, []byte(transcript.Prefix))
	outputAt := bytes.Index(line, []byte(transcript.OutputPrefix))
	switch {
	case turnAt >= 0 && (outputAt < 0 || turnAt < outputAt):
		if t, ok := transcript.Decode(line); ok && s.Turn != nil {
			return s.Turn(t)
		}
	case outputAt >= 0:
		if o, ok := transcript.DecodeOutput(line); ok && s.Output != nil {
			return s.Output(o)
		}
	}
	return nil
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
