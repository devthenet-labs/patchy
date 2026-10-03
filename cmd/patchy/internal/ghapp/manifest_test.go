// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/intentperm"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testWebhook = "https://patchy.acme.test/github/webhooks"

// TestBuildGolden pins the manifest of each feature selection byte for byte:
// what an operator reviews in --dry-run is what GitHub receives.
func TestBuildGolden(t *testing.T) {
	org := Owner{Org: "acme"}
	tests := []struct {
		name string
		cfg  Config
	}{
		{"all", Config{Features: Features{Security: true, Intents: true, Checks: true},
			Name: DefaultName(org), HomepageURL: DefaultHomepageURL, WebhookURL: testWebhook,
			RedirectURL: "http://127.0.0.1:49152/callback"}},
		{"intents", Config{Features: Features{Intents: true},
			Name: DefaultName(org), HomepageURL: DefaultHomepageURL}},
		{"intents-checks", Config{Features: Features{Intents: true, Checks: true},
			Name: DefaultName(org), HomepageURL: DefaultHomepageURL,
			RedirectURL: "https://github.com/organizations/acme/settings/apps"}},
		{"security", Config{Features: Features{Security: true},
			Name: DefaultName(org), HomepageURL: DefaultHomepageURL, WebhookURL: testWebhook}},
		{"user", Config{Features: Features{Intents: true},
			Name: DefaultName(Owner{}), HomepageURL: "https://example.test/patchy",
			RedirectURL: "https://github.com/settings/apps"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Build(tt.cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			got, err := json.MarshalIndent(m, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "manifest-"+tt.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("manifest differs from %s:\n%s", path, got)
			}
		})
	}
}

// TestBuildRefuses: every invocation the manifest cannot honestly carry is
// refused before anything reaches GitHub.
func TestBuildRefuses(t *testing.T) {
	ok := Config{Features: Features{Intents: true}, Name: "patchy-acme", HomepageURL: DefaultHomepageURL}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no feature", func(c *Config) { c.Features = Features{} }, "choose what the App is for"},
		{"checks without intents", func(c *Config) { c.Features = Features{Security: true, Checks: true} },
			"it needs --intents"},
		{"security without a webhook", func(c *Config) { c.Features.Security = true }, "needs --webhook-url"},
		{"a webhook without security", func(c *Config) { c.WebhookURL = testWebhook }, "receives webhook events"},
		{"http webhook", func(c *Config) {
			c.Features.Security, c.WebhookURL = true, "http://patchy.acme.test/github/webhooks"
		}, "not an https URL"},
		{"webhook with credentials", func(c *Config) {
			c.Features.Security, c.WebhookURL = true, "https://u:p@patchy.acme.test/github/webhooks"
		}, "carries credentials"},
		{"relative webhook", func(c *Config) { c.Features.Security, c.WebhookURL = true, "/github/webhooks" },
			"not an https URL"},
		{"empty name", func(c *Config) { c.Name = " " }, "name is empty"},
		{"long name", func(c *Config) { c.Name = strings.Repeat("a", MaxNameLength+1) }, "longer than"},
		{"name with a control character", func(c *Config) { c.Name = "patchy\u202eacme" }, "does not print"},
		{"homepage not a URL", func(c *Config) { c.HomepageURL = "patchy" }, "not an http(s) URL"},
		{"homepage scheme", func(c *Config) { c.HomepageURL = "javascript:alert(1)" }, "not an http(s) URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.mutate(&cfg)
			if _, err := Build(cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Build() error = %v, want one containing %q", err, tt.want)
			}
		})
	}
	if _, err := Build(ok); err != nil {
		t.Errorf("Build(valid) = %v", err)
	}
}

