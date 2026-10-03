// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// jobRun is what one run of a GitHub Actions job writes into its log that has
// nothing to do with why its step failed: when it ran, on which runner and
// machine, from which commits, through which temporary directories, and how
// long each step took.
type jobRun struct {
	start                time.Time
	runner               int
	machine              string
	image                string
	head, base, merge    string
	homeTemp, cleanTemp  string
	stepMillis, testSecs int
}

// liveRun is the first failing run of the live CI-fix demo (2026-10-03):
// patchy-preview-demo#9's changelog check at 7f4d2aae.
var liveRun = jobRun{
	start: time.Date(2026, 10, 3, 7, 0, 12, 345678900, time.UTC), runner: 1000123456,
	machine: "runnervmf4ws1", image: "20260928.1.0",
	head:     "7f4d2aae9c3b1e5f6a7b8c9d0e1f2a3b4c5d6e7f",
	base:     "c8c683651a2b3c4d5e6f708192a3b4c5d6e7f809",
	merge:    "3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d",
	homeTemp: "1f2e3d4c-5b6a-4798-8a7b-6c5d4e3f2a1b", cleanTemp: "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4",
	stepMillis: 412, testSecs: 3,
}

// rerun is the same check failing the same way after a fix round pushed a
// commit that did not fix it: another runner, machine, image, commit and
// temporary directory, other times and durations.
var rerun = jobRun{
	start: time.Date(2026, 10, 3, 7, 3, 29, 1200, time.UTC), runner: 1000654321,
	machine: "runnervmab12c", image: "20261001.2.0",
	head:     "ff754761aa0bb1cc2dd3ee4ff5a6b7c8d9e0f1a2",
	base:     "c8c683651a2b3c4d5e6f708192a3b4c5d6e7f809",
	merge:    "0aa1bb2cc3dd4ee5ff6a7b8c9d0e1f2a3b4c5d6e",
	homeTemp: "0a1b2c3d-4e5f-4061-8728-394a5b6c7d8e", cleanTemp: "aabbccdd-eeff-4011-9223-344556677889",
	stepMillis: 38, testSecs: 12,
}

// changelogLog renders a run of the changelog job as GitHub's job log API
// returns it: every line led by its time stamp, the first by a byte order
// mark, the setup and checkout steps before the failing one, and the cleanup
// after it. failure is what the failing step printed.
func changelogLog(r jobRun, failure string) string {
	at := r.start
	var b strings.Builder
	line := func(format string, args ...any) {
		if b.Len() == 0 {
			b.WriteString("\ufeff")
		}
		fmt.Fprintf(&b, "%s %s\n", at.Format("2006-01-02T15:04:05.0000000Z"), fmt.Sprintf(format, args...))
		at = at.Add(time.Duration(r.stepMillis) * time.Microsecond)
	}
	line("Current runner version: '2.328.0'")
	line("Runner name: 'GitHub Actions %d'", r.runner)
	line("Runner group name: 'GitHub Actions'")
	line("Machine name: '%s'", r.machine)
	line("##[group]Runner Image")
	line("Image: ubuntu-24.04")
	line("Version: %s", r.image)
	line("Included Software: https://github.com/actions/runner-images/blob/ubuntu24/%s/images/ubuntu/Ubuntu2404-Readme.md",
		r.image)
	line("##[endgroup]")
	line("Secret source: Actions")
	line("Download action repository 'actions/checkout@v4' (SHA:08eba0b27e820071cde6df949e0beb9ba4906955)")
	line("Complete job name: changelog")
	line("##[group]Run actions/checkout@v4")
	line("with:")
	line("  repository: devthenet-labs/patchy-preview-demo")
	line("##[endgroup]")
	line("Syncing repository: devthenet-labs/patchy-preview-demo")
	line("Temporarily overriding HOME='/home/runner/work/_temp/%s' before making global git config changes", r.homeTemp)
	line("[command]/usr/bin/git -c protocol.version=2 fetch --no-tags --prune --no-recurse-submodules --depth=1 "+
		"origin +%s:refs/remotes/pull/9/merge", r.head)
	line("HEAD is now at %s Merge %s into %s", r.merge[:7], r.head, r.base)
	line("##[group]Run ./scripts/check-changelog.sh 9")
	line("./scripts/check-changelog.sh 9")
	line("shell: /usr/bin/bash -e {0}")
	line("##[endgroup]")
	line("checked CHANGELOG.md in %d.%03ds", r.testSecs, r.stepMillis)
	line("%s", failure)
	line("##[error]Process completed with exit code 1.")
	line("Post job cleanup.")
	line("Temporarily overriding HOME='/home/runner/work/_temp/%s' before making global git config changes", r.cleanTemp)
	line("Cleaning up orphan processes")
	return b.String()
}

