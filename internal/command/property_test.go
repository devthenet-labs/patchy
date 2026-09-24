// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command_test

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
	"unicode/utf8"

	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/command"
)

// Every property runs seeded, so the gate is deterministic. A failure prints
// the shrunk-by-hand input: pin it as an example in command_test.go.

// hostile are fragments dense in what could confuse the parser: the prefix
// and the alias in several spellings, verbs, every kind of line break and
// whitespace, control and format characters, quote and code markers,
// multi-byte runes and invalid UTF-8.
var hostile = []string{
	"/patchy", "/PATCHY", "/Patchy", "/patchy ", "/approve", "/approve ", "/approved", "@patchy approve",
	"approve", "retry", "revise", "cancel", "replan", "Approve",
	" ", "  ", "\t", "\n", "\n\n", "\r\n", "\r", "\v", "\f", "\u0085", "\u00a0", "\u2028", "\u2029", "\u3000",
	"\x00", "\x1b", "\x7f", "\u200b", "\u200c", "\u200d", "\u00ad", "\u202e", "\u2066", "\ufeff",
	"\ufe0f", "\ufe0e", "\U000e0041", "\U000e0100", "\u2764", "\U0001f468",
	">", "`", "```", "#", "-", ",", "<b>", "x", "é", "日本", "\xff", "\xe2\x80", "ship it",
}

// whitespace are White_Space runes, some of them control characters too.
var whitespace = []string{" ", "\t", "\n", "\r\n", "\r", "\v", "\f", "\u0085", "\u00a0", "\u2028", "\u2029", "\u3000"}

// genFrom concatenates fragments from alphabet: mostly short, occasionally
// well past the note bound so the cut is exercised.
func genFrom(r *rand.Rand, alphabet []string) string {
	n := r.Intn(24)
	if r.Intn(12) == 0 {
		n = command.MaxNoteBytes/4 + r.Intn(command.MaxNoteBytes)
	}
	var b strings.Builder
	for range n {
		b.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	return b.String()
}

// quickConfig draws each argument of the property with its own generator.
func quickConfig(seed int64, gens ...func(*rand.Rand) string) *quick.Config {
	return &quick.Config{
		MaxCount: 3000,
		Rand:     rand.New(rand.NewSource(seed)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			for i, gen := range gens {
				args[i] = reflect.ValueOf(gen(r))
			}
		},
	}
}

func genHostile(r *rand.Rand) string { return genFrom(r, hostile) }

func genWhitespace(r *rand.Rand) string {
	var b strings.Builder
	for range 1 + r.Intn(6) {
		b.WriteString(whitespace[r.Intn(len(whitespace))])
	}
	return b.String()
}

// genSurface is a surface the parser knows, or one it does not.
func genSurface(r *rand.Rand) string {
	surfaces := []command.Surface{command.FindingIssue, command.IntentIssue, command.IntentPR, "", "pull-request"}
	return string(surfaces[r.Intn(len(surfaces))])
}

// genAlias is a configured approve comment: empty (the default), the dev
// overlay's, or hostile text.
func genAlias(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return ""
	case 1:
		return "@patchy approve"
	case 2:
		return command.LegacyApprove
	}
	return genHostile(r)
}

// lineBreak matches every line break Unicode defines (UAX #14's mandatory
// breaks), written out here independently of the parser.
var lineBreak = regexp.MustCompile("\r\n|[\n\r\v\f\u0085\u2028\u2029]")

