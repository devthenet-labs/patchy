// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// The seeded properties below keep the gate deterministic; a counterexample
// a new seed ever finds is pinned as an example test in plan_test.go or
// build_test.go.
const (
	propertySeed  = 20260924
	propertyCount = 600
)

// textAlphabet is dense in what breaks YAML scalars, markdown and naive
// bounds: colons, quotes, comment and indicator characters, backslashes,
// multi-byte runes, and space separators (a no-break and an ideographic
// space), which a line may hold where a line or paragraph separator may not.
var textAlphabet = []string{
	"a", "b", "Z", "0", " ", ":", ": ", `"`, "'", "#", " #", "-", "- ", "`", "@", "\\", "{", "[", "|", ">",
	"*", "&", "!", "%", "é", "漢", "🙂", "---", "~", "?", "\u00a0", "\u3000",
}

// padRune reports a rune checkLayout counts in a gap: a tab or a space
// separator.
func padRune(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) }

// writeElem appends e to b, first putting an "x" between them when b ends
// and e starts with whitespace, or both with a backtick: a valid report
// keeps gaps and backtick runs short (checkLayout, PlanMaxBacktickRun), so
// the generators never join one longer than a single element's.
func writeElem(b *strings.Builder, e string) {
	last, _ := utf8.DecodeLastRuneInString(b.String())
	first, _ := utf8.DecodeRuneInString(e)
	if b.Len() > 0 && (padRune(last) && padRune(first) || last == '`' && first == '`') {
		b.WriteString("x")
	}
	b.WriteString(e)
}

// genLine builds a valid one-line value of at most maxChars characters:
// trimmed, non-empty, free of control and format characters.
func genLine(r *rand.Rand, maxChars int) string {
	n := 1 + r.Intn(40)
	if r.Intn(12) == 0 {
		n = maxChars
	}
	var b strings.Builder
	for range n {
		writeElem(&b, textAlphabet[r.Intn(len(textAlphabet))])
	}
	s := strings.TrimSpace(b.String())
	for utf8.RuneCountInString(s) > maxChars {
		_, size := utf8.DecodeLastRuneInString(s)
		s = strings.TrimSpace(s[:len(s)-size])
	}
	if s == "" {
		s = "x"
	}
	return s
}

// genDependency builds a valid new dependency: a one-line value of at most
// ItemMaxChars characters cut, on a rune boundary, to DependencyMaxBytes.
func genDependency(r *rand.Rand) string {
	s := genLine(r, ItemMaxChars)
	for len(s) > DependencyMaxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = strings.TrimSpace(s[:len(s)-size])
	}
	if s == "" {
		s = "x"
	}
	return s
}

// genBody builds a markdown body that the fence split keeps intact: it may
// hold anything (fences, frontmatter-looking lines) but does not start with
// a line break, which the split trims.
func genBody(r *rand.Rand) string {
	alphabet := append([]string{"\n", "\n---\n", "```", "\t", "## ", "\r\n"}, textAlphabet...)
	n := r.Intn(200)
	if r.Intn(15) == 0 {
		n = 8000 + r.Intn(4000)
	}
	var b strings.Builder
	for range n {
		writeElem(&b, alphabet[r.Intn(len(alphabet))])
	}
	return strings.TrimLeft(b.String(), "\r\n")
}

// genList builds up to max valid items, each built by item.
func genList(r *rand.Rand, max int, item func(*rand.Rand) string) []string {
	n := r.Intn(max + 1)
	if n == 0 && r.Intn(2) == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = item(r)
	}
	return out
}

// genItem builds a valid one-line list item.
func genItem(r *rand.Rand) string { return genLine(r, ItemMaxChars) }

// genPlan builds a random valid plan.
func genPlan(r *rand.Rand) *Plan {
	confidence := r.Float64()
	repos := make([]string, 1+r.Intn(PlanMaxRepositories))
	for i := range repos {
		repos[i] = fmt.Sprintf("https://git%d.example.com/owner-%d/repo.%d", r.Intn(3), r.Intn(1000), i)
	}
	return &Plan{
		Summary:              genLine(r, SummaryMaxChars),
		Repositories:         repos,
		NewDependencies:      genList(r, PlanMaxNewDependencies, genDependency),
		Questions:            genList(r, PlanMaxQuestions, genItem),
		Confidence:           &confidence,
		EstimatedMaxTurns:    1 + r.Intn(1000),
		EstimatedTokenBudget: 1 + r.Intn(10_000_000),
		Body:                 genBody(r),
	}
}

