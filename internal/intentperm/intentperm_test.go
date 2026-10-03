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

const (
	intentURL = "https://github.com/acme/intents"
	webURL    = "https://github.com/acme/Shop.Web"
	apiURL    = "https://github.com/acme/shop-api"
)

// TestFor: the intent repository needs issues write; every application
// repository contents and pull requests write, plus the check-fix reads
// when spec.checks.fix names a check, and never otherwise.
func TestFor(t *testing.T) {
	issuesW := Grant{Permission: Issues, Access: Write}
	contentsW := Grant{Permission: Contents, Access: Write}
	pullsW := Grant{Permission: PullRequests, Access: Write}
	checksR := Grant{Permission: Checks, Access: Read}
	statusesR := Grant{Permission: Statuses, Access: Read}
	actionsR := Grant{Permission: Actions, Access: Read}

	tests := []struct {
		name string
		spec v1alpha1.ProjectSpec
		want []Requirement
	}{
		{
			name: "one repository, no check fixes",
			spec: v1alpha1.ProjectSpec{IntentRepository: intentURL,
				Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}}},
			want: []Requirement{
				{Role: RoleIntent, URL: intentURL, Grants: []Grant{issuesW}},
				{Role: RoleApp, Key: "web", URL: webURL, Grants: []Grant{contentsW, pullsW}},
			},
		},
		{
			name: "check fixes add the reads",
			spec: v1alpha1.ProjectSpec{IntentRepository: intentURL,
				Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}},
				Checks:       v1alpha1.ProjectChecks{Fix: []string{"test"}}},
			want: []Requirement{
				{Role: RoleIntent, URL: intentURL, Grants: []Grant{issuesW}},
				{Role: RoleApp, Key: "web", URL: webURL,
					Grants: []Grant{contentsW, pullsW, checksR, statusesR, actionsR}},
			},
		},
		{
			name: "every repository, in order",
			spec: v1alpha1.ProjectSpec{IntentRepository: intentURL,
				Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}, {Name: "api", URL: apiURL}},
				Checks:       v1alpha1.ProjectChecks{Fix: []string{"test", "lint"}}},
			want: []Requirement{
				{Role: RoleIntent, URL: intentURL, Grants: []Grant{issuesW}},
				{Role: RoleApp, Key: "web", URL: webURL,
					Grants: []Grant{contentsW, pullsW, checksR, statusesR, actionsR}},
				{Role: RoleApp, Key: "api", URL: apiURL,
					Grants: []Grant{contentsW, pullsW, checksR, statusesR, actionsR}},
			},
		},
		{
			name: "the intent repository is also an application repository",
			spec: v1alpha1.ProjectSpec{IntentRepository: webURL,
				Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}}},
			want: []Requirement{
				{Role: RoleIntent, URL: webURL, Grants: []Grant{issuesW}},
				{Role: RoleApp, Key: "web", URL: webURL, Grants: []Grant{contentsW, pullsW}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := For(&tt.spec); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("For() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// TestForIsolatesCallers: a caller editing one requirement's grants cannot
// change another's, or the next call's.
func TestForIsolatesCallers(t *testing.T) {
	spec := v1alpha1.ProjectSpec{IntentRepository: intentURL,
		Repositories: []v1alpha1.ProjectRepository{{Name: "web", URL: webURL}, {Name: "api", URL: apiURL}}}
	got := For(&spec)
	got[1].Grants[0] = Grant{Permission: "administration", Access: Write}
	if again := For(&spec); again[2].Grants[0].Permission != Contents || again[1].Grants[0].Permission != Contents {
		t.Errorf("a caller's edit leaked: %+v", again)
	}
}

// TestGrantString spells a grant as GitHub's API does.
func TestGrantString(t *testing.T) {
	if got := (Grant{Permission: PullRequests, Access: Write}).String(); got != "pull_requests: write" {
		t.Errorf("String() = %q", got)
	}
}

// TestMerge: the highest access wins, one grant per permission, sorted.
func TestMerge(t *testing.T) {
	got := Merge(Intent(), App(true), App(false), []Grant{{Permission: Issues, Access: Read}})
	want := []Grant{
		{Permission: Actions, Access: Read},
		{Permission: Checks, Access: Read},
		{Permission: Contents, Access: Write},
		{Permission: Issues, Access: Write},
		{Permission: PullRequests, Access: Write},
		{Permission: Statuses, Access: Read},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
	if got := Merge(); len(got) != 0 {
		t.Errorf("Merge() of nothing = %+v", got)
	}
}

// TestMergeProperties states Merge's contract as properties over random
// grant sets: the result is sorted with one grant per permission; it holds
// exactly the permissions asked for, each at write exactly when some set
// asks for write; and neither the order of the sets nor merging twice
// changes it. Seeded, so the gate is deterministic.
func TestMergeProperties(t *testing.T) {
	perms := []string{Issues, Contents, PullRequests, Checks, Statuses, Actions, "metadata"}
	gen := func(r *rand.Rand) [][]Grant {
		sets := make([][]Grant, r.Intn(5))
		for i := range sets {
			for range r.Intn(6) {
				access := Read
				if r.Intn(2) == 0 {
					access = Write
				}
				sets[i] = append(sets[i], Grant{Permission: perms[r.Intn(len(perms))], Access: access})
			}
		}
		return sets
	}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261003)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(gen(r))
		},
	}
	prop := func(sets [][]Grant) bool {
		got := Merge(sets...)
		if !slices.IsSortedFunc(got, func(a, b Grant) int { return strings.Compare(a.Permission, b.Permission) }) {
			t.Logf("unsorted: %+v", got)
			return false
		}
		want := map[string]string{}
		for _, set := range sets {
			for _, g := range set {
				if g.Access == Write || want[g.Permission] == "" {
					want[g.Permission] = g.Access
				}
			}
		}
		if len(got) != len(want) {
			t.Logf("Merge(%+v) = %+v, want one grant per permission of %v", sets, got, want)
			return false
		}
		for _, g := range got {
			if want[g.Permission] != g.Access {
				t.Logf("Merge(%+v) = %+v: %s at %s, want %s", sets, got, g.Permission, g.Access, want[g.Permission])
				return false
			}
		}
		reversed := slices.Clone(sets)
		slices.Reverse(reversed)
		return reflect.DeepEqual(Merge(reversed...), got) && reflect.DeepEqual(Merge(got), got)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
