// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"testing"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// TestImageCannotShadowReservedEnv pins that the resolve-time ENV check
// source-controller wires (runnerimage.JobReservedEnv on top of CheckEnv's
// own rules) refuses every name this package reserves. A name added here
// without the resolver learning it fails the gate, instead of reaching a
// repository-declared image's ENV unchecked.
func TestImageCannotShadowReservedEnv(t *testing.T) {
	extra := runnerimage.JobReservedEnv()
	for name := range reservedEnv {
		if !runnerimage.ReservedEnvName(name, extra) {
			t.Errorf("%s is reserved by the Job but an image ENV may set it", name)
		}
	}
}
