// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/bitwise-media-group/patchy/internal/report"
)

// testPlanLimits and testBuildLimits are the runs' own limits the test
// prompts state: the plan's well below the build ceiling it states beside
// them (150 turns, 800000 tokens), so neither can pass for the other.
var (
	testPlanLimits  = StageLimits{MaxTurns: 40, TokenBudget: 250000, Timeout: 20 * time.Minute}
	testBuildLimits = StageLimits{MaxTurns: 120, TokenBudget: 600000, Timeout: 45 * time.Minute}
)

// paragraph returns the blank-line-separated paragraph of s holding sub, or
// "" when there is none.
func paragraph(s, sub string) string {
	for p := range strings.SplitSeq(s, "\n\n") {
		if strings.Contains(p, sub) {
			return p
		}
	}
	return ""
}

// TestPlanPromptStatesItsLimits: every plan prompt, first attempt or retry,
// one repository or several, opens "How to plan" with the plan run's own
// turns, tokens and wall clock — before, and apart from, the build ceiling
// it sizes the plan against — and says plainly what the read-only sandbox
// refuses: every command but four read-only git ones, and every write but
// the report. It asks for independent reads in one response, and points the
// test plan at the repository's own guidance and CI, with the rule against
// planning changes under .github/ in the same paragraph as that pointer.
func TestPlanPromptStatesItsLimits(t *testing.T) {
	renders := map[string]func() (string, error){
		"first attempt": func() (string, error) { return renderTestPlanPrompt(testPlanRequest, nil) },
		"budget retry": func() (string, error) {
			return renderTestPlanPrompt(testPlanRequest, &PreviousAttempt{Attempt: 1, Outcome: "budget_exceeded"})
		},
		"trees": func() (string, error) { return renderTestTreesPlanPrompt(nil) },
	}
	for name, render := range renders {
		t.Run(name, func(t *testing.T) {
			got, err := render()
			if err != nil {
				t.Fatal(err)
			}
			_, howTo, ok := strings.Cut(got, "\n## How to plan\n\n")
			if !ok {
				t.Fatalf("no How to plan section:\n%s", got)
			}
			own := "This stage may take at most 40 agent turns, 250000 output tokens and 20 minutes."
			if !strings.HasPrefix(howTo, own) {
				t.Errorf("How to plan does not open with the plan's own limits %q:\n%.300s", own, howTo)
			}
			build := "A build of this plan can be granted at most 150 agent turns and 800000 output tokens."
			if i, j := strings.Index(got, own), strings.Index(got, build); j < 0 || j < i {
				t.Errorf("the build ceiling %q is not stated after the plan's own limits", build)
			}
			for _, want := range []string{
				"make all of those Read, Glob and Grep calls together in one response",
				"You cannot run commands, tests, builds, package managers or\nscripts",
				"the only shell commands allowed are `git log`, `git show`, `git blame` and `git diff`",
				"no file except your report, `/workspace/reports/plan.md`: a write anywhere else is refused",
				"(CLAUDE.md, AGENTS.md, CONTRIBUTING.md, the README's development notes)",
				"Name the test command its CI runs",
				"Edit it in place",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("plan prompt lacks %q", want)
				}
			}
			pointer := paragraph(got, "`.github/workflows/`")
			if !strings.Contains(pointer, "never plan changes under `.github/`, `.patchy/` or\n`.devcontainer/`") {
				t.Errorf("the pointer at the CI workflows is not beside the rule against changing them:\n%s", pointer)
			}
		})
	}
}

