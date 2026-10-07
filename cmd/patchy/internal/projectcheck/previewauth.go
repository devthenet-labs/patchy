// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The configuration keys of preview sign-in the check reads: the relay's own
// ConfigMap (the chart's preview-auth component) and preview-controller's
// switch.
const (
	keyAuthRequired    = "PATCHY_PREVIEW_AUTH_REQUIRED"
	keyAuthIssuer      = "PATCHY_PREVIEW_AUTH_ISSUER"
	keyAuthDexIssuer   = "PATCHY_PREVIEW_AUTH_DEX_ISSUER_URL"
	keyAuthDexClientID = "PATCHY_PREVIEW_AUTH_DEX_CLIENT_ID"
)

// placeholderLabel is the host label of the chart's placeholder Ingress
// (placeholder.<suffix>), which carries slot 0's sign-in set once sign-in is
// required. No Preview is ever named placeholder.
const placeholderLabel = "placeholder"

// httpTimeout bounds one request of the preview sign-in checks.
const httpTimeout = 10 * time.Second

// bodyLimit bounds what the checks read of any answer.
const bodyLimit = 64 << 10

// HTTPDoer sends one request and returns its answer without following
// redirects; *http.Client with CheckRedirect returning
// http.ErrUseLastResponse is one.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPDoer is the HTTPDoer the CLI uses: no cookies, no redirects
// followed, the system's roots, and a per-request timeout.
func NewHTTPDoer() HTTPDoer {
	return &http.Client{
		Timeout:       httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// previewAuth checks preview sign-in, when the chart turned it on: that the
// relay answers at its issuer, that Dex knows the relay's one redirect URI,
// and, once sign-in is required, that every Ready preview host of the
// Project (and the placeholder) answers an unauthenticated request with the
// ALB's redirect to the relay for its own slot's client. Off, it reports
// nothing.
func (r *run) previewAuth(ctx context.Context, s settings, suffix string) {
	required := s.preview.bool(keyAuthRequired)
	if !s.auth.found() && !required {
		return
	}
	issuer := s.auth.data[keyAuthIssuer]
	switch {
	case s.auth.err != nil:
		r.add(CheckPreviewAuth, "", checkreport.Skip, "cannot read the relay's settings: %v", s.auth.err)
		return
	case !s.auth.found():
		r.add(CheckPreviewAuth, "", checkreport.Fail, "preview-controller requires sign-in (%s), but there is no "+
			"preview-auth ConfigMap in namespace %s: is previewAuth.enabled set in the patchy chart?",
			keyAuthRequired, r.p.Namespace)
		return
	case !previewauth.ValidIssuer(issuer):
		r.add(CheckPreviewAuth, "", checkreport.Fail, "%s sets %s to %q, which is not an https origin", s.auth.from,
			keyAuthIssuer, issuer)
		return
	}
	r.relayDiscovery(ctx, issuer)
	r.dexRedirect(ctx, s, issuer)
	r.previewAuthHosts(ctx, issuer, suffix, required)
}

// relayDiscovery checks the relay answers its own discovery document with
// its issuer. The preview load balancer calls the relay's token endpoint
// from dynamic public addresses, so the relay must answer from anywhere.
func (r *run) relayDiscovery(ctx context.Context, issuer string) {
	status, body, _, err := r.httpGet(ctx, issuer+"/.well-known/openid-configuration")
	if err != nil {
		r.add(CheckPreviewAuth, "", checkreport.Fail, "the relay at %s does not answer: %v (it must be reachable "+
			"from the internet: the preview load balancer calls its token endpoint from dynamic addresses)",
			issuer, err)
		return
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	switch {
	case status != http.StatusOK:
		r.add(CheckPreviewAuth, "", checkreport.Fail, "%s/.well-known/openid-configuration answered %d, not the "+
			"relay's discovery document: does the edge route %s to the preview-auth Service?", issuer, status,
			strings.TrimPrefix(issuer, "https://"))
	case json.Unmarshal(body, &doc) != nil || doc.Issuer != issuer || doc.AuthorizationEndpoint != issuer+"/authorize":
		r.add(CheckPreviewAuth, "", checkreport.Fail, "%s/.well-known/openid-configuration is not the relay's "+
			"discovery document for issuer %s (it names issuer %q)", issuer, issuer, doc.Issuer)
	default:
		r.add(CheckPreviewAuth, "", checkreport.Pass, "the relay answers at %s with its discovery document", issuer)
	}
}

// dexRedirect checks that Dex has the relay's static client with the
// relay's one redirect URI, <issuer>/dex/callback, registered: the one
// per-install IdP setup. Dex's client list is not readable without its
// storage, so the check asks Dex's authorization endpoint the way a
// sign-in would, without a browser session: Dex answers an unknown client
// and an unregistered redirect URI with an error page, and a registered
// pair with its sign-in page or a redirect to its connector. It follows
// nothing, so nobody signs in; Dex may keep the unused request until it
// expires.
func (r *run) dexRedirect(ctx context.Context, s settings, issuer string) {
	dexIssuer, clientID := s.auth.data[keyAuthDexIssuer], s.auth.data[keyAuthDexClientID]
	redirect := issuer + "/dex/callback"
	if dexIssuer == "" || clientID == "" {
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "%s sets no %s or %s", s.auth.from, keyAuthDexIssuer,
			keyAuthDexClientID)
		return
	}
	status, body, _, err := r.httpGet(ctx, strings.TrimSuffix(dexIssuer, "/")+"/.well-known/openid-configuration")
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	switch {
	case err != nil:
		r.add(CheckPreviewAuthDex, "", checkreport.Skip, "cannot reach Dex at %s: %v", dexIssuer, err)
		return
	case status != http.StatusOK || json.Unmarshal(body, &doc) != nil || doc.AuthorizationEndpoint == "":
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "%s/.well-known/openid-configuration answered %d without "+
			"an authorization endpoint: is %s Dex's issuer?", dexIssuer, status, dexIssuer)
		return
	}
	authURL, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil || authURL.Scheme != "https" {
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "Dex's authorization endpoint %q is not an https URL",
			doc.AuthorizationEndpoint)
		return
	}
	authURL.RawQuery = url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid"}, "state": {"patchy-check-project"}}.Encode()
	status, body, location, err := r.httpGet(ctx, authURL.String())
	page := pageText(body)
	switch {
	case err != nil:
		r.add(CheckPreviewAuthDex, "", checkreport.Skip, "cannot reach Dex's authorization endpoint: %v", err)
	case status == http.StatusOK || (status >= 300 && status < 400 && location != ""):
		r.add(CheckPreviewAuthDex, "", checkreport.Pass, "Dex at %s accepts client %s with redirect URI %s "+
			"(it answered %d)", dexIssuer, clientID, redirect, status)
	case strings.Contains(page, "redirect_uri"):
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "Dex refuses redirect URI %s for client %s (%q): register "+
			"it on that static client, the only per-install IdP setup", redirect, clientID,
			excerpt(page, "redirect_uri"))
	case status == http.StatusNotFound || strings.Contains(page, "client_id"):
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "Dex has no client %s (%d %q): add the static client with "+
			"redirect URI %s", clientID, status, excerpt(page, "client_id"), redirect)
	case status >= 400 && status < 500:
		r.add(CheckPreviewAuthDex, "", checkreport.Fail, "Dex refused a sign-in for client %s with redirect URI %s: "+
			"%d %q", clientID, redirect, status, excerpt(page, ""))
	default:
		r.add(CheckPreviewAuthDex, "", checkreport.Skip, "Dex's authorization endpoint answered %d %q, which "+
			"says nothing about the client", status, excerpt(page, ""))
	}
}