// genBuild builds a random valid build report.
func genBuild(r *rand.Rand) *Build {
	ran := r.Intn(2) == 0
	success := r.Intn(2) == 0
	passed := ran && (success || r.Intn(2) == 0)
	b := &Build{
		Success: &success,
		Summary: genLine(r, SummaryMaxChars),
		Tests:   &BuildTests{Ran: &ran, Passed: &passed},
		Notes:   genList(r, BuildMaxNotes, genItem),
		Body:    genBody(r),
	}
	if ran || r.Intn(2) == 0 {
		b.Tests.Command = genLine(r, ItemMaxChars)
	}
	if !success {
		b.Reason = genLine(r, ReasonMaxChars)
	}
	return b
}

// render writes a report the way an agent would: the YAML frontmatter
// fence, then the body.
func render(t *testing.T, frontmatter any, body string) []byte {
	t.Helper()
	raw, err := yaml.Marshal(frontmatter)
	if err != nil {
		t.Fatalf("marshal frontmatter: %v", err)
	}
	return []byte("---\n" + string(raw) + "---\n" + body)
}

// fitBody cuts a body so the document stays within ReportMaxBytes, on a rune
// boundary and never between a CRLF's two bytes, which would leave a lone
// carriage return no report may hold. The document it fits keeps 8 bytes to
// spare, room for any one character a property inserts.
func fitBody(frontmatterBytes int, body string) string {
	return fitBodySpare(frontmatterBytes, len("---\n---\n"), body)
}

// fitBodySpare is fitBody keeping spare bytes to spare.
func fitBodySpare(frontmatterBytes, spare int, body string) string {
	room := ReportMaxBytes - frontmatterBytes - spare
	if len(body) <= room {
		return body
	}
	cut := room
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return strings.TrimSuffix(body[:cut], "\r")
}

func quickConfig(seed int64, values func([]reflect.Value, *rand.Rand)) *quick.Config {
	return &quick.Config{MaxCount: propertyCount, Rand: rand.New(rand.NewSource(seed)), Values: values}
}

// TestPlanRoundTripProperty: every valid plan an agent can write parses back
// to exactly what it wrote, whatever its free text holds.
func TestPlanRoundTripProperty(t *testing.T) {
	cfg := quickConfig(propertySeed, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genPlan(r))
	})
	roundTrips := func(want *Plan) bool {
		head := render(t, want, "")
		want.Body = fitBody(len(head), want.Body)
		got, err := ParsePlan(append(head, want.Body...))
		if err != nil {
			t.Logf("ParsePlan(valid) error = %v\n%s", err, head)
			return false
		}
		return got.Summary == want.Summary && slices.Equal(got.Repositories, want.Repositories) &&
			slices.Equal(got.NewDependencies, want.NewDependencies) && slices.Equal(got.Questions, want.Questions) &&
			*got.Confidence == *want.Confidence && got.EstimatedMaxTurns == want.EstimatedMaxTurns &&
			got.EstimatedTokenBudget == want.EstimatedTokenBudget && got.Body == want.Body
	}
	if err := quick.Check(roundTrips, cfg); err != nil {
		t.Error(err)
	}
}

// TestPlanInputProperty: whatever a revise round appends after an approved
// plan, the build stage reads the plan's frontmatter exactly as the plan
// stage parsed it.
func TestPlanInputProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+1, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genPlan(r))
		args[1] = reflect.ValueOf(genBody(r) + strings.Repeat(genBody(r), r.Intn(8)))
	})
	sameFrontmatter := func(p *Plan, round string) bool {
		head := render(t, p, "")
		doc := append(head, fitBody(len(head), p.Body)...)
		plan, err := ParsePlan(doc)
		if err != nil {
			return false
		}
		input, err := ParsePlanInput(append(slices.Clip(doc), round...))
		if err != nil {
			return false
		}
		plan.Body, input.Body = "", ""
		return reflect.DeepEqual(plan, input)
	}
	if err := quick.Check(sameFrontmatter, cfg); err != nil {
		t.Error(err)
	}
}

// TestBuildRoundTripProperty is the plan property for build reports.
func TestBuildRoundTripProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+2, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genBuild(r))
	})
	roundTrips := func(want *Build) bool {
		head := render(t, want, "")
		want.Body = fitBody(len(head), want.Body)
		got, err := ParseBuild(append(head, want.Body...))
		if err != nil {
			t.Logf("ParseBuild(valid) error = %v\n%s", err, head)
			return false
		}
		return *got.Success == *want.Success && got.Summary == want.Summary &&
			*got.Tests.Ran == *want.Tests.Ran && *got.Tests.Passed == *want.Tests.Passed &&
			got.Tests.Command == want.Tests.Command && slices.Equal(got.Notes, want.Notes) &&
			got.Reason == want.Reason && got.Body == want.Body
	}
	if err := quick.Check(roundTrips, cfg); err != nil {
		t.Error(err)
	}
}

