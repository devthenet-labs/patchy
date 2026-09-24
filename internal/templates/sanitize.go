// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Sanitize makes agent-authored markdown safe to post to GitHub. Every piece
// of agent text that reaches an issue, a comment or a pull request passes
// through it, or through SanitizeInline; patchy's own markers and headings
// are added around the result, never inside it, so the controller's marker
// is the only HTML comment a posted body carries.
//
// Two things are at stake. What the approving human reads must be what the
// agent reads next: an HTML comment, a <details> block, a link reference
// definition ("[//]: # (...)"), a link title or image alt text, a code
// fence's info string and a math expression can all carry text a reader
// never sees, so each is shown literally instead. And agent text must not
// act on GitHub: an issue reference or a closing keyword ("fixes #3",
// "closes owner/repo#3", a full issue URL) links, and in a pull request
// merged to the default branch closes, an issue — possibly a Finding's
// tracking issue in the same repository, which would silently take the
// finding out of automated remediation — and an @mention notifies whoever
// it names. Each is rendered as inline code, where GitHub neither links nor
// notifies, so no closing keyword is ever followed by a live reference.
//
// Concretely, the text is first made plain (invalid UTF-8 replaced, line
// breaks normalised, control and format characters such as bidi overrides
// and zero-width characters dropped, tab and newline kept). Then:
//
//   - a fenced code block at the top level keeps its content but gets a
//     fence patchy chose, which nothing in the block can close; its info
//     string survives only when it is one of a few plain language names,
//     otherwise the whole block, fence lines included, is shown inside a
//     plain text block;
//   - an inline code span is kept when it lies on one line and holds no
//     "|" (which would split it inside a table row), re-delimited so nothing
//     in it can close it early;
//   - in everything else, every "<", "[", "$" and lone backslash is
//     backslash-escaped, as is every backtick that does not delimit a kept
//     code span and every run of three or more tildes (a fence nobody
//     chose), so no raw HTML, link, image, math or unexpected code block can
//     form; and mentions, issue references (#N, GH-N, owner/repo#N, issue
//     and pull request URLs) and character references (&#64;) become inline
//     code.
//
// Everything else (headings, lists, emphasis, tables, block quotes) renders
// as written. The recognition of code is deliberately conservative: code
// patchy does not recognise is treated as prose and neutralised, which can
// only make a block look busier, never let text through. Sanitize is
// idempotent, and never fails.
func Sanitize(s string) string {
	lines := strings.Split(plainText(s), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		f, ok := openingFence(lines[i])
		if !ok {
			out = append(out, sanitizeLine(lines[i]))
			i++
			continue
		}
		block, next := fencedBlock(lines, i, f)
		out = append(out, block...)
		i = next
	}
	return strings.Join(out, "\n")
}

// SanitizeInline is Sanitize for text rendered within one line — a summary,
// a list item, a question: line breaks become spaces, surrounding whitespace
// is trimmed, and no code block is recognised, so the result is always a
// single line. It is idempotent.
func SanitizeInline(s string) string {
	return sanitizeLine(strings.TrimSpace(strings.ReplaceAll(plainText(s), "\n", " ")))
}

// fenceOpen is a fenced code block's opening line.
type fenceOpen struct {
	char byte // '`' or '~'
	n    int  // fence length
	info string
}

// openingFence reports whether line opens a fenced code block at the top
// level (CommonMark: at most three spaces of indentation, three or more
// backticks or tildes, and for backticks an info string free of them).
func openingFence(line string) (fenceOpen, bool) {
	rest := strings.TrimLeft(line, " ")
	if len(line)-len(rest) > 3 || rest == "" || (rest[0] != '`' && rest[0] != '~') {
		return fenceOpen{}, false
	}
	c := rest[0]
	n := runLen(rest, c)
	info := rest[n:]
	if n < 3 || (c == '`' && strings.ContainsRune(info, '`')) {
		return fenceOpen{}, false
	}
	return fenceOpen{char: c, n: n, info: strings.Trim(info, " \t")}, true
}

