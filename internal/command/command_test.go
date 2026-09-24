// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command_test

import (
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
)

// parseCase is one comment body and what it must parse to.
type parseCase struct {
	name string
	body string
	want command.Command
	ok   bool
}

func runParseCases(t *testing.T, p command.Parser, cases []parseCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.Parse(tc.body)
			if ok != tc.ok || got != tc.want {
				t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func approve(note string) command.Command {
	return command.Command{Verb: action.VerbApprove, Note: note}
}

func TestParseGrammar(t *testing.T) {
	long := "a" + strings.Repeat("é", 600) // the byte bound lands inside a rune
	runParseCases(t, command.Parser{}, []parseCase{
		{"bare verb", "/patchy approve", approve(""), true},
		{"verb and note", "/patchy approve ship it", approve("ship it"), true},
		{"leading blank lines and surrounding whitespace",
			"  \n\n\t /patchy approve   ship it  \n\n", approve("ship it"), true},
		{"the note continues onto the lines below",
			"/patchy approve ship it\nafter the review\n\nthanks", approve("ship it\nafter the review\n\nthanks"), true},
		{"a note that starts on the next line",
			"/patchy revise\nPlease rename the handler.", command.Command{Verb: action.VerbRevise,
				Note: "Please rename the handler."}, true},
		{"CRLF line endings are normalised",
			"/patchy approve ship it\r\nsecond line\r\n", approve("ship it\nsecond line"), true},
		{"prefix and verb ignore ASCII case", "/PATCHY Approve", approve(""), true},
		{"any whitespace separates prefix and verb", "/patchy\tapprove\u00a0now", approve("now"), true},
		{"Unicode whitespace before the prefix is trimmed", "\u00a0\u3000/patchy approve", approve(""), true},
		{"the prefix alone names no verb", "/patchy", command.Command{}, true},
		{"the prefix and whitespace name no verb", "/patchy  \t\n", command.Command{}, true},
		{"an unknown verb still parses", "/patchy please approve",
			command.Command{Verb: "please", Note: "approve"}, true},
		{"a verb with punctuation attached is no verb", "/patchy approve, ship it",
			command.Command{Note: "ship it"}, true},
		{"markup in the verb is no verb", "/patchy app<b>rove</b>", command.Command{}, true},
		{"a non-ASCII verb is no verb", "/patchy \u0430pprove", command.Command{}, true}, // \u0430 is Cyrillic
		{"an over-long word is no verb", "/patchy " + strings.Repeat("a", 33), command.Command{}, true},
		{"a 32-letter word is a verb", "/patchy " + strings.Repeat("a", 32),
			command.Command{Verb: strings.Repeat("a", 32)}, true},
		{"a /patchy mention in the note is note text", "/patchy approve see /patchy docs",
			approve("see /patchy docs"), true},
		{"only the first line is a command; a later one is note text", "/patchy approve\n/patchy cancel",
			approve("/patchy cancel"), true},
		{"control and format characters are dropped, tabs kept",
			"/patchy revise fix\x00 the\x1b[31m bug\t\u202eplease\u200b\u007f",
			command.Command{Verb: action.VerbRevise, Note: "fix the[31m bug\tplease"}, true},
		{"a lone CR is a line break", "/patchy approve one\rtwo", approve("one\ntwo"), true},
		{"every Unicode line break becomes a newline in the note",
			"/patchy approve a\u0085b\u2028c\u2029d\ve\ff\rg", approve("a\nb\nc\nd\ne\nf\ng"), true},
		{"trailing whitespace on the command line stays inside the note",
			"/patchy approve one  \ntwo", approve("one  \ntwo"), true},
		// A line break ends the command line wherever it is, so these read as
		// "/patchy" and, on the next line, a note — as GitHub renders them.
		{"a lone CR ends the command line", "/patchy\r approve", command.Command{Note: "approve"}, true},
		{"a lone CR right after the prefix", "/patchy\rapprove", command.Command{Note: "approve"}, true},
		{"a NEL ends the command line", "/patchy\u0085approve", command.Command{Note: "approve"}, true},
		{"a line separator ends the command line", "/patchy\u2028approve", command.Command{Note: "approve"}, true},
		{"a paragraph separator ends the command line", "/patchy\u2029approve", command.Command{Note: "approve"}, true},
		{"a vertical tab ends the command line", "/patchy\vapprove", command.Command{Note: "approve"}, true},
		{"a form feed ends the command line", "/patchy\fapprove", command.Command{Note: "approve"}, true},
		{"blank lines ended by lone CRs", "\r \r/patchy approve", approve(""), true},
		{"invalid UTF-8 is replaced", "/patchy approve bad \xff byte", approve("bad \uFFFD byte"), true},
		{"a long note is cut on a rune boundary", "/patchy approve " + long,
			approve(long[:command.MaxNoteBytes-1]), true},
		{"whitespace left at the cut is trimmed",
			"/patchy approve " + strings.Repeat("x", command.MaxNoteBytes-1) + " y",
			approve(strings.Repeat("x", command.MaxNoteBytes-1)), true},

		{"empty body", "", command.Command{}, false},
		{"blank body", "  \n\t\r\n", command.Command{}, false},
		{"ordinary comment", "looks fine to me", command.Command{}, false},
		{"prefix glued to a word", "/patchyapprove", command.Command{}, false},
		{"prefix glued to punctuation", "/patchy-approve", command.Command{}, false},
		{"no slash", "patchy approve", command.Command{}, false},
		{"a command below the first line", "Thanks!\n/patchy approve", command.Command{}, false},
		{"a command after a lone CR is below the first line", "Thanks!\r/patchy approve", command.Command{}, false},
		{"a command after a line separator is below the first line", "Thanks!\u2028/patchy approve",
			command.Command{}, false},
		{"a quoted command", "> /patchy approve\n\nI disagree", command.Command{}, false},
		{"a command in a code span", "`/patchy approve`", command.Command{}, false},
		{"a command in a code block", "```\n/patchy approve\n```", command.Command{}, false},
		{"a zero-width space before the prefix", "\u200b/patchy approve", command.Command{}, false},
	})
}

// TestParseLegacyApprove pins the legacy alias to what the Finding webhook
// handler accepts today (internal/controller/integration/webhooks.go): the
// first block is TestSignalsApprove's bodies, the rest the edges of its
// matching rule.
func TestParseLegacyApprove(t *testing.T) {
	legacy := func(note string) command.Command {
		return command.Command{Verb: action.VerbApprove, Note: note, Alias: command.LegacyApprove}
	}
	runParseCases(t, command.Parser{Surface: command.FindingIssue}, []parseCase{
		// From TestSignalsApprove.
		{"collaborator approves", "/approve", legacy(""), true},
		{"owner approves with note", "/approve ship it", legacy("ship it"), true},
		{"non-command ignored", "looks fine to me", command.Command{}, false},
		{"prefix-only word ignored", "/approved", command.Command{}, false},

		{"the whole comment is trimmed", "\n\n  /approve  \n", legacy(""), true},
		{"the note keeps the lines below", "/approve ship it\nafter the review\n", legacy("ship it\nafter the review"), true},
		{"extra spaces before the note", "/approve   ship it", legacy("ship it"), true},
		{"CRLF in the note is normalised", "/approve ship it\r\nthanks", legacy("ship it\nthanks"), true},
		{"every line break in the note is normalised", "/approve ship\u0085it\u2028now", legacy("ship\nit\nnow"), true},
		// Today's rule needs an ASCII space after the command, so these have
		// never approved and must not start to.
		{"a note on the next line", "/approve\nship it", command.Command{}, false},
		{"a tab after the command", "/approve\tship it", command.Command{}, false},
		{"a no-break space after the command", "/approve\u00a0ship it", command.Command{}, false},
		{"case-sensitive", "/Approve", command.Command{}, false},
		{"text before the command", "Thanks\n/approve", command.Command{}, false},
		{"quoted", "> /approve", command.Command{}, false},
	})
}

func TestParseConfiguredAlias(t *testing.T) {
	const alias = "@patchy approve" // the dev overlay's approveComment
	legacy := func(note string) command.Command {
		return command.Command{Verb: action.VerbApprove, Note: note, Alias: alias}
	}
	runParseCases(t, command.Parser{Surface: command.FindingIssue, ApproveAlias: alias}, []parseCase{
		{"the configured alias approves", "@patchy approve", legacy(""), true},
		{"with a note", "@patchy approve lgtm", legacy("lgtm"), true},
		{"it replaces the default, as today", "/approve", command.Command{}, false},
		{"the grammar still parses", "/patchy retry", command.Command{Verb: action.VerbRetry}, true},
	})

	// A configured alias that starts with the /patchy prefix is shadowed by
	// the grammar: its verb wins, rather than every /patchy command becoming
	// an approval.
	runParseCases(t, command.Parser{Surface: command.FindingIssue, ApproveAlias: "/patchy"}, []parseCase{
		{"shadowed alias", "/patchy retry", command.Command{Verb: action.VerbRetry}, true},
	})
}

// TestParseAliasOnlyOnFindingIssue: the legacy approve comment is the
// Finding tracking issue's alone. Anywhere else, including with an alias
// configured, it is text: it neither approves an intent's plan nor draws a
// help reply on an intent's pull request, where "/approve" is as likely to
// be meant for another bot.
func TestParseAliasOnlyOnFindingIssue(t *testing.T) {
	bodies := []string{"/approve", "/approve ship it", "@patchy approve", "@patchy approve lgtm"}
	for _, p := range []command.Parser{
		{},
		{ApproveAlias: "@patchy approve"},
		{Surface: command.IntentIssue},
		{Surface: command.IntentIssue, ApproveAlias: "@patchy approve"},
		{Surface: command.IntentPR},
		{Surface: command.IntentPR, ApproveAlias: "@patchy approve"},
		{Surface: "pull-request"},
	} {
		for _, body := range bodies {
			if c, ok := p.Parse(body); ok {
				t.Errorf("%+v.Parse(%q) = %+v, true; want no command", p, body, c)
			}
		}
		// The grammar is the same on every surface.
		if c, ok := p.Parse("/patchy approve ship it"); !ok || c != approve("ship it") {
			t.Errorf("%+v.Parse(/patchy approve ship it) = %+v, %v", p, c, ok)
		}
	}
	for _, body := range bodies {
		if c, ok := command.Parse(body); ok {
			t.Errorf("Parse(%q) = %+v, true; want no command", body, c)
		}
	}
	finding := command.Parser{Surface: command.FindingIssue}
	if c, ok := finding.Parse("/approve"); !ok || c.Alias != command.LegacyApprove {
		t.Errorf("FindingIssue Parse(/approve) = %+v, %v; want the legacy alias", c, ok)
	}
}