// hiddenJunk widens mutate's alphabet with a character of every class the
// parsers refuse, and with the CR and tab they admit only in places: a lone
// CR, a CRLF, a tab. Built from code points so that no editor renders one
// away.
var hiddenJunk = []string{
	"\r", "\r\n", "\t", string(rune(0x1b)) + "[2J", string(rune(0x9b)), string(rune(0xfe0f)), string(rune(0xe0100)),
	string([]rune{0xe0001, 0xe0041}), string(rune(0x00ad)), string(rune(0x2066)), string(rune(0xfeff)),
	string(rune(0x3164)), string(rune(0x034f)), string(rune(0x2065)),
}

// mutate damages a valid document the way a model or a truncated write
// might: bytes replaced, dropped or inserted from a YAML-significant
// alphabet, the document cut short, or padded past every bound.
func mutate(r *rand.Rand, doc []byte) []byte {
	out := slices.Clone(doc)
	junk := []string{":", "\n", "- ", "\"", "'", "{", "[", "---", "\xff", "\x00", "\u202e", " ", "#", "&a", "*a", "|",
		"\u2028", "\u2029", "\u0085", "\u200b", "\u00a0"}
	if r.Intn(2) == 0 {
		// Half the time only, so that as many damaged documents as before
		// stay free of hidden characters and test the other bounds.
		junk = append(junk, hiddenJunk...)
	}
	for range 1 + r.Intn(6) {
		switch op := r.Intn(8); {
		case op < 3 && len(out) > 0:
			i := r.Intn(len(out))
			out = slices.Insert(slices.Delete(out, i, i+1), i, []byte(junk[r.Intn(len(junk))])...)
		case op < 5 && len(out) > 0:
			i := r.Intn(len(out))
			out = slices.Delete(out, i, min(len(out), i+1+r.Intn(8)))
		case op < 7:
			i := r.Intn(len(out) + 1)
			out = slices.Insert(out, i, []byte(junk[r.Intn(len(junk))])...)
		default:
			if r.Intn(2) == 0 {
				out = out[:r.Intn(len(out)+1)]
			} else {
				out = append(out, strings.Repeat("p", r.Intn(2*ReportMaxBytes))...)
			}
		}
	}
	return out
}

// forbiddenInLine is the properties' own statement of what a one-line value
// may never carry, written apart from the parser's predicate so that a gap in
// that predicate fails a property instead of being shared by its oracle: the
// C0 and C1 controls and DEL (NEL, U+0085, among them), U+2028 LINE
// SEPARATOR and U+2029 PARAGRAPH SEPARATOR, the format characters (bidi
// controls, zero-width characters, the BOM, the assigned tag characters),
// and every default-ignorable code point Unicode lists (the variation
// selectors, the whole tag block, the Hangul fillers and the reserved
// ranges among them) — spelled out as ranges here, where the parser reads
// the standard library's tables.
func forbiddenInLine(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return unicode.Is(unicode.Cf, r) || slices.ContainsFunc(defaultIgnorable, func(rg [2]rune) bool {
		return r >= rg[0] && r <= rg[1]
	})
}

// defaultIgnorable are Unicode 15's Default_Ignorable_Code_Point ranges
// (DerivedCoreProperties.txt), format characters included: the code points
// a renderer draws as nothing.
var defaultIgnorable = [][2]rune{
	{0x00ad, 0x00ad}, {0x034f, 0x034f}, {0x061c, 0x061c}, {0x115f, 0x1160}, {0x17b4, 0x17b5},
	{0x180b, 0x180f}, {0x200b, 0x200f}, {0x202a, 0x202e}, {0x2060, 0x206f}, {0x3164, 0x3164},
	{0xfe00, 0xfe0f}, {0xfeff, 0xfeff}, {0xffa0, 0xffa0}, {0xfff0, 0xfff8}, {0x1bca0, 0x1bca3},
	{0x1d173, 0x1d17a}, {0xe0000, 0xe0fff},
}

