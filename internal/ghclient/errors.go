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

// IsRefused reports whether err (anywhere in its chain) is GitHub refusing
// the request itself: a 4xx answer other than 401 (a credential that may be
// re-minted), 408 (a timeout) and 429 (throttling). Repeating such a request
// unchanged gets the same answer: a missing permission (403), a ruleset
// refusing a ref (422), a repository gone (404). Rate limiting and the
// secondary limit are go-github's own error types, never a refusal, and so
// is any failure that never reached GitHub.
func IsRefused(err error) bool {
	var ger *github.ErrorResponse
	if !errors.As(err, &ger) || ger.Response == nil {
		return false
	}
	switch code := ger.Response.StatusCode; code {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	default:
		return code >= 400 && code < 500
	}
}

// IsUnprocessable reports whether err (anywhere in its chain) is a GitHub
// API 422. Minting an installation token for a repository the installation
// does not cover answers so, as does creating something that already
// exists; the caller knows which it asked for.
func IsUnprocessable(err error) bool {
	_, ok := unprocessable(err)
	return ok
}