const liveFailure = "CHANGELOG.md has no entry for #9: add a line under ## Unreleased"

// changelogPrint is the stable form of the changelog check failing with log.
func changelogPrint(log string) string {
	return checkRunPrint(ghclient.CheckRun{ID: 111151376149, Name: "changelog", Conclusion: "failure",
		Output: ghclient.CheckOutput{Title: "changelog", Summary: "Process completed with exit code 1."}},
		[]ghclient.CheckAnnotation{{Path: ".github", Line: 1, Message: "Process completed with exit code 1."}},
		log, false)
}

// TestCheckFingerprintIgnoresWhatEachRunDiffersBy is the live regression:
// in the 2026-10-03 CI-fix demo the repeated-failure stop could never fire,
// because the fingerprint was taken over the job log as read, every line of
// which carries the moment it was written. The same failure from another run
// must fingerprint the same; another failure, or another check, must not.
func TestCheckFingerprintIgnoresWhatEachRunDiffersBy(t *testing.T) {
	first, again := changelogLog(liveRun, liveFailure), changelogLog(rerun, liveFailure)
	if first == again {
		t.Fatal("the two runs' logs are identical; the test proves nothing")
	}
	sig := func(log string) string { return failureSignature([]string{changelogPrint(log)}) }
	if sig(first) != sig(again) {
		t.Errorf("the same failure from another run fingerprints differently:\n%s\n---\n%s",
			changelogPrint(first), changelogPrint(again))
	}
	other := changelogLog(rerun, "CHANGELOG.md: the entry for #9 is not under ## Unreleased")
	if sig(first) == sig(other) {
		t.Error("a different failure fingerprints the same")
	}
	renamed := checkRunPrint(ghclient.CheckRun{Name: "test", Conclusion: "failure"}, nil, first, false)
	if sig(first) == failureSignature([]string{renamed}) {
		t.Error("the same log under another check fingerprints the same")
	}
}

// TestStableLogTailReadsTheFailingStep: only the failing step's lines are
// kept, in an order its parallel output cannot change; a tail cut from a
// longer log drops its partial first line.
func TestStableLogTailReadsTheFailingStep(t *testing.T) {
	got := stableLogTail(changelogLog(liveRun, liveFailure), false)
	want := []string{
		"##[error]Process completed with exit code 1.",
		"##[group]Run ./scripts/check-changelog.sh 9",
		"##[endgroup]",
		"./scripts/check-changelog.sh 9",
		"CHANGELOG.md has no entry for #9: add a line under ## Unreleased",
		"checked CHANGELOG.md in <dur>",
		"shell: /usr/bin/bash -e {0}",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("stable log tail =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	parallel := func(order []string) string {
		return "##[group]Run go test ./...\n" + strings.Join(order, "\n") +
			"\nFAIL\n##[error]Process completed with exit code 1.\n"
	}
	a := stableLogTail(parallel([]string{"--- FAIL: TestA (0.01s)", "--- PASS: TestB (0.20s)"}), false)
	b := stableLogTail(parallel([]string{"--- PASS: TestB (1.03s)", "--- FAIL: TestA (0.00s)"}), false)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("interleaved parallel output reads differently: %q, %q", a, b)
	}
	if got := stableLogTail("ntial line\n"+parallel([]string{"--- FAIL: TestA (0.01s)"}), true); strings.Contains(
		strings.Join(got, "\n"), "ntial") {
		t.Errorf("a cut tail kept its partial first line: %q", got)
	}
	if got := stableLogTail("no step at all\nFAIL", false); strings.Join(got, "\n") != "FAIL\nno step at all" {
		t.Errorf("a tail with no step or error line is read whole: %q", got)
	}
}