// firstHidden is the properties' own scan of a whole document for the
// first byte a reader cannot see: a byte that begins no UTF-8 encoding, a
// carriage return not followed by a line feed, or any other rune
// forbiddenInLine names but tab and line feed. It returns that byte's line
// and column (1-based; lines counted in line feeds, columns in characters),
// the rune or, for invalid UTF-8, the byte; found is false for a document
// that is visible throughout.
func firstHidden(doc []byte) (line, column int, r rune, b byte, invalid, found bool) {
	s := string(doc)
	line, column = 1, 1
	for i, r := range s {
		switch _, size := utf8.DecodeRuneInString(s[i:]); {
		case r == utf8.RuneError && size == 1:
			return line, column, r, s[i], true, true
		case r == '\n':
			line, column = line+1, 1
			continue
		case r == '\t', r == '\r' && strings.HasPrefix(s[i+1:], "\n"):
		case forbiddenInLine(r):
			return line, column, r, 0, false, true
		}
		column++
	}
	return 0, 0, 0, 0, false, false
}

// sameRepository is the properties' own statement of when two repository
// URLs name one repository: equal ignoring case once a ".git" suffix, in any
// case, is dropped from each.
func sameRepository(a, b string) bool {
	trim := func(u string) string {
		if n := len(u) - len(".git"); n >= 0 && strings.EqualFold(u[n:], ".git") {
			return u[:n]
		}
		return u
	}
	return strings.EqualFold(trim(a), trim(b))
}

// repositoryAlias spells an existing repository URL differently — its case
// changed, a ".git" suffix added in some case — as a model listing one
// repository twice might.
func repositoryAlias(r *rand.Rand, u string) string {
	switch r.Intn(3) {
	case 0:
		// The scheme stays lower-case: the URL shape requires it.
		u = "https://" + strings.ToUpper(strings.TrimPrefix(u, "https://"))
	case 1:
		u = strings.Replace(u, "repo", "Repo", 1)
	}
	return u + []string{"", ".git", ".GIT", ".Git"}[r.Intn(4)]
}

// planWithinBounds reports whether an accepted plan honours every bound the
// contract promises.
func planWithinBounds(p *Plan) bool {
	return planFrontmatterWithinBounds(p) && len(p.Body) <= BodyMaxBytes
}

// planFrontmatterWithinBounds reports whether an accepted plan's
// frontmatter honours every bound the contract promises: the bounds a build
// input is held to, besides being visible throughout.
func planFrontmatterWithinBounds(p *Plan) bool {
	for i, u := range p.Repositories {
		if slices.ContainsFunc(p.Repositories[:i], func(v string) bool { return sameRepository(u, v) }) {
			return false
		}
	}
	ok := func(s string, maxChars int) bool { return s != "" && boundedLine(s, maxChars) }
	all := func(items []string, maxItems, maxChars int) bool {
		return len(items) <= maxItems && !slices.ContainsFunc(items, func(s string) bool { return !ok(s, maxChars) })
	}
	return ok(p.Summary, SummaryMaxChars) &&
		len(p.Repositories) >= 1 && len(p.Repositories) <= PlanMaxRepositories &&
		!slices.ContainsFunc(p.Repositories, func(u string) bool {
			return len(u) > RepositoryURLMaxBytes || !utf8.ValidString(u) || !repositoryURL.MatchString(u) ||
				strings.ContainsFunc(u, forbiddenInLine)
		}) &&
		all(p.NewDependencies, PlanMaxNewDependencies, ItemMaxChars) && all(p.Questions, PlanMaxQuestions, ItemMaxChars) &&
		!slices.ContainsFunc(p.NewDependencies, func(d string) bool { return len(d) > DependencyMaxBytes }) &&
		p.Confidence != nil && *p.Confidence >= 0 && *p.Confidence <= 1 &&
		p.EstimatedMaxTurns >= 1 && p.EstimatedTokenBudget >= 1
}

// visibleDocument reports whether firstHidden finds nothing in doc.
func visibleDocument(doc []byte) bool {
	_, _, _, _, _, found := firstHidden(doc)
	return !found
}

// badConfidences are the confidences no plan may carry: outside [0, 1], and
// the IEEE values that compare false both ways (NaN) or sit past every
// bound. yaml.Marshal writes them as YAML's own .nan, .inf and -.inf.
var badConfidences = []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.25, -math.SmallestNonzeroFloat64, 1.5}

// damagedPlan builds a random valid plan, sometimes given a confidence or a
// repository it may not have, and renders it as a document.
func damagedPlan(r *rand.Rand) []byte {
	p := genPlan(r)
	if r.Intn(4) == 0 {
		bad := badConfidences[r.Intn(len(badConfidences))]
		p.Confidence = &bad
	}
	if r.Intn(4) == 0 {
		p.Repositories = append(p.Repositories, repositoryAlias(r, p.Repositories[r.Intn(len(p.Repositories))]))
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		panic(err)
	}
	return []byte("---\n" + string(raw) + "---\n" + p.Body)
}

