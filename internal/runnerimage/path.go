// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import "strings"

// DefaultPath is what runc gives a container whose image config sets no
// PATH; the sanitizer substitutes it so the pod never inherits an empty
// value, which a shell reads as the untrusted working tree.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// SanitizePath derives the pod's search path from an image config's Env
// ("KEY=VALUE" entries; the last PATH wins, as in the runtime). Empty and
// relative components are dropped, so the image's own toolchain directories
// survive while nothing a working tree could shadow does, and a result with
// no absolute component is a *Rejection.
func SanitizePath(env []string) ([]string, error) {
	raw, present := DefaultPath, false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			raw, present = v, true
		}
	}
	var out []string
	for _, p := range strings.Split(raw, ":") {
		if strings.HasPrefix(p, "/") {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		if !present {
			// Unreachable while DefaultPath is all absolute; kept so a future
			// edit to the constant cannot silently emit an empty PATH.
			return nil, reject("default PATH `%s` has no absolute entries", raw)
		}
		return nil, reject("image PATH `%s` has no absolute entries; the pod would have no search path", raw)
	}
	return out, nil
}
