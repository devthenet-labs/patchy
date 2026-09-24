// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command

import (
	"strings"

	"github.com/bitwise-media-group/patchy/internal/action"
)

// Surface is where on GitHub a command was made. Each surface offers its own
// verbs, and a verb shared between surfaces means what that surface's object
// makes of it.
type Surface string

// The surfaces commands arrive on.
const (
	// FindingIssue is a Finding's tracking issue. Its verbs mean exactly what
	// they mean on the status page and in the CLI.
	FindingIssue Surface = "finding-issue"
	// IntentIssue is the issue an intent was written in.
	IntentIssue Surface = "intent-issue"
	// IntentPR is a pull request patchy opened for an intent.
	IntentPR Surface = "intent-pr"
)

// usage is one verb as a surface's help lists it.
type usage struct {
	verb string
	// note says whether the verb uses its note, which the help then shows.
	note    bool
	summary string
}

// surfaces is the design's table of which verbs apply where, in the order the
// help lists them. A Finding issue offers action.ActionVerbs, in that order.
var surfaces = map[Surface][]usage{
	FindingIssue: {
		{action.VerbApprove, true, "release the hold on this finding, or revive it after it was handed off"},
		{action.VerbRetry, false, "retry this finding from the state it failed in"},
		{action.VerbExpedite, false, "skip the accumulation window, the minimum age and the queue"},
		{action.VerbSuspend, false, "pause this finding's progress through the pipeline"},
		{action.VerbResume, false, "resume this finding after a suspend"},
	},
	IntentIssue: {
		{action.VerbApprove, false, "approve the posted plan and start the build"},
		{action.VerbReplan, false, "plan again, taking the comments since the last plan into account"},
		{action.VerbCancel, false, "stop work on this intent and close it; open pull requests are left to you"},
	},
	IntentPR: {
		{action.VerbRevise, true, "start a revision round from your review feedback and the note"},
		{action.VerbRetry, false, "retry the round that failed"},
	},
}

// Available returns the verbs surface offers, in the order its help lists
// them, or nil for an unknown surface. Whether one of them means anything in
// the object's current phase, and whether the commenter may use it, is the
// caller's to decide.
func Available(surface Surface) []string {
	usages := surfaces[surface]
	if usages == nil {
		return nil
	}
	verbs := make([]string, len(usages))
	for i, u := range usages {
		verbs[i] = u.verb
	}
	return verbs
}

// Help is the Markdown reply to a command whose verb surface does not offer:
// every verb it does offer, one per line, or "" for an unknown surface.
func Help(surface Surface) string {
	usages := surfaces[surface]
	if usages == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Commands go on the first line of a comment. Here you can use:\n")
	for _, u := range usages {
		b.WriteString("\n- `" + Prefix + " " + u.verb)
		if u.note {
			b.WriteString(" [note]")
		}
		b.WriteString("`: " + u.summary)
	}
	return b.String()
}
