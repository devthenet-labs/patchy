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
	// Note is the free text after the verb, as Note makes it; possibly empty.
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

// lineBreaks turns every line break Unicode says must end a line (UAX #14:
// CRLF, CR, LF, VT, FF, NEL, U+2028 and U+2029) into "\n". The grammar and
// the note both use it, so the command line ends exactly where the note's
// first line break does.
var lineBreaks = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
)

// parseGrammar reads "/patchy <verb> [note]" from the first non-blank line.
// Code indentation is decided twice, and either decision makes the line
// text: on that line, and on the Markdown line holding it, which GitHub ends
// only at CRLF, CR and LF. So "    \f/patchy approve", which GitHub renders
// as one indented code block, is text although the form feed starts the
// command on a line of its own.
func parseGrammar(body string) (Command, bool) {
	if indentedCode(markdownLine(body)) {
		return Command{}, false
	}
	line, below := firstLine(lineBreaks.Replace(body))
	if indentedCode(line) {
		return Command{}, false
	}
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	if len(line) < len(Prefix) || !strings.EqualFold(line[:len(Prefix)], Prefix) {
		return Command{}, false
	}
	rest := line[len(Prefix):]
	// The prefix is a whole word: "/patchyx" and "/patchy-approve" are text.
	if r, _ := utf8.DecodeRuneInString(rest); rest != "" && !unicode.IsSpace(r) {
		return Command{}, false
	}
	word, after := cutWord(strings.TrimLeftFunc(rest, unicode.IsSpace))
	return Command{Verb: verb(word), Note: Note(after + "\n" + below)}, true
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
		Note:  Note(strings.TrimPrefix(trimmed, alias)),
		Alias: alias,
	}, true
}

// firstLine returns body's first non-blank "\n"-terminated line, as it
// stands, and the text below it; both are empty when body has no such line.
func firstLine(body string) (line, below string) {
	for body != "" {
		var l string
		l, body, _ = strings.Cut(body, "\n")
		if strings.TrimSpace(l) != "" {
			return l, body
		}
	}
	return "", ""
}

// markdownLine returns body's first non-blank line as CommonMark, and so
// GitHub, splits it: at CRLF, CR and LF, and at no other line break. A CRLF
// splits here as a CR and an empty line, which is blank and so skipped.
func markdownLine(body string) string {
	for body != "" {
		l := body
		if i := strings.IndexAny(body, "\r\n"); i >= 0 {
			l, body = body[:i], body[i+1:]
		} else {
			body = ""
		}
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// indentedCode reports whether line opens with four or more columns of
// spaces and tabs, which GitHub renders as an indented code block when, as
// here, no paragraph comes before it. A tab advances to the next multiple of
// four columns, as CommonMark counts it; any other character ends the
// indentation.
func indentedCode(line string) bool {
	col := 0
	for i := 0; i < len(line) && col < 4; i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return false
		}
	}
	return col >= 4
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

// Note makes s a command's note, by the one rule every note follows however
// the command arrived: the text after a comment's verb, after the legacy
// approve alias, or the body of an event alias such as a "Request changes"
// review. The result is valid UTF-8 with every line break normalised to
// "\n"; control characters other than "\n" and "\t" removed, as are the
// characters that make text read differently from what it holds (see
// hides, and variation selectors beyond one per character); trimmed; and at
// most MaxNoteBytes, cut on a rune boundary. Every other character, format
// characters included, is kept. Note is idempotent.
func Note(s string) string {
	s = lineBreaks.Replace(strings.ToValidUTF8(s, string(utf8.RuneError)))
	var b strings.Builder
	b.Grow(len(s))
	prev := '\n' // no character to select a variation of
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
		case unicode.IsControl(r) || hides(r):
			continue
		case unicode.Is(unicode.Variation_Selector, r) && !selectable(prev):
			continue
		}
		b.WriteRune(r)
		prev = r
	}
	s = strings.TrimSpace(b.String())
	if len(s) <= MaxNoteBytes {
		return s
	}
	cut := MaxNoteBytes
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRightFunc(s[:cut], unicode.IsSpace)
}

// hides reports whether r is a format character that makes text read
// differently from what it holds: a bidi embedding, override or isolate,
// which reorders the text around it (the Trojan Source characters); a tag
// character, which spells ASCII that renders as nothing but that a model
// reads; or U+FEFF, a byte-order mark with no business inside text. Other
// format characters are ordinary text: ZWJ builds emoji sequences, ZWNJ
// spells Persian and Indic words, and soft hyphens, bidi marks and
// zero-width spaces are all written on purpose.
func hides(r rune) bool {
	switch {
	case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
		return true
	case r >= '\U000e0000' && r <= '\U000e007f':
		return true
	}
	return r == '\ufeff'
}

// selectable reports whether a variation selector may follow r in a note:
// only straight after a visible character that is not itself one. A
// variation sequence is always one character and one selector, so this
// keeps every real one (an emoji's presentation, an ideograph's variant)
// while a run of selectors, which renders as nothing and can carry a byte
// each, keeps only its first.
func selectable(r rune) bool {
	return unicode.IsGraphic(r) && !unicode.IsSpace(r) && !unicode.Is(unicode.Variation_Selector, r)
}
