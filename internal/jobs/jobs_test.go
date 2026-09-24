// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/harness"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func testConfig() Config {
	return Config{
		Namespace:      "patchy-agents",
		ServiceAccount: "patchy-agent",
		Deadline:       time.Hour,
		TTL:            2 * time.Hour,
		Runners: map[string]Runner{
			"claude": {
				Image:     "ghcr.io/bitwise-media-group/patchy/claude-agent-runner:1",
				Secret:    "anthropic",
				SecretEnv: "ANTHROPIC_API_KEY",
			},
			"codex": {
				Image:     "ghcr.io/bitwise-media-group/patchy/codex-agent-runner:1",
				Secret:    "openai",
				SecretEnv: "OPENAI_API_KEY",
			},
		},
		Env: map[string]string{
			"PATCHY_INVESTIGATE_TIMEOUT": "15m",
			"GITHUB_TOKEN":               "must-never-pass-through",
			"CLAUDE_CODE_OAUTH_TOKEN":    "must-never-pass-through",
			"CODEX_API_KEY":              "must-never-pass-through",
			"CODEX_ACCESS_TOKEN":         "must-never-pass-through",
		},
		CPURequest:    "500m",
		MemoryRequest: "1Gi",
		CPULimit:      "2",
		MemoryLimit:   "4Gi",
	}
}

func testSpec() Spec {
	return Spec{
		Repo:           "octo/repo",
		Attempt:        1,
		Phase:          "investigate",
		Harness:        "claude",
		Model:          "anthropic/claude-sonnet-5",
		BaseSHA:        "0123456789abcdef0123456789abcdef01234567",
		IssueMarkdown:  "# Finding\n",
		Kind:           "investigation",
		Owner:          "finding-abc123def0-1-inv-1",
		Finding:        "finding-abc123def0-1",
		ArtifactURL:    "http://patchy-source-controller.patchy.svc.cluster.local:9790/artifacts/deadbeef.tar.gz",
		ArtifactDigest: "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233",
	}
}

func TestNameFor(t *testing.T) {
	tests := []struct {
		name               string
		findingA, findingB string
		kindA, kindB       string
		attA, attB         int32
		wantEqual          bool
	}{
		{"same inputs", "finding-a-1", "finding-a-1", "investigation", "investigation", 1, 1, true},
		{"different finding", "finding-a-1", "finding-b-1", "investigation", "investigation", 1, 1, false},
		{"different kind", "finding-a-1", "finding-a-1", "investigation", "remediation", 1, 1, false},
		{"different attempt", "finding-a-1", "finding-a-1", "remediation", "remediation", 1, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NameFor(tt.findingA, tt.kindA, tt.attA)
			b := NameFor(tt.findingB, tt.kindB, tt.attB)
			if (a == b) != tt.wantEqual {
				t.Errorf("NameFor equality = %v (%q vs %q), want %v", a == b, a, b, tt.wantEqual)
			}
		})
	}
}

func TestNameForShape(t *testing.T) {
	tests := []struct {
		name    string
		finding string
		kind    string
	}{
		{"investigation", "finding-abc123def0-1", "investigation"},
		{"remediation", "finding-abc123def0-1", "remediation"},
		{"long finding", "finding-" + strings.Repeat("a", 300), "investigation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NameFor(tt.finding, tt.kind, 99)
			if len(got) > 63 {
				t.Errorf("NameFor(%q) = %q: %d chars, want <= 63", tt.finding, got, len(got))
			}
			if !dns1123.MatchString(got) {
				t.Errorf("NameFor(%q) = %q is not DNS-1123 safe", tt.finding, got)
			}
			if !strings.HasPrefix(got, "patchy-") {
				t.Errorf("NameFor(%q) = %q, want patchy- prefix", tt.finding, got)
			}
		})
	}
}

func TestCreateRequiresKindAndFinding(t *testing.T) {
	c := New(fake.NewClientset(), testConfig(), nil)
	spec := testSpec()
	spec.Kind = ""
	if _, _, err := c.Create(context.Background(), spec); err == nil {
		t.Error("Create without Kind succeeded, want error")
	}
	spec = testSpec()
	spec.Finding = ""
	if _, _, err := c.Create(context.Background(), spec); err == nil {
		t.Error("Create without Finding succeeded, want error")
	}
}