// TestCheckFingerprintKeepsValues pins which numbers count. A fix that moves
// only a value (a coverage gate creeping up, a count getting closer) is
// progress, not the same failure: every number used to be zeroed, so such a
// round stopped the intent with RepeatedFailure while check-fix rounds were
// left. The same failure with its line moved, or in another time, is the same.
func TestCheckFingerprintKeepsValues(t *testing.T) {
	sig := func(failure string) string {
		return failureSignature([]string{changelogPrint(changelogLog(liveRun, failure))})
	}
	for _, tt := range []struct {
		name, a, b string
		same       bool
	}{
		{"coverage rose", "coverage: 71.3% of statements, want >= 80%", "coverage: 76.1% of statements, want >= 80%",
			false},
		{"closer count", "version_test.go:12: got 3, want 5", "version_test.go:12: got 4, want 5", false},
		{"the line moved", "version_test.go:12: got 3, want 5", "version_test.go:14: got 3, want 5", true},
		{"another duration", "--- FAIL: TestVersion (0.01s)", "--- FAIL: TestVersion (1.20s)", true},
		{"another goroutine", "goroutine 17 [running]:", "goroutine 9 [running]:", true},
	} {
		if same := sig(tt.a) == sig(tt.b); same != tt.same {
			t.Errorf("%s: %q and %q fingerprint the same: %v, want %v", tt.name, tt.a, tt.b, same, tt.same)
		}
	}
}

