// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command agent-runner is the coding-agent runtime that executes inside the
// ephemeral Job pod: it drives the classification and remediation harness
// stages against a pre-cloned repository and reports results as an event
// stream on stdout. It never talks to GitHub.
//
// Invoked as `agent-runner sandbox-probe` (sandboxprobe.Command) it instead
// runs the negative-connectivity check the trusted prepare init container
// performs before a repository-declared image gets to run anything, and
// exits sandboxprobe.ExitUnenforced when egress is still open at the end of
// the window.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/sandboxprobe"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args, os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the process: args and the environment in, diagnostics to stderr
// (stdout is reserved for the envelope event stream the controller parses),
// the exit status out.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))

	if len(args) > 1 && args[1] == sandboxprobe.Command {
		return sandboxprobe.Main(ctx, getenv, log)
	}

	cfg, err := agentrun.FromEnv(getenv)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "invalid configuration", slog.Any("error", err))
		return 2
	}
	cfg.Log = log

	if err := agentrun.New(cfg, &runner.Exec{}).Run(ctx); err != nil {
		// The failure was already emitted as a fatal envelope event; exit 2
		// so the Job is marked failed for the controller's orphan handling.
		log.LogAttrs(ctx, slog.LevelError, "agent run failed", slog.Any("error", err))
		return 2
	}
	return 0
}
