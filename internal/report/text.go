// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Bounds shared by the intent reports (plan and build).
const (
	// SummaryMaxChars bounds a report's one-line summary, in characters. It
	// is the Intent status field's bound, and the summary is also what the
	// intent controller composes the pushed commit's subject from.
	SummaryMaxChars = 200
	// ItemMaxChars bounds one list item or other short free-text value, in
	// characters.
	ItemMaxChars = 500
	// BodyMaxBytes bounds the markdown after an intent report's frontmatter:
	// it is posted to GitHub as an issue comment or a pull-request body.
	BodyMaxBytes = 48 << 10
	// ReportMaxBytes bounds a whole intent report, frontmatter included — the
	// run status field that records it holds at most 64 KiB.
	ReportMaxBytes = 64 << 10
)

// checkDocument applies the bounds every intent report shares before any of
// it is parsed: the whole document's size, then what it may hold
// (checkVisible). Invalid UTF-8 and invisible characters are refused rather
// than repaired, because the report's exact bytes are what the plan's
// approval digest is taken over — a repair would change what was approved,
// and JSON would silently rewrite invalid UTF-8 on the way out of the pod.
func checkDocument(kind string, data []byte) error {
	if len(data) > ReportMaxBytes {
		return fmt.Errorf("report: %s: %d bytes, over the %d-byte bound", kind, len(data), ReportMaxBytes)
	}
	return checkVisible(kind, data)
}

// checkBody applies the body bound.
func checkBody(kind, body string) error {
	if len(body) > BodyMaxBytes {
		return fmt.Errorf("report: %s: body is %d bytes, over the %d-byte bound", kind, len(body), BodyMaxBytes)
	}
	return nil
}

// oneLine trims s and checks it is a non-empty single line of at most
// maxChars characters holding no rune that hidden names: no line break,
// tab or other control character (a terminal escape, NEL), no Unicode line
// or paragraph separator, and nothing that renders invisibly or reorders
// text (a bidi override, a zero-width space, a variation selector, a tag
// character). checkVisible has already refused such a rune written into
// the document; this catches one a YAML escape spells ("\u200b"), which
// the document shows as visible text but the decoded value holds. These
// values reach GitHub and later prompts, and a summary becomes a commit
// subject, so they carry nothing a reader cannot see and break nowhere.
func oneLine(field, s string, maxChars int) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return s, fmt.Errorf("%s is required", field)
	case utf8.RuneCountInString(s) > maxChars:
		return s, fmt.Errorf("%s is %d characters, over %d", field, utf8.RuneCountInString(s), maxChars)
	}
	if i := strings.IndexFunc(s, invisible); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return s, fmt.Errorf("%s holds U+%04X, %s, where one line of visible text is required", field, r, hidden(r))
	}
	return s, nil
}

// lines applies oneLine to every item of a list of at most maxItems,
// trimming each in place.
func lines(field string, items []string, maxItems, maxChars int) error {
	if len(items) > maxItems {
		return fmt.Errorf("%s has %d items, over %d", field, len(items), maxItems)
	}
	var errs []error
	for i := range items {
		var err error
		if items[i], err = oneLine(fmt.Sprintf("%s[%d]", field, i), items[i], maxChars); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// freeTextLine captures a mapping key and its value, and listItemLine a
// sequence item, as a YAML frontmatter lays them out one per line.
var (
	freeTextLine = regexp.MustCompile(`^([ \t]*([A-Za-z_]+):[ \t]+)(.*)$`)
	listItemLine = regexp.MustCompile(`^([ \t]*-[ \t]+)(.*)$`)
)

// repairFreeText double-quotes the unquoted values of the named free-text
// keys, and of every sequence item, in a frontmatter block. It is the intent
// reports' counterpart of repairSummaries: models write these values as
// plain prose, and prose holding a colon ("Question: which store?") is not
// a valid plain scalar — in a list it even parses as a mapping. Quoting a
// plain scalar that was already valid keeps its string value, and every
// list in these schemas is a list of strings, so callers only attempt this
// after a strict parse has failed and surface the original error if the
// repaired block fails too. A null (yamlNull) is the exception: quoting it
// would turn "no value" into the string "null", so it is left as it is.
func repairFreeText(block []byte, keys map[string]bool) ([]byte, bool) {
	lines := strings.Split(string(block), "\n")
	changed := false
	for i, line := range lines {
		var prefix, val string
		if m := listItemLine.FindStringSubmatch(line); m != nil {
			prefix, val = m[1], m[2]
		} else if m := freeTextLine.FindStringSubmatch(line); m != nil && keys[m[2]] {
			prefix, val = m[1], m[3]
		} else {
			continue
		}
		val = strings.TrimRight(val, " \t\r")
		if val == "" || yamlNull(val) {
			// A null stays null: quoted, "~" or "null" would become that
			// string, and a reason a model left out that way would then read
			// as given.
			continue
		}
		switch val[0] {
		case '"', '\'', '|', '>', '&', '*', '[', '{', '!':
			// Already quoted, a block or flow collection, anchored, an alias
			// or tagged — leave it.
			continue
		}
		val = strings.ReplaceAll(val, `\`, `\\`)
		val = strings.ReplaceAll(val, `"`, `\"`)
		lines[i] = prefix + `"` + val + `"`
		changed = true
	}
	if !changed {
		return block, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// yamlNull reports whether a plain value is one of YAML's null spellings,
// alone or before a comment.
func yamlNull(val string) bool {
	if i := strings.IndexAny(val, " \t"); i >= 0 {
		if rest := strings.TrimLeft(val[i:], " \t"); rest != "" && rest[0] != '#' {
			return false
		}
		val = val[:i]
	}
	switch val {
	case "~", "null", "Null", "NULL":
		return true
	}
	return false
}

// decodeRepairing decodes a frontmatter block strictly into out, retrying
// once over repairFreeText's quoting when the strict decode fails. reset
// zeroes out before the retry. The original error is the one reported.
func decodeRepairing(block []byte, out any, keys map[string]bool, reset func()) error {
	err := decodeStrict(block, out)
	if err == nil {
		return nil
	}
	repaired, changed := repairFreeText(block, keys)
	if !changed {
		return err
	}
	reset()
	if rerr := decodeStrict(repaired, out); rerr != nil {
		return err
	}
	return nil
}