// previewAuthHosts probes the placeholder host and every Ready Preview of
// the Project without credentials, as the relay's own probe does: each must
// answer with the ALB's redirect to the relay's /authorize for its own
// slot's client and callback. Before sign-in is required (the permit stage)
// nothing is expected to redirect yet.
func (r *run) previewAuthHosts(ctx context.Context, issuer, suffix string, required bool) {
	if !required {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "preview-controller does not require sign-in yet "+
			"(previewAuth.stage=permit), so no preview host redirects to the relay")
		return
	}
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "preview-controller's host suffix %q is unusable: %v",
			suffix, err)
		return
	}
	type host struct {
		label string
		slot  int
	}
	hosts := []host{{label: placeholderLabel, slot: 0}}
	var list v1alpha1.PreviewList
	if err := r.cfg.Reader.List(ctx, &list, client.InNamespace(r.p.Namespace)); err != nil {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "cannot list the Previews in namespace %s (%v), so only "+
			"the placeholder host is probed", r.p.Namespace, err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.Project == r.p.Name && p.Status.Phase == v1alpha1.PreviewReady && p.Status.Slot != nil &&
			p.DeletionTimestamp == nil {
			hosts = append(hosts, host{label: p.Spec.HostLabel, slot: int(*p.Status.Slot)})
		}
	}
	for _, h := range hosts {
		name := h.label + "." + suffix
		status, _, location, err := r.httpGet(ctx, "https://"+name+"/")
		switch {
		case err != nil && timedOut(err):
			r.add(CheckPreviewAuthHost, "", checkreport.Skip, "no answer from %s within %s: the preview load "+
				"balancer admits only the chart's preview.inboundCIDRs, so run this from an admitted address",
				name, httpTimeout)
		case err != nil:
			r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s: %v", name, err)
		case previewauth.JudgeProbe(status, location, issuer, h.slot, h.label, cb) == nil:
			r.add(CheckPreviewAuthHost, "", checkreport.Pass, "%s (slot %d) answers without credentials with the "+
				"redirect to %s/authorize for client %s", name, h.slot, issuer, previewauth.ClientID(h.slot))
		default:
			r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s (slot %d) answered %d (Location %q) without "+
				"credentials, not the redirect to %s/authorize for client %s: the load balancer serves it without "+
				"sign-in (its rules may be stale: compare them with aws elbv2 describe-rules)", name, h.slot, status,
				location, issuer, previewauth.ClientID(h.slot))
		}
	}
}

// httpGet GETs u with the configured doer, returning the status, at most
// bodyLimit bytes of the body and the Location header.
func (r *run) httpGet(ctx context.Context, u string) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, "", err
	}
	resp, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only probe; the body has been read
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, nil, "", fmt.Errorf("read the answer: %w", err)
	}
	return resp.StatusCode, body, resp.Header.Get("Location"), nil
}

// pageText is a page's text: tags dropped, whitespace collapsed.
func pageText(body []byte) string {
	var b strings.Builder
	in := false
	for _, c := range string(body) {
		switch {
		case c == '<':
			in = true
			b.WriteByte(' ')
		case c == '>':
			in = false
		case !in:
			b.WriteRune(c)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// excerptRunes bounds a quoted excerpt.
const excerptRunes = 160

// excerpt quotes text from the sentence holding key (from the start when
// key is empty or absent), at most excerptRunes runes of it.
func excerpt(text, key string) string {
	if i := strings.Index(text, key); key != "" && i >= 0 {
		start := strings.LastIndexAny(text[:i], ".!?") + 1
		text = strings.TrimSpace(text[start:])
	}
	if r := []rune(text); len(r) > excerptRunes {
		return string(r[:excerptRunes]) + "..."
	}
	return text
}
