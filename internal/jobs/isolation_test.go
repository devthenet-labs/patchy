// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/provider"
)

// credentialFindings lists every way a model credential reaches a pod: an
// env entry sourced from anywhere (a SecretKeyRef above all), an EnvFrom, a
// credential-channel name carrying anything but the public placeholder, a
// Secret volume other than the per-Job handoff Secret (which only the
// trusted prepare init mounts), or a projection other than the broker's
// audience-bound caller token. Empty means the pod holds no credential.
func credentialFindings(job *batchv1.Job, audience string) []string {
	var out []string
	pod := job.Spec.Template.Spec
	for _, ct := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, e := range ct.Env {
			if e.ValueFrom != nil {
				out = append(out, fmt.Sprintf("%s: env %s is sourced from %+v", ct.Name, e.Name, *e.ValueFrom))
			}
			if !credentialChannelEnv[e.Name] || e.Value == "" {
				continue
			}
			if e.Name == provider.PlaceholderAuthEnv && e.Value == provider.PlaceholderAuthToken {
				continue
			}
			out = append(out, fmt.Sprintf("%s: credential channel %s = %q", ct.Name, e.Name, e.Value))
		}
		if len(ct.EnvFrom) > 0 {
			out = append(out, fmt.Sprintf("%s: envFrom %+v", ct.Name, ct.EnvFrom))
		}
		if ct.Name != agentContainerName {
			continue
		}
		for _, m := range ct.VolumeMounts {
			if m.Name == volInput {
				out = append(out, "agent: mounts the per-Job Secret")
			}
		}
	}
	for _, v := range pod.Volumes {
		switch {
		case v.EmptyDir != nil:
		case v.Secret != nil:
			if v.Name != volInput || v.Secret.SecretName != job.Name {
				out = append(out, fmt.Sprintf("volume %s mounts Secret %s", v.Name, v.Secret.SecretName))
			}
		case v.Projected != nil:
			for _, src := range v.Projected.Sources {
				if src.ServiceAccountToken == nil || src.ServiceAccountToken.Audience != audience {
					out = append(out, fmt.Sprintf("volume %s projects %+v", v.Name, src))
				}
			}
		default:
			out = append(out, fmt.Sprintf("volume %s is %+v", v.Name, v.VolumeSource))
		}
	}
	return out
}

// credentialRunner is the claude runner of testConfig, which injects the
// model key from a Secret, contributing binaries as the brokered one does.
func credentialRunner(harnessID string) Runner {
	r := testConfig().Runners[harnessID]
	r.Inject = []string{"agent-runner", "claude"}
	return r
}

// TestRepositoryImageRefusedForCredentialRunner: a runner that would carry
// a model credential into the pod never runs a repository-declared image.
// Inject is only ever meant for a credential-free runner, so a runner with
// both is a configuration contradiction: Create refuses it and creates
// nothing rather than quietly running the default image, which would hide
// the misconfiguration behind an audit trail that just says "default".
func TestRepositoryImageRefusedForCredentialRunner(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		runner  Runner
	}{
		{"claude with an API key Secret", "claude", credentialRunner("claude")},
		{"claude with an OAuth token Secret", "claude", func() Runner {
			r := credentialRunner("claude")
			r.SecretEnv = "CLAUDE_CODE_OAUTH_TOKEN"
			return r
		}()},
		{"codex with its key", "codex", credentialRunner("codex")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := injectedConfig()
			cfg.Runners[tt.harness] = tt.runner
			spec := injectedSpec()
			spec.Harness = tt.harness
			cs := fake.NewClientset()
			name, ref, err := New(cs, cfg, nil).Create(context.Background(), spec)
			if err == nil {
				t.Fatalf("Create = (%q, %+v, nil), want a refusal: the repository image would hold the %s credential",
					name, ref, tt.runner.SecretEnv)
			}
			for _, want := range []string{tt.harness, "credential", tt.runner.Secret} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Create error = %q, want it to name %q", err, want)
				}
			}
			assertNothingCreated(t, cs, cfg.Namespace)
		})
	}
}

