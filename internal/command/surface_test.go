// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
)

// TestAvailable pins the design's table of which verbs apply where. A Finding
// issue offers exactly the status page's and the CLI's Finding verbs, in
// their order, so a new Finding verb fails here until its GitHub form is
// decided.
func TestAvailable(t *testing.T) {
	cases := []struct {
		surface command.Surface
		want    []string
	}{
		{command.FindingIssue, action.ActionVerbs},
		{command.IntentIssue, []string{action.VerbApprove, action.VerbReplan, action.VerbCancel}},
		{command.IntentPR, []string{action.VerbRevise, action.VerbRetry}},
		{"pull-request", nil},
		{"", nil},
	}
	for _, tc := range cases {
		t.Run(string(tc.surface), func(t *testing.T) {
			if got := command.Available(tc.surface); !slices.Equal(got, tc.want) {
				t.Errorf("Available(%q) = %v, want %v", tc.surface, got, tc.want)
			}
		})
	}
	if !slices.Equal(action.ActionVerbs, []string{
		action.VerbApprove, action.VerbRetry, action.VerbExpedite, action.VerbSuspend, action.VerbResume,
	}) {
		t.Errorf("action.ActionVerbs = %v: the Finding issue's verbs changed with it", action.ActionVerbs)
	}
}

// TestAvailableReturnsACopy: a caller that edits the slice it got cannot
// change what the next caller is offered.
func TestAvailableReturnsACopy(t *testing.T) {
	for _, s := range []command.Surface{command.FindingIssue, command.IntentIssue, command.IntentPR} {
		got := command.Available(s)
		got[0] = "tampered"
		if again := command.Available(s); slices.Contains(again, "tampered") {
			t.Errorf("Available(%q) shares its backing array with callers: %v", s, again)
		}
	}
	if slices.Contains(action.ActionVerbs, "tampered") {
		t.Errorf("Available(FindingIssue) shares action.ActionVerbs: %v", action.ActionVerbs)
	}
}

// TestAvailableVerbsParse: every verb a surface offers is one the grammar
// can express.
func TestAvailableVerbsParse(t *testing.T) {
	for _, s := range []command.Surface{command.FindingIssue, command.IntentIssue, command.IntentPR} {
		for _, verb := range command.Available(s) {
			if c, ok := command.Parse(command.Prefix + " " + verb); !ok || c.Verb != verb {
				t.Errorf("%s: Parse(%q) = %+v, %v", s, command.Prefix+" "+verb, c, ok)
			}
		}
	}
}

func TestHelp(t *testing.T) {
	const intro = "Commands go on the first line of a comment. Here you can use:\n\n"
	cases := []struct {
		surface command.Surface
		want    string
	}{
		{command.FindingIssue, intro +
			"- `/patchy approve [note]`: release the hold on this finding, or revive it after it was handed off\n" +
			"- `/patchy retry`: retry this finding from the state it failed in\n" +
			"- `/patchy expedite`: skip the accumulation window, the minimum age and the queue\n" +
			"- `/patchy suspend`: pause this finding's progress through the pipeline\n" +
			"- `/patchy resume`: resume this finding after a suspend"},
		{command.IntentIssue, intro +
			"- `/patchy approve`: approve the posted plan and start the build\n" +
			"- `/patchy replan [note]`: plan again, taking the note and approvers' comments since the last plan into account\n" +
			"- `/patchy cancel`: stop work on this intent and close it; open pull requests are left to you"},
		{command.IntentPR, intro +
			"- `/patchy revise [note]`: start a revision round from your review feedback and the note\n" +
			"- `/patchy retry`: retry the round that failed"},
		{"pull-request", ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.surface), func(t *testing.T) {
			if got := command.Help(tc.surface); got != tc.want {
				t.Errorf("Help(%q) =\n%s\nwant\n%s", tc.surface, got, tc.want)
			}
		})
	}
}