// firstNonBlank is the body's first non-blank line, untrimmed — computed
// here independently of the parser.
func firstNonBlank(body string) string {
	for _, line := range lineBreak.Split(body, -1) {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// indentColumns is how many columns of spaces and tabs open line, a tab
// advancing to the next multiple of four as CommonMark counts it.
func indentColumns(line string) int {
	col := 0
	for _, r := range line {
		switch r {
		case ' ':
			col++
		case '\t':
			col = (col/4 + 1) * 4
		default:
			return col
		}
	}
	return col
}

// wellFormedNote reports what every note must be: valid UTF-8, within the
// bound, trimmed, with "\n" its only line break and no other control
// character but "\t", none of the characters that reorder or hide text
// (bidi embeddings, overrides and isolates, tag characters, U+FEFF), and a
// variation selector only straight after a visible character that is not
// one.
func wellFormedNote(note string) bool {
	if !utf8.ValidString(note) || len(note) > command.MaxNoteBytes || note != strings.TrimSpace(note) {
		return false
	}
	prev := rune(-1)
	for _, r := range note {
		switch {
		case r == '\n' || r == '\t':
		case unicode.IsControl(r), r == '\u2028', r == '\u2029', r == '\ufeff',
			'\u202a' <= r && r <= '\u202e', '\u2066' <= r && r <= '\u2069', '\U000e0000' <= r && r <= '\U000e007f':
			return false
		case unicode.Is(unicode.Variation_Selector, r):
			if prev < 0 || !unicode.IsGraphic(prev) || unicode.IsSpace(prev) ||
				unicode.Is(unicode.Variation_Selector, prev) {
				return false
			}
		}
		prev = r
	}
	return true
}

// wellFormedVerb reports what every verb must be: empty, or 1-32 lower-case
// ASCII letters.
func wellFormedVerb(verb string) bool {
	if len(verb) > 32 {
		return false
	}
	for i := range len(verb) {
		if verb[i] < 'a' || verb[i] > 'z' {
			return false
		}
	}
	return true
}

// TestParseNeverPanicsProperty: whatever the surface, the body and the
// configured alias, Parse returns, a miss is the zero Command, and a hit is
// well formed.
func TestParseNeverPanicsProperty(t *testing.T) {
	total := func(surface, alias, body string) bool {
		c, ok := command.Parser{Surface: command.Surface(surface), ApproveAlias: alias}.Parse(body)
		if !ok {
			return c == command.Command{}
		}
		return wellFormedVerb(c.Verb) && wellFormedNote(c.Note)
	}
	if err := quick.Check(total, quickConfig(20260923, genSurface, genAlias, genHostile)); err != nil {
		t.Error(err)
	}
	// testing/quick's own arbitrary strings, too.
	arbitrary := func(body string) bool { return total(string(command.FindingIssue), "", body) }
	cfg := &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20260924))}
	if err := quick.Check(arbitrary, cfg); err != nil {
		t.Error(err)
	}
}

// TestParseRequiresPrefixProperty: a body parses only when its first
// non-blank line is not indented as code and starts with the prefix or, on a
// Finding issue and nowhere else, the whole trimmed body is the legacy alias
// form — and text whose first non-blank line starts with anything else, or
// is indented as code, never parses as the grammar, whatever follows it.
func TestParseRequiresPrefixProperty(t *testing.T) {
	onlyWithPrefix := func(surface, body string) bool {
		c, ok := command.Parser{Surface: command.Surface(surface)}.Parse(body)
		if !ok {
			return true
		}
		if c.Alias != "" {
			trimmed := strings.TrimSpace(body)
			return command.Surface(surface) == command.FindingIssue &&
				(trimmed == command.LegacyApprove || strings.HasPrefix(trimmed, command.LegacyApprove+" "))
		}
		raw := firstNonBlank(body)
		line := strings.TrimSpace(raw)
		return indentColumns(raw) < 4 &&
			len(line) >= len(command.Prefix) && strings.EqualFold(line[:len(command.Prefix)], command.Prefix)
	}
	if err := quick.Check(onlyWithPrefix, quickConfig(20260925, genSurface, genHostile)); err != nil {
		t.Error(err)
	}

	// A first line that opens with anything but "/" — a word, a quote, a code
	// marker, a zero-width character — never parses, however many commands
	// follow it.
	text := func(blank, start, rest string) bool {
		_, ok := command.Parse(blank + start + rest)
		return !ok
	}
	if err := quick.Check(text, quickConfig(20260926, genBlankLines, genTextStart, genHostile)); err != nil {
		t.Error(err)
	}

	// Nor does a command indented as a code block, however it is spelled.
	indented := func(blank, indent, head, rest string) bool {
		_, ok := command.Parse(blank + indent + head + rest)
		return !ok
	}
	cfg := quickConfig(20261005, genBlankLines, genCodeIndent, genCommandLine, genHostile)
	if err := quick.Check(indented, cfg); err != nil {
		t.Error(err)
	}
}

