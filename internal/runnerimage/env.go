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

// JobReservedEnv returns the names the agent Job sets or reserves for itself
// that CheckEnv's own rules (the prefixes, the gateway names, the proxies)
// do not already cover: the other harnesses' credential channels, the
// GitHub tokens the no-GitHub-token invariant keeps out of the pod, and
// HOME. It is the extra set source-controller passes, so resolve-time
// rejection covers the Job builder's reservedEnv union
// provider.GatewayEnvNames as the design specifies; a jobs test pins that
// every name the Job reserves is refused with it.
func JobReservedEnv() map[string]bool {
	return map[string]bool{
		"HOME":                 true,
		"GH_TOKEN":             true,
		"GITHUB_TOKEN":         true,
		"COPILOT_GITHUB_TOKEN": true,
		"OPENAI_API_KEY":       true,
		"CODEX_API_KEY":        true,
		"CODEX_ACCESS_TOKEN":   true,
	}
}

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