// TestHelpFor: the reply to a verb the phase does not admit lists only what
// it does, with the same usage lines as the full help.
func TestHelpFor(t *testing.T) {
	const intro = "Commands go on the first line of a comment. Here you can use:\n\n"
	cases := []struct {
		name    string
		surface command.Surface
		verbs   []string
		want    string
	}{
		{"a held finding", command.FindingIssue, []string{action.VerbApprove, action.VerbSuspend}, intro +
			"- `/patchy approve [note]`: release the hold on this finding, or revive it after it was handed off\n" +
			"- `/patchy suspend`: pause this finding's progress through the pipeline"},
		{"the surface's order, not the caller's", command.IntentIssue,
			[]string{action.VerbCancel, action.VerbReplan}, intro +
				"- `/patchy replan [note]`: plan again, taking the note and approvers' comments since the last plan " +
				"into account\n" +
				"- `/patchy cancel`: stop work on this intent and close it; open pull requests are left to you"},
		{"verbs the surface does not offer are ignored", command.IntentPR,
			[]string{action.VerbExpedite, action.VerbRetry, "bogus"}, intro +
				"- `/patchy retry`: retry the round that failed"},
		{"nothing admitted", command.FindingIssue, nil, ""},
		{"nothing the surface offers", command.IntentPR, []string{action.VerbApprove}, ""},
		{"an unknown surface", "pull-request", []string{action.VerbApprove}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := command.HelpFor(tc.surface, tc.verbs); got != tc.want {
				t.Errorf("HelpFor(%q, %v) =\n%s\nwant\n%s", tc.surface, tc.verbs, got, tc.want)
			}
		})
	}
}

// helpLines are the usage lines of a help reply, keyed by verb, in order.
func helpLines(help string) (verbs, lines []string) {
	for _, line := range strings.Split(help, "\n") {
		if rest, ok := strings.CutPrefix(line, "- `"+command.Prefix+" "); ok {
			verb, _, _ := strings.Cut(rest, "`")
			verb, _, _ = strings.Cut(verb, " ")
			verbs = append(verbs, verb)
			lines = append(lines, line)
		}
	}
	return verbs, lines
}

// TestHelpListsExactlyAvailable: the help and Available cannot disagree — the
// help names every offered verb once, in order, and no other — and for any
// set of verbs, HelpFor lists exactly those the surface offers, in the
// surface's order, each with the line the full help gives it.
func TestHelpListsExactlyAvailable(t *testing.T) {
	candidates := []string{
		action.VerbApprove, action.VerbRetry, action.VerbExpedite, action.VerbSuspend, action.VerbResume,
		action.VerbReplan, action.VerbCancel, action.VerbRevise, "bogus",
	}
	for _, s := range []command.Surface{command.FindingIssue, command.IntentIssue, command.IntentPR} {
		offered := command.Available(s)
		listed, full := helpLines(command.Help(s))
		if !slices.Equal(listed, offered) {
			t.Errorf("Help(%q) lists %v, Available offers %v", s, listed, offered)
		}
		lineOf := map[string]string{}
		for i, verb := range listed {
			lineOf[verb] = full[i]
		}
		// Every subset of the candidates, passed in reverse so the order must
		// come from the surface.
		for mask := range 1 << len(candidates) {
			var verbs, want []string
			for i := len(candidates) - 1; i >= 0; i-- {
				if mask&(1<<i) != 0 {
					verbs = append(verbs, candidates[i])
				}
			}
			for _, verb := range offered {
				if slices.Contains(verbs, verb) {
					want = append(want, verb)
				}
			}
			help := command.HelpFor(s, verbs)
			got, lines := helpLines(help)
			if !slices.Equal(got, want) || (len(want) == 0) != (help == "") {
				t.Fatalf("HelpFor(%q, %v) lists %v (%q), want %v", s, verbs, got, help, want)
			}
			for i, verb := range got {
				if lines[i] != lineOf[verb] {
					t.Fatalf("HelpFor(%q, %v) gives %s the line %q, Help gives %q", s, verbs, verb, lines[i], lineOf[verb])
				}
			}
		}
	}
}

// TestPureImports keeps the parser pure: it may import the standard library
// and the shared verb vocabulary, and nothing that talks to GitHub or
// Kubernetes.
func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const allowed = "github.com/bitwise-media-group/patchy/internal/action"
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if path != allowed && strings.Contains(first, ".") {
				t.Errorf("%s imports %s: the command package imports only the standard library and %s",
					name, path, allowed)
			}
		}
	}
}