// closesFence reports whether line closes the block f opened: at most three
// spaces of indentation, at least as many of the same character, and
// nothing after them but spaces or tabs.
func closesFence(line string, f fenceOpen) bool {
	rest := strings.TrimLeft(line, " ")
	if len(line)-len(rest) > 3 {
		return false
	}
	n := runLen(rest, f.char)
	return n >= f.n && strings.Trim(rest[n:], " \t") == ""
}

// fencedBlock renders the block lines[i] opens and returns the index of the
// first line after it. An unclosed block runs to the end of the text, as it
// does on GitHub, less any trailing empty lines, which stay outside it.
func fencedBlock(lines []string, i int, f fenceOpen) ([]string, int) {
	j := i + 1
	for j < len(lines) && !closesFence(lines[j], f) {
		j++
	}
	closed := j < len(lines)
	end, next := j, j+1
	if !closed {
		for end > i+1 && lines[end-1] == "" {
			end--
		}
		next = end
	}
	if plainInfo(f.info) {
		return codeBlock(f.info, lines[i+1:end]), next
	}
	// The info string would be hidden: show the block as written, its
	// fence lines included.
	body := make([]string, 0, end-i+1)
	body = append(body, lines[i:end]...)
	if closed {
		body = append(body, lines[j])
	}
	return codeBlock("text", body), next
}

// codeBlock renders body as a fenced code block whose fence no line of body
// can close: backticks, at least three and more than any run in body.
func codeBlock(info string, body []string) []string {
	longest := 0
	for _, l := range body {
		longest = max(longest, longestBacktickRun(l))
	}
	delim := strings.Repeat("`", max(3, longest+1))
	out := make([]string, 0, len(body)+2)
	out = append(out, delim+info)
	out = append(out, body...)
	return append(out, delim)
}

// fenceLanguages are the info strings a code block keeps: plain language
// names, which GitHub uses only to highlight. Anything else — a longer info
// string, which GitHub never shows, or a language GitHub renders as
// something other than code (math, mermaid, geojson, ...) — is shown
// literally instead.
var fenceLanguages = map[string]bool{
	"bash": true, "c": true, "c++": true, "console": true, "cpp": true, "cs": true, "csharp": true,
	"css": true, "csv": true, "diff": true, "docker": true, "dockerfile": true, "go": true,
	"golang": true, "graphql": true, "hcl": true, "html": true, "ini": true, "java": true,
	"javascript": true, "js": true, "json": true, "jsonc": true, "jsx": true, "kotlin": true,
	"make": true, "makefile": true, "markdown": true, "md": true, "patch": true, "php": true,
	"plaintext": true, "proto": true, "protobuf": true, "py": true, "python": true, "rb": true,
	"ruby": true, "rust": true, "rs": true, "scss": true, "sh": true, "shell": true, "sql": true,
	"swift": true, "terraform": true, "text": true, "tf": true, "toml": true, "ts": true, "tsx": true,
	"txt": true, "typescript": true, "xml": true, "yaml": true, "yml": true, "zsh": true,
}

func plainInfo(info string) bool {
	return info == "" || fenceLanguages[strings.ToLower(info)]
}

// atom is one character of a prose run as markdown renders it: a single
// rune, or a backslash escape ("\" + an ASCII punctuation character).
type atom struct {
	src     string // the atom's text in the input
	ch      rune   // the character it renders as
	escaped bool   // src is a backslash escape
}

// part is a piece of a sanitised line: inline code (text is the span's
// content) or prose (text is already-neutralised markdown).
type part struct {
	code bool
	text string
}

