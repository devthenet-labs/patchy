// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/bitwise-media-group/patchy/internal/intentperm"
)

// DefaultWebURL is github.com, the only host the flow supports.
const DefaultWebURL = "https://github.com"

// DefaultAPIURL is github.com's REST API.
const DefaultAPIURL = "https://api.github.com"

// DefaultHomepageURL is the App's homepage when none is given: the project
// the App runs, which is what an installer wants to read about.
const DefaultHomepageURL = "https://github.com/bitwise-media-group/patchy"

// MaxNameLength is the longest App name GitHub accepts.
const MaxNameLength = 34

// Features selects what the App is for, one switch per intentperm.Feature;
// the CLI names each flag after its feature. Each adds only the
// permissions and events its controllers use, as the table says: an App
// for intents alone holds no security_events grant and has no webhook.
type Features struct {
	// Security is the findings pipeline: code scanning alerts in through the
	// integration-controller's webhook, tracking issues, remediation pull
	// requests.
	Security bool
	// Intents is intent-driven development. intent-controller polls GitHub,
	// so intents add permissions but no webhook event.
	Intents bool
	// Checks adds the reads check-fix rounds make (a Project's
	// spec.checks.fix). It extends Intents.
	Checks bool
}

// Selected is f as the table's features, in the table's order.
func (f Features) Selected() []intentperm.Feature {
	var out []intentperm.Feature
	for _, s := range []struct {
		on      bool
		feature intentperm.Feature
	}{
		{f.Security, intentperm.FeatureSecurity},
		{f.Intents, intentperm.FeatureIntents},
		{f.Checks, intentperm.FeatureChecks},
	} {
		if s.on {
			out = append(out, s.feature)
		}
	}
	return out
}

// Validate reports a selection the flow cannot serve, in the CLI's terms:
// nothing patchy runs on, or a feature without the one it extends.
func (f Features) Validate() error {
	selected := f.Selected()
	for _, feature := range selected {
		for _, base := range feature.Requires() {
			if !slices.Contains(selected, base) {
				return fmt.Errorf("--%s extends --%s: it needs --%s", feature, base, base)
			}
		}
	}
	if !f.Security && !f.Intents {
		return fmt.Errorf("choose what the App is for: --%s, --%s, or both",
			intentperm.FeatureSecurity, intentperm.FeatureIntents)
	}
	return nil
}

// Needs is what the App must hold for f, and nothing more: the table's
// ForApp, metadata read included.
func (f Features) Needs() (intentperm.Needs, error) {
	if err := f.Validate(); err != nil {
		return intentperm.Needs{}, err
	}
	return intentperm.ForApp(f.Selected()...)
}

// describe is the App's description: what it is for.
func (f Features) describe() string {
	var uses []string
	if f.Security {
		uses = append(uses, "security findings")
	}
	if f.Intents {
		uses = append(uses, "intents")
	}
	if f.Checks {
		uses = append(uses, "check-fix rounds")
	}
	return "patchy: " + strings.Join(uses, ", ")
}

// Owner is the account the App is created under: an organization, or the
// signed-in user when Org is empty.
type Owner struct {
	Org string
}

// loginPattern is a GitHub login: alphanumerics and single hyphens, neither
// leading nor trailing.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// ValidateOrg reports an organization login GitHub could not have issued.
func ValidateOrg(login string) error {
	if len(login) > 39 || !loginPattern.MatchString(login) {
		return fmt.Errorf("%q is not a GitHub organization login", login)
	}
	return nil
}

// DefaultName is the App name for owner when none is given: patchy-<org>,
// cut to MaxNameLength, or patchy for a user (GitHub's form lets the person
// creating it change a name that is taken).
func DefaultName(owner Owner) string {
	if owner.Org == "" {
		return "patchy"
	}
	name := "patchy-" + strings.ToLower(owner.Org)
	if len(name) > MaxNameLength {
		name = strings.TrimRight(name[:MaxNameLength], "-")
	}
	return name
}

// Config is what a manifest is built from.
type Config struct {
	Features Features
	// Name is the App's name, at most MaxNameLength characters.
	Name string
	// HomepageURL is the App's homepage (the manifest's required url).
	HomepageURL string
	// WebhookURL is the integration-controller's /github/webhooks URL:
	// required with Security, refused without it.
	WebhookURL string
	// RedirectURL is where GitHub sends the browser back to with the code.
	// Empty leaves it out (a dry run, which posts nothing).
	RedirectURL string
}

