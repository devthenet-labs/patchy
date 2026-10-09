// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package wiz

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestHandlerIDsAndForeignEvents(t *testing.T) {
	iss, def := NewIssues(Options{}), NewDefend(Options{})
	if iss.ID() != IssuesID || def.ID() != DefendID {
		t.Fatalf("IDs = %q / %q", iss.ID(), def.ID())
	}
	// Each handler ignores the other feed's events.
	if got, err := iss.Findings(context.Background(), EventThreat, []byte(threatJSON)); got != nil || err != nil {
		t.Errorf("issues handler on a threat = %v, %v", got, err)
	}
	if got, err := def.Findings(context.Background(), EventIssue, []byte(issueJSON)); got != nil || err != nil {
		t.Errorf("defend handler on an issue = %v, %v", got, err)
	}
}

func TestFindingsDecodeAndShapeErrors(t *testing.T) {
	iss, def := NewIssues(Options{}), NewDefend(Options{})
	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"issue not JSON", func() error {
			_, err := iss.Findings(context.Background(), EventIssue, []byte("{"))
			return err
		}, "decode issue delivery"},
		{"threat not JSON", func() error {
			_, err := def.Findings(context.Background(), EventThreat, []byte("{"))
			return err
		}, "decode threat delivery"},
		{"threat without id", func() error {
			_, err := def.Findings(context.Background(), EventThreat, mutate(t, threatJSON, "threat", "id", ""))
			return err
		}, "missing threat.id"},
		{"threat without a body", func() error {
			_, err := def.Findings(context.Background(), EventThreat, []byte(`{"trigger":{"type":"Created"}}`))
			return err
		}, "missing threat.id"},
	}
	for _, c := range cases {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestDefendSkipsClosedThreats: a resolved or below-floor threat is not
// actionable.
func TestDefendSkipsClosedThreats(t *testing.T) {
	def := NewDefend(Options{})
	got, err := def.Findings(context.Background(), EventThreat, mutate(t, threatJSON, "threat", "status", "RESOLVED"))
	if got != nil || err != nil {
		t.Errorf("resolved threat = %v, %v", got, err)
	}
	strict := NewDefend(Options{MinSeverity: "critical"})
	got, err = strict.Findings(context.Background(), EventThreat, mutate(t, threatJSON, "threat", "severity", "LOW"))
	if got != nil || err != nil {
		t.Errorf("below-floor threat = %v, %v", got, err)
	}
}

func TestIssueWithoutEntityIsSkipped(t *testing.T) {
	payload := []byte(`{"trigger":{"type":"Created"},"issue":{"id":"i-1","status":"OPEN","severity":"HIGH",` +
		`"control":{"id":"c-1","name":"Open bucket"}}}`)
	got, err := NewIssues(Options{}).Findings(context.Background(), EventIssue, payload)
	if got != nil || err != nil {
		t.Errorf("entity-less issue = %v, %v; want skipped", got, err)
	}
}

func TestTitlesFallBack(t *testing.T) {
	cases := []struct {
		env  envelope
		want string
	}{
		{envelope{Issue: &issue{ID: "i1", Control: control{Name: "Control"}}, Trigger: trigger{RuleName: "Rule"}}, "Control"},
		{envelope{Issue: &issue{ID: "i1"}, Trigger: trigger{RuleName: "Rule"}}, "Rule"},
		{envelope{Issue: &issue{ID: "i1"}}, "Wiz issue i1"},
	}
	for _, c := range cases {
		if got := issueTitle(&c.env); got != c.want {
			t.Errorf("issueTitle = %q, want %q", got, c.want)
		}
	}
	threats := []struct {
		th   threat
		want string
	}{
		{threat{ID: "t1", Name: "Name", RuleName: "Rule"}, "Name"},
		{threat{ID: "t1", RuleName: "Rule"}, "Rule"},
		{threat{ID: "t1"}, "Wiz threat t1"},
	}
	for _, c := range threats {
		if got := threatTitle(&c.th); got != c.want {
			t.Errorf("threatTitle = %q, want %q", got, c.want)
		}
	}
}

func TestAdvisoriesFallBack(t *testing.T) {
	if got := issueAdvisories(&issue{ID: "i1"}); len(got) != 1 || got[0] != "wiz-issue:i1" {
		t.Errorf("control-less issue advisories = %v", got)
	}
	cases := []struct {
		th   threat
		want string
	}{
		{threat{ID: "t1", RuleID: "r1", DetectionIDs: []string{"d1"}, MitreTechniques: []string{"t1059", ""}},
			"wiz-rule:r1,T1059"},
		{threat{ID: "t1", DetectionIDs: []string{"d1", "d2"}}, "wiz-detection:d1"},
		{threat{ID: "t1"}, "wiz-threat:t1"},
	}
	for _, c := range cases {
		if got := strings.Join(threatAdvisories(&c.th), ","); got != c.want {
			t.Errorf("threatAdvisories(%+v) = %q, want %q", c.th, got, c.want)
		}
	}
}

// TestEntityNamesFallBack: an unsupported platform's resource is named by
// its provider id, else by its Wiz id.
func TestEntityNamesFallBack(t *testing.T) {
	es := []struct {
		in   entitySnapshot
		want string
	}{
		{entitySnapshot{CloudPlatform: "GCP", ProviderID: "https://www.googleapis.com/storage/v1/b/logs"},
			"//storage.googleapis.com/logs"},
		{entitySnapshot{CloudPlatform: "Kubernetes", ProviderID: "k8s://pod", ID: "wiz-1"}, "k8s://pod"},
		{entitySnapshot{CloudPlatform: "Kubernetes", ID: "wiz-1"}, "wiz-1"},
	}
	for _, c := range es {
		if got := entityName(&c.in); got != c.want {
			t.Errorf("entityName(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
	th := &threat{CloudPlatform: "AWS"}
	res := []struct {
		in       threatResource
		want     string
		platform string
	}{
		{threatResource{ProviderID: "arn:aws:s3:::b"}, "arn:aws:s3:::b", "AWS"},
		{threatResource{CloudPlatform: "OCI", ProviderID: "ocid1.x", ID: "w"}, "ocid1.x", "OCI"},
		{threatResource{CloudPlatform: "OCI", ID: "w"}, "w", "OCI"},
	}
	for _, c := range res {
		if got := resourceEntityName(th, &c.in); got != c.want {
			t.Errorf("resourceEntityName(%+v) = %q, want %q", c.in, got, c.want)
		}
		if got := platformOf(th, &c.in); got != c.platform {
			t.Errorf("platformOf(%+v) = %q, want %q", c.in, got, c.platform)
		}
	}
	if accountResource(th, "") != nil {
		t.Error("an empty account became a resource")
	}
}

func TestDescriptionPrefersIssueProse(t *testing.T) {
	if got := description(&issue{Description: "own", Control: control{Description: "ctl"}}); got != "own" {
		t.Errorf("description = %q", got)
	}
	if got := description(&issue{Control: control{Description: "ctl"}}); got != "ctl" {
		t.Errorf("description fallback = %q", got)
	}
}

// TestTags: empty values are dropped, newlines and pipes cannot break the
// table, keys come sorted, and at most maxTags are kept.
func TestTags(t *testing.T) {
	if tags(nil) != nil {
		t.Error("no tags rendered something")
	}
	got := tags(map[string]string{"b": "x|y\nz", "a": "  ", "c": "ok"})
	if len(got) != 2 || got[0].Key != "b" || got[0].Value != `x\|y z` || got[1].Key != "c" {
		t.Errorf("tags = %+v", got)
	}
	many := map[string]string{}
	for i := range maxTags + 10 {
		many[fmt.Sprintf("k%03d", i)] = "v"
	}
	capped := tags(many)
	if len(capped) != maxTags || capped[0].Key != "k000" {
		t.Errorf("capped tags = %d entries starting %q", len(capped), capped[0].Key)
	}
}

// TestThreatDescriptionNamesActors: actors render by name, else id, with
// their type; nameless, idless ones are dropped.
func TestThreatDescriptionNamesActors(t *testing.T) {
	env := &envelope{Threat: &threat{
		ID: "t1", Name: "Crypto miner", Severity: "HIGH", CloudPlatform: "AWS",
		Actors: []actor{{Name: "alice", Type: "USER"}, {ID: "role-7"}, {}},
		Resources: []threatResource{{ProviderID: "arn:aws:ec2:i-1", NativeType: "ec2:instance", Type: "VIRTUAL_MACHINE",
			Name: "web-1", Region: "eu-west-2"}},
	}}
	out, err := renderThreatDescription(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice (user)", "role-7", "arn:aws:ec2:i-1", "ec2:instance"} {
		if !strings.Contains(out, want) {
			t.Errorf("description lacks %q:\n%s", want, out)
		}
	}
}

func TestNormalizeGCPNameEdges(t *testing.T) {
	cases := map[string]string{
		"https://www.googleapis.com":               "https://www.googleapis.com",
		"https://www.googleapis.com/compute":       "https://www.googleapis.com/compute",
		"https://example.com/x/y":                  "https://example.com/x/y",
		"plain-id":                                 "plain-id",
		"https://storage.googleapis.com/b/bucket1": "//storage.googleapis.com/bucket1",
	}
	for in, want := range cases {
		if got := NormalizeGCPName(in); got != want {
			t.Errorf("NormalizeGCPName(%q) = %q, want %q", in, got, want)
		}
	}
}