// TestPlanParseBoundedProperty: however a plan is damaged, parsing it never
// panics, never accepts a document past the size bound, holding a byte a
// reader cannot see or laying text out of view, and whatever it does
// accept honours every bound of the contract.
func TestPlanParseBoundedProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+3, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(mutate(r, damagedPlan(r)))
	})
	bounded := func(doc []byte) bool {
		p, err := ParsePlan(doc)
		if err != nil {
			return true
		}
		_, _, _, outOfView := firstOutOfView(doc)
		_, _, _, longRun := firstLongBacktickRun(doc)
		return len(doc) <= ReportMaxBytes && utf8.Valid(doc) && visibleDocument(doc) && !outOfView && !longRun &&
			planWithinBounds(p)
	}
	if err := quick.Check(bounded, cfg); err != nil {
		t.Error(err)
	}
}

// TestPlanInputParseBoundedProperty: however a build input — a plan and a
// revise round after it — is damaged, parsing it never panics, never
// accepts an input holding a byte a reader cannot see, and whatever it
// accepts has a frontmatter within every bound of the plan's contract.
func TestPlanInputParseBoundedProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+5, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(mutate(r, append(damagedPlan(r), genBody(r)...)))
	})
	bounded := func(doc []byte) bool {
		p, err := ParsePlanInput(doc)
		if err != nil {
			return true
		}
		return utf8.Valid(doc) && visibleDocument(doc) && planFrontmatterWithinBounds(p)
	}
	if err := quick.Check(bounded, cfg); err != nil {
		t.Error(err)
	}
}

// TestBuildParseBoundedProperty is the plan property for build reports,
// including the consistency rules between success, tests and reason.
func TestBuildParseBoundedProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+4, func(args []reflect.Value, r *rand.Rand) {
		b := genBuild(r)
		raw, err := yaml.Marshal(b)
		if err != nil {
			panic(err)
		}
		args[0] = reflect.ValueOf(mutate(r, []byte("---\n"+string(raw)+"---\n"+b.Body)))
	})
	bounded := func(doc []byte) bool {
		b, err := ParseBuild(doc)
		if err != nil {
			return true
		}
		_, _, _, outOfView := firstOutOfView(doc)
		return len(doc) <= ReportMaxBytes && utf8.Valid(doc) && visibleDocument(doc) && !outOfView &&
			buildWithinBounds(b)
	}
	if err := quick.Check(bounded, cfg); err != nil {
		t.Error(err)
	}
}

// boundedLine reports whether s is a trimmed line of valid UTF-8, of at
// most maxChars characters, with no rune forbiddenInLine; empty passes. The
// UTF-8 check is its own: ranged over, an invalid byte reads as U+FFFD,
// which forbiddenInLine does not name.
func boundedLine(s string, maxChars int) bool {
	return utf8.ValidString(s) && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= maxChars &&
		!strings.ContainsFunc(s, forbiddenInLine)
}

// buildWithinBounds reports whether an accepted build report honours every
// bound and consistency rule of the contract.
func buildWithinBounds(b *Build) bool {
	ran, passed, success := *b.Tests.Ran, *b.Tests.Passed, *b.Success
	consistent := (!ran || b.Tests.Command != "") && (ran || !passed) && (!success || !ran || passed) &&
		success == (b.Reason == "")
	notes := len(b.Notes) <= BuildMaxNotes &&
		!slices.ContainsFunc(b.Notes, func(s string) bool { return s == "" || !boundedLine(s, ItemMaxChars) })
	return consistent && notes && len(b.Body) <= BodyMaxBytes && b.Summary != "" &&
		boundedLine(b.Summary, SummaryMaxChars) && boundedLine(b.Tests.Command, ItemMaxChars) &&
		boundedLine(b.Reason, ReasonMaxChars)
}

// formatRunes are every format character (Cf) there is, for genHidden to
// draw from.
var formatRunes = func() []rune {
	var out []rune
	for _, rg := range unicode.Cf.R16 {
		for c := rune(rg.Lo); c <= rune(rg.Hi); c += rune(rg.Stride) {
			out = append(out, c)
		}
	}
	for _, rg := range unicode.Cf.R32 {
		for c := rune(rg.Lo); c <= rune(rg.Hi); c += rune(rg.Stride) {
			out = append(out, c)
		}
	}
	return out
}()

