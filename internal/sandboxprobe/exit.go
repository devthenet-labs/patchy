// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package sandboxprobe

import (
	"context"
	"log/slog"
)

// Command is the agent-runner subcommand that runs the probe instead of a
// stage. internal/jobs writes it into the prepare script and
// cmd/agent-runner dispatches on it; it is defined once, here, because the
// two sides of the pod boundary must agree and cmd/ cannot be imported.
const Command = "sandbox-probe"

// Exit statuses of a probe that reached no verdict. Neither is 0, which
// hands over to the agent container, nor ExitUnenforced, the verdict the
// collectors map to SandboxUnenforced: the prepare init fails as an
// ordinary error and the untrusted image never starts.
const (
	exitInterrupted   = 1
	exitMisconfigured = 2
)

// Main runs the probe the way `agent-runner sandbox-probe` does: its
// configuration from the environment, one line to log, and the process
// exit status as the result.
func Main(ctx context.Context, getenv func(string) string, log *slog.Logger) int {
	p, err := FromEnv(getenv)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "invalid sandbox probe configuration", slog.Any("error", err))
		return exitMisconfigured
	}
	return p.Exit(ctx, log)
}

// Exit runs the probe and maps its conclusion onto the process exit status,
// logging the verdict line: 0 when every target was blocked, ExitUnenforced
// when one still answered as the window closed, exitInterrupted when the
// context ended first (never a verdict).
func (p Prober) Exit(ctx context.Context, log *slog.Logger) int {
	res, err := p.Run(ctx)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "sandbox probe interrupted", slog.Any("error", err))
		return exitInterrupted
	}
	if !res.Enforced {
		log.LogAttrs(ctx, slog.LevelError, res.Verdict())
		return ExitUnenforced
	}
	log.LogAttrs(ctx, slog.LevelInfo, res.Verdict())
	return 0
}
