// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command intent-controller runs intent-driven development: it polls each
// Project's intent repository for issues carrying its trigger label, plans
// the work in a read-only agent Job, posts the plan for an approver, builds
// the approved plan in the application repository's own image, pushes it to
// a branch patchy creates once, opens the pull request, and closes the
// intent issue when it merges. Optional: deployments without Projects simply
// do not run it.
package main

import (
	"os"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("intent-controller",
		"Plan, approve and build intents from GitHub issues in sandboxed agent jobs", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