// genCodeIndent is spaces and tabs reaching at least the four columns that
// make an indented code block.
func genCodeIndent(r *rand.Rand) string {
	var b strings.Builder
	for indentColumns(b.String()) < 4 || r.Intn(3) == 0 {
		b.WriteString([]string{" ", "\t"}[r.Intn(2)])
	}
	return b.String()
}

// genCommandLine is the prefix in random ASCII case, inline whitespace and a
// verb.
func genCommandLine(r *rand.Rand) string {
	return anyCasePrefix(r) + inline[r.Intn(len(inline))] + genVerb(r)
}

// anyCasePrefix is the prefix in random ASCII case.
func anyCasePrefix(r *rand.Rand) string {
	var b strings.Builder
	for _, c := range command.Prefix {
		if r.Intn(2) == 0 {
			c = unicode.ToUpper(c)
		}
		b.WriteRune(c)
	}
	return b.String()
}

// genBlankLines is up to three blank lines.
func genBlankLines(r *rand.Rand) string {
	return strings.Repeat(" \t\r\n", r.Intn(4))
}

// genTextStart opens a line with something other than "/" or whitespace.
func genTextStart(r *rand.Rand) string {
	starts := []string{"x", "patchy", "Thanks", ">", "`", "```", "#", "-", "é", "\u200b", "\ufeff", "@patchy approve"}
	return starts[r.Intn(len(starts))]
}

// noteText is hostile text that cannot spell any part of a command line: no
// "/" and no letter of any verb genVerb draws, in either case. So the only
// "/patchy", "/approve" or verb in a generated command is its command line,
// and one found in the note was leaked there by the parser.
func noteText(r *rand.Rand) string {
	var clean []string
	for _, f := range hostile {
		if !strings.ContainsAny(strings.ToLower(f), "/"+strings.Join(verbs, "")) {
			clean = append(clean, f)
		}
	}
	return genFrom(r, clean)
}

// spelling is the prefix in random ASCII case, after random blank lines and
// leading whitespace, followed by whitespace that does not end the line.
func spelling(r *rand.Rand) string {
	var b strings.Builder
	b.WriteString(genBlankLines(r))
	b.WriteString(strings.Repeat(" ", r.Intn(4)))
	b.WriteString(anyCasePrefix(r))
	for range 1 + r.Intn(3) {
		b.WriteString(inline[r.Intn(len(inline))])
	}
	return b.String()
}

// inline are whitespace runes that do not end a line.
var inline = []string{" ", "\t", "\u00a0", "\u2003", "\u202f", "\u3000"}

// verbs are the real verbs and an unknown one.
var verbs = []string{
	action.VerbApprove, action.VerbRetry, action.VerbRevise, action.VerbCancel, action.VerbReplan, "please",
}

// genVerb is a real verb or an unknown one.
func genVerb(r *rand.Rand) string { return verbs[r.Intn(len(verbs))] }

// TestParseNoteExcludesCommandLineProperty: the note never contains the
// command line. It holds neither the prefix, the alias nor the verb — the
// note text can spell none of them, so any found there leaked from the
// command line — and it is exactly the note rule applied to the text after
// the verb, however the command line is spelled.
func TestParseNoteExcludesCommandLineProperty(t *testing.T) {
	excluded := func(head, verb, note string) bool {
		c, ok := command.Parse(head + verb + " " + note)
		if !ok || c.Verb != verb || c.Alias != "" {
			return false
		}
		lower := strings.ToLower(c.Note)
		return !strings.Contains(lower, command.Prefix) && !strings.Contains(lower, verb) &&
			c.Note == command.Note(note)
	}
	if err := quick.Check(excluded, quickConfig(20260927, spelling, genVerb, noteText)); err != nil {
		t.Error(err)
	}

	legacy := func(note string) bool {
		c, ok := command.Parser{Surface: command.FindingIssue}.Parse(command.LegacyApprove + " " + note)
		return ok && c.Verb == action.VerbApprove && c.Alias == command.LegacyApprove &&
			!strings.Contains(strings.ToLower(c.Note), action.VerbApprove) && c.Note == command.Note(note)
	}
	if err := quick.Check(legacy, quickConfig(20260928, noteText)); err != nil {
		t.Error(err)
	}
}