// sanitizeLine neutralises one line of prose; see Sanitize. It never looks
// past the line, so a code span, a mention or a reference patchy recognises
// never depends on anything a block structure could split off.
func sanitizeLine(line string) string {
	var parts []part
	var prose []atom
	flush := func() {
		parts = append(parts, neutralize(prose)...)
		prose = nil
	}
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line) && isASCIIPunct(line[i+1]):
			prose = append(prose, atom{src: line[i : i+2], ch: rune(line[i+1]), escaped: true})
			i += 2
		case c == '`':
			n := runLen(line[i:], '`')
			if end, ok := codeSpanEnd(line, i+n, n); ok {
				flush()
				parts = append(parts, part{code: true, text: decodeCodeSpan(line[i+n : end])})
				i = end + n
				continue
			}
			for range n {
				prose = append(prose, atom{src: "`", ch: '`'})
			}
			i += n
		default:
			r, size := utf8.DecodeRuneInString(line[i:])
			prose = append(prose, atom{src: line[i : i+size], ch: r})
			i += size
		}
	}
	flush()
	return joinParts(parts)
}

// codeSpanEnd finds the backtick run closing a code span whose n-backtick
// opening run ends at from (CommonMark: the next run of exactly n, with
// backslashes literal inside the span). The span is refused when it has no
// closer on the line or its content holds "|", which GitHub would split
// the span on inside a table row.
func codeSpanEnd(line string, from, n int) (int, bool) {
	for j := from; j < len(line); {
		if line[j] != '`' {
			j++
			continue
		}
		m := runLen(line[j:], '`')
		if m == n {
			return j, !strings.Contains(line[from:j], "|")
		}
		j += m
	}
	return 0, false
}

// decodeCodeSpan is a code span's content as CommonMark reads it: one space
// stripped from each side when both sides have one and it is not all
// spaces.
func decodeCodeSpan(s string) string {
	if len(s) >= 2 && s[0] == ' ' && s[len(s)-1] == ' ' && strings.Trim(s, " ") != "" {
		return s[1 : len(s)-1]
	}
	return s
}

// Anchored token patterns, matched against the rendered characters of a
// prose run (backslash escapes resolved), so an escape cannot hide a token
// from them. Each extends at least as far as GitHub's own reading: whatever
// it takes in lands inside the code span, where nothing is live.
var (
	// issueURLPattern is a GitHub issue, pull request or discussion URL,
	// which GitHub renders as a reference, and which a closing keyword may
	// name.
	issueURLPattern = regexp.MustCompile(`^(?i:(?:https?://)?(?:www\.)?github\.com/` +
		`[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/(?:issues|pulls?|discussions)/[0-9]+)[A-Za-z0-9/?#=&%._~+:-]*`)
	// entityPattern is a character reference, which renders as the
	// character it names — "&#64;" as "@".
	entityPattern = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9A-Fa-f]{1,6}|[A-Za-z][A-Za-z0-9]{0,31});`)
	// mentionPattern is a user or team mention.
	mentionPattern = regexp.MustCompile(`^@[A-Za-z0-9](?:[A-Za-z0-9_-]|\.[A-Za-z0-9_-])*` +
		`(?:/[A-Za-z0-9_-](?:[A-Za-z0-9_-]|\.[A-Za-z0-9_-])*)?`)
	ghRefPattern   = regexp.MustCompile(`^(?i:gh)-[0-9]+`)
	repoRefPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+`)
	hashRefPattern = regexp.MustCompile(`^#[0-9]+`)
)

// neutralize splits one prose run into escaped prose and the tokens that
// become inline code.
func neutralize(atoms []atom) []part {
	if len(atoms) == 0 {
		return nil
	}
	t := newTokenizer(atoms)
	var parts []part
	start, afterToken := 0, true // a run starts after a code span or at the line's start
	for k := 0; k < len(atoms); {
		end := t.token(k, afterToken)
		if end == k {
			k++
			afterToken = false
			continue
		}
		if start < k {
			parts = append(parts, part{text: escapeProse(atoms[start:k])})
		}
		var src strings.Builder
		for _, a := range atoms[k:end] {
			src.WriteString(a.src)
		}
		parts = append(parts, part{code: true, text: src.String()})
		k, start, afterToken = end, end, true
	}
	if start < len(atoms) {
		parts = append(parts, part{text: escapeProse(atoms[start:])})
	}
	return parts
}

