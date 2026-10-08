// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The six annotations that put a slot's Ingress behind the sign-in relay. A
// slot's pinned set holds exactly these keys, with the values the chart
// rendered once for it; the slot admission policy compares an Ingress against
// the same rendered values.
const (
	annotationAuthType            = "alb.ingress.kubernetes.io/auth-type"
	annotationAuthIDPOIDC         = "alb.ingress.kubernetes.io/auth-idp-oidc"
	annotationAuthOnUnauthRequest = "alb.ingress.kubernetes.io/auth-on-unauthenticated-request"
	annotationAuthScope           = "alb.ingress.kubernetes.io/auth-scope"
	annotationAuthSessionCookie   = "alb.ingress.kubernetes.io/auth-session-cookie"
	annotationAuthSessionTimeout  = "alb.ingress.kubernetes.io/auth-session-timeout"
	// maxSessionTimeout is the ALB's own limit on auth-session-timeout (7
	// days), in seconds.
	maxSessionTimeout = 604800
)

// authKeys are the pinned set's keys, sorted.
var authKeys = []string{
	annotationAuthIDPOIDC, annotationAuthOnUnauthRequest, annotationAuthScope,
	annotationAuthSessionCookie, annotationAuthSessionTimeout, annotationAuthType,
}

// secretNamePattern is a Secret name (a DNS subdomain), the auth-idp-oidc
// secretName the load balancer reads the slot's client id and secret from.
var secretNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// AuthAnnotations is the pinned annotation set of every slot, keyed by slot
// namespace (patchy-preview-<n>), as the chart renders it: the same JSON the
// slot admission policy's per-namespace expectation is rendered from, so an
// Ingress the controller writes carries exactly the bytes the policy
// compares (auth-idp-oidc is itself a JSON string, carried verbatim).
type AuthAnnotations map[string]map[string]string

// ParseAuthAnnotations decodes the chart's JSON. Empty input is no set.
func ParseAuthAnnotations(raw string) (AuthAnnotations, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var out AuthAnnotations
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("preview auth annotations: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("preview auth annotations: trailing data after the JSON object")
	}
	return out, nil
}

// AuthSettings put every preview Ingress behind the sign-in relay. Off (the
// zero value), the controller renders exactly what it rendered before the
// relay existed.
type AuthSettings struct {
	// Required renders each slot's pinned set onto its Ingresses, and has the
	// sweeper report (and, after a grace period, delete) a slot Ingress that
	// carries neither generation's set.
	Required bool
	// Annotations is the current key generation's pinned sets.
	Annotations AuthAnnotations
	// Previous is the previous key generation's, during a rotation's overlap:
	// an Ingress still on it conforms, and is patched to the current set by
	// its own reconcile, never deleted by the sweeper.
	Previous AuthAnnotations
}

// validate checks the auth settings against the slot count: off carries no
// sets, and on carries a valid set for exactly the configured slots, in each
// generation present.
func (a AuthSettings) validate(s Settings) error {
	if !a.Required {
		if len(a.Annotations) > 0 || len(a.Previous) > 0 {
			return fmt.Errorf("invalid preview-controller settings: preview auth annotations are set " +
				"but preview auth is not required")
		}
		return nil
	}
	if len(a.Annotations) == 0 {
		return fmt.Errorf("invalid preview-controller settings: preview auth is required but no " +
			"annotations are set")
	}
	if err := a.Annotations.validate(s); err != nil {
		return fmt.Errorf("invalid preview-controller settings: current preview auth annotations: %w", err)
	}
	if len(a.Previous) > 0 {
		if err := a.Previous.validate(s); err != nil {
			return fmt.Errorf("invalid preview-controller settings: previous preview auth annotations: %w", err)
		}
	}
	return nil
}