// TestStableLine pins each volatile token's placeholder, that a word made
// only of hex letters is kept, and that a number of no volatile shape is
// kept.
func TestStableLine(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"2026-10-03T07:00:29.1234567Z ok", "ok"},
		{"\ufeff2026-10-03T07:00:29.1234567Z first", "first"},
		{"at 2026-10-03 07:00:29,123+02:00 done", "at <time> done"},
		{"tmp /home/runner/work/_temp/1F2E3D4C-5B6A-4798-8A7B-6C5D4E3F2A1B x", "tmp /home/runner/work/_temp/<uuid> x"},
		{"HEAD is now at 3a4b5c6 Merge", "HEAD is now at <hex> Merge"},
		{"the patch was acceded to", "the patch was acceded to"},
		{"--- FAIL: TestA2 (0.123s)", "--- FAIL: TestA2 (<dur>)"},
		{"ok  \tacme/app\t1.52s", "ok acme/app <dur>"},
		{"took 412ms, then 1m2.5s", "took <dur>, then <dur>"},
		{"build /tmp/go-build1234567890/b001", "build /tmp/go-build<n>/b001"},
		{"Runner name: 'GitHub Actions 1000123456'", "Runner name: 'GitHub Actions <n>'"},
		{"version_test.go:12: got 404, want 200", "version_test.go:<line>: got 404, want 200"},
		{"src/main.rs:4:7: error", "src/main.rs:<line>: error"},
		{"goroutine 17 [running]:", "goroutine <n> [running]:"},
		{"main.f(0xc000123456, 0x1a)", "main.f(<addr>, <addr>)"},
		{"[12:04:59] lint failed", "[<time>] lint failed"},
		{"coverage: 71.3% of statements, want >= 80%", "coverage: 71.3% of statements, want >= 80%"},
		{"got 3, want 5", "got 3, want 5"},
		{"  spaced \t out  ", "spaced out"},
		{"crlf\r", "crlf"},
	} {
		if got := stableLine(tt.in); got != tt.want {
			t.Errorf("stableLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCheckFingerprintSeededProperty: over random runs of one job, a failure
// fingerprints the same whatever the runs differ by, and differently from
// another failure. Seeded, so the gate is deterministic.
func TestCheckFingerprintSeededProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(0xf1a9e7))
	hexOf := func(n int) string {
		const digits = "0123456789abcdef"
		b := make([]byte, n)
		for i := range b {
			b[i] = digits[rng.Intn(len(digits))]
		}
		if !strings.ContainsAny(string(b), "0123456789") {
			b[0] = '7'
		}
		return string(b)
	}
	uuid := func() string {
		return hexOf(8) + "-" + hexOf(4) + "-4" + hexOf(3) + "-a" + hexOf(3) + "-" + hexOf(12)
	}
	random := func() jobRun {
		return jobRun{
			start: time.Date(2026, 1+time.Month(rng.Intn(12)), 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), 0,
				rng.Intn(1e9), time.UTC),
			runner: 1 + rng.Intn(2_000_000_000), machine: "runnervm" + hexOf(5),
			image: fmt.Sprintf("%d.%d.%d", 20260000+rng.Intn(9999), rng.Intn(9), rng.Intn(9)),
			head:  hexOf(40), base: hexOf(40), merge: hexOf(40), homeTemp: uuid(), cleanTemp: uuid(),
			stepMillis: 1 + rng.Intn(999), testSecs: rng.Intn(600),
		}
	}
	failures := []string{
		liveFailure,
		"version_test.go:12: got 404, want 200",
		"--- FAIL: TestVersionHandler (0.00s)",
		"go: updates to go.mod needed; to update it: go mod tidy",
	}
	for i := range 300 {
		failure := failures[rng.Intn(len(failures))]
		a := failureSignature([]string{changelogPrint(changelogLog(random(), failure))})
		b := failureSignature([]string{changelogPrint(changelogLog(random(), failure))})
		if a != b {
			t.Fatalf("case %d: %q fingerprints differently in two runs", i, failure)
		}
		other := failures[(slices.Index(failures, failure)+1+rng.Intn(len(failures)-1))%len(failures)]
		if c := failureSignature([]string{changelogPrint(changelogLog(random(), other))}); c == a {
			t.Fatalf("case %d: %q and %q fingerprint the same", i, failure, other)
		}
	}
}

// TestRepeatedCIFailureFromAnotherRunBlocks is the live regression end to
// end: a check-fix round pushes, the same check fails again the same way in a
// new job run (new times, runner, commit), and the Intent stops with
// RepeatedFailure instead of starting another round.
func TestRepeatedCIFailureFromAnotherRunBlocks(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"changelog"}
	e := newEnv(t, project)
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	failAt := func(head string, id, runID, jobID int64, r jobRun) {
		e.gh.checks[head] = []ghclient.CheckRun{{ID: id, Name: "changelog", HeadSHA: head,
			Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
			DetailsURL: fmt.Sprintf("https://github.com/acme/app/actions/runs/%d/job/%d", runID, jobID),
			Output:     ghclient.CheckOutput{Title: "changelog", Summary: "Process completed with exit code 1."}}}
		e.gh.workflowJobs[runID] = []ghclient.WorkflowJob{{ID: jobID, CheckRunID: id, HeadSHA: head,
			Name: "changelog", Conclusion: "failure"}}
		e.gh.jobLogs[jobID] = changelogLog(r, liveFailure)
	}
	failAt(in.Status.PullRequests[0].HeadSHA, 111151376149, 18001, 51001, liveRun)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.CheckFixes != 1 {
		t.Fatalf("check fixes = %d, want the first round complete", in.Status.CheckFixes)
	}
	failAt(in.Status.PullRequests[0].HeadSHA, 111151399999, 18002, 51002, rerun)
	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 1 {
		t.Errorf("revise runs = %d: the repeated failure started another round", n)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionChecksFailing); c == nil ||
		c.Reason != "RepeatedFailure" {
		t.Errorf("ChecksFailing = %+v, want RepeatedFailure", c)
	}
}
