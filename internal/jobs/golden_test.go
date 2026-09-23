// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenJob pins the whole Job a config and spec build — every label,
// annotation, volume, container, env entry and security setting — as YAML
// under testdata, so a change to the pod shape is a visible diff in review
// rather than a surprise on the cluster. The default and brokered shapes
// were captured before repository-declared runner images existed and must
// stay byte-identical while nothing asks for one.
func goldenJob(t *testing.T, name string, job *batchv1.Job) {
	t.Helper()
	raw, err := yaml.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	path := filepath.Join("testdata", name+".yaml")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if string(raw) != string(want) {
		t.Errorf("job shape differs from %s (run with -update to accept):\n--- want\n%s\n--- got\n%s",
			path, want, raw)
	}
}

// buildJobForTest builds the Job object Create would submit, without
// submitting it, so the golden pins construction alone.
func buildJobForTest(t *testing.T, cfg Config, spec Spec) *batchv1.Job {
	t.Helper()
	c := New(fake.NewClientset(), cfg, nil)
	job, err := c.buildJob(NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	return job
}

// TestGoldenDefaultJob is the byte-identity proof for the default shape: a
// claude Job with the Secret credential channel and nothing repository-image
// shaped.
func TestGoldenDefaultJob(t *testing.T) {
	goldenJob(t, "job_default", buildJobForTest(t, testConfig(), testSpec()))
}

// TestGoldenDefaultRemediationJob pins the remediation flavour of the
// default shape: the analysis handoff, the per-run grant and the codex
// runner.
func TestGoldenDefaultRemediationJob(t *testing.T) {
	spec := testSpec()
	spec.Phase = "remediate"
	spec.Kind = "remediation"
	spec.Harness = "codex"
	spec.Model = "openai/gpt-5-codex"
	spec.Owner = "finding-abc123def0-1-rem-1"
	spec.InvestigationMarkdown = "# Investigation\n"
	spec.MaxTurns = 120
	spec.TokenBudget = 600000
	goldenJob(t, "job_default_remediation", buildJobForTest(t, testConfig(), spec))
}
