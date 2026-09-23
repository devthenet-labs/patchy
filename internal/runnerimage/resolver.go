// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"context"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
)

// Resolved is what a Resolver pins and checks for one declared reference.
type Resolved struct {
	// Image is "<repository>@sha256:<digest>": the reference every check ran
	// against and the pod pulls. For an index it is the index digest.
	Image string
	// SearchPath is the sanitized PATH the pod is given (SanitizePath).
	SearchPath []string
	// Verified is true when a signature was verified against Image's digest.
	Verified bool
}

// Resolver turns a declared reference, already parsed and allowed by Policy,
// into the digest-pinned, checked image the pod will run: one HEAD for a tag
// (none for a digest pin), then every size, platform, ENV, PATH, VOLUME and
// signature check on the pinned reference. The registry-backed
// implementation lives beside the registry client; reconciler tests fake
// this seam.
//
// A *Rejection is deterministic and the caller records it; any other error
// is transient (registry unreachable) and retried with backoff.
type Resolver interface {
	Resolve(ctx context.Context, ref imageref.Ref) (Resolved, error)
}
