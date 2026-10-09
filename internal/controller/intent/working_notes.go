// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// A build-side run (build, revise or check-fix) may end its report with
// working notes (report.WorkingNotesHeading): what the next agent on the
// intent should know. patchy hands the latest of them on: to the next round
// of the same repository and plan revision, after the approved plan and
// before the round's feedback, and to a replan's or revival's context file.
// The report is re-parsed here, never trusted from the event: a repository's
// image is untrusted, and a report that no longer parses gives no notes.
// Nothing refuses notes for their size; patchy cuts them at
// maxWorkingNotesBytes with a visible marker, so an over-long note costs the
// next agent its tail, never a round.
const maxWorkingNotesBytes = 6 << 10

// workingNotesHeading heads the notes in a round's input.
const workingNotesHeading = "Working notes from the previous round"

// notesTruncated ends notes patchy cut.
const notesTruncated = "\n[patchy truncated these working notes here: they ran past 6 KiB]"

// runWorkingNotes is the working notes of run's stored report, escaped so
// every character shows (backticks too, when no fence would fit), cut to maxWorkingNotesBytes with notesTruncated,
// and fenced as data; "" when the run has no report, its report does not
// parse as a build report, or it has no notes.
func runWorkingNotes(run *v1alpha1.IntentRun) string {
	if run.Spec.Stage == v1alpha1.IntentStagePlan || run.Status.Report == "" {
		return ""
	}
	b, err := report.ParseBuild([]byte(run.Status.Report))
	if err != nil {
		return ""
	}
	notes, _ := report.Section(b.Body, report.WorkingNotesHeading)
	notes = strings.TrimSpace(visibleFeedback(notes))
	if notes == "" {
		return ""
	}
	// A fence longer than the bound leaves no room for the notes beside it
	// (a long run of backticks): show the backticks as escapes first, so the
	// cut below is the only one and its marker is patchy's.
	if len(fenced(cutBytes(notes, maxWorkingNotesBytes))) > maxWorkingNotesBytes+64 {
		notes = strings.ReplaceAll(notes, "`", "<U+0060>")
	}
	if len(notes) > maxWorkingNotesBytes {
		end := maxWorkingNotesBytes - len(notesTruncated)
		for end > 0 && !utf8.RuneStart(notes[end]) {
			end--
		}
		notes = notes[:end] + notesTruncated
	}
	// The fence's own lines always fit beside the cut notes now, so
	// fencedBounded never cuts or escapes them again.
	return fencedBounded(notes, maxWorkingNotesBytes+64)
}

// workingNotesOf is what a run's working notes are introduced by: the round
// that wrote them.
func workingNotesOf(run *v1alpha1.IntentRun) string {
	switch {
	case run.Spec.Stage == v1alpha1.IntentStageBuild:
		return fmt.Sprintf("the build of plan r%d (attempt %d)", run.Spec.Inputs.PlanRevision, run.Spec.Attempt)
	case run.Spec.Trigger == v1alpha1.IntentRunTriggerChecks:
		return fmt.Sprintf("check-fix round %d (attempt %d)", run.Spec.Round, run.Spec.Attempt)
	default:
		return fmt.Sprintf("revise round %d (attempt %d)", run.Spec.Round, run.Spec.Attempt)
	}
}

// roundWorkingNotes is the working notes a round is handed: those of the
// latest earlier run of its repository and plan revision (the build, an
// earlier round, or an earlier attempt of its own) that wrote any, failed or
// not, introduced by which run it was; "" when none did.
func (p *pass) roundWorkingNotes(run *v1alpha1.IntentRun) string {
	var earlier []*v1alpha1.IntentRun
	for _, older := range p.runs {
		if older.Name != run.Name && older.Spec.Inputs.PlanRevision == run.Spec.Inputs.PlanRevision &&
			sameRepo(older.Spec.Repository.URL, run.Spec.Repository.URL) && buildSideBefore(older, run) {
			earlier = append(earlier, older)
		}
	}
	from, notes := latestWorkingNotes(earlier)
	if notes == "" {
		return ""
	}
	return fmt.Sprintf("Written by %s:\n\n%s", workingNotesOf(from), notes)
}

// buildSideBefore orders a repository's build-side runs: by plan revision,
// then the build before each round by number, then each attempt.
func buildSideBefore(a, b *v1alpha1.IntentRun) bool {
	if a.Spec.Inputs.PlanRevision != b.Spec.Inputs.PlanRevision {
		return a.Spec.Inputs.PlanRevision < b.Spec.Inputs.PlanRevision
	}
	if ra, rb := buildSideRound(a), buildSideRound(b); ra != rb {
		return ra < rb
	}
	return a.Spec.Attempt < b.Spec.Attempt
}

// buildSideRound is a build-side run's place in its plan revision: 0 for
// the build, the round number for a revise or check-fix round.
func buildSideRound(run *v1alpha1.IntentRun) int32 {
	if run.Spec.Stage == v1alpha1.IntentStageBuild {
		return 0
	}
	return run.Spec.Round
}

// latestWorkingNotes is the latest build-side run of runs (buildSideBefore)
// whose report holds working notes, and those notes (runWorkingNotes); nil
// and "" when none does.
func latestWorkingNotes(runs []*v1alpha1.IntentRun) (*v1alpha1.IntentRun, string) {
	var from *v1alpha1.IntentRun
	var notes string
	for _, run := range runs {
		if run.Spec.Stage == v1alpha1.IntentStagePlan || from != nil && !buildSideBefore(from, run) {
			continue
		}
		if n := runWorkingNotes(run); n != "" {
			from, notes = run, n
		}
	}
	return from, notes
}

// lastWorkingNotes is the working notes for a replan's or revival's context
// file: for each repository, those of its latest build-side run that wrote
// any, of whichever plan revision, ordered by repository URL. Each is bounded on
// its own, and a Project lists at most eight repositories, so the whole is
// at most about 50 KiB.
func (p *pass) lastWorkingNotes() string {
	byRepo := map[string][]*v1alpha1.IntentRun{}
	for _, run := range p.runs {
		key := normalizeRepoURL(run.Spec.Repository.URL)
		byRepo[key] = append(byRepo[key], run)
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(byRepo)) {
		from, notes := latestWorkingNotes(byRepo[key])
		if notes == "" {
			continue
		}
		out = append(out, fmt.Sprintf("In %s, written by %s:\n\n%s",
			visibleFeedback(repoSlug(from.Spec.Repository.URL)), workingNotesOf(from), notes))
	}
	return strings.Join(out, "\n\n")
}
