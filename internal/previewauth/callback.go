// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CallbackPath is the ALB's fixed OIDC callback path on every preview host.
const CallbackPath = "/oauth2/idpresponse"

const (
	// maxLabel is the DNS limit on one label.
	maxLabel = 63
	// maxHost is the DNS limit on a host name in text form.
	maxHost = 253
	// maxCallback bounds a redirect URI before any parsing.
	maxCallback = len("https://") + maxHost + len(CallbackPath)
)

// ErrCallback is returned for every redirect URI the grammar refuses. It never
// says why, because the caller only ever renders "not a preview address".
var ErrCallback = errors.New("previewauth: redirect_uri is not a preview callback")

// Callbacks is the redirect-URI grammar for one host suffix.
type Callbacks struct {
	suffix string
}

// NewCallbacks validates hostSuffix (lowercase LDH labels, at least two, no
// leading or trailing dot) and returns its grammar.
func NewCallbacks(hostSuffix string) (Callbacks, error) {
	if !validSuffix(hostSuffix) {
		return Callbacks{}, fmt.Errorf("previewauth: host suffix %q is not a lowercase DNS name of two or more labels",
			hostSuffix)
	}
	return Callbacks{suffix: hostSuffix}, nil
}

// Suffix is the host suffix the grammar was built for.
func (c Callbacks) Suffix() string { return c.suffix }

// Parse returns the host label of raw when raw is exactly
// https://<label>.<suffix>/oauth2/idpresponse: no port, userinfo, query,
// fragment, percent-escape, trailing dot or upper case, and a label that is a
// single lowercase LDH DNS label that is not an IDN (no "--" at positions 3
// and 4, so no xn-- A-label). Anything else is ErrCallback.
func (c Callbacks) Parse(raw string) (string, error) {
	if c.suffix == "" || len(raw) > maxCallback {
		return "", ErrCallback
	}
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		return "", ErrCallback
	}
	label, ok := strings.CutSuffix(rest, "."+c.suffix+CallbackPath)
	if !ok || !ValidLabel(label) || len(label)+1+len(c.suffix) > maxHost {
		return "", ErrCallback
	}
	// The byte grammar above already fixes the whole string. url.Parse is a
	// second opinion: the URL a standard parser sees must be the same one.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Host != label+"."+c.suffix || u.Port() != "" || u.Path != CallbackPath ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || u.String() != raw {
		return "", ErrCallback
	}
	return label, nil
}

// URL is the one redirect URI Parse accepts for label.
func (c Callbacks) URL(label string) (string, error) {
	if c.suffix == "" || !ValidLabel(label) || len(label)+1+len(c.suffix) > maxHost {
		return "", ErrCallback
	}
	return "https://" + label + "." + c.suffix + CallbackPath, nil
}

// ValidLabel reports whether l is a lowercase LDH DNS label of 1 to 63
// characters that starts and ends with a letter or digit and is not
// reserved for IDNs ("--" in positions 3 and 4).
func ValidLabel(l string) bool {
	if l == "" || len(l) > maxLabel || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	if len(l) >= 4 && l[2] == '-' && l[3] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		if !ldh(l[i]) {
			return false
		}
	}
	return true
}

// validSuffix accepts two or more lowercase LDH labels. A suffix label may be
// an A-label (the operator's own domain may be an IDN); only the preview's
// own label is held to the stricter rule.
func validSuffix(s string) bool {
	if len(s) > maxHost-2 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > maxLabel || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if !ldh(l[i]) {
				return false
			}
		}
	}
	return true
}

func ldh(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' }