func TestCreateJobShape(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testSpec()

	name, _, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)); name != want {
		t.Fatalf("Create returned %q, want deterministic %q", name, want)
	}

	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	wantLabels := map[string]string{
		"app.kubernetes.io/name":         "patchy-agent",
		"app.kubernetes.io/managed-by":   "patchy",
		"patchy.bitwisemedia.uk/attempt": "1",
		"patchy.bitwisemedia.uk/kind":    "investigation",
		"patchy.bitwisemedia.uk/owner":   "finding-abc123def0-1-inv-1",
		"patchy.bitwisemedia.uk/finding": "finding-abc123def0-1",
	}
	for _, lbls := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		for k, want := range wantLabels {
			if got := lbls[k]; got != want {
				t.Errorf("label %s = %q, want %q", k, got, want)
			}
		}
	}
	if got := job.Annotations["patchy.bitwisemedia.uk/repo"]; got != "octo/repo" {
		t.Errorf("annotation patchy.bitwisemedia.uk/repo = %q, want octo/repo", got)
	}

	if got := *job.Spec.BackoffLimit; got != 0 {
		t.Errorf("backoffLimit = %d, want 0", got)
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != 3600 {
		t.Errorf("activeDeadlineSeconds = %d, want 3600", got)
	}
	if got := *job.Spec.TTLSecondsAfterFinished; got != 7200 {
		t.Errorf("ttlSecondsAfterFinished = %d, want 7200", got)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if pod.ServiceAccountName != "patchy-agent" {
		t.Errorf("serviceAccountName = %q, want patchy-agent", pod.ServiceAccountName)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Errorf("pod fsGroup = %+v, want 65532", pod.SecurityContext)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "prepare" {
		t.Fatalf("init containers = %+v, want one named prepare", pod.InitContainers)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != "agent" {
		t.Fatalf("containers = %+v, want one named agent", pod.Containers)
	}
}

func TestCreateArtifactPrepare(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testSpec()

	name, _, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	prepare := job.Spec.Template.Spec.InitContainers[0]
	script := strings.Join(prepare.Command, " ")

	// The tree arrives as a digest-verified tarball; the base commit is
	// synthetic. No git fetch, no clone, no credential.
	if !strings.Contains(script, `curl -fsSL --retry 5 --retry-all-errors "$PATCHY_ARTIFACT_URL"`) {
		t.Errorf("prepare script does not fetch the artifact with retries:\n%s", script)
	}
	if !strings.Contains(script, "sha256sum -c") {
		t.Errorf("prepare script does not verify the digest:\n%s", script)
	}
	if strings.Contains(script, "git fetch") || strings.Contains(script, "git clone") {
		t.Errorf("prepare script still talks to a remote:\n%s", script)
	}

	envs := map[string]string{}
	for _, env := range prepare.Env {
		envs[env.Name] = env.Value
	}
	if envs["PATCHY_ARTIFACT_URL"] != spec.ArtifactURL {
		t.Errorf("init PATCHY_ARTIFACT_URL = %q, want %q", envs["PATCHY_ARTIFACT_URL"], spec.ArtifactURL)
	}
	if envs["PATCHY_ARTIFACT_DIGEST"] != spec.ArtifactDigest {
		t.Errorf("init PATCHY_ARTIFACT_DIGEST = %q, want %q", envs["PATCHY_ARTIFACT_DIGEST"], spec.ArtifactDigest)
	}
	if envs["PATCHY_BASE_SHA"] != spec.BaseSHA {
		t.Errorf("init PATCHY_BASE_SHA = %q, want %q", envs["PATCHY_BASE_SHA"], spec.BaseSHA)
	}
}

func TestCreateCredentialIsolation(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testSpec()

	name, _, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	prepare := job.Spec.Template.Spec.InitContainers[0]
	agent := job.Spec.Template.Spec.Containers[0]

	// No container gets any GitHub credential in any form; the reserved
	// GITHUB_TOKEN passthrough in Config.Env must not leak either.
	for _, ct := range []corev1.Container{prepare, agent} {
		for _, env := range ct.Env {
			if env.Name == "GITHUB_TOKEN" {
				t.Errorf("%s container has a GITHUB_TOKEN env", ct.Name)
			}
		}
	}
	// The agent container must not reference the per-Job secret or mount it.
	for _, env := range agent.Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == name {
			t.Errorf("agent env %s references the per-Job secret", env.Name)
		}
	}
	for _, mount := range agent.VolumeMounts {
		if mount.Name == "input" {
			t.Error("agent container mounts the per-Job secret volume")
		}
	}
}

func TestCreateAgentEnv(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	spec := testSpec()

	name, _, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	agent := job.Spec.Template.Spec.Containers[0]
	if got := strings.Join(agent.Command, " "); got != "agent-runner" {
		t.Errorf("agent command = %q, want agent-runner", got)
	}

	envs := map[string]corev1.EnvVar{}
	for _, env := range agent.Env {
		envs[env.Name] = env
	}
	wantValues := map[string]string{
		"HOME":             "/workspace",
		"PATCHY_WORKSPACE": "/workspace",
		"PATCHY_REPO":      "octo/repo",
		"PATCHY_PHASE":     "investigate",
		"PATCHY_FINDING":   "finding-abc123def0-1",
		"PATCHY_BASE_SHA":  spec.BaseSHA,
		// The stage harness and model are set per-Job from the Spec, not the
		// controller-global Env.
		"PATCHY_INVESTIGATE_HARNESS": "claude",
		"PATCHY_INVESTIGATE_MODEL":   "anthropic/claude-sonnet-5",
		"PATCHY_INVESTIGATE_TIMEOUT": "15m",
	}
	for k, want := range wantValues {
		if got, ok := envs[k]; !ok || got.Value != want {
			t.Errorf("agent env %s = %+v, want value %q", k, got, want)
		}
	}
	anthropic, ok := envs["ANTHROPIC_API_KEY"]
	if !ok || anthropic.ValueFrom == nil || anthropic.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("ANTHROPIC_API_KEY = %+v, want a secretKeyRef", anthropic)
	}
	ref := anthropic.ValueFrom.SecretKeyRef
	if ref.Name != "anthropic" || ref.Key != "api-key" {
		t.Errorf("ANTHROPIC_API_KEY ref = %s/%s, want anthropic/api-key (defaulted)", ref.Name, ref.Key)
	}
	// The credential channels not selected stay reserved, both this harness's
	// and the other's: a passthrough value in Config.Env must never reach the
	// pod, or a credential would travel as a literal in the Job spec.
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if got, ok := envs[name]; ok {
			t.Errorf("%s = %+v, want absent (reserved)", name, got)
		}
	}
}

