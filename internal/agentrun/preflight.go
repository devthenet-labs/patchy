// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

// preflightTimeout bounds each preflight command; `claude --version` is the
// slowest of them and takes a second or two on a cold image.
const preflightTimeout = 30 * time.Second

// preflight proves a repository-declared image can run what the stage
// needs before the stage spends a single model call on it: the injected
// harness CLI (`--version`, which exercises the image's dynamic loader and
// libc — the classic musl failure), `git --version` (the changeset flow)
// and `bash -c true` (the CLI's shell tool refuses busybox sh). It returns
// the absolute path of the injected CLI, which the stage then runs by that
// path rather than by name. A failure names the command and how it died so
// the finding's status reads like a diagnosis, not "empty CLI output".
//
// This is a compatibility check only: the image supplies the loader and
// libc the CLI links against, so a passing preflight says nothing about
// the CLI's integrity. On the default runner image (BinDir empty) nothing
// runs and the CLI is left to PATH resolution as before.
func (a *Agent) preflight(ctx context.Context, h harness.Harness) (cli string, err error) {
	if a.cfg.BinDir == "" {
		return "", nil
	}
	cli, ok := harness.AvailableIn(h, a.cfg.BinDir)
	if !ok {
		return "", fmt.Errorf("preflight: no %s binary in %s; the prepare step injects %v there",
			h.ID(), a.cfg.BinDir, h.CLI())
	}
	for _, argv := range [][]string{{cli, "--version"}, {"git", "--version"}, {"bash", "-c", "true"}} {
		if err := a.preflightCommand(ctx, argv); err != nil {
			return "", err
		}
	}
	return cli, nil
}

// preflightCommand runs one check through the executor and folds how it
// ended into an error, or nil when it exited cleanly.
func (a *Agent) preflightCommand(ctx context.Context, argv []string) error {
	name := strings.Join(argv, " ")
	res, err := a.exec.Run(ctx, runner.CommandSpec{Argv: argv, Dir: a.cfg.Workspace}, preflightTimeout, nil)
	switch {
	case err != nil:
		// An unstartable command: not on PATH, not executable, or an ELF
		// whose interpreter the image lacks (which exec reports as ENOENT
		// even though the file exists).
		return fmt.Errorf("preflight: `%s` could not start: %v", name, err)
	case res.TimedOut:
		return fmt.Errorf("preflight: `%s` timed out after %s", name, preflightTimeout)
	case res.ExitCode != 0:
		return fmt.Errorf("preflight: `%s` failed%s", name, runEvidence(res, nil))
	}
	return nil
}

// pinCLI replaces the spec's command name with the injected CLI's absolute
// path when one was resolved, so neither the image's ENTRYPOINT nor its
// PATH order can redirect the stage to a different binary. With no
// injected CLI the spec is returned unchanged.
func pinCLI(spec runner.CommandSpec, cli string) runner.CommandSpec {
	if cli == "" || len(spec.Argv) == 0 {
		return spec
	}
	argv := make([]string, len(spec.Argv))
	copy(argv, spec.Argv)
	argv[0] = cli
	spec.Argv = argv
	return spec
}
