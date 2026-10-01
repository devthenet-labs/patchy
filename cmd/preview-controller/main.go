// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command preview-controller renders approved PR-head runtime images into a
// fixed pool of chart-guarded slots. It has no GitHub, ECR or cloud client.
package main

import (
	"os"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("preview-controller", "Deploy immutable PR heads into isolated preview slots", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
