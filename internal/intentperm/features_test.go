// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentperm

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestForApp pins what an App holds for each selection: the findings
// pipeline's grants and four events; the intent grants and no event; the
// check-fix reads only with checks; actions write only with rerun-failed;
// metadata read always.
func TestForApp(t *testing.T) {
	metadataR := Grant{Permission: Metadata, Access: Read}
	securityW := Grant{Permission: SecurityEvents, Access: Write}
	issuesW := Grant{Permission: Issues, Access: Write}
	contentsW := Grant{Permission: Contents, Access: Write}
	pullsW := Grant{Permission: PullRequests, Access: Write}
	checksR := Grant{Permission: Checks, Access: Read}
	statusesR := Grant{Permission: Statuses, Access: Read}
	actionsR := Grant{Permission: Actions, Access: Read}
	actionsW := Grant{Permission: Actions, Access: Write}
	findingEvs := []string{EventCodeScanningAlert, EventIssueComment, EventIssues, EventPullRequest}

	tests := []struct {
		name     string
		features []Feature
		want     Needs
	}{
		{"nothing", nil, Needs{Grants: []Grant{metadataR}}},
		{"security", []Feature{FeatureSecurity}, Needs{
			Grants: []Grant{contentsW, issuesW, metadataR, pullsW, securityW}, Events: findingEvs}},
		{"intents", []Feature{FeatureIntents}, Needs{
			Grants: []Grant{contentsW, issuesW, metadataR, pullsW}}},
		{"intents and checks", []Feature{FeatureIntents, FeatureChecks}, Needs{
			Grants: []Grant{actionsR, checksR, contentsW, issuesW, metadataR, pullsW, statusesR}}},
		{"intents, checks and re-runs", []Feature{FeatureIntents, FeatureChecks, FeatureRerunFailed}, Needs{
			Grants: []Grant{actionsW, checksR, contentsW, issuesW, metadataR, pullsW, statusesR}}},
		{"everything but re-runs", []Feature{FeatureSecurity, FeatureIntents, FeatureChecks}, Needs{
			Grants: []Grant{actionsR, checksR, contentsW, issuesW, metadataR, pullsW, securityW, statusesR},
			Events: findingEvs}},
		{"everything", Features(), Needs{
			Grants: []Grant{actionsW, checksR, contentsW, issuesW, metadataR, pullsW, securityW, statusesR},
			Events: findingEvs}},
		{"a feature named twice counts once", []Feature{FeatureSecurity, FeatureSecurity}, Needs{
			Grants: []Grant{contentsW, issuesW, metadataR, pullsW, securityW}, Events: findingEvs}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ForApp(tt.features...)
			if err != nil {
				t.Fatalf("ForApp(%v): %v", tt.features, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ForApp(%v) =\n%+v\nwant\n%+v", tt.features, got, tt.want)
			}
		})
	}
}

// TestForAppRefuses: a feature the table does not know, check fixes
// without the intents they extend, or re-runs without the check fixes they
// extend, is an error rather than an App that holds something no controller
// uses.
func TestForAppRefuses(t *testing.T) {
	for _, tt := range []struct {
		features []Feature
		want     string
	}{
		{[]Feature{FeatureChecks}, "extends intents"},
		{[]Feature{FeatureSecurity, FeatureChecks}, "extends intents"},
		{[]Feature{FeatureIntents, "previews"}, `unknown feature "previews"`},
		{[]Feature{FeatureRerunFailed}, "extends checks"},
		{[]Feature{FeatureIntents, FeatureRerunFailed}, "extends checks"},
		{[]Feature{FeatureChecks, FeatureRerunFailed}, "extends intents"},
	} {
		if got, err := ForApp(tt.features...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ForApp(%v) = %+v, %v; want an error containing %q", tt.features, got, err, tt.want)
		}
	}
}

// TestForAppServesEveryProject: an App registered for intents holds, at
// sufficient access, every grant For asks of any Project; with checks, of a
// Project with check fixes too; with rerun-failed as well, of a Project that
// re-runs failed checks. Without checks it holds none of the check-fix
// reads, without rerun-failed no actions write, and an intents App holds no
// findings grant or event: the least each selection can hold.
func TestForAppServesEveryProject(t *testing.T) {
	spec := func(fix []string, rerun bool) *v1alpha1.ProjectSpec {
		return &v1alpha1.ProjectSpec{IntentRepository: intentURL,
			Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}, {Name: "api", URL: apiURL}},
			Checks:       v1alpha1.ProjectChecks{Fix: fix, RerunFailed: rerun}}
	}
	for _, tc := range []struct{ checks, rerun bool }{{false, false}, {true, false}, {true, true}} {
		features := []Feature{FeatureIntents}
		var fix []string
		if tc.checks {
			features, fix = append(features, FeatureChecks), []string{"test"}
		}
		if tc.rerun {
			features = append(features, FeatureRerunFailed)
		}
		app, err := ForApp(features...)
		if err != nil {
			t.Fatal(err)
		}
		held := map[string]string{}
		for _, g := range app.Grants {
			held[g.Permission] = g.Access
		}
		for _, need := range For(spec(fix, tc.rerun)) {
			for _, g := range need.Grants {
				if have := held[g.Permission]; have != g.Access && have != Write {
					t.Errorf("%+v: the App holds %s %q; the Project needs %s on %s", tc, g.Permission, have,
						g, need.URL)
				}
			}
		}
		for _, g := range CheckFix() {
			if _, has := held[g.Permission]; has != tc.checks {
				t.Errorf("%+v: the App holds %s: %v", tc, g.Permission, has)
			}
		}
		if wrote := held[Actions] == Write; wrote != tc.rerun {
			t.Errorf("%+v: the App holds actions write: %v", tc, wrote)
		}
		if _, has := held[SecurityEvents]; has || len(app.Events) != 0 {
			t.Errorf("%+v: an intents App holds %s or events %v", tc, SecurityEvents, app.Events)
		}
	}
}

