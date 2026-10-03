// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentperm

import (
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
// check-fix reads only with checks; metadata read always.
func TestForApp(t *testing.T) {
	metadataR := Grant{Permission: Metadata, Access: Read}
	securityW := Grant{Permission: SecurityEvents, Access: Write}
	issuesW := Grant{Permission: Issues, Access: Write}
	contentsW := Grant{Permission: Contents, Access: Write}
	pullsW := Grant{Permission: PullRequests, Access: Write}
	checksR := Grant{Permission: Checks, Access: Read}
	statusesR := Grant{Permission: Statuses, Access: Read}
	actionsR := Grant{Permission: Actions, Access: Read}
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
		{"everything", Features(), Needs{
			Grants: []Grant{actionsR, checksR, contentsW, issuesW, metadataR, pullsW, securityW, statusesR},
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

// TestForAppRefuses: a feature the table does not know, or check fixes
// without the intents they extend, is an error rather than an App that
// holds something no controller uses.
func TestForAppRefuses(t *testing.T) {
	for _, tt := range []struct {
		features []Feature
		want     string
	}{
		{[]Feature{FeatureChecks}, "extends intents"},
		{[]Feature{FeatureSecurity, FeatureChecks}, "extends intents"},
		{[]Feature{FeatureIntents, "previews"}, `unknown feature "previews"`},
	} {
		if got, err := ForApp(tt.features...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ForApp(%v) = %+v, %v; want an error containing %q", tt.features, got, err, tt.want)
		}
	}
}

// TestForAppServesEveryProject: an App registered for intents holds, at
// sufficient access, every grant For asks of any Project; with checks, of a
// Project with check fixes too. Without checks it holds none of the
// check-fix reads, and an intents App holds no findings grant or event: the
// least each selection can hold.
func TestForAppServesEveryProject(t *testing.T) {
	spec := func(fix []string) *v1alpha1.ProjectSpec {
		return &v1alpha1.ProjectSpec{IntentRepository: intentURL,
			Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}, {Name: "api", URL: apiURL}},
			Checks:       v1alpha1.ProjectChecks{Fix: fix}}
	}
	for _, checks := range []bool{false, true} {
		features := []Feature{FeatureIntents}
		var fix []string
		if checks {
			features, fix = append(features, FeatureChecks), []string{"test"}
		}
		app, err := ForApp(features...)
		if err != nil {
			t.Fatal(err)
		}
		held := map[string]string{}
		for _, g := range app.Grants {
			held[g.Permission] = g.Access
		}
		for _, need := range For(spec(fix)) {
			for _, g := range need.Grants {
				if have := held[g.Permission]; have != g.Access && have != Write {
					t.Errorf("checks=%v: the App holds %s %q; the Project needs %s on %s", checks, g.Permission, have,
						g, need.URL)
				}
			}
		}
		for _, g := range CheckFix() {
			if _, has := held[g.Permission]; has != checks {
				t.Errorf("checks=%v: the App holds %s: %v", checks, g.Permission, has)
			}
		}
		if _, has := held[SecurityEvents]; has || len(app.Events) != 0 {
			t.Errorf("checks=%v: an intents App holds %s or events %v", checks, SecurityEvents, app.Events)
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
//   - it is minimal: every grant and event comes from metadata or a
//     selected feature's row, at the highest access the rows ask for;
//   - it is complete: every selected row's grants are held at least at
//     their access, and its events are delivered;
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
		if slices.Contains(out, FeatureChecks) && !slices.Contains(out, FeatureIntents) {
			out = append(out, FeatureIntents)
		}
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
	rank := map[string]int{Read: 1, Write: 2}
	prop := func(sel []Feature, extra Feature) bool {
		got, err := ForApp(sel...)
		if err != nil {
			t.Logf("ForApp(%v): %v", sel, err)
			return false
		}
		held := map[string]string{}
		for _, g := range got.Grants {
			held[g.Permission] = g.Access
		}
		if held[Metadata] != Read || !slices.IsSorted(got.Events) || len(slices.Compact(slices.Clone(got.Events))) !=
			len(got.Events) {
			t.Logf("ForApp(%v) = %+v: no metadata read, or events unsorted or repeated", sel, got)
			return false
		}
		want := map[string]string{Metadata: Read}
		wantEvents := map[string]bool{}
		for _, f := range sel {
			row, _ := f.Needs()
			for _, g := range row.Grants {
				if rank[g.Access] > rank[want[g.Permission]] {
					want[g.Permission] = g.Access
				}
			}
			for _, e := range row.Events {
				wantEvents[e] = true
			}
		}
		if !reflect.DeepEqual(held, want) || len(got.Events) != len(wantEvents) {
			t.Logf("ForApp(%v) = %+v, want grants %v and events %v", sel, got, want, wantEvents)
			return false
		}
		for _, e := range got.Events {
			if !wantEvents[e] {
				t.Logf("ForApp(%v) delivers %s, which no selected feature consumes", sel, e)
				return false
			}
		}
		reordered := slices.Clone(sel)
		slices.Reverse(reordered)
		if again, _ := ForApp(append(reordered, sel...)...); !reflect.DeepEqual(again, got) {
			t.Logf("ForApp(%v) = %+v, but reordered and repeated %+v", sel, got, again)
			return false
		}
		grown := append(slices.Clone(sel), extra)
		if extra == FeatureChecks {
			grown = append(grown, FeatureIntents)
		}
		more, err := ForApp(grown...)
		if err != nil {
			t.Logf("ForApp(%v): %v", grown, err)
			return false
		}
		moreHeld := map[string]string{}
		for _, g := range more.Grants {
			moreHeld[g.Permission] = g.Access
		}
		for p, a := range held {
			if rank[moreHeld[p]] < rank[a] {
				t.Logf("adding %s to %v lowered %s from %s to %q", extra, sel, p, a, moreHeld[p])
				return false
			}
		}
		for _, e := range got.Events {
			if !slices.Contains(more.Events, e) {
				t.Logf("adding %s to %v dropped event %s", extra, sel, e)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
