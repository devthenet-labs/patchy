// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"errors"
	"os/exec"
)

// ExecCommander is the Commander that runs real processes. It uses only
// os/exec, so it builds for every target the CLI ships on (windows
// included); docker itself is found on PATH at run time.
type ExecCommander struct{}

var _ Commander = ExecCommander{}

// LookPath implements Commander.
func (ExecCommander) LookPath(file string) (string, error) { return exec.LookPath(file) }

// outputTail is how much of each stream Run keeps: the end, where the line
// a check reads is, and no more. The commands run the image under test,
// which may write without end for as long as runTimeout allows.
const outputTail = 64 << 10

// tailBuffer is an io.Writer that keeps only the last outputTail bytes
// written to it.
type tailBuffer struct{ buf []byte }

// Write implements io.Writer; it never fails.
func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= outputTail {
		p = p[len(p)-outputTail:]
		t.buf = t.buf[:0]
	}
	if over := len(t.buf) + len(p) - outputTail; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the kept tail.
func (t *tailBuffer) String() string { return string(t.buf) }

// Run implements Commander. It keeps only the last outputTail bytes of
// each stream.
func (ExecCommander) Run(ctx context.Context, name string, args ...string) (Result, error) {
	var stdout, stderr tailBuffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return res, err
}