// TestCreateAgentEnvPreviousAttempt: a retry's previous attempt reaches the
// pod as PATCHY_PREVIOUS_ATTEMPT, verbatim; a first attempt sets nothing.
func TestCreateAgentEnvPreviousAttempt(t *testing.T) {
	const previous = `{"attempt":1,"outcome":"commit_failed","detail":"M patchy-target"}`
	for _, tt := range []struct {
		name, previous string
	}{{"retry", previous}, {"first attempt", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			spec := testSpec()
			spec.PreviousAttempt = tt.previous
			name, _, err := New(cs, testConfig(), nil).Create(context.Background(), spec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			var got *corev1.EnvVar
			for i, env := range job.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "PATCHY_PREVIOUS_ATTEMPT" {
					got = &job.Spec.Template.Spec.Containers[0].Env[i]
				}
			}
			switch {
			case tt.previous == "" && got != nil:
				t.Errorf("PATCHY_PREVIOUS_ATTEMPT = %+v on a first attempt, want absent", got)
			case tt.previous != "" && (got == nil || got.Value != tt.previous):
				t.Errorf("PATCHY_PREVIOUS_ATTEMPT = %+v, want %q", got, tt.previous)
			}
		})
	}
}

// TestOperatorEnvCannotShadowPerJobHandoff: the per-Job handoff variables
// are the controller's to set from the Spec, so an operator's entry of the
// same name must never reach the pod — whether it comes through Config.Env
// or through Runner.Env, which carries the operator's provider env
// (--claude-provider-env, the chart's provider `env`). Not on a Job that
// sets the name (a second, conflicting entry whose winner Kubernetes leaves
// undefined), not on one that leaves it unset (a first attempt told it is a
// retry), and not on a repository-image Job, where it would stand in for
// the blank that keeps an image's ENV from reaching agent-runner.
func TestOperatorEnvCannotShadowPerJobHandoff(t *testing.T) {
	const operator = `{"attempt":9,"outcome":"unknown","detail":"from the operator"}`
	handoff := []struct {
		env string
		set func(*Spec, string)
	}{
		{"PATCHY_CALIBRATION", func(s *Spec, v string) { s.Calibration = v }},
		{"PATCHY_PREVIOUS_ATTEMPT", func(s *Spec, v string) { s.PreviousAttempt = v }},
	}
	sources := []struct {
		name string
		set  func(cfg *Config, name, value string)
	}{
		{"Config.Env", func(cfg *Config, name, value string) { cfg.Env[name] = value }},
		{"Runner.Env", func(cfg *Config, name, value string) {
			r := cfg.Runners["claude"]
			env := maps.Clone(r.Env)
			if env == nil {
				env = map[string]string{}
			}
			env[name] = value
			r.Env = env
			cfg.Runners["claude"] = r
		}},
	}
	shapes := []struct {
		name string
		cfg  func() Config
		spec func() Spec
		// blank is whether a Job that leaves the name unset still lists it,
		// with an explicit empty value.
		blank bool
	}{
		{"default", testConfig, testSpec, false},
		{"repository image", injectedConfig, injectedSpec, true},
	}
	for _, h := range handoff {
		for _, src := range sources {
			for _, shape := range shapes {
				for _, jobValue := range []string{`{"attempt":1}`, ""} {
					name := fmt.Sprintf("%s/%s/%s/job sets %q", h.env, src.name, shape.name, jobValue)
					t.Run(name, func(t *testing.T) {
						cfg := shape.cfg()
						src.set(&cfg, h.env, operator)
						spec := shape.spec()
						h.set(&spec, jobValue)
						var got []corev1.EnvVar
						for _, e := range buildJobForTest(t, cfg, spec).Spec.Template.Spec.Containers[0].Env {
							if e.Name == h.env {
								got = append(got, e)
							}
						}
						want := []corev1.EnvVar{{Name: h.env, Value: jobValue}}
						if jobValue == "" && !shape.blank {
							want = nil
						}
						if !slices.Equal(got, want) {
							t.Errorf("%s entries = %+v, want %+v", h.env, got, want)
						}
					})
				}
			}
		}
	}
}

