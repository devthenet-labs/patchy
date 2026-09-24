// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func testEvalSpec() EvalSpec {
	return EvalSpec{
		Evaluation: "eval-20260812-1",
		Unit:       "eval-20260812-1-u000",
		Index:      0,
		Harness:    "claude",
		UnitJSON:   []byte(`{"skill":"workflow-commit","tier":2}`),
		ArtifactURL: "http://patchy-source-controller.patchy.svc.cluster.local:9790/artifacts/" +
			"aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233.tar.gz",
		ArtifactDigest: "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233",
	}
}

func TestEvalNameForIsDeterministicAndSafe(t *testing.T) {
	a := EvalNameFor("eval-1-u000")
	b := EvalNameFor("eval-1-u000")
	c := EvalNameFor("eval-1-u001")
	if a != b {
		t.Errorf("EvalNameFor not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("EvalNameFor collides across units: %q", a)
	}
	if !dns1123.MatchString(a) || len(a) > 63 {
		t.Errorf("EvalNameFor %q is not a valid DNS-1123 name", a)
	}
	if !strings.HasSuffix(a, "-evl") {
		t.Errorf("EvalNameFor %q lacks the -evl discriminator", a)
	}
}

func TestCreateEvalJobShape(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	ctx := context.Background()
	spec := testEvalSpec()

	name, err := c.CreateEval(ctx, spec)
	if err != nil {
		t.Fatalf("CreateEval: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	checkEvalLabels(t, job.Labels, spec)

	pod := job.Spec.Template.Spec
	if got := len(pod.InitContainers); got != 1 {
		t.Fatalf("init containers = %d, want 1", got)
	}
	prep := pod.InitContainers[0]
	script := prep.Command[len(prep.Command)-1]
	if strings.Contains(script, "git init") || strings.Contains(script, "--strip-components") {
		t.Error("eval prepare script must not git init or strip components")
	}
	if !strings.Contains(script, "sha256sum -c") {
		t.Error("eval prepare script must verify the bundle digest")
	}

	checkEvalAgent(t, pod.Containers[0])
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("backoffLimit != 0")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64(3600) {
		t.Errorf("activeDeadlineSeconds = %v, want 3600", job.Spec.ActiveDeadlineSeconds)
	}

	secret, err := cs.CoreV1().Secrets("patchy-agents").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(secret.Data[secretKeyUnit]) != string(spec.UnitJSON) {
		t.Error("secret unit.json does not match spec")
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != name {
		t.Error("secret is not owner-referenced to the job")
	}
}

// checkEvalLabels asserts the evaluation lineage labels.
func checkEvalLabels(t *testing.T, lbls map[string]string, spec EvalSpec) {
	t.Helper()
	want := map[string]string{
		v1alpha1.LabelRunKind:    KindEvaluation,
		v1alpha1.LabelEvaluation: spec.Evaluation,
		v1alpha1.LabelOwner:      spec.Unit,
		v1alpha1.LabelHarness:    spec.Harness,
		v1alpha1.LabelUnitIndex:  "0",
	}
	for key, w := range want {
		if lbls[key] != w {
			t.Errorf("label %s = %q, want %q", key, lbls[key], w)
		}
	}
}

// checkEvalAgent asserts the agent container's command, env, credential, and
// security posture (identical to the finding runners).
func checkEvalAgent(t *testing.T, agent corev1.Container) {
	t.Helper()
	if got := strings.Join(agent.Command, " "); got != "evolve exec-unit" {
		t.Errorf("agent command = %q, want %q", got, "evolve exec-unit")
	}
	env := map[string]string{}
	var cred bool
	for _, e := range agent.Env {
		env[e.Name] = e.Value
		if e.Name == "ANTHROPIC_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			cred = true
		}
	}
	if env["EVOLVE_UNIT_FILE"] != evalUnitFilePath {
		t.Errorf("EVOLVE_UNIT_FILE = %q, want %q", env["EVOLVE_UNIT_FILE"], evalUnitFilePath)
	}
	if env["EVOLVE_BUNDLE_DIR"] != evalBundleDir {
		t.Errorf("EVOLVE_BUNDLE_DIR = %q, want %q", env["EVOLVE_BUNDLE_DIR"], evalBundleDir)
	}
	if !cred {
		t.Error("agent container lacks the SecretKeyRef credential")
	}
	sec := agent.SecurityContext
	if sec == nil || sec.ReadOnlyRootFilesystem == nil || !*sec.ReadOnlyRootFilesystem {
		t.Error("agent container root filesystem is not read-only")
	}
	if sec == nil || sec.RunAsUser == nil || *sec.RunAsUser != 65532 {
		t.Error("agent container does not run as 65532")
	}
}

// TestOperatorEnvCannotShadowEvalHandoff: an evaluation agent container
// sets HOME and the EVOLVE_* handoff itself, and Runner.Env carries the
// operator's provider env (--claude-provider-env, the chart's provider
// `env`), so an operator entry of one of those names must never reach the
// pod. A copy would be a second entry, and the kubelet takes the last one:
// EVOLVE_UNIT_FILE=/tmp/x would point evolve exec-unit away from the unit
// it was handed and fail every evaluation unit. Both claude runner shapes
// (a Secret credential and the brokered one) build the same env.
func TestOperatorEnvCannotShadowEvalHandoff(t *testing.T) {
	wantNames := []string{"EVOLVE_BUNDLE_DIR", "EVOLVE_UNIT_FILE", "HOME"}
	if got := EvalJobEnvNames(); !slices.Equal(got, wantNames) {
		t.Errorf("EvalJobEnvNames() = %v, want %v", got, wantNames)
	}
	shapes := []struct {
		name string
		cfg  func() Config
	}{
		{"secret", testConfig},
		{"brokered", brokeredConfig},
	}
	for _, shape := range shapes {
		own := envMap(evalAgentFor(t, shape.cfg()))
		for _, name := range EvalJobEnvNames() {
			t.Run(shape.name+"/"+name, func(t *testing.T) {
				cfg := shape.cfg()
				claude := cfg.Runners["claude"]
				claude.Env = maps.Clone(claude.Env)
				if claude.Env == nil {
					claude.Env = map[string]string{}
				}
				claude.Env[name] = "/tmp/operator"
				cfg.Runners["claude"] = claude

				agent := evalAgentFor(t, cfg)
				var got []corev1.EnvVar
				seen := map[string]bool{}
				for _, e := range agent.Env {
					if seen[e.Name] {
						t.Errorf("eval agent env lists %s twice", e.Name)
					}
					seen[e.Name] = true
					if e.Name == name {
						got = append(got, e)
					}
				}
				want := []corev1.EnvVar{{Name: name, Value: own[name].Value}}
				if !slices.Equal(got, want) {
					t.Errorf("%s entries = %+v, want only the Job's own %+v", name, got, want)
				}
			})
		}
	}
}

// evalAgentFor creates the standard evaluation Job under cfg and returns its
// agent container.
func evalAgentFor(t *testing.T, cfg Config) corev1.Container {
	t.Helper()
	cs := fake.NewClientset()
	c := New(cs, cfg, nil)
	name, err := c.CreateEval(context.Background(), testEvalSpec())
	if err != nil {
		t.Fatalf("CreateEval: %v", err)
	}
	job, err := cs.BatchV1().Jobs(cfg.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job.Spec.Template.Spec.Containers[0]
}

func TestCreateEvalIsIdempotent(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	ctx := context.Background()

	first, err := c.CreateEval(ctx, testEvalSpec())
	if err != nil {
		t.Fatalf("CreateEval: %v", err)
	}
	second, err := c.CreateEval(ctx, testEvalSpec())
	if err != nil {
		t.Fatalf("CreateEval(again): %v", err)
	}
	if first != second {
		t.Errorf("CreateEval names differ: %q vs %q", first, second)
	}
	jobsList, _ := cs.BatchV1().Jobs("patchy-agents").List(ctx, metav1.ListOptions{})
	if len(jobsList.Items) != 1 {
		t.Errorf("jobs = %d, want 1", len(jobsList.Items))
	}
}

func TestCreateEvalDeadlineOverride(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testEvalSpec()
	spec.Deadline = 30 * time.Minute

	name, err := c.CreateEval(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateEval: %v", err)
	}
	job, _ := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64(1800) {
		t.Errorf("activeDeadlineSeconds = %v, want 1800", job.Spec.ActiveDeadlineSeconds)
	}
}
