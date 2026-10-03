// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// A check-fix round stops the Intent (ChecksFailing, RepeatedFailure) when a
// named check fails again with the diagnostic it failed with before an
// earlier round. The two are compared by fingerprint, and a fingerprint of the
// diagnostic exactly as the agent reads it never matched: an Actions job log
// stamps every line with the moment it was written, and each run's log also
// carries its runner's and machine's names, the commits it checked out, its
// temporary paths and its durations. None of that says why the check failed.
// So the fingerprint is taken over a stable form of the diagnostic, with those
// tokens replaced, and of a job log only the failing step's own lines. The
// agent still reads the diagnostic itself.
//
// Only numbers of a volatile shape are replaced: durations, times, ids long
// enough to be generated, addresses, goroutine numbers, and a source line
// number after a file name (a fix moves lines). Any other number is a value,
// and counts: a coverage gate that rose from 71.3% to 76.1%, or a test that
// went from "got 3" to "got 4", is progress, not the same failure. A false
// "same" stops automatic fixing while the agent was getting somewhere, a false
// "different" spends another round, up to maxCheckFixes.

// fingerprintVersion heads every fingerprint, so a stable form changed later
// never compares equal to one taken under an earlier rule.
const fingerprintVersion = "patchy-check-fingerprint/3"

// fingerprintLogLines bounds how many lines of a failing step's log the
// fingerprint keeps: its last ones, which say why it failed. Fewer than a 32
// KiB tail holds, so a tail window that slid by a line or two (a duration
// printed a digit longer upstream) still keeps the same lines.
const fingerprintLogLines = 100

var (
	// actionsStamp is the time GitHub Actions writes at the start of every
	// job log line, the log's first line led by a byte order mark.
	actionsStamp = regexp.MustCompile(`^\x{FEFF}?\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z ?`)
	// uuidToken, timeToken and hexToken are volatile anywhere in a line: a
	// temporary directory or request id, a time printed by a tool, a commit
	// (or any hex token of seven or more characters holding a digit).
	uuidToken = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timeToken = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	hexToken  = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	// addrToken is a pointer or offset a stack trace prints, goroutineToken
	// a goroutine's number.
	addrToken      = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	goroutineToken = regexp.MustCompile(`\bgoroutine \d+\b`)
	// durationToken is a duration as tools print one: "(0.01s)", "412ms",
	// "1m2.5s".
	durationToken = regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+\b`)
	// lineToken is a source line (and column) number after a file name:
	// "version_test.go:12:", "main.rs:4:7".
	lineToken = regexp.MustCompile(`(\.[A-Za-z0-9]+):\d+(?::\d+)?\b`)
	// clockToken is a time of day without a date.
	clockToken = regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`)
	// idDigits are five or more digits in a row: a run, job, process or
	// runner number, a port, the digits of a generated name. Shorter numbers
	// are values.
	idDigits = regexp.MustCompile(`\d{5,}`)
	spaceRun = regexp.MustCompile(`[ \t]+`)
)

// stableLine is one line in the stable form: its Actions time stamp dropped
// and every volatile token replaced by a placeholder, runs of blanks as one
// space, trimmed. A number of no volatile shape is kept.
func stableLine(line string) string {
	line = actionsStamp.ReplaceAllString(strings.TrimRight(line, "\r"), "")
	line = uuidToken.ReplaceAllString(line, "<uuid>")
	line = timeToken.ReplaceAllString(line, "<time>")
	line = addrToken.ReplaceAllString(line, "<addr>")
	line = hexToken.ReplaceAllStringFunc(line, func(tok string) string {
		switch {
		case strings.Trim(tok, "0123456789") == "":
			return "<n>" // an id: idDigits' placeholder
		case strings.ContainsAny(tok, "0123456789"):
			return "<hex>"
		}
		return tok // a word made of hex letters, such as "acceded"
	})
	line = goroutineToken.ReplaceAllString(line, "goroutine <n>")
	line = lineToken.ReplaceAllString(line, "$1:<line>")
	line = clockToken.ReplaceAllString(line, "<time>")
	line = durationToken.ReplaceAllString(line, "<dur>")
	line = idDigits.ReplaceAllString(line, "<n>")
	return strings.TrimSpace(spaceRun.ReplaceAllString(line, " "))
}

// stableLines is s in the stable form, line by line, its blank lines dropped.
func stableLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if l := stableLine(line); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// stableText is s in the stable form, one string.
func stableText(s string) string {
	return strings.Join(stableLines(s), "\n")
}

// stableLogTail is the stable form of a job log's tail: the lines of the step
// that failed, sorted. cut reports a tail cut from a longer log, whose first
// line is then a partial one and is dropped. The failing step's lines run
// from the last "##[group]Run " line (where Actions opens a step) before the
// last "##[error]" line (where it reports the step failed) through that error
// line, so neither the job's setup (its runner's names) nor the cleanup after
// it is read; a tail with no error line is read whole. Of those lines only the
// last fingerprintLogLines are kept, and they are sorted, so the output of
// tests run in parallel, which interleaves differently each run, reads the
// same.
func stableLogTail(tail string, cut bool) []string {
	raw := strings.Split(tail, "\n")
	if cut && len(raw) > 0 {
		raw = raw[1:]
	}
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if l := stableLine(line); l != "" {
			lines = append(lines, l)
		}
	}
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "##[error]") {
			end = i + 1
			break
		}
	}
	start := 0
	for i := end - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "##[group]Run ") {
			start = i
			break
		}
	}
	step := lines[start:end]
	if len(step) > fingerprintLogLines {
		step = step[len(step)-fingerprintLogLines:]
	}
	step = slices.Clone(step)
	slices.Sort(step)
	return step
}

// checkRunPrint is the stable form of one failed check run, as the
// fingerprint compares it: its name and conclusion as GitHub reports them
// (the name is one of the Project's checks.fix), its output and annotations
// in the stable form, each annotation by path and message (never by line,
// which a fix moves), and the failing step of its Actions job log, if read.
// Each field is cut to the bound its diagnostic is cut to before the stable
// form is taken.
func checkRunPrint(r ghclient.CheckRun, annotations []ghclient.CheckAnnotation, logTail string, logCut bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "check %q\nconclusion %q\n", r.Name, strings.ToLower(r.Conclusion))
	fmt.Fprintf(&b, "title %q\nsummary %q\ntext %q\n", stableText(cutBytes(r.Output.Title, 4<<10)),
		stableText(cutBytes(r.Output.Summary, 8<<10)), stableText(cutBytes(r.Output.Text, 8<<10)))
	for _, a := range annotations {
		fmt.Fprintf(&b, "annotation %q %q\n", stableText(cutBytes(a.Path, 512)), stableText(cutBytes(a.Message, 1<<10)))
	}
	if logTail != "" {
		for _, line := range stableLogTail(logTail, logCut) {
			fmt.Fprintf(&b, "log %q\n", line)
		}
	}
	return b.String()
}

// statusPrint is the stable form of one failed commit status.
func statusPrint(s ghclient.CommitStatus) string {
	return fmt.Sprintf("status %q\nstate %q\ndescription %q\n", s.Context, strings.ToLower(s.State),
		stableText(cutBytes(s.Description, 8<<10)))
}

// failureSignature fingerprints a round's failures from their stable forms,
// in any order.
func failureSignature(prints []string) string {
	sorted := slices.Clone(prints)
	slices.Sort(sorted)
	return digest([]byte(fingerprintVersion + "\n" + strings.Join(sorted, "\x00\n")))
}
