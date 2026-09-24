// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghas

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/pkg/source"
)

type fakeAlerts struct {
	alert *ghclient.Alert
	err   error
	calls int
}

func (f *fakeAlerts) GetAlert(_ context.Context, _ ghclient.Repo, _ int) (*ghclient.Alert, error) {
	f.calls++
	return f.alert, f.err
}

const createdPayload = `{
	"action": "created",
	"alert": {"number": 7},
	"repository": {"name": "shop", "owner": {"login": "acme"}}
}`

func testAlert() *ghclient.Alert {
	return &ghclient.Alert{
		Number:          7,
		RuleID:          "js/reflected-xss",
		RuleName:        "js/reflected-xss",
		RuleDescription: "Reflected cross-site scripting",
		RuleHelp:        "Long help markdown.",
		Tags:            []string{"security", "external/cwe/cwe-079", "external/cwe/cwe-116"},
		Severity:        "high",
		HTMLURL:         "https://github.com/acme/shop/security/code-scanning/7",
		Path:            "src/render.js",
		StartLine:       42,
		EndLine:         44,
		Snippet:         "Reflected XSS sink.",
	}
}

func TestFindings(t *testing.T) {
	alerts := &fakeAlerts{alert: testAlert()}
	h := New(alerts)

	got, err := h.Findings(context.Background(), "code_scanning_alert", []byte(createdPayload))
	if err != nil {
		t.Fatalf("Findings() error = %v", err)
	}
	want := []source.Finding{{
		Source:      "ghas",
		Repo:        source.Repo{Owner: "acme", Name: "shop"},
		AlertNumber: 7,
		Advisories:  []string{"CWE-79", "CWE-116"},
		RuleID:      "js/reflected-xss",
		Title:       "Reflected cross-site scripting",
		Description: "Long help markdown.",
		Severity:    "high",
		HTMLURL:     "https://github.com/acme/shop/security/code-scanning/7",
		Locations: []source.Location{{
			Path: "src/render.js", StartLine: 42, EndLine: 44, Snippet: "Reflected XSS sink.",
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Findings() = %+v\nwant %+v", got, want)
	}
}

func TestFindingsSkips(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload string
	}{
		{"other event", "issues", createdPayload},
		{"fixed action", "code_scanning_alert", `{"action":"fixed","alert":{"number":7},
			"repository":{"name":"shop","owner":{"login":"acme"}}}`},
		{"closed action", "code_scanning_alert", `{"action":"closed_by_user","alert":{"number":7},
			"repository":{"name":"shop","owner":{"login":"acme"}}}`},
		// A merge carries an already-known alert onto the default branch;
		// it is the same alert, not a new sighting.
		{"appeared in branch", "code_scanning_alert", `{"action":"appeared_in_branch","alert":{"number":7},
			"repository":{"name":"shop","owner":{"login":"acme"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alerts := &fakeAlerts{alert: testAlert()}
			got, err := New(alerts).Findings(context.Background(), tt.event, []byte(tt.payload))
			if err != nil || got != nil {
				t.Errorf("Findings() = %v, %v; want nil, nil", got, err)
			}
			if alerts.calls != 0 {
				t.Errorf("GetAlert called %d times for a skipped delivery", alerts.calls)
			}
		})
	}
}

// refPayload is a created delivery whose most recent instance sits on ref,
// in a repository whose default branch is main. An empty ref omits the
// instance's ref field entirely.
func refPayload(ref string) string {
	inst := `{}`
	if ref != "" {
		inst = `{"ref":"` + ref + `"}`
	}
	return `{"action":"created","alert":{"number":7,"most_recent_instance":` + inst + `},
		"repository":{"name":"shop","owner":{"login":"acme"},"default_branch":"main"}}`
}

// TestFindingsRefFilter pins the default-branch-only ingest: an alert raised
// on any other ref (patchy's own remediation branches, or the PR merge refs
// CodeQL analyses) must not become a finding, or a rejected fix loops back in
// as the next generation of the same finding.
func TestFindingsRefFilter(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		ingest  bool
	}{
		{"default branch ref is ingested", refPayload("refs/heads/main"), true},
		{"patchy remediation branch is skipped", refPayload("refs/heads/patchy/finding-x"), false},
		{"pull request merge ref is skipped", refPayload("refs/pull/7/merge"), false},
		{"pull request head ref is skipped", refPayload("refs/pull/7/head"), false},
		{"missing ref fails open", refPayload(""), true},
		{"missing default branch fails open", `{"action":"created",
			"alert":{"number":7,"most_recent_instance":{"ref":"refs/heads/feature"}},
			"repository":{"name":"shop","owner":{"login":"acme"}}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alerts := &fakeAlerts{alert: testAlert()}
			got, err := New(alerts).Findings(context.Background(), "code_scanning_alert", []byte(tt.payload))
			if err != nil {
				t.Fatalf("Findings() error = %v", err)
			}
			if tt.ingest {
				if len(got) != 1 || got[0].AlertNumber != 7 {
					t.Errorf("Findings() = %+v, want alert 7 ingested", got)
				}
				return
			}
			if got != nil {
				t.Errorf("Findings() = %+v, want nil for an off-default-branch alert", got)
			}
			if alerts.calls != 0 {
				t.Errorf("GetAlert called %d times for a skipped delivery", alerts.calls)
			}
		})
	}
}

// TestFindingsCommit pins which revision a finding is recorded as observed
// at, and on which ref: the analysis the delivery reports (commit_oid and
// ref, else the payload's most recent instance), and only when the delivery
// names none, the alert fetched afterwards — which may already reflect a
// later analysis.
func TestFindingsCommit(t *testing.T) {
	const (
		delivered = "45b1bec980a1aba44367aa7bf871e8b658776d40"
		instance  = "1111111111111111111111111111111111111111"
		fetched   = "f4ce2c98005506f003d9d9ff981dea0db4a4b513"
	)
	payload := func(commitOID, ref, instanceSHA string) string {
		return `{"action":"reopened","commit_oid":"` + commitOID + `","ref":"` + ref + `",
			"alert":{"number":7,"most_recent_instance":{"ref":"main","commit_sha":"` + instanceSHA + `"}},
			"repository":{"name":"shop","owner":{"login":"acme"},"default_branch":"main"}}`
	}
	tests := []struct {
		name       string
		payload    string
		wantCommit string
		wantRef    string
	}{
		{"delivery commit wins", payload(delivered, "refs/heads/main", instance), delivered, "refs/heads/main"},
		{"delivery commit without its ref takes the instance's", payload(delivered, "", instance), delivered, "main"},
		{"payload instance when no commit_oid", payload("", "", instance), instance, "main"},
		{"fetched alert when the payload names none", payload("", "", ""), fetched, "refs/heads/fetched"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alert := testAlert()
			alert.MostRecentSHA = fetched
			alert.MostRecentRef = "refs/heads/fetched"
			got, err := New(&fakeAlerts{alert: alert}).Findings(context.Background(),
				"code_scanning_alert", []byte(tt.payload))
			if err != nil {
				t.Fatalf("Findings() error = %v", err)
			}
			if len(got) != 1 || got[0].Commit != tt.wantCommit || got[0].Ref != tt.wantRef {
				t.Errorf("Findings() = %+v, want one finding at commit %s on %s", got, tt.wantCommit, tt.wantRef)
			}
		})
	}
}

func TestFindingsErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		alerts  *fakeAlerts
	}{
		{"bad json", "{nope", &fakeAlerts{}},
		{"missing repo", `{"action":"created","alert":{"number":7}}`, &fakeAlerts{}},
		{"missing number", `{"action":"created","repository":{"name":"shop","owner":{"login":"acme"}}}`, &fakeAlerts{}},
		{"fetch fails", createdPayload, &fakeAlerts{err: errors.New("boom")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.alerts).Findings(context.Background(), "code_scanning_alert",
				[]byte(tt.payload)); err == nil {
				t.Error("Findings() error = nil, want error")
			}
		})
	}
}

