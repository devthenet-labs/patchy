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
			"- `/patchy replan`: plan again, taking the comments since the last plan into account\n" +
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

// TestHelpListsExactlyAvailable: the help and Available cannot disagree — the
// help names every offered verb once, in order, and no other.
func TestHelpListsExactlyAvailable(t *testing.T) {
	for _, s := range []command.Surface{command.FindingIssue, command.IntentIssue, command.IntentPR} {
		var listed []string
		for _, line := range strings.Split(command.Help(s), "\n") {
			if rest, ok := strings.CutPrefix(line, "- `"+command.Prefix+" "); ok {
				verb, _, _ := strings.Cut(rest, "`")
				verb, _, _ = strings.Cut(verb, " ")
				listed = append(listed, verb)
			}
		}
		if want := command.Available(s); !slices.Equal(listed, want) {
			t.Errorf("Help(%q) lists %v, Available offers %v", s, listed, want)
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