// HookAttributes is the manifest's webhook.
type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// Manifest is a GitHub App manifest. encoding/json writes the fields in this
// order and the permission map sorted, so its JSON is deterministic.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description"`
	HookAttributes     *HookAttributes   `json:"hook_attributes,omitempty"`
	RedirectURL        string            `json:"redirect_url,omitempty"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events,omitempty"`
}

// Build makes the manifest for cfg: the App is private, requests exactly
// what cfg.Features needs (intentperm.ForApp), and has a webhook exactly
// when a selected feature consumes webhook events.
func Build(cfg Config) (Manifest, error) {
	needs, err := cfg.Features.Needs()
	if err != nil {
		return Manifest{}, err
	}
	if err := validateName(cfg.Name); err != nil {
		return Manifest{}, err
	}
	if err := validateURL("homepage URL", cfg.HomepageURL, false); err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Name:               cfg.Name,
		URL:                cfg.HomepageURL,
		Description:        cfg.Features.describe(),
		RedirectURL:        cfg.RedirectURL,
		DefaultPermissions: map[string]string{},
		DefaultEvents:      needs.Events,
	}
	for _, g := range needs.Grants {
		m.DefaultPermissions[g.Permission] = g.Access
	}
	switch {
	case len(needs.Events) > 0:
		if cfg.WebhookURL == "" {
			return Manifest{}, fmt.Errorf("the App receives webhook events (%s), so it needs --webhook-url: "+
				"the integration-controller's https://<host>/github/webhooks", strings.Join(needs.Events, ", "))
		}
		if err := validateURL("webhook URL", cfg.WebhookURL, true); err != nil {
			return Manifest{}, err
		}
		m.HookAttributes = &HookAttributes{URL: cfg.WebhookURL, Active: true}
	case cfg.WebhookURL != "":
		return Manifest{}, fmt.Errorf("--webhook-url is for an App that receives webhook events (--%s); "+
			"intents poll GitHub, so this App has no webhook", intentperm.FeatureSecurity)
	}
	return m, nil
}

// validateName reports a name GitHub would refuse, or one that could hide
// what it says.
func validateName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("the App name is empty")
	case len(name) > MaxNameLength:
		return fmt.Errorf("the App name %q is longer than GitHub's %d characters", name, MaxNameLength)
	case strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }):
		return fmt.Errorf("the App name %q holds a character that does not print", name)
	}
	return nil
}

// validateURL reports a URL the manifest cannot carry: not absolute, not
// http(s) (https only when httpsOnly), or holding credentials.
func validateURL(what, raw string, httpsOnly bool) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("the %s %q: %w", what, raw, err)
	case u.Host == "" || (u.Scheme != "https" && (httpsOnly || u.Scheme != "http")):
		if httpsOnly {
			return fmt.Errorf("the %s %q is not an https URL", what, raw)
		}
		return fmt.Errorf("the %s %q is not an http(s) URL", what, raw)
	case u.User != nil:
		return fmt.Errorf("the %s %q carries credentials", what, raw)
	}
	return nil
}

// CreateURL is GitHub's page that registers an App from a manifest posted to
// it, under owner, carrying state back on the redirect (none when state is
// empty: a URL only to show).
func CreateURL(webURL string, owner Owner, state string) string {
	u := ownerSettings(webURL, owner) + "/apps/new"
	if state == "" {
		return u
	}
	return u + "?state=" + url.QueryEscape(state)
}

// AppsURL is owner's list of GitHub Apps: where a person deletes an App or
// generates a new private key.
func AppsURL(webURL string, owner Owner) string {
	return ownerSettings(webURL, owner) + "/apps"
}

// InstallURL is where an App is installed on an account's repositories.
func InstallURL(webURL, slug string) string {
	return strings.TrimSuffix(webURL, "/") + "/apps/" + url.PathEscape(slug) + "/installations/new"
}

// ownerSettings is owner's settings root on webURL.
func ownerSettings(webURL string, owner Owner) string {
	base := strings.TrimSuffix(webURL, "/")
	if owner.Org == "" {
		return base + "/settings"
	}
	return base + "/organizations/" + url.PathEscape(owner.Org) + "/settings"
}