// TestCredentialRunnerDefaultJobUnchanged: the refusal is about the
// repository image only. The same runner still builds its ordinary default
// Job — the credential in the trusted runner image is the non-brokered
// posture — whenever it would not inject.
func TestCredentialRunnerDefaultJobUnchanged(t *testing.T) {
	cfg := injectedConfig()
	cfg.Runners["claude"] = credentialRunner("claude")
	cfg.AllowRepositoryImages = false
	job, ref := createWithRef(t, cfg, injectedSpec())
	if ref.Source != v1alpha1.RunnerImageSourceDefault {
		t.Errorf("Create source = %q, want default", ref.Source)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != cfg.Runners["claude"].Image {
		t.Errorf("agent image = %q, want the trusted runner image", got)
	}
	if envMap(job.Spec.Template.Spec.Containers[0])["ANTHROPIC_API_KEY"].ValueFrom == nil {
		t.Error("the default Job lost its SecretKeyRef credential")
	}
}

func assertNothingCreated(t *testing.T, cs *fake.Clientset, namespace string) {
	t.Helper()
	jobs, err := cs.BatchV1().Jobs(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := cs.CoreV1().Secrets(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 || len(secrets.Items) != 0 {
		t.Errorf("refused Create left %d Job(s) and %d Secret(s) behind", len(jobs.Items), len(secrets.Items))
	}
}

// TestInjectedPodHoldsNoCredential pins the threat model's central claim on
// the shape runnercfg produces: nothing in a repository-image pod is a
// model or forge credential.
func TestInjectedPodHoldsNoCredential(t *testing.T) {
	job := createJob(t, injectedConfig(), injectedSpec())
	if job.Annotations[annotationRunnerImageSource] != v1alpha1.RunnerImageSourceRepository {
		t.Fatalf("fixture did not inject: annotations %v", job.Annotations)
	}
	for _, f := range credentialFindings(job, DefaultBrokerAudience) {
		t.Error(f)
	}
}

// TestPropertyNoCredentialInRepositoryImagePod: for any runner fleet,
// operator env, kill-switch position and declared image, a Job whose runner
// image source is repository holds no credential of any kind. Create may
// refuse a combination; it may never build one that breaks the invariant.
// Seeded so the gate is deterministic; the generator reaches every branch
// (asserted below) so the property is never vacuously true.
func TestPropertyNoCredentialInRepositoryImagePod(t *testing.T) {
	rng := rand.New(rand.NewSource(20260923))
	pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
	channels := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY",
		"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
	injects := [][]string{nil, {"agent-runner", "claude"}, {"claude"}, {"agent-runner", "codex"}}

	var injected, refused, plain int
	for i := range 2000 {
		cfg := testConfig()
		cfg.AllowRepositoryImages = rng.Intn(4) != 0
		cfg.EphemeralStorage = pick("", "8Gi")
		for range rng.Intn(4) {
			cfg.Env[pick(channels...)] = "operator-leak"
		}
		cfg.Runners = map[string]Runner{}
		for _, id := range []string{"claude", "codex", "fake"} {
			r := Runner{
				Image:     "ghcr.io/bitwise-media-group/patchy/" + id + "-agent-runner:1",
				Brokered:  rng.Intn(2) == 0,
				Secret:    pick("", "model-credential"),
				SecretEnv: pick(channels...),
				Inject:    injects[rng.Intn(len(injects))],
				Env:       map[string]string{},
			}
			for range rng.Intn(3) {
				r.Env[pick(channels...)] = "runner-leak"
			}
			cfg.Runners[id] = r
		}
		spec := testSpec()
		spec.Harness = pick("claude", "codex", "fake")
		spec.RunnerImage = pick("", repoImage, "ghcr.io/devthenet-labs/go-agent-env:latest")
		spec.RunnerSearchPath = repoSearchPath

		c := New(fake.NewClientset(), cfg, nil)
		job, err := c.buildJob(fmt.Sprintf("patchy-prop-%d", i), spec)
		if err != nil {
			refused++
			continue
		}
		if job.Annotations[annotationRunnerImageSource] != v1alpha1.RunnerImageSourceRepository {
			plain++
			continue
		}
		injected++
		if findings := credentialFindings(job, DefaultBrokerAudience); len(findings) > 0 {
			t.Fatalf("iteration %d: a repository-image pod holds a credential:\n  runner %s: %+v\n  config env: %v\n  %s",
				i, spec.Harness, cfg.Runners[spec.Harness], cfg.Env, strings.Join(findings, "\n  "))
		}
	}
	if injected == 0 || refused == 0 || plain == 0 {
		t.Errorf("generator did not reach every branch: injected=%d refused=%d default=%d", injected, refused, plain)
	}
}