func TestCreateAgentEnvOAuthToken(t *testing.T) {
	cs := fake.NewClientset()
	cfg := testConfig()
	claude := cfg.Runners["claude"]
	claude.SecretEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	cfg.Runners["claude"] = claude
	c := New(cs, cfg, nil)

	name, _, err := c.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	envs := map[string]corev1.EnvVar{}
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		envs[env.Name] = env
	}
	oauth, ok := envs["CLAUDE_CODE_OAUTH_TOKEN"]
	if !ok || oauth.ValueFrom == nil || oauth.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %+v, want a secretKeyRef (not the reserved passthrough literal)", oauth)
	}
	ref := oauth.ValueFrom.SecretKeyRef
	if ref.Name != "anthropic" || ref.Key != "api-key" {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN ref = %s/%s, want anthropic/api-key", ref.Name, ref.Key)
	}
	if got, ok := envs["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY = %+v, want absent when the OAuth env is selected", got)
	}
}

// TestCreateRunnerSelection asserts a Job runs the image, credential, and
// harness label of the runner its Spec.Harness selects.
func TestCreateRunnerSelection(t *testing.T) {
	tests := []struct {
		name      string
		harness   string
		secretEnv string // override the runner's credential channel; "" keeps the default
		wantImage string
		wantEnv   string
		wantRef   string // credential Secret name
	}{
		{"claude", "claude", "",
			"ghcr.io/bitwise-media-group/patchy/claude-agent-runner:1", "ANTHROPIC_API_KEY", "anthropic"},
		{"codex", "codex", "",
			"ghcr.io/bitwise-media-group/patchy/codex-agent-runner:1", "OPENAI_API_KEY", "openai"},
		// The ChatGPT-plan channel: same Secret, injected under the token env
		// var instead of the API-key one.
		{"codex access token", "codex", "CODEX_ACCESS_TOKEN",
			"ghcr.io/bitwise-media-group/patchy/codex-agent-runner:1", "CODEX_ACCESS_TOKEN", "openai"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			cfg := testConfig()
			if tt.secretEnv != "" {
				r := cfg.Runners[tt.harness]
				r.SecretEnv = tt.secretEnv
				cfg.Runners[tt.harness] = r
			}
			c := New(cs, cfg, nil)
			spec := testSpec()
			spec.Harness = tt.harness
			name, _, err := c.Create(context.Background(), spec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			// Both the init and agent containers run the selected runner image.
			for _, ct := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
				if ct.Image != tt.wantImage {
					t.Errorf("%s image = %q, want %q", ct.Name, ct.Image, tt.wantImage)
				}
			}
			if got := job.Spec.Template.Labels["patchy.bitwisemedia.uk/harness"]; got != tt.harness {
				t.Errorf("harness label = %q, want %q", got, tt.harness)
			}
			envs := map[string]corev1.EnvVar{}
			for _, env := range job.Spec.Template.Spec.Containers[0].Env {
				envs[env.Name] = env
			}
			cred, ok := envs[tt.wantEnv]
			if !ok || cred.ValueFrom == nil || cred.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("%s = %+v, want a secretKeyRef", tt.wantEnv, cred)
			}
			if ref := cred.ValueFrom.SecretKeyRef; ref.Name != tt.wantRef || ref.Key != "api-key" {
				t.Errorf("%s ref = %s/%s, want %s/api-key", tt.wantEnv, ref.Name, ref.Key, tt.wantRef)
			}
		})
	}
}