// genHidden draws what no report may hold, from every class and across each
// class's whole range: a C0 or C1 control or DEL, a lone carriage return
// (lone reports it: it must not be inserted before a line feed, where it
// would be half a CRLF), a line or paragraph separator, a tag character, a
// variation selector, a format character, a default-ignorable code point,
// or a byte sequence that is not UTF-8.
func genHidden(r *rand.Rand) (s string, lone bool) {
	in := func(lo, hi rune) string { return string(lo + rune(r.Intn(int(hi-lo+1)))) }
	switch r.Intn(10) {
	case 0:
		c := rune(r.Intn(0x20 - 3))
		// Past tab, line feed and carriage return, which a document may hold.
		for _, allowed := range []rune{'\t', '\n', '\r'} {
			if c >= allowed {
				c++
			}
		}
		return string(c), false
	case 1:
		if r.Intn(8) == 0 {
			return string(rune(0x7f)), false
		}
		return in(0x80, 0x9f), false
	case 2:
		return "\r", true
	case 3:
		return in(0x2028, 0x2029), false
	case 4:
		return in(0xe0000, 0xe007f), false
	case 5:
		switch r.Intn(3) {
		case 0:
			return in(0xfe00, 0xfe0f), false
		case 1:
			return in(0xe0100, 0xe01ef), false
		}
		return string([]rune{0x180b, 0x180c, 0x180d, 0x180f}[r.Intn(4)]), false
	case 6:
		return string(formatRunes[r.Intn(len(formatRunes))]), false
	case 7:
		rg := defaultIgnorable[r.Intn(len(defaultIgnorable))]
		return in(rg[0], rg[1]), false
	case 8:
		// Any byte past ASCII alone begins no encoding where it is put: at
		// a rune boundary, before ASCII or another rune's first byte.
		return string([]byte{byte(0x80 + r.Intn(0x80))}), false
	}
	return invalidUTF8[r.Intn(len(invalidUTF8))].seq, false
}

// insertHidden puts s into doc at a random rune boundary — for a lone
// carriage return, one not followed by a line feed.
func insertHidden(r *rand.Rand, doc []byte, s string, lone bool) []byte {
	var at []int
	for i := 0; i <= len(doc); i++ {
		if i < len(doc) && (!utf8.RuneStart(doc[i]) || lone && doc[i] == '\n') {
			continue
		}
		at = append(at, i)
	}
	i := at[r.Intn(len(at))]
	return slices.Concat(doc[:i], []byte(s), doc[i:])
}

// planDocument renders a random valid plan as the plan stage writes one,
// within the document bound with room to spare.
func planDocument(r *rand.Rand) []byte { return planDocumentSpare(r, len("---\n---\n")) }

// planDocumentSpare is planDocument keeping spare bytes to spare.
func planDocumentSpare(r *rand.Rand, spare int) []byte {
	p := genPlan(r)
	raw, err := yaml.Marshal(p)
	if err != nil {
		panic(err)
	}
	head := "---\n" + string(raw) + "---\n"
	return []byte(head + fitBodySpare(len(head), spare, p.Body))
}

// buildDocument is planDocument for a build report.
func buildDocument(r *rand.Rand) []byte { return buildDocumentSpare(r, len("---\n---\n")) }

// buildDocumentSpare is planDocumentSpare for a build report.
func buildDocumentSpare(r *rand.Rand, spare int) []byte {
	b := genBuild(r)
	raw, err := yaml.Marshal(b)
	if err != nil {
		panic(err)
	}
	head := "---\n" + string(raw) + "---\n"
	return []byte(head + fitBodySpare(len(head), spare, b.Body))
}

// TestHiddenCharacterRefusedProperty: whatever a document every parser
// accepts holds, inserting one character that renders invisibly or
// reorders text — of any class, anywhere — or one byte that is not UTF-8
// makes it refused, and the refusal names what the properties' own scan
// (firstHidden) finds first: its line, its column and its code point or
// byte. The build input is held to it past the plan's bounds, across a
// revise round appended to the plan.
func TestHiddenCharacterRefusedProperty(t *testing.T) {
	parsers := []struct {
		name  string
		seed  int64
		gen   func(*rand.Rand) []byte
		parse func([]byte) error
	}{
		{"ParsePlan", propertySeed + 6, planDocument,
			func(doc []byte) error { _, err := ParsePlan(doc); return err }},
		{"ParsePlanInput", propertySeed + 7,
			func(r *rand.Rand) []byte { return append(planDocument(r), genBody(r)...) },
			func(doc []byte) error { _, err := ParsePlanInput(doc); return err }},
		{"ParseBuild", propertySeed + 8, buildDocument,
			func(doc []byte) error { _, err := ParseBuild(doc); return err }},
	}
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			cfg := quickConfig(p.seed, func(args []reflect.Value, r *rand.Rand) {
				doc := p.gen(r)
				s, lone := genHidden(r)
				args[0] = reflect.ValueOf(doc)
				args[1] = reflect.ValueOf(insertHidden(r, doc, s, lone))
			})
			refused := func(doc, damaged []byte) bool {
				if err := p.parse(doc); err != nil {
					t.Logf("%s(valid) error = %v", p.name, err)
					return false
				}
				line, column, r, b, invalid, found := firstHidden(damaged)
				if !found {
					t.Logf("firstHidden found nothing in the damaged document")
					return false
				}
				want := fmt.Sprintf("line %d, column %d: U+%04X is ", line, column, r)
				if invalid {
					want = fmt.Sprintf("line %d, column %d: byte 0x%02X is not valid UTF-8", line, column, b)
				}
				if err := p.parse(damaged); err == nil || !strings.Contains(err.Error(), want) {
					t.Logf("%s(damaged) error = %v, want it to name %q", p.name, err, want)
					return false
				}
				return true
			}
			if err := quick.Check(refused, cfg); err != nil {
				t.Error(err)
			}
		})
	}
}