// TestParseLineBreaksProperty: every line break is one to the command line
// and the note alike. One straight after the prefix leaves the verb empty,
// whatever follows it; and writing one line break in place of another never
// changes what a comment parses to.
func TestParseLineBreaksProperty(t *testing.T) {
	breaks := []string{"\n", "\r\n", "\r", "\v", "\f", "\u0085", "\u2028", "\u2029"}
	genBreak := func(r *rand.Rand) string { return breaks[r.Intn(len(breaks))] }
	verbless := func(lb, rest string) bool {
		c, ok := command.Parse(command.Prefix + lb + rest)
		return ok && c.Verb == ""
	}
	if err := quick.Check(verbless, quickConfig(20261003, genBreak, genHostile)); err != nil {
		t.Error(err)
	}

	// Without CRs of their own, the text before the break cannot end in the
	// CR of a CRLF, nor the text after it start one.
	var noCR []string
	for _, f := range hostile {
		if !strings.Contains(f, "\r") {
			noCR = append(noCR, f)
		}
	}
	genSide := func(r *rand.Rand) string {
		if r.Intn(2) == 0 {
			return command.Prefix + " " + genVerb(r) + " " + genFrom(r, noCR)
		}
		return genFrom(r, noCR)
	}
	oneBreak := func(before, lb, after string) bool {
		c, ok := command.Parse(before + lb + after)
		if lb == "\r" && strings.HasPrefix(after, "\n") {
			after = after[1:] // the CR and the LF after it are one CRLF
		}
		c2, ok2 := command.Parse(before + "\n" + after)
		return c == c2 && ok == ok2
	}
	if err := quick.Check(oneBreak, quickConfig(20261004, genSide, genBreak, genSide)); err != nil {
		t.Error(err)
	}
}

// TestParseTrailingWhitespaceProperty: appending whitespace — spaces, tabs,
// any line break, Unicode spaces — never changes the result.
func TestParseTrailingWhitespaceProperty(t *testing.T) {
	stable := func(surface, alias, body, ws string) bool {
		p := command.Parser{Surface: command.Surface(surface), ApproveAlias: alias}
		c, ok := p.Parse(body)
		c2, ok2 := p.Parse(body + ws)
		return c == c2 && ok == ok2
	}
	if err := quick.Check(stable, quickConfig(20260929, genSurface, genAlias, genHostile, genWhitespace)); err != nil {
		t.Error(err)
	}
}

// TestParseNoteBoundProperty: however long the body, the note is at most
// MaxNoteBytes and valid UTF-8 — the cut never splits a rune.
func TestParseNoteBoundProperty(t *testing.T) {
	long := []string{"é", "日本", "𝄞", "x", " ", "\n", "\x00", "\xff", "\u202e"}
	bounded := func(head, fill string) bool {
		c, ok := command.Parser{Surface: command.FindingIssue}.Parse(head + " approve " + fill)
		return ok && len(c.Note) <= command.MaxNoteBytes && utf8.ValidString(c.Note)
	}
	fill := func(r *rand.Rand) string {
		var b strings.Builder
		for n := command.MaxNoteBytes - 8 + r.Intn(3*command.MaxNoteBytes); b.Len() < n; {
			b.WriteString(long[r.Intn(len(long))])
		}
		return b.String()
	}
	heads := func(r *rand.Rand) string {
		if r.Intn(2) == 0 {
			return command.LegacyApprove + " x" // the alias shares the bound
		}
		return command.Prefix
	}
	if err := quick.Check(bounded, quickConfig(20260930, heads, fill)); err != nil {
		t.Error(err)
	}
}

// approveToday is the Finding webhook handler's approve match and note as
// they stand when this package is introduced, copied from
// internal/controller/integration: the comment handler in webhooks.go and
// truncate in ingest.go. It is the oracle the legacy alias must not drift
// from.
func approveToday(body, cmd string) (note string, ok bool) {
	body = strings.TrimSpace(body)
	if body != cmd && !strings.HasPrefix(body, cmd+" ") {
		return "", false
	}
	note = strings.TrimSpace(strings.TrimPrefix(body, cmd))
	if len(note) <= 1024 {
		return note, true
	}
	cut := note[:1024]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}

// withoutPrefix keeps the fragments that cannot put the /patchy prefix in a
// body, so every hit is the alias and never the grammar.
func withoutPrefix(fragments []string) []string {
	var out []string
	for _, f := range fragments {
		if !strings.Contains(strings.ToLower(f), command.Prefix) {
			out = append(out, f)
		}
	}
	return out
}

