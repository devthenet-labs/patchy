// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"bytes"
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

// Run implements Commander.
func (ExecCommander) Run(ctx context.Context, name string, args ...string) (Result, error) {
	var stdout, stderr bytes.Buffer
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
