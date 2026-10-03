// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestCIFixRound: a round failed checks started is named as one, with its
// checks, each a code span it cannot close early, on one line.
func TestCIFixRound(t *testing.T) {
	for _, tt := range []struct {
		checks []string
		want   string
	}{
		{nil, "CI-fix round"},
		{[]string{"", " \n"}, "CI-fix round"},
		{[]string{"changelog"}, "CI-fix round for `changelog`"},
		{[]string{"lint", "test"}, "CI-fix round for `lint` and `test`"},
		{[]string{"build", "lint", "test (1.26)"}, "CI-fix round for `build`, `lint` and `test (1.26)`"},
		{[]string{"a`b", "two\nlines @octocat"}, "CI-fix round for ``a`b`` and `two lines @octocat`"},
	} {
		if got := CIFixRound(tt.checks); got != tt.want {
			t.Errorf("CIFixRound(%q) = %q, want %q", tt.checks, got, tt.want)
		}
	}
}

// TestFixingChecksOnlyInRevising: FixingChecks changes the Revising sentence
// alone; every other phase renders as without it, and a review round's
// Revising comment is unchanged.
func TestFixingChecksOnlyInRevising(t *testing.T) {
	for _, phase := range []string{"Planning", "Building", "InReview", "Blocked", "Merged"} {
		plain, err := RenderIntentStatusComment(IntentStatusComment{Namespace: "patchy", Intent: "x-1", Phase: phase})
		if err != nil {
			t.Fatal(err)
		}
		fixing, err := RenderIntentStatusComment(IntentStatusComment{Namespace: "patchy", Intent: "x-1", Phase: phase,
			FixingChecks: true})
		if err != nil {
			t.Fatal(err)
		}
		if plain != fixing {
			t.Errorf("%s: FixingChecks changed the comment:\n%s\n---\n%s", phase, plain, fixing)
		}
	}
	review, err := RenderIntentStatusComment(IntentStatusComment{Namespace: "patchy", Intent: "x-1", Phase: "Revising"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(review, "revising the pull requests from review feedback") {
		t.Errorf("a review round's Revising sentence changed:\n%s", review)
	}
}
