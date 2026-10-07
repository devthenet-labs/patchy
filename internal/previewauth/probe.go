// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"net/http"
	"net/url"
)

// ErrUnprotected means a preview host answered an unauthenticated request
// with something other than the ALB's redirect to this relay for its own
// slot's client: the host serves its app, or signs viewers in somewhere
// else, without the relay.
var ErrUnprotected = errors.New("previewauth: preview host is not protected by the relay")

// JudgeProbe judges a preview host's answer to an unauthenticated request
// (no cookies, redirects not followed). The host is protected only when it
// answered 302 to exactly <issuer>/authorize with response_type=code, slot's
// client id and the host's own callback as redirect_uri. Every other answer,
// a 200 from the app above all, is ErrUnprotected.
func JudgeProbe(status int, location, issuer string, slot int, label string, cb Callbacks) error {
	if status != http.StatusFound {
		return ErrUnprotected
	}
	want, err := cb.URL(label)
	if err != nil {
		return ErrUnprotected
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme+"://"+u.Host != issuer || u.Path != "/authorize" || u.User != nil || u.Fragment != "" {
		return ErrUnprotected
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["client_id"]) != 1 || len(q["redirect_uri"]) != 1 || len(q["response_type"]) != 1 {
		return ErrUnprotected
	}
	if q.Get("client_id") != ClientID(slot) || q.Get("redirect_uri") != want || q.Get("response_type") != "code" {
		return ErrUnprotected
	}
	return nil
}