// TestManifestIsTheTable: for every selection of features, the manifest
// requests exactly the permissions and events the shared table
// (intentperm.ForApp) gives it, which intent-controller's own checks come
// from, and has a webhook exactly when it subscribes to an event; a
// selection the table refuses is never built.
func TestManifestIsTheTable(t *testing.T) {
	for _, f := range allFeatureSelections() {
		cfg := Config{Features: f, Name: "patchy-acme", HomepageURL: DefaultHomepageURL}
		if f.Security {
			cfg.WebhookURL = testWebhook
		}
		m, err := Build(cfg)
		want, tableErr := intentperm.ForApp(f.Selected()...)
		if f.Validate() != nil {
			if err == nil {
				t.Errorf("%+v: Build accepted a selection Validate refuses", f)
			}
			continue
		}
		if err != nil || tableErr != nil {
			t.Fatalf("%+v: Build: %v; ForApp: %v", f, err, tableErr)
		}
		perms := map[string]string{}
		for _, g := range want.Grants {
			perms[g.Permission] = g.Access
		}
		if !maps.Equal(m.DefaultPermissions, perms) || !slices.Equal(m.DefaultEvents, want.Events) {
			t.Errorf("%+v: manifest permissions %v, events %v; the table says %v, %v", f, m.DefaultPermissions,
				m.DefaultEvents, perms, want.Events)
		}
		if (m.HookAttributes != nil) != (len(want.Events) > 0) {
			t.Errorf("%+v: webhook %+v with events %v", f, m.HookAttributes, want.Events)
		}
	}
}

// allFeatureSelections is every combination of the three switches.
func allFeatureSelections() []Features {
	out := make([]Features, 0, 8)
	for i := range 8 {
		out = append(out, Features{Security: i&1 != 0, Intents: i&2 != 0, Checks: i&4 != 0})
	}
	return out
}

// TestFeaturesAreTheTable: the switches cover exactly the table's
// features, in its order.
func TestFeaturesAreTheTable(t *testing.T) {
	if got := (Features{Security: true, Intents: true, Checks: true}).Selected(); !slices.Equal(got,
		intentperm.Features()) {
		t.Errorf("Selected() = %v, want the table's %v", got, intentperm.Features())
	}
}

func TestDefaultName(t *testing.T) {
	tests := []struct {
		owner Owner
		want  string
	}{
		{Owner{}, "patchy"},
		{Owner{Org: "Acme"}, "patchy-acme"},
		{Owner{Org: strings.Repeat("a", 26) + "-b"}, "patchy-" + strings.Repeat("a", 26)},
		{Owner{Org: strings.Repeat("x", 39)}, "patchy-" + strings.Repeat("x", 27)},
	}
	for _, tt := range tests {
		got := DefaultName(tt.owner)
		if got != tt.want || len(got) > MaxNameLength {
			t.Errorf("DefaultName(%+v) = %q, want %q", tt.owner, got, tt.want)
		}
	}
}

func TestValidateOrg(t *testing.T) {
	for _, good := range []string{"acme", "Acme-Labs", "a", "a1-b2-c3", strings.Repeat("a", 39)} {
		if err := ValidateOrg(good); err != nil {
			t.Errorf("ValidateOrg(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", "-acme", "acme-", "ac--me", "acme/x", "acme.labs", "../x", "a b",
		strings.Repeat("a", 40)} {
		if err := ValidateOrg(bad); err == nil {
			t.Errorf("ValidateOrg(%q) accepted it", bad)
		}
	}
}

func TestURLs(t *testing.T) {
	org, user := Owner{Org: "acme"}, Owner{}
	for _, tt := range []struct{ got, want string }{
		{CreateURL(DefaultWebURL, org, "s1"), "https://github.com/organizations/acme/settings/apps/new?state=s1"},
		{CreateURL(DefaultWebURL+"/", user, "s 2"), "https://github.com/settings/apps/new?state=s+2"},
		{CreateURL(DefaultWebURL, org, ""), "https://github.com/organizations/acme/settings/apps/new"},
		{AppsURL(DefaultWebURL, org), "https://github.com/organizations/acme/settings/apps"},
		{AppsURL(DefaultWebURL, user), "https://github.com/settings/apps"},
		{InstallURL(DefaultWebURL, "patchy-acme"), "https://github.com/apps/patchy-acme/installations/new"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %s, want %s", tt.got, tt.want)
		}
	}
}