func TestAdvisories(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{
			"cwe order preserved, zero-padding stripped",
			[]string{"security", "external/cwe/cwe-079", "external/cwe/cwe-116"},
			[]string{"CWE-79", "CWE-116"},
		},
		{
			"ghsa and cve outrank cwe",
			[]string{"external/cwe/cwe-400", "external/advisory/ghsa-abcd-1234-wxyz", "external/cve/cve-2026-1111"},
			[]string{"GHSA-ABCD-1234-WXYZ", "CVE-2026-1111", "CWE-400"},
		},
		{
			"fallback to rule id",
			[]string{"security", "maintainability"},
			[]string{"rule:js/reflected-xss"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAlert()
			a.Tags = tt.tags
			if got := advisories(a); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("advisories() = %v, want %v", got, tt.want)
			}
		})
	}
}

// dismissal is one recorded DismissAlert call.
type dismissal struct {
	repo    ghclient.Repo
	number  int
	reason  string
	comment string
}

type fakeDismisser struct {
	got []dismissal
	err error
}

func (f *fakeDismisser) DismissAlert(
	_ context.Context, repo ghclient.Repo, number int, reason, comment string,
) error {
	f.got = append(f.got, dismissal{repo, number, reason, comment})
	return f.err
}

func TestResolve(t *testing.T) {
	repo := ghclient.Repo{Owner: "acme", Name: "shop"}
	ignore := source.Verdict{Kind: source.VerdictIgnore, Reason: "false positive", Comment: "not reachable"}

	t.Run("dismisses every decimal alert id", func(t *testing.T) {
		d := &fakeDismisser{}
		err := NewResolver(d, repo).Resolve(context.Background(),
			[]source.AlertRef{{ID: "7"}, {ID: "9"}}, ignore)
		if err != nil {
			t.Fatalf("Resolve() = %v, want nil", err)
		}
		want := []dismissal{
			{repo, 7, "false positive", "not reachable"},
			{repo, 9, "false positive", "not reachable"},
		}
		if !reflect.DeepEqual(d.got, want) {
			t.Errorf("dismissals = %+v, want %+v", d.got, want)
		}
	})

	// The caller hands each resolver only its own alerts, so a non-decimal id
	// here is corrupt state. It is reported, but the rest of the batch still
	// gets dismissed.
	t.Run("reports a non-decimal alert id but dismisses the rest", func(t *testing.T) {
		d := &fakeDismisser{}
		err := NewResolver(d, repo).Resolve(context.Background(), []source.AlertRef{
			{ID: "organizations/1/sources/2/findings/abc", Source: "ghas"},
			{ID: "7", Source: "ghas"},
		}, ignore)
		if err == nil {
			t.Error("Resolve() = nil, want an error naming the unparseable id")
		}
		if len(d.got) != 1 || d.got[0].number != 7 {
			t.Errorf("dismissals = %+v, want alert 7 dismissed anyway", d.got)
		}
	})

	t.Run("no-ops", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			repo    ghclient.Repo
			verdict source.Verdict
		}{
			{"repo-less finding has nothing on GitHub to dismiss", ghclient.Repo{}, ignore},
			{"a verdict other than ignore never dismisses", repo, source.Verdict{Kind: "remediate"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				d := &fakeDismisser{}
				if err := NewResolver(d, tt.repo).Resolve(
					context.Background(), []source.AlertRef{{ID: "7"}}, tt.verdict); err != nil {
					t.Fatalf("Resolve() = %v, want nil", err)
				}
				if len(d.got) != 0 {
					t.Errorf("dismissals = %+v, want none", d.got)
				}
			})
		}
	})

	t.Run("reports dismissal failures", func(t *testing.T) {
		d := &fakeDismisser{err: errors.New("boom")}
		if err := NewResolver(d, repo).Resolve(
			context.Background(), []source.AlertRef{{ID: "7"}}, ignore); err == nil {
			t.Error("Resolve() = nil, want the dismissal error")
		}
	})
}

func TestNormalizeSeverity(t *testing.T) {
	for in, want := range map[string]string{
		"critical": "critical",
		"High":     "high",
		"medium":   "medium",
		"low":      "low",
		"error":    "high",
		"warning":  "medium",
		"note":     "low",
		"":         "low",
	} {
		if got := normalizeSeverity(in); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}