// gapRun, markRun and tickRun match, in one line, a run of spaces and
// tabs, of combining marks and of backticks: the properties' own statement
// of the runs the layout rules bound, by regular expression where the
// parsers scan rune by rune.
var (
	gapRun  = regexp.MustCompile(`[\t\p{Zs}]+`)
	markRun = regexp.MustCompile(`[\p{Mn}\p{Me}]+`)
	tickRun = regexp.MustCompile("`+")
)

// gapColumns is the width the layout rule gives a run of spaces and tabs:
// a space one column, a tab TabColumns, any other space separator two.
func gapColumns(run string) int {
	n := 0
	for _, r := range run {
		switch r {
		case ' ':
			n++
		case '\t':
			n += TabColumns
		default:
			n += 2
		}
	}
	return n
}

// firstOutOfView is the properties' own scan of a document for the first
// run the layout rule refuses: a gap of spaces and tabs with more text
// after it on its line, wider than PadMaxColumns, or than IndentMaxColumns
// when it indents the line; or more than CombiningMaxMarks combining marks
// in a row. It returns where the run starts (its line, and its column in
// characters, both 1-based) and whether it is a gap.
func firstOutOfView(doc []byte) (line, column int, gap, found bool) {
	for i, l := range strings.Split(string(doc), "\n") {
		l = strings.TrimSuffix(l, "\r")
		at := -1
		for _, m := range gapRun.FindAllStringIndex(l, -1) {
			limit := PadMaxColumns
			if m[0] == 0 {
				limit = IndentMaxColumns
			}
			if m[1] < len(l) && gapColumns(l[m[0]:m[1]]) > limit {
				at, gap = m[0], true
				break
			}
		}
		for _, m := range markRun.FindAllStringIndex(l, -1) {
			if utf8.RuneCountInString(l[m[0]:m[1]]) > CombiningMaxMarks && (at < 0 || m[0] < at) {
				at, gap = m[0], false
				break
			}
		}
		if at >= 0 {
			return i + 1, utf8.RuneCountInString(l[:at]) + 1, gap, true
		}
	}
	return 0, 0, false, false
}

// firstLongBacktickRun finds a document's first run of more than
// PlanMaxBacktickRun backticks: its line, column and length.
func firstLongBacktickRun(doc []byte) (line, column, run int, found bool) {
	for i, l := range strings.Split(string(doc), "\n") {
		for _, m := range tickRun.FindAllStringIndex(l, -1) {
			if m[1]-m[0] > PlanMaxBacktickRun {
				return i + 1, utf8.RuneCountInString(l[:m[0]]) + 1, m[1] - m[0], true
			}
		}
	}
	return 0, 0, 0, false
}

// genOutOfView builds a run for the layout property to insert, as often
// within its bound as past it: a gap of spaces and tabs (and other space
// separators) with a visible "x" after it, which indents the line when it
// lands at a line's start; a stack of combining marks; and, when backticks
// is set, a run of backticks. Built from code points, so that no editor
// renders one away.
func genOutOfView(r *rand.Rand, backticks bool) string {
	kind := r.Intn(3)
	if backticks {
		kind = r.Intn(4)
	}
	switch kind {
	case 1:
		marks := []rune{0x0300, 0x0301, 0x0308, 0x0336, 0x0489, 0x05b0, 0x20dd, 0x20e3}
		out := make([]rune, 1+r.Intn(3*CombiningMaxMarks))
		for i := range out {
			out[i] = marks[r.Intn(len(marks))]
		}
		return string(out)
	case 3:
		return strings.Repeat("`", 1+r.Intn(3*PlanMaxBacktickRun))
	}
	pads := []string{" ", " ", " ", "\t", string(rune(0x00a0)), string(rune(0x2003)), string(rune(0x3000))}
	width := 1 + r.Intn(2*IndentMaxColumns+PadMaxColumns)
	var b strings.Builder
	for w := 0; w < width; {
		p := pads[r.Intn(len(pads))]
		b.WriteString(p)
		w += gapColumns(p)
	}
	return b.String() + "x"
}

