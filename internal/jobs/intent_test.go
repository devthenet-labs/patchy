// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestNameForKinds pins the Job name of each kind for one owner: the Finding
// kinds' names are the ones every existing Job and golden carries, and an
// intent run gets its own discriminator beside them.
func TestNameForKinds(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"investigation", "patchy-f41d59eea6-inv-a1"},
		{"remediation", "patchy-f41d59eea6-rem-a1"},
		{"intent", "patchy-f41d59eea6-int-a1"},
	}
	for _, tt := range tests {
		if got := NameFor("finding-abc123def0-1", tt.kind, 1); got != tt.want {
			t.Errorf("NameFor(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	// An IntentRun name at its longest still makes a short, DNS-safe name.
	run := "target-" + strings.Repeat("9", 40) + "-rev999-" + strings.Repeat("r", 16) + "-a16"
	if got := NameFor(run, "intent", 16); len(got) > 63 || !dns1123.MatchString(got) ||
		!strings.HasSuffix(got, "-int-a16") {
		t.Errorf("NameFor(%q) = %q, want a DNS-1123 name of at most 63 characters ending -int-a16", run, got)
	}
}

func TestStageEnvNames(t *testing.T) {
	tests := []struct {
		phase              string
		wantHarness, model string
	}{
		{"investigate", "PATCHY_INVESTIGATE_HARNESS", "PATCHY_INVESTIGATE_MODEL"},
		{"remediate", "PATCHY_REMEDIATE_HARNESS", "PATCHY_REMEDIATE_MODEL"},
		{"plan", "PATCHY_INVESTIGATE_HARNESS", "PATCHY_INVESTIGATE_MODEL"},
		{"build", "PATCHY_REMEDIATE_HARNESS", "PATCHY_REMEDIATE_MODEL"},
	}
	for _, tt := range tests {
		h, m := stageEnvNames(tt.phase)
		if h != tt.wantHarness || m != tt.model {
			t.Errorf("stageEnvNames(%s) = %s, %s; want %s, %s", tt.phase, h, m, tt.wantHarness, tt.model)
		}
	}
}

// container returns the named container of a Job's pod, init or agent.
func container(t *testing.T, job *batchv1.Job, name string) corev1.Container {
	t.Helper()
	pod := job.Spec.Template.Spec
	for _, ct := range slices.Concat(pod.InitContainers, pod.Containers) {
		if ct.Name == name {
			return ct
		}
	}
	t.Fatalf("job %s has no %s container", job.Name, name)
	return corev1.Container{}
}

// withoutPhase is a container env with the PATCHY_PHASE entry's value
// blanked, so two stages' envs compare on everything else.
func withoutPhase(env []corev1.EnvVar) []corev1.EnvVar {
	out := slices.Clone(env)
	for i := range out {
		if out[i].Name == "PATCHY_PHASE" {
			out[i].Value = ""
		}
	}
	return out
}

// TestIntentJobsReuseFindingStages proves the intent stages ride the Job
// exactly as their Finding counterparts do: a plan Job is an investigation
// Job and a build Job a remediation Job — same prepare container and
// script, same agent command, same env in the same order, the same blanks on
// a repository image — but for PATCHY_PHASE, the kind label and the -int-
// name. With the goldens pinning the Finding Jobs themselves, nothing a
// Finding Job carries moved to make room for them.
func TestIntentJobsReuseFindingStages(t *testing.T) {
	remediation := func(spec Spec) Spec {
		spec.Phase, spec.Kind = "remediate", "remediation"
		spec.Owner = "finding-abc123def0-1-rem-1"
		spec.InvestigationMarkdown = "# Approved plan\n"
		spec.MaxTurns, spec.TokenBudget = 150, 800000
		return spec
	}
	configs := []struct {
		name string
		cfg  func() Config
		spec func() Spec
	}{
		{"default", testConfig, testSpec},
		{"brokered", brokeredConfig, testSpec},
		{"repository image", injectedConfig, injectedSpec},
	}
	stages := []struct {
		finding, intent string
		spec            func(Spec) Spec
	}{
		{"investigate", "plan", func(s Spec) Spec { return s }},
		{"remediate", "build", remediation},
	}
	for _, c := range configs {
		for _, st := range stages {
			t.Run(c.name+"/"+st.intent, func(t *testing.T) {
				findingSpec := st.spec(c.spec())
				intentSpec := findingSpec
				intentSpec.Phase, intentSpec.Kind = st.intent, "intent"

				findingJob := buildJobForTest(t, c.cfg(), findingSpec)
				intentJob := buildJobForTest(t, c.cfg(), intentSpec)

				if !strings.HasSuffix(intentJob.Name, "-int-a1") {
					t.Errorf("intent Job name = %q, want the -int- discriminator", intentJob.Name)
				}
				if got := intentJob.Labels[v1alpha1.LabelRunKind]; got != "intent" {
					t.Errorf("run-kind label = %q, want intent", got)
				}
				if got := envMap(container(t, intentJob, agentContainerName))["PATCHY_PHASE"].Value; got != st.intent {
					t.Errorf("PATCHY_PHASE = %q, want %q", got, st.intent)
				}

				// Everything else is the Finding stage's.
				if !reflect.DeepEqual(container(t, intentJob, initContainerName),
					container(t, findingJob, initContainerName)) {
					t.Error("the prepare container differs from the Finding stage's")
				}
				agent, findingAgent := container(t, intentJob, agentContainerName),
					container(t, findingJob, agentContainerName)
				if !reflect.DeepEqual(withoutPhase(agent.Env), withoutPhase(findingAgent.Env)) {
					t.Errorf("agent env differs beyond PATCHY_PHASE:\n%s\nvs the %s stage's\n%s",
						marshal(t, agent.Env), st.finding, marshal(t, findingAgent.Env))
				}
				agent.Env, findingAgent.Env = nil, nil
				if !reflect.DeepEqual(agent, findingAgent) {
					t.Error("the agent container differs from the Finding stage's beyond its env")
				}
				if !reflect.DeepEqual(intentJob.Annotations, findingJob.Annotations) {
					t.Errorf("annotations differ from the Finding stage's:\n%s\nvs\n%s",
						marshal(t, intentJob.Annotations), marshal(t, findingJob.Annotations))
				}
				// The handoff volume mounts the per-Job Secret, named for the Job.
				rename := strings.NewReplacer(intentJob.Name, findingJob.Name)
				if got, want := rename.Replace(marshal(t, intentJob.Spec.Template.Spec.Volumes)),
					marshal(t, findingJob.Spec.Template.Spec.Volumes); got != want {
					t.Errorf("volumes differ from the Finding stage's beyond the Job's name:\n%s\nvs\n%s", got, want)
				}
			})
		}
	}
}
