// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scc

import (
	"fmt"
	"strings"
	"testing"
)

func TestHandlerID(t *testing.T) {
	if got := New(Options{}).ID(); got != ID {
		t.Errorf("ID() = %q, want %q", got, ID)
	}
}

// TestRenderDescriptionCarriesEveryBlock: a fully populated finding renders
// its CVE and CVSS, its MITRE tactic and techniques, the compliance
// standards that list ids (skipping those that do not), its properties and
// its operator marks.
func TestRenderDescriptionCarriesEveryBlock(t *testing.T) {
	v := cve("cve-2026-1234")
	v.CVE.Cvssv3 = &struct {
		BaseScore float64 `json:"baseScore"`
	}{BaseScore: 9.8}
	n := &notification{
		Finding: finding{
			Name: "organizations/1/sources/2/findings/f1", Category: "OPEN_FIREWALL", Severity: "HIGH",
			Description: "  desc  ", NextSteps: "fix it", Vulnerability: v,
			MitreAttack: &mitreAttack{PrimaryTactic: "INITIAL_ACCESS", PrimaryTechniques: []string{"T1190"}},
			Compliances: []compliance{
				{Standard: "cis", Version: "1.0", IDs: []string{"3.6"}},
				{Standard: "empty", Version: "1"},
			},
			SourceProperties: map[string]any{"Port": float64(22), "Open": true, "Note": "a|b\nc", "Nil": nil,
				"Nested": map[string]any{"k": "v"}},
			SecurityMarks: &securityMarks{Marks: map[string]string{"owner": "team-a"}},
		},
		Resource: resource{Name: "//compute.googleapis.com/projects/p/global/firewalls/fw"},
	}
	out, err := renderDescription(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CVE-2026-1234", "9.8", "INITIAL_ACCESS", "T1190", "cis", "3.6", "22", "true",
		`a\|b c`, "map[k:v]", "owner", "team-a", "//compute.googleapis.com/projects/p/global/firewalls/fw"} {
		if !strings.Contains(out, want) {
			t.Errorf("description lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "empty") {
		t.Errorf("a compliance standard with no ids rendered:\n%s", out)
	}
}

func TestRenderDescriptionUnspecifiedSeverity(t *testing.T) {
	for _, sev := range []string{"", "SEVERITY_UNSPECIFIED"} {
		out, err := renderDescription(&notification{Finding: finding{Name: "f", Category: "X", Severity: sev}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(out), "unspecified") {
			t.Errorf("severity %q rendered: %s", sev, out)
		}
	}
}

func TestFormatValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"  x  ", "x"},
		{float64(1.5), "1.5"},
		{float64(3), "3"},
		{false, "false"},
		{[]any{"a", "b"}, "[a b]"},
		{"line1\nline2|x", `line1 line2\|x`},
	}
	for _, c := range cases {
		if got := formatValue(c.in); got != c.want {
			t.Errorf("formatValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPropertiesCapAndSkipEmpty(t *testing.T) {
	if properties(nil) != nil {
		t.Error("no properties rendered something")
	}
	src := map[string]any{"blank": "  ", "nil": nil}
	for i := range maxProperties + 5 {
		src[fmt.Sprintf("p%03d", i)] = "v"
	}
	got := properties(src)
	if len(got) != maxProperties || got[0].Key != "p000" {
		t.Errorf("properties = %d starting %q", len(got), got[0].Key)
	}
	for _, kv := range got {
		if kv.Key == "blank" || kv.Key == "nil" {
			t.Errorf("empty property %q rendered", kv.Key)
		}
	}
	if marks(nil) != nil || marks(&securityMarks{}) != nil {
		t.Error("absent marks rendered something")
	}
}

func TestNamesFallBack(t *testing.T) {
	if got := ruleID(&finding{ModuleName: "mod", Category: "CAT"}); got != "mod" {
		t.Errorf("ruleID with a module = %q", got)
	}
	if got := ruleID(&finding{Category: "CAT"}); got != "CAT" {
		t.Errorf("ruleID without a module = %q", got)
	}
	if got := title(&finding{Name: "organizations/1/sources/2/findings/abc"}); got != "abc" {
		t.Errorf("title without a category = %q", got)
	}
	if got := lastSegment("no-slash"); got != "no-slash" {
		t.Errorf("lastSegment = %q", got)
	}
	n := &notification{Resource: resource{Name: "//r"}}
	if got := resourceName(n); got != "//r" {
		t.Errorf("resourceName fallback = %q", got)
	}
	if cloudResource(&notification{}) != nil {
		t.Error("a nameless resource became a cloud resource")
	}
	if cr := cloudResource(n); cr == nil || cr.Name != "//r" {
		t.Errorf("cloudResource fallback = %+v", cr)
	}
}

func TestFindingURL(t *testing.T) {
	h := New(Options{})
	if got := h.findingURL(&finding{ExternalURI: "https://x"}); got != "https://x" {
		t.Errorf("external URI = %q", got)
	}
	if got := h.findingURL(&finding{}); got != "" {
		t.Errorf("no organisation = %q, want none", got)
	}
	h = New(Options{Organization: "1234"})
	if got := h.findingURL(&finding{}); !strings.HasSuffix(got, "organizationId=1234") {
		t.Errorf("console link = %q", got)
	}
}