// TestFeatureRows: every listed feature has a row, every row's grants are
// already merged (one per permission, sorted), and Needs hands each caller
// its own slices.
func TestFeatureRows(t *testing.T) {
	for _, f := range Features() {
		row, ok := f.Needs()
		if !ok || len(row.Grants) == 0 {
			t.Fatalf("%s has no row", f)
		}
		if !reflect.DeepEqual(Merge(row.Grants), row.Grants) || !slices.IsSorted(row.Events) {
			t.Errorf("%s's row is not merged and sorted: %+v", f, row)
		}
		row.Grants[0] = Grant{Permission: "administration", Access: Write}
		if again, _ := f.Needs(); again.Grants[0].Permission == "administration" {
			t.Errorf("a caller's edit to %s's row leaked", f)
		}
	}
	if _, ok := Feature("previews").Needs(); ok {
		t.Error("an unknown feature has a row")
	}
}

// TestForAppProperties states ForApp's contract as properties over random
// valid selections, seeded so the gate is deterministic:
//   - metadata read is always held, events are sorted and distinct;
//   - it is exact: every grant and event comes from metadata or a selected
//     feature's row, at the highest access the rows ask for, and every
//     selected row's grants and events are there;
//   - neither the order of the selection nor repeating a feature changes
//     it, and adding a feature never takes a grant, an access level or an
//     event away.
func TestForAppProperties(t *testing.T) {
	all := Features()
	valid := func(r *rand.Rand) []Feature {
		var out []Feature
		for range r.Intn(6) {
			out = append(out, all[r.Intn(len(all))])
		}
		out = withRequired(out)
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261003)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(valid(r))
			args[1] = reflect.ValueOf(all[r.Intn(len(all))])
		},
	}
	prop := func(sel []Feature, extra Feature) bool {
		got, err := ForApp(sel...)
		if err != nil {
			t.Logf("ForApp(%v): %v", sel, err)
			return false
		}
		for _, check := range []func() string{
			func() string { return exactNeeds(sel, got) },
			func() string { return orderFree(sel, got) },
			func() string { return monotone(sel, extra, got) },
		} {
			if why := check(); why != "" {
				t.Logf("ForApp(%v) = %+v: %s", sel, got, why)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// withRequired is sel with every feature a selected one Requires, and
// theirs in turn, appended: a selection ForApp accepts.
func withRequired(sel []Feature) []Feature {
	for i := 0; i < len(sel); i++ {
		for _, r := range sel[i].Requires() {
			if !slices.Contains(sel, r) {
				sel = append(sel, r)
			}
		}
	}
	return sel
}

// accessRank orders access levels; nothing ranks below read.
var accessRank = map[string]int{Read: 1, Write: 2}

// held is grants as permission to access.
func held(grants []Grant) map[string]string {
	out := map[string]string{}
	for _, g := range grants {
		out[g.Permission] = g.Access
	}
	return out
}

// exactNeeds reports how got is not exactly metadata read plus sel's rows
// (highest access wins) and their events, sorted and distinct; "" when it is.
func exactNeeds(sel []Feature, got Needs) string {
	want := map[string]string{Metadata: Read}
	var events []string
	for _, f := range sel {
		row, _ := f.Needs()
		for _, g := range row.Grants {
			if accessRank[g.Access] > accessRank[want[g.Permission]] {
				want[g.Permission] = g.Access
			}
		}
		events = append(events, row.Events...)
	}
	slices.Sort(events)
	events = slices.Compact(events)
	switch {
	case !reflect.DeepEqual(held(got.Grants), want):
		return fmt.Sprintf("grants are not exactly %v", want)
	case !slices.Equal(got.Events, events):
		return fmt.Sprintf("events are not exactly %v", events)
	}
	return ""
}

// orderFree reports a selection whose reverse, repeated, answers otherwise.
func orderFree(sel []Feature, got Needs) string {
	reordered := slices.Clone(sel)
	slices.Reverse(reordered)
	if again, _ := ForApp(append(reordered, sel...)...); !reflect.DeepEqual(again, got) {
		return fmt.Sprintf("reordered and repeated, it is %+v", again)
	}
	return ""
}

// monotone reports a grant, access level or event that adding extra to sel
// (with the features it extends) takes away.
func monotone(sel []Feature, extra Feature, got Needs) string {
	grown := withRequired(append(slices.Clone(sel), extra))
	more, err := ForApp(grown...)
	if err != nil {
		return fmt.Sprintf("ForApp(%v): %v", grown, err)
	}
	moreHeld := held(more.Grants)
	for p, a := range held(got.Grants) {
		if accessRank[moreHeld[p]] < accessRank[a] {
			return fmt.Sprintf("adding %s lowered %s from %s to %q", extra, p, a, moreHeld[p])
		}
	}
	for _, e := range got.Events {
		if !slices.Contains(more.Events, e) {
			return fmt.Sprintf("adding %s dropped event %s", extra, e)
		}
	}
	return ""
}