// tokenizer finds the tokens of one prose run.
type tokenizer struct {
	atoms   []atom
	view    string // the atoms' rendered characters
	offsets []int  // offsets[k] is atom k's offset in view; the last is len(view)
}

func newTokenizer(atoms []atom) tokenizer {
	var view strings.Builder
	offsets := make([]int, len(atoms)+1)
	for k, a := range atoms {
		offsets[k] = view.Len()
		view.WriteRune(a.ch)
	}
	offsets[len(atoms)] = view.Len()
	return tokenizer{atoms: atoms, view: view.String(), offsets: offsets}
}

// token returns the end (an atom index) of the token starting at atom k, or
// k when none does. afterToken reports that k starts the run or follows a
// token, which, like a non-alphanumeric character, lets a mention or a
// reference begin there.
func (t tokenizer) token(k int, afterToken bool) int {
	a := t.atoms[k]
	boundary, wordStart := afterToken, afterToken
	if !afterToken {
		prev := t.atoms[k-1].ch
		boundary = !isASCIIAlnum(prev)
		wordStart = !isRefChar(prev)
	}
	rest := t.view[t.offsets[k]:]
	match := func(re *regexp.Regexp) int {
		loc := re.FindStringIndex(rest)
		if loc == nil {
			return k
		}
		// Patterns are ASCII, so a match ends on an atom boundary.
		return sort.SearchInts(t.offsets, t.offsets[k]+loc[1])
	}
	candidates := []struct {
		when bool
		re   *regexp.Regexp
	}{
		// Wherever it starts: a URL glued to a word is cheap to over-quote.
		{true, issueURLPattern},
		{a.ch == '&', entityPattern},
		// An escaped "@" renders as "@" whatever precedes it.
		{a.ch == '@' && (boundary || a.escaped), mentionPattern},
		{boundary, ghRefPattern},
		{wordStart, repoRefPattern},
		{a.ch == '#', hashRefPattern},
	}
	for _, c := range candidates {
		if !c.when {
			continue
		}
		if end := match(c.re); end > k {
			return end
		}
	}
	return k
}

// escapeProse renders prose atoms as markdown that shows each character
// literally where it could otherwise start raw HTML, a link or image, math,
// a code span or a code fence.
func escapeProse(atoms []atom) string {
	var b strings.Builder
	for k := 0; k < len(atoms); k++ {
		a := atoms[k]
		if a.escaped {
			b.WriteString(a.src)
			continue
		}
		switch a.ch {
		case '\\', '<', '[', '$', '`':
			b.WriteByte('\\')
			b.WriteRune(a.ch)
		case '~':
			n := 1
			for k+n < len(atoms) && !atoms[k+n].escaped && atoms[k+n].ch == '~' {
				n++
			}
			tilde := "~"
			if n >= 3 {
				tilde = `\~`
			}
			b.WriteString(strings.Repeat(tilde, n))
			k += n - 1
		default:
			b.WriteString(a.src)
		}
	}
	return b.String()
}

// joinParts renders a line's parts, merging adjacent code into one span: two
// spans side by side would fuse their delimiters into one backtick run.
func joinParts(parts []part) string {
	var b strings.Builder
	for k := 0; k < len(parts); {
		if !parts[k].code {
			b.WriteString(parts[k].text)
			k++
			continue
		}
		var c strings.Builder
		for ; k < len(parts) && parts[k].code; k++ {
			c.WriteString(parts[k].text)
		}
		b.WriteString(code(c.String()))
	}
	return b.String()
}

// runLen is the number of leading c bytes in s.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// isASCIIPunct reports CommonMark's ASCII punctuation, the characters a
// backslash escapes.
func isASCIIPunct(c byte) bool {
	return (c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isRefChar reports a character an owner/repo reference's names may hold.
func isRefChar(r rune) bool {
	return isASCIIAlnum(r) || r == '_' || r == '.' || r == '-'
}
