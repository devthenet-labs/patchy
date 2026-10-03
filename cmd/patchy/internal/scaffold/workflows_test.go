// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowFile is the part of a GitHub Actions workflow the publishing
// boundary is made of. yaml.v3, not a YAML 1.1 decoder, so `on` stays a key
// rather than becoming true.
type workflowFile struct {
	Name        string                 `yaml:"name"`
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

// workflowJob is one job: a called workflow (Uses) or steps of its own.
type workflowJob struct {
	If          string            `yaml:"if"`
	Uses        string            `yaml:"uses"`
	Secrets     any               `yaml:"secrets"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

// workflowStep is one step of a job.
type workflowStep struct {
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
	Run  string            `yaml:"run"`
}

// configureAWS is the action that assumes a role.
const configureAWS = "aws-actions/configure-aws-credentials@"

// workflows parses every generated workflow, keyed by file name.
func workflows(t *testing.T, files []File) map[string]workflowFile {
	t.Helper()
	out := map[string]workflowFile{}
	for _, f := range files {
		name, ok := strings.CutPrefix(f.Path, ".github/workflows/")
		if !ok {
			continue
		}
		var w workflowFile
		if err := yaml.Unmarshal(f.Data, &w); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		out[name] = w
	}
	return out
}

// TestOnlyThePublishersAssumeARole pins the publishing boundary in the
// generated workflows themselves, for both modes. Each AWS role trusts
// exactly one job_workflow_ref, so each kind's role-assuming job must live
// only in its own workflow_call callee (publish-runtime.yml,
// publish-agent.yml); the workflow_run dispatcher grants id-token to the
// callee it calls and holds no role, runs no step and passes no secret;
// the builds hold no id-token at all; and each kind is gated on its own
// repository variable.
func TestOnlyThePublishersAssumeARole(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		files := plan(t, o)
		wfs := workflows(t, files)
		build := o.runtimeWorkflow().file

		wantFiles := []string{build, "agent-image.yml", "publish-images.yml", "publish-runtime.yml", "publish-agent.yml"}
		if got := slices.Sorted(maps.Keys(wfs)); !slices.Equal(got, slices.Sorted(slices.Values(wantFiles))) {
			t.Fatalf("existing=%v: workflows %v, want %v", existing, got, wantFiles)
		}

		// Every role assumption in every workflow, as file/job → role.
		roles := map[string]string{}
		for name, w := range wfs {
			if _, ok := w.Permissions["id-token"]; ok {
				t.Errorf("%s grants id-token to every job", name)
			}
			for jobName, job := range w.Jobs {
				for _, step := range job.Steps {
					if strings.HasPrefix(step.Uses, configureAWS) {
						roles[name+"/"+jobName] = step.With["role-to-assume"]
					}
				}
			}
		}
		wantRoles := map[string]string{
			"publish-runtime.yml/publish": "${{ vars.RUNTIME_ROLE_ARN }}",
			"publish-agent.yml/publish":   "${{ vars.AGENT_ROLE_ARN }}",
		}
		if !maps.Equal(roles, wantRoles) {
			t.Errorf("existing=%v: roles are assumed in %v, want only %v", existing, roles, wantRoles)
		}

		// The builds: no OIDC, no repository variable, no role.
		for _, name := range []string{build, "agent-image.yml"} {
			for jobName, job := range wfs[name].Jobs {
				if _, ok := job.Permissions["id-token"]; ok {
					t.Errorf("%s/%s, an uncredentialed build, grants id-token", name, jobName)
				}
			}
		}

		// The dispatcher: triggered by the builds, no permission of its
		// own, and two jobs that only call their own publisher behind their
		// own gate.
		d := wfs["publish-images.yml"]
		if _, ok := d.On["workflow_run"]; !ok || len(d.On) != 1 {
			t.Errorf("publish-images.yml runs on %v, want workflow_run only", slices.Collect(maps.Keys(d.On)))
		}
		if d.Permissions == nil || len(d.Permissions) != 0 {
			t.Errorf("publish-images.yml permissions = %v, want {}", d.Permissions)
		}
		gates := map[string][2]string{
			"runtime": {"./.github/workflows/publish-runtime.yml", PreviewPublishEnabled},
			"agent":   {"./.github/workflows/publish-agent.yml", AgentPublishEnabled},
		}
		if got := slices.Sorted(maps.Keys(d.Jobs)); !slices.Equal(got, []string{"agent", "runtime"}) {
			t.Errorf("publish-images.yml jobs %v, want agent and runtime", got)
		}
		for jobName, want := range gates {
			job := d.Jobs[jobName]
			if job.Uses != want[0] || len(job.Steps) != 0 || job.Secrets != nil {
				t.Errorf("publish-images.yml/%s: uses %q with %d steps and secrets %v; want only a call of %s",
					jobName, job.Uses, len(job.Steps), job.Secrets, want[0])
			}
			if !strings.Contains(job.If, "vars."+want[1]+" == 'true'") {
				t.Errorf("publish-images.yml/%s is not gated on %s: %q", jobName, want[1], job.If)
			}
			for other, otherWant := range gates {
				if other != jobName && strings.Contains(job.If, otherWant[1]) {
					t.Errorf("publish-images.yml/%s is gated on the other kind's %s", jobName, otherWant[1])
				}
			}
		}

		// The callees: called only, by the dispatcher, and blind to the
		// other kind's role and repository.
		for _, kind := range []string{"runtime", "agent"} {
			name := "publish-" + kind + ".yml"
			w := wfs[name]
			if _, ok := w.On["workflow_call"]; !ok || len(w.On) != 1 {
				t.Errorf("%s runs on %v, want workflow_call only", name, slices.Collect(maps.Keys(w.On)))
			}
			if len(w.Jobs) != 1 || w.Jobs["publish"].Permissions["id-token"] != "write" {
				t.Errorf("%s: want one publish job holding id-token: write, got %+v", name, w.Jobs)
			}
		}
		for _, f := range files {
			name, ok := strings.CutPrefix(f.Path, ".github/workflows/")
			if !ok {
				continue
			}
			data := string(f.Data)
			for _, v := range []string{"RUNTIME_ROLE_ARN", "RUNTIME_IMAGE_REPOSITORY"} {
				if strings.Contains(data, v) != (name == "publish-runtime.yml") {
					t.Errorf("%s: names %s = %v", name, v, strings.Contains(data, v))
				}
			}
			for _, v := range []string{"AGENT_ROLE_ARN", "AGENT_IMAGE_REPOSITORY"} {
				if strings.Contains(data, v) != (name == "publish-agent.yml") {
					t.Errorf("%s: names %s = %v", name, v, strings.Contains(data, v))
				}
			}
			if strings.Contains(data, "secrets.") || strings.Contains(data, "pull_request_target") {
				t.Errorf("%s uses a secret or pull_request_target", name)
			}
		}
	}
}

// TestPublishersCheckOutOnlyThemselves: a publisher checks out its own
// scripts at the workflow's commit, sparsely, and nothing else, and asks
// for the role only after the guard and the OCI validation have run.
func TestPublishersCheckOutOnlyThemselves(t *testing.T) {
	wfs := workflows(t, plan(t, testOptions(false)))
	for _, name := range []string{"publish-runtime.yml", "publish-agent.yml"} {
		steps := wfs[name].Jobs["publish"].Steps
		first := steps[0]
		if !strings.HasPrefix(first.Uses, "actions/checkout@") || first.With["ref"] != "${{ github.workflow_sha }}" ||
			first.With["sparse-checkout"] != ".github/actions/publish" || first.With["persist-credentials"] != "false" {
			t.Errorf("%s: first step %+v is not a sparse checkout of the publisher at the workflow's commit", name, first)
		}
		role := slices.IndexFunc(steps, func(s workflowStep) bool { return strings.HasPrefix(s.Uses, configureAWS) })
		for _, before := range []string{"check-config.sh", "guard.cjs", "validate_oci.py"} {
			at := slices.IndexFunc(steps, func(s workflowStep) bool {
				return strings.Contains(s.Run, before) || strings.Contains(s.With["script"], before)
			})
			if at < 0 || at > role {
				t.Errorf("%s: %s runs at step %d, not before the role at step %d", name, before, at, role)
			}
		}
		for _, s := range steps[1:] {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				t.Errorf("%s checks out a second time", name)
			}
		}
	}
}
