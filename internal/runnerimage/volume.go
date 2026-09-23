// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"slices"
	"strings"
)

// CheckVolumes rejects an image that declares VOLUME instructions: containerd
// honours image volumes as writable host-backed mounts outside
// ephemeral-storage accounting, so the only writable paths must stay
// patchy's emptyDirs. The paths are listed sorted.
func CheckVolumes(volumes []string) error {
	if len(volumes) == 0 {
		return nil
	}
	paths := slices.Clone(volumes)
	slices.Sort(paths)
	return reject("image declares VOLUME `%s`; only patchy's emptyDirs are writable", strings.Join(paths, "`, `"))
}
