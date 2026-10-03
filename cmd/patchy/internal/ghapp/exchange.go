// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// maxResponseBytes bounds the conversion response: an App, a PEM key and a
// few strings.
const maxResponseBytes = 1 << 20

// slugPattern is an App slug patchy builds links from: GitHub's are
// lowercase words joined by hyphens, and this admits underscores and dots
// too rather than lose an App over its spelling.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// App is the App GitHub created from the manifest.
type App struct {
	ID int64
	// Slug names the App in GitHub's URLs; empty when GitHub answered with
	// one patchy will not build a link from.
	Slug        string
	Name        string
	HTMLURL     string
	Owner       string
	Permissions map[string]string
	Events      []string
	// Credentials are the App's private key and webhook secret. They leave
	// the process only through SecretManifest.
	Credentials Credentials
}

// Credentials hold an App's secrets. Formatting one with any verb prints
// nothing of them, so an App can be logged or put in an error safely, even
// where fmt cannot call Format: a value reached through an unexported field
// is printed field by field by reflection. The secrets are therefore held
// by a function, which reflection prints only as an address. A pointer
// would not do: under a verb it does not fit (%s, %q, %t, ...) fmt reports
// the value again from the top, where it follows a pointer to a struct.
type Credentials struct {
	open func() secrets
}

// secrets are the values Credentials guards.
type secrets struct {
	privateKey    []byte
	webhookSecret string
}

// newCredentials holds privateKey and webhookSecret ("" for an App without
// a webhook).
func newCredentials(privateKey []byte, webhookSecret string) Credentials {
	s := secrets{privateKey: privateKey, webhookSecret: webhookSecret}
	return Credentials{open: func() secrets { return s }}
}

// privateKey is the PEM private key; nil for the zero Credentials.
func (c Credentials) privateKey() []byte {
	if c.open == nil {
		return nil
	}
	return c.open().privateKey
}

// webhookSecret is the webhook secret; "" when GitHub issued none.
func (c Credentials) webhookSecret() string {
	if c.open == nil {
		return ""
	}
	return c.open().webhookSecret
}

// redacted is all a formatted Credentials ever shows.
const redacted = "{redacted}"

// Format redacts the credentials under every verb: fmt consults a
// Formatter before Stringer and GoStringer, and a verb neither covers (%d,
// %x, %c) would otherwise print the key's bytes.
func (Credentials) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// String redacts the credentials outside fmt too.
func (Credentials) String() string { return redacted }

// HasWebhookSecret reports whether GitHub issued a webhook secret: only an
// App with a webhook has one.
func (c Credentials) HasWebhookSecret() bool { return c.webhookSecret() != "" }

// conversion is GitHub's answer to a manifest code. The OAuth client_id and
// client_secret it also carries are left undecoded: patchy authenticates as
// the App, never through OAuth.
type conversion struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Owner   struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions   map[string]string `json:"permissions"`
	Events        []string          `json:"events"`
	PEM           string            `json:"pem"`
	WebhookSecret *string           `json:"webhook_secret"`
}

// ErrCodeRejected reports a code GitHub would not convert: unknown, used
// already, or older than its hour.
var ErrCodeRejected = errors.New("GitHub did not accept the code")

// Convert exchanges a manifest code for the App GitHub created and its
// credentials: POST {apiURL}/app-manifests/{code}/conversions, which needs
// no authentication. A code works once, within an hour of the App's
// creation.
func Convert(ctx context.Context, client *http.Client, apiURL, code string) (*App, error) {
	if !validCode(code) {
		return nil, fmt.Errorf("%w: it is not shaped like a manifest code", ErrCodeRejected)
	}
	endpoint := strings.TrimSuffix(apiURL, "/") + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "patchy-cli")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange the code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the conversion: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("the conversion response is larger than any App GitHub returns")
	}
	if resp.StatusCode != http.StatusCreated {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, fmt.Errorf("%w (HTTP %d %q): codes are single-use and expire an hour after the App "+
				"is created", ErrCodeRejected, resp.StatusCode, msg.Message)
		}
		return nil, fmt.Errorf("exchange the code: HTTP %d %q", resp.StatusCode, msg.Message)
	}
	var c conversion
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("decode the conversion: %w", err)
	}
	// The code is spent: from here on only what makes the Secret useless
	// (no App ID, no key) is fatal, so a key is never thrown away over a
	// detail.
	if c.ID <= 0 {
		return nil, errors.New("the conversion names no App ID")
	}
	if block, _ := pem.Decode([]byte(c.PEM)); block == nil || !strings.HasSuffix(block.Type, "PRIVATE KEY") {
		return nil, errors.New("the conversion carries no PEM private key")
	}
	slug := c.Slug
	if !slugPattern.MatchString(slug) {
		slug = ""
	}
	var webhookSecret string
	if c.WebhookSecret != nil {
		webhookSecret = *c.WebhookSecret
	}
	return &App{
		ID: c.ID, Slug: slug, Name: c.Name, HTMLURL: c.HTMLURL, Owner: c.Owner.Login,
		Permissions: c.Permissions, Events: c.Events,
		Credentials: newCredentials([]byte(c.PEM), webhookSecret),
	}, nil
}

// Drift lists how what GitHub created differs from manifest m: the person
// creating the App may change its permissions or events on GitHub's form.
// Empty when it matches.
func Drift(m Manifest, app *App) []string {
	var out []string
	for p, want := range m.DefaultPermissions {
		if got := app.Permissions[p]; got != want {
			out = append(out, fmt.Sprintf("permission %s is %q, the manifest asked for %q", p, got, want))
		}
	}
	for p, got := range app.Permissions {
		if _, ok := m.DefaultPermissions[p]; !ok {
			out = append(out, fmt.Sprintf("permission %s is %q, which the manifest did not ask for", p, got))
		}
	}
	want := map[string]bool{}
	for _, e := range m.DefaultEvents {
		want[e] = true
	}
	got := map[string]bool{}
	for _, e := range app.Events {
		got[e] = true
		if !want[e] {
			out = append(out, fmt.Sprintf("event %s is subscribed, which the manifest did not ask for", e))
		}
	}
	for _, e := range m.DefaultEvents {
		if !got[e] {
			out = append(out, fmt.Sprintf("event %s is not subscribed, which the manifest asked for", e))
		}
	}
	slices.Sort(out)
	return out
}
