// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/transcript"
)

// The in-pod report repair, end to end. The kubelet runs the real
// agent-runner for build Jobs, with hack/fake-agent/claude in the model's
// place, so a build's report check, the repair in the agent's own session,
// the tree guard, commit.sh and the changeset are the product's own, and
// intent-controller collects what they emit as it would in a cluster. The
// plan still comes from hack/fake-agent.

// The scripted claude's spend, per run: the build's first run, and each
// resumed one (whose total_cost_usd the script makes cumulative, as the CLI
// does).
var (
	scriptedFirst = v1alpha1.UsageSummary{
		InputTokens: 1000, CacheCreationTokens: 200, CacheReadTokens: 3000, OutputTokens: 400,
	}
	scriptedRepair = v1alpha1.UsageSummary{
		InputTokens: 10, CacheCreationTokens: 20, CacheReadTokens: 3000, OutputTokens: 30,
	}
)

// scalarNotesDetail is the build report's refusal for the scripted notes
// written as one string.
const scalarNotesDetail = "report: frontmatter: yaml: unmarshal errors:\n" +
	"  line 8: cannot unmarshal !!str `What wa...` into []string"

// startRepairIntent runs the intent stack with the real agent-runner for
// builds, the scripted claude told to write the report given and to repair
// it as given, and files an intent and approves its plan.
func startRepairIntent(t *testing.T, report, repair string) (*intentEnv, string) {
	t.Helper()
	e := startIntents(t)
	e.kubelet.useAgentRunner(t, "build")
	e.kubelet.setAgentEnv("PATCHY_FAKE_CLAUDE_REPORT", report)
	e.kubelet.setAgentEnv("PATCHY_FAKE_CLAUDE_REPAIR", repair)
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	return e, name
}

// scriptedCall is one invocation the scripted claude recorded in a pod.
type scriptedCall struct{ mode, session, maxTurns, tools string }

// scriptedCalls reads what the scripted claude recorded in a run's pod.
func scriptedCalls(t *testing.T, r agentRun) []scriptedCall {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "calls"))
	if err != nil {
		t.Fatalf("read the scripted claude's calls in %s: %v", r.Job.Name, err)
	}
	var calls []scriptedCall
	for line := range strings.Lines(string(raw)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), " ", 4)
		for len(f) < 4 {
			f = append(f, "")
		}
		calls = append(calls, scriptedCall{f[0], f[1], f[2], f[3]})
	}
	return calls
}

// buildAttempt waits for the build Job of attempt n to have run, and for
// its run to be collected.
func (e *intentEnv) buildAttempt(t *testing.T, name string, n int32) (agentRun, *v1alpha1.IntentRun) {
	t.Helper()
	runName := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, n)
	job := e.kubelet.waitRun(t, "build attempt "+runName+" to run", func(r agentRun) bool {
		return r.Env["PATCHY_PHASE"] == "build" && r.Env["PATCHY_FINDING"] == runName
	})
	var run v1alpha1.IntentRun
	eventually(t, "build attempt "+runName+" to be collected", func() bool {
		err := e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: runName}, &run)
		return err == nil && run.Status.Outcome != ""
	})
	return job, &run
}

// checkOneTranscript checks the pod's transcript turns are numbered 1, 2, 3,
// … across every run of the stage: one recorder for the first run and its
// repairs, or the status page's rows would collide.
func checkOneTranscript(t *testing.T, r agentRun) {
	t.Helper()
	var seqs []int
	for line := range bytes.SplitSeq(r.Stdout, []byte("\n")) {
		if turn, ok := transcript.Decode(line); ok {
			seqs = append(seqs, turn.Seq)
		}
	}
	if len(seqs) == 0 {
		t.Fatalf("%s printed no transcript", r.Job.Name)
	}
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("%s transcript seqs = %v, want 1..%d with no repeat", r.Job.Name, seqs, len(seqs))
		}
	}
}

// TestIntentBuildReportRepaired: a build whose report patchy refuses — notes
// written as one string, a slip that threw a build away live — is repaired in
// the same session and pushed. The resumed run continued the first run's
// session under its tool grammar, the run records both runs' spend without
// counting the resumed run's cumulative cost twice, and the pod's one
// transcript stays numbered.
func TestIntentBuildReportRepaired(t *testing.T) {
	e, name := startRepairIntent(t, "scalar-notes", "fix")
	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "ok" {
		t.Fatalf("build run = %s %q, want ok after the repair", run.Status.Outcome, run.Status.Detail)
	}

	calls := scriptedCalls(t, job)
	if len(calls) != 2 || calls[0].mode != "first" || calls[1].mode != "resume" ||
		calls[1].session != calls[0].session || calls[1].tools != calls[0].tools || calls[1].maxTurns != "6" {
		t.Errorf("claude calls = %+v, want the first run, then one resume of its session under its tools, "+
			"capped at 6 turns", calls)
	}

	u := run.Status.Usage
	if u.InputTokens != scriptedFirst.InputTokens+scriptedRepair.InputTokens ||
		u.OutputTokens != scriptedFirst.OutputTokens+scriptedRepair.OutputTokens ||
		u.CacheReadTokens != scriptedFirst.CacheReadTokens+scriptedRepair.CacheReadTokens ||
		u.CacheCreationTokens != scriptedFirst.CacheCreationTokens+scriptedRepair.CacheCreationTokens {
		t.Errorf("usage = %+v, want both runs' tokens summed", u)
	}
	m, ok := model.ModelByID(model.Builtins(), job.Env["PATCHY_REMEDIATE_MODEL"])
	if !ok {
		t.Fatalf("build model %q is not in the registry", job.Env["PATCHY_REMEDIATE_MODEL"])
	}
	repairCost := model.UsageCostUSD(m, int(scriptedRepair.InputTokens), int(scriptedRepair.CacheReadTokens),
		int(scriptedRepair.CacheCreationTokens), int(scriptedRepair.OutputTokens))
	if want := agentresult.FormatCost(0.1 + *repairCost); u.CostUSD != want {
		t.Errorf("cost = %s, want %s: the first run's 0.1 plus the repair priced, never the resumed run's "+
			"cumulative 0.11 added on", u.CostUSD, want)
	}
	checkOneTranscript(t, job)
	if run.Status.Transcript == nil {
		t.Error("the build's transcript was not persisted")
	}

	in := e.waitPhase(t, name, v1alpha1.IntentInReview)
	run = e.run(t, run.Name)
	commit, ok := e.gh.CommitOf(run.Status.PushedCommit)
	if !ok || len(commit.Files) != 1 || string(commit.Files["VERSION"]) != "0.1.0\n" {
		t.Errorf("pushed commit = %+v, want the build's VERSION, built before the repair", commit)
	}
	if len(in.Status.PullRequests) != 1 {
		t.Errorf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}
}

