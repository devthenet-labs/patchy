// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package agentrun orchestrates the coding-agent stage inside the Job pod:
// render the prompt, drive the harness under a token-budget kill switch, and
// report the stage as an envelope event on stdout. The investigate phase
// runs the analysis and emits the verdict without ever deciding
// continuation; the remediate phase executes a controller-provided analysis,
// then verifies and packages the resulting changeset.
//
// An intent adds two phases beside them. Plan reads the request and the
// tree read-only and emits the plan report as an envelope plan event, for a
// human to approve; build follows the approved plan with the workspace
// writable — for the first build and every revise round — and packages its
// changeset through the remediation path's own helpers, as a remediation
// event. They run on the Finding stages' configuration (plan on
// investigate's, build on remediate's) and on brokered claude only.
//
// An intent may change several repositories. Its plan Job then holds every
// tree — the planning repository as the working tree, the others read-only
// under the workspace's repos/, with no git history — and a manifest of
// them, which the plan stage checks whole before any agent runs and holds
// the plan's repositories to afterwards. Each build is in one repository
// of such a plan and learns which from PATCHY_REPO alone, matched against
// the approved plan's own list: no new configuration key exists for either.
//
// The runner trusts the repository over the agent: a remediation or build
// report claiming success is downgraded unless commit.sh ran cleanly and
// left real commits on the branch. It never talks to a forge — the pod has
// no forge credentials by design; the controllers own every side effect.
//
// A report the runner refuses, missing or failing its parser, does not end
// the stage at once on a harness that can continue a session (claude, and
// the fake harness that stands in for it): the agent is asked, in its own
// session (harness.Resumer), to repair the report, for at most
// repairRounds rounds, each within what the stage has left of its turns,
// output tokens and wall clock, sharing the stage's transcript and adding
// to its recorded spend (repair.go). On a stage that writes the working
// tree a repair may change nothing in the clone, and one that does is
// refused. Only a report still refused after that ends the stage
// report_missing or report_invalid, with how the repair went after the
// reason. Codex and copilot cannot resume, so their stages end on the
// first refusal, as before.
//
// While a run's CLI runs a foreground shell command, the runner prints that
// command's output as it is produced, as PATCHY-OUTPUT chunks beside the
// turns (output.go, on a harness.TaskWatcher: claude, and the fake harness
// whose fixtures write no such file). The CLI's own file is read, never its
// stream, so the output is bounded per command and per process, and it is
// live only: never persisted, never a turn, never progress to the idle
// watchdog. All of stdout goes through one lock, so the three streams'
// lines never interleave.
package agentrun