// validate checks one generation: exactly the configured slots' namespaces,
// each with exactly the six keys, every value the one the relay serves (OIDC
// to the relay, sign-in on an unauthenticated request, the openid scope, the
// slot's own cookie name, a session timeout the load balancer accepts), and
// one issuer for every slot.
func (a AuthAnnotations) validate(s Settings) error {
	want := make([]string, 0, s.SlotCount)
	for n := range s.SlotCount {
		want = append(want, s.slotName(int32(n)))
	}
	if got := slices.Sorted(maps.Keys(a)); !slices.Equal(got, want) {
		return fmt.Errorf("slot namespaces %q, want exactly %q", got, want)
	}
	issuer := ""
	for n, ns := range want {
		set := a[ns]
		if got := slices.Sorted(maps.Keys(set)); !slices.Equal(got, authKeys) {
			return fmt.Errorf("%s: keys %q, want exactly %q", ns, got, authKeys)
		}
		if set[annotationAuthType] != "oidc" {
			return fmt.Errorf("%s: auth-type %q, want oidc", ns, set[annotationAuthType])
		}
		if set[annotationAuthOnUnauthRequest] != "authenticate" {
			return fmt.Errorf("%s: auth-on-unauthenticated-request %q, want authenticate", ns,
				set[annotationAuthOnUnauthRequest])
		}
		if !slices.Contains(strings.Fields(set[annotationAuthScope]), "openid") {
			return fmt.Errorf("%s: auth-scope %q lacks openid", ns, set[annotationAuthScope])
		}
		if want := previewauth.ALBCookieName(n); set[annotationAuthSessionCookie] != want {
			return fmt.Errorf("%s: auth-session-cookie %q, want %q", ns, set[annotationAuthSessionCookie], want)
		}
		timeout := set[annotationAuthSessionTimeout]
		if t, err := strconv.Atoi(timeout); err != nil || strconv.Itoa(t) != timeout || t < 1 ||
			t > maxSessionTimeout {
			return fmt.Errorf("%s: auth-session-timeout %q is not a whole number of seconds in [1, %d]", ns,
				timeout, maxSessionTimeout)
		}
		got, err := validIDP(set[annotationAuthIDPOIDC])
		if err != nil {
			return fmt.Errorf("%s: auth-idp-oidc: %w", ns, err)
		}
		if issuer != "" && got != issuer {
			return fmt.Errorf("%s: issuer %q differs from another slot's %q", ns, got, issuer)
		}
		issuer = got
	}
	return nil
}

// idpOIDC is the auth-idp-oidc annotation's JSON.
type idpOIDC struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorizationEndpoint"`
	TokenEndpoint         string `json:"tokenEndpoint"`
	UserInfoEndpoint      string `json:"userInfoEndpoint"`
	SecretName            string `json:"secretName"`
}

// validIDP checks auth-idp-oidc names the relay: an https issuer with no
// path, query or trailing slash, its three endpoints under it, and a Secret
// name, with no other field. It returns the issuer.
func validIDP(raw string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var idp idpOIDC
	if err := dec.Decode(&idp); err != nil {
		return "", err
	}
	if dec.More() {
		return "", fmt.Errorf("trailing data after the JSON object")
	}
	u, err := url.Parse(idp.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.String() != idp.Issuer {
		return "", fmt.Errorf("issuer %q is not https://<host>", idp.Issuer)
	}
	if idp.AuthorizationEndpoint != idp.Issuer+"/authorize" || idp.TokenEndpoint != idp.Issuer+"/token" ||
		idp.UserInfoEndpoint != idp.Issuer+"/userinfo" {
		return "", fmt.Errorf("endpoints are not the issuer's /authorize, /token and /userinfo")
	}
	if len(idp.SecretName) > 253 || !secretNamePattern.MatchString(idp.SecretName) {
		return "", fmt.Errorf("secretName %q is not a Secret name", idp.SecretName)
	}
	return idp.Issuer, nil
}

// authFor is the pinned set an Ingress in slot renders, or nil with auth off.
func (s Settings) authFor(slot int32) map[string]string {
	if !s.Auth.Required {
		return nil
	}
	return s.Auth.Annotations[s.slotName(slot)]
}

// authConforms reports whether annotations carry the slot namespace's whole
// pinned set of the current or the previous generation, as the slot admission
// policy judges it: every key exactly, except auth-session-timeout, which may
// be any whole number of seconds up to the set's (a shorter ALB session is
// never weaker; an Ingress rendered before a raise still carries the shorter
// one). Other annotations are the admission policy's to judge.
func (s Settings) authConforms(namespace string, annotations map[string]string) bool {
	for _, gen := range []AuthAnnotations{s.Auth.Annotations, s.Auth.Previous} {
		set, ok := gen[namespace]
		if !ok || len(set) == 0 {
			continue
		}
		match := true
		for k, v := range set {
			got, ok := annotations[k]
			if !ok || (k == annotationAuthSessionTimeout && !timeoutWithin(got, v)) ||
				(k != annotationAuthSessionTimeout && got != v) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// timeoutWithin reports whether got is a canonical whole number of seconds
// from 1 to ceiling, the slot admission policy's rule for
// auth-session-timeout.
func timeoutWithin(got, ceiling string) bool {
	limit, err := strconv.Atoi(ceiling)
	if err != nil {
		return false
	}
	t, err := strconv.Atoi(got)
	return err == nil && strconv.Itoa(t) == got && t >= 1 && t <= limit
}
