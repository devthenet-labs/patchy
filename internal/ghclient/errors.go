// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"errors"
	"net/http"

	"github.com/google/go-github/v90/github"
)

// IsNotFound reports whether err (anywhere in its chain) is a GitHub API
// 404 — the projected object no longer exists on the forge.
func IsNotFound(err error) bool {
	var ger *github.ErrorResponse
	return errors.As(err, &ger) && ger.Response != nil && ger.Response.StatusCode == http.StatusNotFound
}

// IsForbidden reports whether err (anywhere in its chain) is a GitHub API
// 403 refusing the credential — a missing permission. Rate limiting also
// answers 403, but go-github reports it as its own error types, so a
// throttled call is never mistaken for one.
func IsForbidden(err error) bool {
	var ger *github.ErrorResponse
	return errors.As(err, &ger) && ger.Response != nil && ger.Response.StatusCode == http.StatusForbidden
}
