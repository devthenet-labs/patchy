// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// reservedEnvPrefixes are the name prefixes an image may not set: patchy's
// own configuration surface and every variable the claude CLI reads.
// CLAUDE_CODE_ is listed for fidelity with the contract even though CLAUDE_
// already covers it.
var reservedEnvPrefixes = []string{"PATCHY_", "ANTHROPIC_", "CLAUDE_", "CLAUDE_CODE_"}

// proxyEnv are the proxy variables, refused in either case (and any mixed
// case, which curl and Go both honour for some of them), because a proxy set
// by the image would redirect the broker traffic.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY"}

// ReservedEnvName reports whether an image may not set name: it is one of
// extra (the caller passes the Job's own reserved names, which the image
// must not shadow), a provider gateway name, carries a reserved prefix, or is
// a proxy variable in any case.
func ReservedEnvName(name string, extra map[string]bool) bool {
	if extra[name] || slices.Contains(provider.GatewayEnvNames, name) {
		return true
	}
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return slices.Contains(proxyEnv, strings.ToUpper(name))
}

// CheckEnv rejects an image config Env ("KEY=VALUE" entries) that names any
// reserved variable. The message lists every offender, sorted, so the owner
// fixes the image once.
func CheckEnv(env []string, extra map[string]bool) error {
	var offenders []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if ReservedEnvName(name, extra) && !slices.Contains(offenders, name) {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	slices.Sort(offenders)
	return reject("image ENV sets `%s`; `PATCHY_*`, `ANTHROPIC_*`, `CLAUDE_*`, proxy and patchy-reserved "+
		"variables cannot come from the image", strings.Join(offenders, "`, `"))
}