// TestParseLegacyMatchesTodayProperty: on the legacy alias, Parse approves
// exactly the comments today's handler approves, whatever they hold; and on
// ordinary text, format characters such as ZWJ and ZWNJ included, it records
// the same note, except for whitespace the 1 KiB cut leaves at the end,
// which the note rule trims.
func TestParseLegacyMatchesTodayProperty(t *testing.T) {
	aliases := []string{command.LegacyApprove, "@patchy approve"}
	genConfigured := func(r *rand.Rand) string { return aliases[r.Intn(len(aliases))] }
	// Lead with an alias half the time, so hits are common.
	leadWith := func(r *rand.Rand, sep string) string {
		if r.Intn(2) == 0 {
			return aliases[r.Intn(len(aliases))] + sep
		}
		return ""
	}

	noPrefix := withoutPrefix(hostile)
	genBody := func(r *rand.Rand) string { return leadWith(r, "") + genFrom(r, noPrefix) }
	sameDecision := func(alias, body string) bool {
		c, ok := command.Parser{Surface: command.FindingIssue, ApproveAlias: alias}.Parse(body)
		_, want := approveToday(body, alias)
		if ok != want {
			return false
		}
		return !ok || (c.Verb == action.VerbApprove && c.Alias == alias)
	}
	if err := quick.Check(sameDecision, quickConfig(20261001, genConfigured, genBody)); err != nil {
		t.Error(err)
	}

	clean := append([]string{
		"/approve", "/approve ", "/approved", "@patchy approve", "@patchy approve ", "approve",
	}, ordinaryText...)
	genClean := func(r *rand.Rand) string { return leadWith(r, " ") + genFrom(r, clean) }
	sameNote := func(alias, body string) bool {
		c, ok := command.Parser{Surface: command.FindingIssue, ApproveAlias: alias}.Parse(body)
		note, want := approveToday(body, alias)
		return ok == want && c.Note == strings.TrimRightFunc(note, unicode.IsSpace)
	}
	if err := quick.Check(sameNote, quickConfig(20261002, genConfigured, genClean)); err != nil {
		t.Error(err)
	}
}

// ordinaryText are fragments of text people write, format characters
// included, with nothing the note rule removes: each keeps its variation
// selectors straight after the character they select.
var ordinaryText = []string{
	" ", "  ", "\t", "\n", "\n\n", "x", "é", "日本", "ship it", ">", "`", "#",
	"\U0001f468\u200d\U0001f469\u200d\U0001f467", // a family, joined by ZWJ
	"\U0001f3f3\ufe0f\u200d\U0001f308",           // a rainbow flag: selector, then ZWJ
	"❤\ufe0f", "1\ufe0f⃣", "葛\U000e0100",         // emoji and ideographic variants, a keycap
	"می\u200cخواهم",                                       // Persian, with a ZWNJ
	"co\u00adoperate", "a\u200eb\u200f", "@\u200bsomeone", // a soft hyphen, bidi marks, a broken mention
}

// TestNoteProperty: the note rule is idempotent — an event alias's caller or
// a later reader can apply it again without changing a note — and whatever
// it is given, it returns a well-formed note. Ordinary text, format
// characters and all, comes through untouched but for the trim.
func TestNoteProperty(t *testing.T) {
	idempotent := func(s string) bool {
		n := command.Note(s)
		return command.Note(n) == n && wellFormedNote(n)
	}
	if err := quick.Check(idempotent, quickConfig(20261006, genHostile)); err != nil {
		t.Error(err)
	}
	cfg := &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261007))}
	if err := quick.Check(idempotent, cfg); err != nil {
		t.Error(err)
	}

	// Short enough that the bound never cuts it.
	genOrdinary := func(r *rand.Rand) string {
		var b strings.Builder
		for range r.Intn(24) {
			b.WriteString(ordinaryText[r.Intn(len(ordinaryText))])
		}
		return b.String()
	}
	untouched := func(s string) bool { return command.Note(s) == strings.TrimSpace(s) }
	if err := quick.Check(untouched, quickConfig(20261008, genOrdinary)); err != nil {
		t.Error(err)
	}
}
