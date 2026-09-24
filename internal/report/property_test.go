// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
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

// genLine builds a valid one-line value of at most maxChars characters:
// trimmed, non-empty, free of control and format characters.
func genLine(r *rand.Rand, maxChars int) string {
	n := 1 + r.Intn(40)
	if r.Intn(12) == 0 {
		n = maxChars
	}
	var b strings.Builder
	for range n {
		b.WriteString(textAlphabet[r.Intn(len(textAlphabet))])
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
		b.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	return strings.TrimLeft(b.String(), "\r\n")
}

// genList builds up to max valid one-line items.
func genList(r *rand.Rand, max int) []string {
	n := r.Intn(max + 1)
	if n == 0 && r.Intn(2) == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = genLine(r, ItemMaxChars)
	}
	return out
}

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
		NewDependencies:      genList(r, PlanMaxNewDependencies),
		Questions:            genList(r, PlanMaxQuestions),
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
		Notes:   genList(r, BuildMaxNotes),
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
// boundary.
func fitBody(frontmatterBytes int, body string) string {
	room := ReportMaxBytes - frontmatterBytes - len("---\n---\n")
	if len(body) <= room {
		return body
	}
	cut := room
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
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

// mutate damages a valid document the way a model or a truncated write
// might: bytes replaced, dropped or inserted from a YAML-significant
// alphabet, the document cut short, or padded past every bound.
func mutate(r *rand.Rand, doc []byte) []byte {
	out := slices.Clone(doc)
	junk := []string{":", "\n", "- ", "\"", "'", "{", "[", "---", "\xff", "\x00", "\u202e", " ", "#", "&a", "*a", "|",
		"\u2028", "\u2029", "\u0085", "\u200b", "\u00a0"}
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
// SEPARATOR and U+2029 PARAGRAPH SEPARATOR, and the format characters (bidi
// controls, zero-width characters, the BOM, tag characters).
func forbiddenInLine(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// planWithinBounds reports whether an accepted plan honours every bound the
// contract promises.
func planWithinBounds(p *Plan) bool {
	ok := func(s string, maxChars int) bool {
		return s != "" && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= maxChars &&
			!strings.ContainsFunc(s, forbiddenInLine)
	}
	all := func(items []string, maxItems, maxChars int) bool {
		return len(items) <= maxItems && !slices.ContainsFunc(items, func(s string) bool { return !ok(s, maxChars) })
	}
	return ok(p.Summary, SummaryMaxChars) &&
		len(p.Repositories) >= 1 && len(p.Repositories) <= PlanMaxRepositories &&
		!slices.ContainsFunc(p.Repositories, func(u string) bool {
			return len(u) > RepositoryURLMaxBytes || !repositoryURL.MatchString(u) ||
				strings.ContainsFunc(u, forbiddenInLine)
		}) &&
		all(p.NewDependencies, PlanMaxNewDependencies, ItemMaxChars) && all(p.Questions, PlanMaxQuestions, ItemMaxChars) &&
		p.Confidence != nil && *p.Confidence >= 0 && *p.Confidence <= 1 &&
		p.EstimatedMaxTurns >= 1 && p.EstimatedTokenBudget >= 1 && len(p.Body) <= BodyMaxBytes
}

// badConfidences are the confidences no plan may carry: outside [0, 1], and
// the IEEE values that compare false both ways (NaN) or sit past every
// bound. yaml.Marshal writes them as YAML's own .nan, .inf and -.inf.
var badConfidences = []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.25, -math.SmallestNonzeroFloat64, 1.5}

// TestPlanParseBoundedProperty: however a plan is damaged, parsing it never
// panics, never accepts a document past the size bound, and whatever it
// does accept honours every bound of the contract.
func TestPlanParseBoundedProperty(t *testing.T) {
	cfg := quickConfig(propertySeed+3, func(args []reflect.Value, r *rand.Rand) {
		p := genPlan(r)
		if r.Intn(4) == 0 {
			bad := badConfidences[r.Intn(len(badConfidences))]
			p.Confidence = &bad
		}
		raw, err := yaml.Marshal(p)
		if err != nil {
			panic(err)
		}
		args[0] = reflect.ValueOf(mutate(r, []byte("---\n"+string(raw)+"---\n"+p.Body)))
	})
	bounded := func(doc []byte) bool {
		p, err := ParsePlan(doc)
		if err != nil {
			return true
		}
		return len(doc) <= ReportMaxBytes && utf8.Valid(doc) && planWithinBounds(p)
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
		return len(doc) <= ReportMaxBytes && utf8.Valid(doc) && buildWithinBounds(b)
	}
	if err := quick.Check(bounded, cfg); err != nil {
		t.Error(err)
	}
}

// boundedLine reports whether s is a trimmed line of at most maxChars
// characters with no rune forbiddenInLine; empty passes.
func boundedLine(s string, maxChars int) bool {
	return s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= maxChars &&
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