// TestIntentBuildLongNoteTruncated: the 574-character note that threw
// overdub-10's build away is cut to the bound, not refused, so the build is
// pushed from its first run with no repair round.
func TestIntentBuildLongNoteTruncated(t *testing.T) {
	e, name := startRepairIntent(t, "long-note", "keep")
	job, run := e.buildAttempt(t, name, 1)
	if run.Status.Outcome != "ok" {
		t.Fatalf("build run = %s %q, want ok with no repair", run.Status.Outcome, run.Status.Detail)
	}
	if calls := scriptedCalls(t, job); len(calls) != 1 || calls[0].mode != "first" {
		t.Errorf("claude calls = %+v, want the first run alone", calls)
	}
	if in := e.waitPhase(t, name, v1alpha1.IntentInReview); len(in.Status.PullRequests) != 1 {
		t.Errorf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}
}

// waitFailed waits for the Intent to fail, as one whose every build attempt
// was refused does.
func (e *intentEnv) waitFailed(t *testing.T, name string) {
	t.Helper()
	eventually(t, "intent "+name+" to fail", func() bool {
		return e.intent(t, name).Status.Phase == v1alpha1.IntentFailed
	})
}

// TestIntentBuildReportNotRepaired: a report the agent leaves refused
// through both rounds ends the attempt report_invalid, with the report's
// reason and how the repair went; the retry is told so, and nothing is
// pushed.
func TestIntentBuildReportNotRepaired(t *testing.T) {
	e, name := startRepairIntent(t, "scalar-notes", "keep")
	job, run := e.buildAttempt(t, name, 1)
	want := scalarNotesDetail + " (not repaired in 2 rounds)"
	if run.Status.Outcome != "report_invalid" || run.Status.Detail != want {
		t.Errorf("build attempt 1 = %s %q, want report_invalid %q", run.Status.Outcome, run.Status.Detail, want)
	}
	calls := scriptedCalls(t, job)
	if len(calls) != 3 || calls[1].mode != "resume" || calls[2].mode != "resume" {
		t.Errorf("claude calls = %+v, want the first run and two resumes", calls)
	}
	checkOneTranscript(t, job)

	retry, _ := e.buildAttempt(t, name, 2)
	if prev := retry.Env["PATCHY_PREVIOUS_ATTEMPT"]; !strings.Contains(prev, `"outcome":"report_invalid"`) ||
		!strings.Contains(prev, "not repaired in 2 rounds") {
		t.Errorf("the retry's previous attempt = %s, want the refusal and the failed repair", prev)
	}
	e.waitFailed(t, name)
	if writes := e.gh.RefWrites(); len(writes) != 0 {
		t.Errorf("ref writes = %+v, want nothing pushed", writes)
	}
}

// TestIntentBuildRepairTouchingCodeRefused: a repair that changes the tree
// — nothing it changes was built or tested — is refused whole, even though
// the report it wrote is valid: the attempt ends report_invalid naming the
// change, with no second round, and nothing is pushed.
func TestIntentBuildRepairTouchingCodeRefused(t *testing.T) {
	e, name := startRepairIntent(t, "scalar-notes", "touch")
	job, run := e.buildAttempt(t, name, 1)
	want := scalarNotesDetail + " (repair refused in round 1: it changed the working tree: VERSION, and a repair " +
		"may change nothing but the report and commit.sh)"
	if run.Status.Outcome != "report_invalid" || run.Status.Detail != want {
		t.Errorf("build attempt 1 = %s %q, want report_invalid %q", run.Status.Outcome, run.Status.Detail, want)
	}
	if calls := scriptedCalls(t, job); len(calls) != 2 || !slices.ContainsFunc(calls[1:], func(c scriptedCall) bool {
		return c.mode == "resume"
	}) {
		t.Errorf("claude calls = %+v, want the first run and one refused repair", calls)
	}
	e.waitFailed(t, name)
	if writes := e.gh.RefWrites(); len(writes) != 0 {
		t.Errorf("ref writes = %+v, want nothing pushed", writes)
	}
}
