// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command agent-runner is the coding-agent runtime that executes inside the
// ephemeral Job pod: it drives the classification and remediation harness
// stages against a pre-cloned repository and reports results as an event
// stream on stdout. It never talks to GitHub.
//
// Invoked as `agent-runner sandbox-probe` it instead runs the
// negative-connectivity check the trusted prepare init container performs
// before a repository-declared image gets to run anything, and exits
// sandboxprobe.ExitUnenforced when egress is still open at the end of the
// window.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/runner"
	"github.com/bitwise-media-group/patchy/internal/sandboxprobe"
)

// probeCommand is the argument that selects the sandbox probe instead of a
// stage; internal/jobs writes it into the prepare script.
const probeCommand = "sandbox-probe"

func main() {
	os.Exit(run())
}

func run() int {
	// Diagnostics go to stderr; stdout is reserved for the envelope event
	// stream the controller parses.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == probeCommand {
		return probe(ctx, log)
	}

	cfg, err := agentrun.FromEnv(os.Getenv)
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

// probe runs the sandbox probe and prints its one verdict line to stderr.
// Exit 0 means every target was blocked; ExitUnenforced means one still
// answered when the window closed; anything else is the probe itself
// failing to run.
func probe(ctx context.Context, log *slog.Logger) int {
	p, err := sandboxprobe.FromEnv(os.Getenv)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "invalid sandbox probe configuration", slog.Any("error", err))
		return 2
	}
	res, err := p.Run(ctx)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "sandbox probe interrupted", slog.Any("error", err))
		return 1
	}
	if !res.Enforced {
		log.LogAttrs(ctx, slog.LevelError, res.Verdict())
		return sandboxprobe.ExitUnenforced
	}
	log.LogAttrs(ctx, slog.LevelInfo, res.Verdict())
	return 0
}