// insertVisible puts s into doc at a random rune boundary that does not
// split a CRLF.
func insertVisible(r *rand.Rand, doc []byte, s string) []byte {
	var at []int
	for i := 0; i <= len(doc); i++ {
		if i < len(doc) && !utf8.RuneStart(doc[i]) || i > 0 && doc[i-1] == '\r' {
			continue
		}
		at = append(at, i)
	}
	i := at[r.Intn(len(at))]
	return slices.Concat(doc[:i], []byte(s), doc[i:])
}

// layoutRefusal reports an error as one the layout rules gave.
func layoutRefusal(err error) bool {
	return strings.Contains(err.Error(), "out of the reader's view") ||
		strings.Contains(err.Error(), "combining marks in a row") || strings.Contains(err.Error(), "backticks, over")
}

// TestLayoutRefusedProperty: whatever a document the parser accepts holds,
// inserting a run the layout rule bounds — a gap of whitespace before more
// text, an indentation, a stack of combining marks, and in a plan a run of
// backticks — anywhere, is refused exactly when the properties' own scan
// finds a run past its bound, naming where that run starts; within every
// bound, the layout rule refuses nothing. A build report may hold a run of
// backticks of any length: only the plan is fenced for its approver.
func TestLayoutRefusedProperty(t *testing.T) {
	const spare = 1 << 10 // room for any run genOutOfView builds
	parsers := []struct {
		name      string
		seed      int64
		gen       func(*rand.Rand, int) []byte
		parse     func([]byte) error
		backticks bool
	}{
		{"ParsePlan", propertySeed + 9, planDocumentSpare,
			func(doc []byte) error { _, err := ParsePlan(doc); return err }, true},
		{"ParseBuild", propertySeed + 10, buildDocumentSpare,
			func(doc []byte) error { _, err := ParseBuild(doc); return err }, false},
	}
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			cfg := quickConfig(p.seed, func(args []reflect.Value, r *rand.Rand) {
				doc := p.gen(r, spare)
				args[0] = reflect.ValueOf(doc)
				args[1] = reflect.ValueOf(insertVisible(r, doc, genOutOfView(r, r.Intn(2) == 0)))
			})
			// seen counts the refusals the property checked, by what they
			// name, so that a generator drifting off the bounds cannot leave
			// it checking nothing.
			seen := map[string]int{}
			refused := func(doc, damaged []byte) bool {
				if err := p.parse(doc); err != nil {
					t.Logf("%s(valid) error = %v", p.name, err)
					return false
				}
				err := p.parse(damaged)
				if line, column, gap, found := firstOutOfView(damaged); found {
					want, what := fmt.Sprintf("line %d, column %d: ", line, column), "combining marks in a row"
					if gap {
						what = fmt.Sprintf("(a tab counts as %d)", TabColumns)
					}
					if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), what) {
						t.Logf("%s(damaged) error = %v, want it to name %q and %q", p.name, err, want, what)
						return false
					}
					switch {
					case !gap:
						seen["marks"]++
					case strings.Contains(err.Error(), "is indented"):
						seen["indent"]++
					default:
						seen["gap"]++
					}
					return true
				}
				if line, column, run, found := firstLongBacktickRun(damaged); found && p.backticks {
					want := fmt.Sprintf("line %d, column %d: a run of %d backticks, over %d", line, column, run,
						PlanMaxBacktickRun)
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Logf("%s(damaged) error = %v, want it to name %q", p.name, err, want)
						return false
					}
					seen["backticks"]++
					return true
				}
				// Nothing past a bound: the insertion may have broken the
				// frontmatter's YAML, but the layout rule refuses nothing.
				if err != nil && layoutRefusal(err) {
					t.Logf("%s(damaged) error = %v, want no layout refusal", p.name, err)
					return false
				}
				seen["within bounds"]++
				return true
			}
			if err := quick.Check(refused, cfg); err != nil {
				t.Error(err)
			}
			want := []string{"gap", "indent", "marks", "within bounds"}
			if p.backticks {
				want = append(want, "backticks")
			}
			for _, k := range want {
				if seen[k] == 0 {
					t.Errorf("no case of %q among %v: the generator no longer reaches it", k, seen)
				}
			}
			t.Logf("cases: %v", seen)
		})
	}
}