// TestPlanPromptStatesTheReportBounds: the plan prompt states each bound
// report.ParsePlan holds the frontmatter and the document to, read from the
// parser's own constants, so a bound that changes there fails here until
// the prompt says the same.
func TestPlanPromptStatesTheReportBounds(t *testing.T) {
	got, err := renderTestPlanPrompt(testPlanRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("`summary`: a double-quoted string on one line, not empty, at most %d characters.",
			report.SummaryMaxChars),
		fmt.Sprintf("`repositories`: 1 to %d double-quoted `https://<host>/<owner>/<name>` URLs",
			report.PlanMaxRepositories),
		fmt.Sprintf("`new_dependencies`: at most %d double-quoted one-line strings (`[]` for none), each new "+
			"dependency at most %d bytes.", report.PlanMaxNewDependencies, report.DependencyMaxBytes),
		fmt.Sprintf("`questions`: at most %d double-quoted one-line strings (`[]` for none), each at most %d "+
			"characters.", report.PlanMaxQuestions, report.ItemMaxChars),
		"`confidence`: a bare number from 0.0 to 1.0",
		"`estimated_max_turns` and `estimated_token_budget`: positive whole numbers in plain digits",
		fmt.Sprintf("in at most\n%d KiB", report.BodyMaxBytes>>10),
		fmt.Sprintf("the whole report, frontmatter included, is at most %d KiB", report.ReportMaxBytes>>10),
		fmt.Sprintf("no gap of more than %d spaces", report.PadMaxColumns),
		fmt.Sprintf("(a tab counts as %d)", report.TabColumns),
		fmt.Sprintf("no line indented\nmore than %d columns", report.IndentMaxColumns),
		fmt.Sprintf("no more than %d combining marks in a row", report.CombiningMaxMarks),
		fmt.Sprintf("no run of more than %d backticks", report.PlanMaxBacktickRun),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan prompt lacks %q", want)
		}
	}
}

// TestBuildPromptStatesItsLimits: every build prompt — a first build's, a
// revise or check-fix round's, and every retry's, in one repository or
// several — states the run's own turns, tokens and wall clock.
func TestBuildPromptStatesItsLimits(t *testing.T) {
	const want = "This run may take at most 120 agent turns, 600000 output tokens and 45 minutes."
	renders := map[string]func() (string, error){
		"first build": func() (string, error) { return renderTestBuildPrompt(nil) },
		"timeout retry": func() (string, error) {
			return renderTestBuildPrompt(&PreviousAttempt{Attempt: 1, Outcome: "timeout"})
		},
		"budget retry": func() (string, error) {
			return renderTestBuildPrompt(&PreviousAttempt{Attempt: 2, Outcome: "budget_exceeded"})
		},
		"commit_failed retry": func() (string, error) {
			return renderTestBuildPrompt(&PreviousAttempt{Attempt: 1, Outcome: "commit_failed"})
		},
		"one of several repositories": func() (string, error) { return renderTestSiblingBuildPrompt(nil, testWebURL) },
	}
	for name, render := range renders {
		got, err := render()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: build prompt lacks %q", name, want)
		}
	}
}

func TestStageLimitsWallClock(t *testing.T) {
	for _, tt := range []struct {
		timeout time.Duration
		want    string
	}{
		{15 * time.Minute, "15 minutes"},
		{time.Minute, "1 minute"},
		{2 * time.Hour, "120 minutes"},
		{90 * time.Second, "90 seconds"},
		{time.Second, "1 second"},
		{1500 * time.Millisecond, "2 seconds"},
		{0, "0 seconds"},
	} {
		if got := (StageLimits{Timeout: tt.timeout}).WallClock(); got != tt.want {
			t.Errorf("WallClock(%s) = %q, want %q", tt.timeout, got, tt.want)
		}
	}
}

// TestStageLimitsWallClockProperty: whatever the timeout, the wall clock a
// prompt states is that timeout to the second, in minutes exactly when it
// is a whole number of them, and singular only for one.
func TestStageLimitsWallClockProperty(t *testing.T) {
	cfg := &quick.Config{Rand: rand.New(rand.NewSource(20261005)), MaxCount: 1000}
	stated := func(seconds uint16, millis uint16, whole bool) bool {
		d := time.Duration(seconds) * time.Second
		if whole {
			d = time.Duration(seconds) * time.Minute
		} else {
			d += time.Duration(millis%1000) * time.Millisecond
		}
		n, unit, ok := strings.Cut((StageLimits{Timeout: d}).WallClock(), " ")
		count, err := strconv.Atoi(n)
		if !ok || err != nil || (unit == "minute" || unit == "second") != (count == 1) {
			return false
		}
		switch strings.TrimSuffix(unit, "s") {
		case "minute":
			return d >= time.Minute && d%time.Minute == 0 && time.Duration(count)*time.Minute == d
		case "second":
			return (d < time.Minute || d%time.Minute != 0) && time.Duration(count)*time.Second == d.Round(time.Second)
		}
		return false
	}
	if err := quick.Check(stated, cfg); err != nil {
		t.Error(err)
	}
}
