// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/action"
)

const (
	// Prefix starts every command line.
	Prefix = "/patchy"
	// LegacyApprove is the approve comment Finding tracking issues have always
	// accepted, kept as a deprecated alias of "/patchy approve".
	LegacyApprove = "/approve"
	// MaxNoteBytes bounds a command's note, as the approve handler always has.
	MaxNoteBytes = 1024
	// maxVerbBytes bounds the verb word; the longest real verb is far shorter.
	maxVerbBytes = 32
)

// Command is one parsed human command.
type Command struct {
	// Verb is the lower-cased verb, or "" when the command line names none or
	// names something that is not a word (1-32 ASCII letters). It is not
	// checked against any vocabulary: an unknown verb still parses, so the
	// caller can reply with what is available.
	Verb string
	// Note is the sanitised free text after the verb, possibly empty.
	Note string
	// Alias is the legacy form the comment used, such as "/approve", or ""
	// for the /patchy grammar.
	Alias string
}

// Parser parses the comment bodies made on one surface. The zero value
// parses the /patchy grammar alone.
type Parser struct {
	// Surface is where the comments were made. It decides only whether the
	// legacy approve alias is honoured, which it is on FindingIssue and
	// nowhere else: on an intent, "/approve" is text, as it is when another
	// bot's users type it.
	Surface Surface
	// ApproveAlias is the legacy approve comment, the Integration's
	// spec.github.issues.approveComment; empty means LegacyApprove. It is
	// read only on FindingIssue.
	ApproveAlias string
}

// Parse reads body with the zero Parser: the /patchy grammar alone, never
// an alias.
func Parse(body string) (Command, bool) {
	return parseGrammar(body)
}

// Parse reports the command body carries, and false when it carries none.
// The /patchy grammar is tried first, then, on FindingIssue only, the
// legacy approve alias.
func (p Parser) Parse(body string) (Command, bool) {
	if c, ok := parseGrammar(body); ok {
		return c, true
	}
	if p.Surface != FindingIssue {
		return Command{}, false
	}
	return p.parseAlias(body)
}

// parseGrammar reads "/patchy <verb> [note]" from the first non-blank line.
func parseGrammar(body string) (Command, bool) {
	line, below := firstLine(body)
	if len(line) < len(Prefix) || !strings.EqualFold(line[:len(Prefix)], Prefix) {
		return Command{}, false
	}
	rest := line[len(Prefix):]
	// The prefix is a whole word: "/patchyx" and "/patchy-approve" are text.
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
		return Command{}, false
	}
	word, after := cutWord(strings.TrimLeftFunc(rest, unicode.IsSpace))
	return Command{Verb: verb(word), Note: sanitize(after + "\n" + below)}, true
}

// parseAlias matches the legacy approve comment exactly as the Finding
// webhook handler always has: the whole trimmed body equals the alias or
// starts with it and one ASCII space, case-sensitively, and the note is
// everything after the alias.
func (p Parser) parseAlias(body string) (Command, bool) {
	alias := p.ApproveAlias
	if alias == "" {
		alias = LegacyApprove
	}
	trimmed := strings.TrimSpace(body)
	if trimmed != alias && !strings.HasPrefix(trimmed, alias+" ") {
		return Command{}, false
	}
	return Command{
		Verb:  action.VerbApprove,
		Note:  sanitize(strings.TrimPrefix(trimmed, alias)),
		Alias: alias,
	}, true
}

// firstLine returns body's first non-blank line, trimmed, and the text below
// it; both are empty when body has no such line.
func firstLine(body string) (line, below string) {
	for body != "" {
		var l string
		l, body, _ = strings.Cut(body, "\n")
		if l = strings.TrimSpace(l); l != "" {
			return l, body
		}
	}
	return "", ""
}

// cutWord splits s at its first whitespace.
func cutWord(s string) (word, rest string) {
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// verb lower-cases word when it is 1-32 ASCII letters, and returns "" for
// anything else, so a non-empty verb is always safe to echo in a reply.
func verb(word string) string {
	if word == "" || len(word) > maxVerbBytes {
		return ""
	}
	for i := range len(word) {
		if c := word[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return ""
		}
	}
	return strings.ToLower(word)
}

// sanitize makes s a note: valid UTF-8, line breaks normalised to "\n",
// control and format characters other than "\n" and "\t" removed, trimmed,
// and at most MaxNoteBytes, cut on a rune boundary.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return '\n'
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= MaxNoteBytes {
		return s
	}
	cut := MaxNoteBytes
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRightFunc(s[:cut], unicode.IsSpace)
}