func TestCreateSecurityContexts(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)

	name, _, err := c.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	containers := append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...)
	for _, ct := range containers {
		sc := ct.SecurityContext
		if sc == nil {
			t.Errorf("%s: no securityContext", ct.Name)
			continue
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s: runAsNonRoot not true", ct.Name)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
			t.Errorf("%s: runAsUser = %v, want 65532", ct.Name, sc.RunAsUser)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allowPrivilegeEscalation not false", ct.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: readOnlyRootFilesystem not true", ct.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: capabilities = %+v, want drop ALL", ct.Name, sc.Capabilities)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: seccompProfile = %+v, want RuntimeDefault", ct.Name, sc.SeccompProfile)
		}
		if cpu := ct.Resources.Requests.Cpu().String(); cpu != "500m" {
			t.Errorf("%s: cpu request = %s, want 500m", ct.Name, cpu)
		}
		if mem := ct.Resources.Limits.Memory().String(); mem != "4Gi" {
			t.Errorf("%s: memory limit = %s, want 4Gi", ct.Name, mem)
		}
	}
}

func TestCreateSecret(t *testing.T) {
	tests := []struct {
		name              string
		investigation     string
		wantInvestigation bool
	}{
		{"investigation run carries no analysis", "", false},
		{"remediation run carries the analysis", "# Investigation\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			c := New(cs, testConfig(), nil)
			spec := testSpec()
			spec.InvestigationMarkdown = tt.investigation

			name, _, err := c.Create(context.Background(), spec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			secret, err := cs.CoreV1().Secrets("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get secret: %v", err)
			}
			if _, ok := secret.Data["token"]; ok {
				t.Error("secret carries a token key — the flow must be credential-less")
			}
			if got := string(secret.Data["issue.md"]); got != spec.IssueMarkdown {
				t.Errorf("secret issue.md = %q, want %q", got, spec.IssueMarkdown)
			}
			if _, ok := secret.Data["investigation.md"]; ok != tt.wantInvestigation {
				t.Errorf("investigation.md present = %v, want %v", ok, tt.wantInvestigation)
			}
			if len(secret.OwnerReferences) != 1 {
				t.Fatalf("secret ownerReferences = %+v, want exactly one", secret.OwnerReferences)
			}
			owner := secret.OwnerReferences[0]
			if owner.Kind != "Job" || owner.APIVersion != "batch/v1" || owner.Name != name {
				t.Errorf("secret owner = %+v, want batch/v1 Job %s", owner, name)
			}

			// The Job's volume must reference this secret so the init
			// container can stage the handoff files.
			job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			var found bool
			for _, vol := range job.Spec.Template.Spec.Volumes {
				if vol.Secret != nil && vol.Secret.SecretName == name {
					found = true
				}
			}
			if !found {
				t.Error("no pod volume references the per-Job secret")
			}
		})
	}
}

