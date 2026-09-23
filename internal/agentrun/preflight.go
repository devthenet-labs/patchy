// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/runner"
)

// preflightTimeout bounds each preflight command; `claude --version` is the
// slowest of them and takes a second or two on a cold image.
const preflightTimeout = 30 * time.Second

// PreflightCommand is the agent-runner subcommand that runs the preflight
// alone, outside any stage: `agent-runner preflight [harness]`
// (PreflightMain). The workstation check (`patchy check image --run`) runs
// it in a sandboxed container of the image under test, so its verdict is
// exactly what a stage would conclude before spending a model call. It is
// defined here, beside the preflight, because cmd/ cannot be imported.
const PreflightCommand = "preflight"

// Exit statuses of `agent-runner preflight`. ExitPreflightFailed is the
// verdict that the image cannot run what a stage needs, the stage outcome
// image_incompatible; exitPreflightMisconfigured means no verdict was
// reached. An agent-runner that predates the subcommand treats it as a
// stage and exits 2 on the missing stage configuration, so a caller reads
// any status but 0 and ExitPreflightFailed as "the preflight did not run".
const (
	ExitPreflightFailed        = 3
	exitPreflightMisconfigured = 2
)

// preflight proves a repository-declared image can run what the stage
// needs before the stage spends a single model call on it (Preflight). On
// the default runner image (BinDir empty) nothing runs and the CLI is left
// to PATH resolution as before.
func (a *Agent) preflight(ctx context.Context, h harness.Harness) (cli string, err error) {
	if a.cfg.BinDir == "" {
		return "", nil
	}
	return Preflight(ctx, a.exec, h, a.cfg.BinDir, a.cfg.Workspace)
}

// Preflight runs, in dir, the harness CLI injected under binDir
// (`--version`, which exercises the image's dynamic loader and libc — the
// classic musl failure), `git --version` (the changeset flow) and
// `bash -c true` (the CLI's shell tool refuses busybox sh). It returns the
// absolute path of the injected CLI, which a stage then runs by that path
// rather than by name. A failure names the command and how it died so the
// finding's status reads like a diagnosis, not "empty CLI output".
//
// This is a compatibility check only: the image supplies the loader and
// libc the CLI links against, so a passing preflight says nothing about
// the CLI's integrity.
func Preflight(ctx context.Context, exec Executor, h harness.Harness, binDir, dir string) (string, error) {
	cli, ok := harness.AvailableIn(h, binDir)
	if !ok {
		return "", fmt.Errorf("preflight: no %s binary in %s; the prepare step injects %v there",
			h.ID(), binDir, h.CLI())
	}
	for _, argv := range [][]string{{cli, "--version"}, {"git", "--version"}, {"bash", "-c", "true"}} {
		if err := preflightCommand(ctx, exec, dir, argv); err != nil {
			return "", err
		}
	}
	return cli, nil
}

// preflightCommand runs one check through the executor and folds how it
// ended into an error, or nil when it exited cleanly.
func preflightCommand(ctx context.Context, exec Executor, dir string, argv []string) error {
	name := strings.Join(argv, " ")
	res, err := exec.Run(ctx, runner.CommandSpec{Argv: argv, Dir: dir}, preflightTimeout, nil)
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

// PreflightMain is `agent-runner preflight [harness]`: the injected
// binaries' directory from BinDirEnv (required: there is nothing to check
// without it), the working directory from PATCHY_WORKSPACE (default
// /workspace, as a stage has it) and the harness from the argument
// (default claude, the one harness that runs repository images). It prints
// its one-line verdict to out — the subcommand runs no stage, so stdout
// carries no event stream — and returns the process exit status: 0 when
// the image passes, ExitPreflightFailed when it does not, and
// exitPreflightMisconfigured, logged, when no verdict could be reached.
func PreflightMain(ctx context.Context, args []string, getenv func(string) string, exec Executor, out io.Writer,
	log *slog.Logger) int {
	binDir := getenv(BinDirEnv)
	if binDir == "" {
		log.LogAttrs(ctx, slog.LevelError, "invalid preflight configuration",
			slog.String("error", BinDirEnv+" is required"))
		return exitPreflightMisconfigured
	}
	if len(args) > 1 {
		log.LogAttrs(ctx, slog.LevelError, "invalid preflight configuration",
			slog.String("error", "usage: agent-runner "+PreflightCommand+" [harness]"))
		return exitPreflightMisconfigured
	}
	id := "claude"
	if len(args) > 0 {
		id = args[0]
	}
	h, ok := harness.ByID(id)
	if !ok {
		log.LogAttrs(ctx, slog.LevelError, "invalid preflight configuration",
			slog.String("error", fmt.Sprintf("unknown harness %q", id)))
		return exitPreflightMisconfigured
	}
	dir := getenv("PATCHY_WORKSPACE")
	if dir == "" {
		dir = "/workspace"
	}
	cli, err := Preflight(ctx, exec, h, binDir, dir)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return ExitPreflightFailed
	}
	_, _ = fmt.Fprintf(out, "preflight passed: `%s --version`, `git --version` and `bash -c true` ran\n", cli)
	return 0
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
