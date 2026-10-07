// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command preview-auth serves the preview sign-in relay: the OpenID provider
// every preview host's ALB signs viewers in through, itself signing viewers
// in once per browser session at the installation's Dex. A viewer is let in
// when an access review allows get on projects/previews for the Preview's
// Project; what the ALB then forwards to the preview's own code is opaque,
// pairwise and short-lived. Not a controller: no reconcilers and no leases.
// Its Kubernetes access is Previews (get, list, watch), SubjectAccessReviews
// (create) and its one code-ledger Lease (get, update); its keys come from a
// mounted Secret, so it reads no Secret through the API.
package main

import (
	"os"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

func main() {
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("preview-auth",
		"Serve the preview sign-in relay preview hosts' load balancers sign viewers in through", opts)
	root.AddCommand(newServeCmd(opts))
	os.Exit(cli.Execute(root, opts.Log))
}