func TestCreateInvalidResources(t *testing.T) {
	cfg := testConfig()
	cfg.CPULimit = "not-a-quantity"
	c := New(fake.NewClientset(), cfg, nil)
	if _, _, err := c.Create(context.Background(), testSpec()); err == nil {
		t.Fatal("Create with invalid resource quantity succeeded, want error")
	}
}

func TestStatus(t *testing.T) {
	base := metav1.ObjectMeta{Name: "j", Namespace: "patchy-agents"}
	tests := []struct {
		name string
		job  batchv1.Job
		want Status
	}{
		{
			"active",
			batchv1.Job{ObjectMeta: base, Status: batchv1.JobStatus{Active: 1}},
			Status{Active: 1, RunnerImageSource: "default"},
		},
		{
			"complete",
			batchv1.Job{ObjectMeta: base, Status: batchv1.JobStatus{
				Succeeded: 1,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			}},
			Status{Succeeded: 1, Done: true, RunnerImageSource: "default"},
		},
		{
			"failed",
			batchv1.Job{ObjectMeta: base, Status: batchv1.JobStatus{
				Failed: 1,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			}},
			Status{Failed: 1, Done: true, RunnerImageSource: "default"},
		},
		{
			"false condition is not done",
			batchv1.Job{ObjectMeta: base, Status: batchv1.JobStatus{
				Active: 1,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
				},
			}},
			Status{Active: 1, RunnerImageSource: "default"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := tt.job
			c := New(fake.NewClientset(&job), testConfig(), nil)
			got, err := c.Status(context.Background(), "j")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got != tt.want {
				t.Errorf("Status = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestStatusCreated: the Job's creation time rides on its Status, so a
// collector can time the pull grace of a repository-image pod from it.
func TestStatusCreated(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "j", Namespace: "patchy-agents", CreationTimestamp: created,
	}}
	c := New(fake.NewClientset(job), testConfig(), nil)
	got, err := c.Status(context.Background(), "j")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Created.Equal(created.Time) {
		t.Errorf("Created = %v, want %v", got.Created, created.Time)
	}
}

func TestStatusNotFound(t *testing.T) {
	c := New(fake.NewClientset(), testConfig(), nil)
	if _, err := c.Status(context.Background(), "missing"); err == nil {
		t.Fatal("Status of a missing job succeeded, want error")
	}
}

func TestDelete(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, testConfig(), nil)
	name, _, err := c.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.Delete(context.Background(), name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("job still present after Delete: %v", err)
	}
	if err := c.Delete(context.Background(), name); err == nil {
		t.Error("Delete of a missing job succeeded, want error")
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"octo/repo", "octo-repo"},
		{"Octo/RePo", "octo-repo"},
		{"a/b.c_d-e", "a-b.c_d-e"},
		{"---", "unknown"},
		{strings.Repeat("x", 70) + "/y", strings.Repeat("x", 63)},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sanitizeLabelValue(tt.in); got != tt.want {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestReservedEnvCoversEveryCredentialChannel: every credential env var a
// harness accepts must also be reserved here. The two lists live in different
// packages — harness owns what a CLI authenticates with, jobs owns what the
// Job spec refuses to carry as a literal — and only this test keeps them from
// drifting. An accepted-but-unreserved name is a real leak: an operator could
// select it with --<harness>-secret-env while a controller-global Config.Env
// entry of the same name puts the credential into the Job spec in plaintext.
func TestReservedEnvCoversEveryCredentialChannel(t *testing.T) {
	for _, h := range harness.All() {
		for _, env := range h.EnvKeys() {
			if !reservedEnv[env] {
				t.Errorf("harness %q accepts credential env %q, but it is not reserved", h.ID(), env)
			}
		}
	}
}
