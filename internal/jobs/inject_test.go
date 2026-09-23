// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/sandboxprobe"
)

const (
	repoImage = "ghcr.io/devthenet-labs/go-agent-env@sha256:" +
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	repoSearchPath = "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// injectedConfig is brokeredConfig with the feature on and the claude
// runner contributing its binaries — the shape runnercfg will produce.
func injectedConfig() Config {
	cfg := brokeredConfig()
	claude := cfg.Runners["claude"]
	claude.Inject = []string{"agent-runner", "claude"}
	cfg.Runners["claude"] = claude
	cfg.AllowRepositoryImages = true
	cfg.EphemeralStorage = "8Gi"
	return cfg
}

// injectedSpec is testSpec carrying a Repository's pinned image.
func injectedSpec() Spec {
	spec := testSpec()
	spec.RunnerImage = repoImage
	spec.RunnerSearchPath = repoSearchPath
	spec.RunnerImageManifest = runnerimage.AgentYAMLPath
	return spec
}

// TestGoldenInjectedJob pins the repository-image shape: the patchy-bin
// volume, the copy and probe tail, the absolute command, the
// controller-owned PATH, the audit annotations and label, the ephemeral
// storage, and every blanked name.
func TestGoldenInjectedJob(t *testing.T) {
	goldenJob(t, "job_injected", buildJobForTest(t, injectedConfig(), injectedSpec()))
}

// createWithRef is createJob returning the RunnerImageRef Create reported.
func createWithRef(t *testing.T, cfg Config, spec Spec) (*batchv1.Job, v1alpha1.RunnerImageRef) {
	t.Helper()
	cs := fake.NewClientset()
	c := New(cs, cfg, nil)
	name, ref, err := c.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	job, err := cs.BatchV1().Jobs(cfg.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job, ref
}

// TestCreateReturnsEffectiveRunnerImage: what Create returns is what the
// Job's annotations say, and both say default whenever injection did not
// happen — for any reason — so the audit trail can never claim a
// repository image for a pod that ran the default one.
func TestCreateReturnsEffectiveRunnerImage(t *testing.T) {
	tests := []struct {
		name   string
		cfg    func() Config
		spec   func() Spec
		inject bool
	}{
		{"claude with inject and the feature on", injectedConfig, injectedSpec, true},
		{"codex has nothing to inject", injectedConfig, func() Spec {
			s := injectedSpec()
			s.Harness = "codex"
			return s
		}, false},
		{"the kill switch is off", func() Config {
			c := injectedConfig()
			c.AllowRepositoryImages = false
			return c
		}, injectedSpec, false},
		{"nothing declared", injectedConfig, func() Spec {
			s := injectedSpec()
			s.RunnerImage, s.RunnerSearchPath, s.RunnerImageManifest = "", "", ""
			return s
		}, false},
		{"claude runner without inject configured", func() Config {
			c := injectedConfig()
			claude := c.Runners["claude"]
			claude.Inject = nil
			c.Runners["claude"] = claude
			return c
		}, injectedSpec, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, spec := tt.cfg(), tt.spec()
			job, ref := createWithRef(t, cfg, spec)
			runnerImage := cfg.Runners[spec.Harness].Image
			if tt.inject {
				want := v1alpha1.RunnerImageRef{
					Image: repoImage, Source: v1alpha1.RunnerImageSourceRepository, Manifest: runnerimage.AgentYAMLPath,
				}
				if ref != want {
					t.Errorf("Create returned %+v, want %+v", ref, want)
				}
				for _, meta := range []metav1.ObjectMeta{job.ObjectMeta, job.Spec.Template.ObjectMeta} {
					wantAnn := map[string]string{
						annotationRunnerImage:       repoImage,
						annotationRunnerImageSource: "repository",
						annotationToolsImage:        runnerImage,
					}
					for k, v := range wantAnn {
						if got := meta.Annotations[k]; got != v {
							t.Errorf("annotation %s = %q, want %q", k, got, v)
						}
					}
					if got := meta.Labels[labelRunnerImageSource]; got != "repository" {
						t.Errorf("label %s = %q, want repository", labelRunnerImageSource, got)
					}
				}
				return
			}
			want := v1alpha1.RunnerImageRef{Image: runnerImage, Source: v1alpha1.RunnerImageSourceDefault}
			if ref != want {
				t.Errorf("Create returned %+v, want %+v", ref, want)
			}
			// A non-injected Job carries none of the audit metadata: it is
			// byte-identical to the same Job built without the image fields.
			for _, meta := range []metav1.ObjectMeta{job.ObjectMeta, job.Spec.Template.ObjectMeta} {
				for _, k := range []string{annotationRunnerImage, annotationRunnerImageSource, annotationToolsImage} {
					if v, ok := meta.Annotations[k]; ok {
						t.Errorf("annotation %s = %q on a default Job, want absent", k, v)
					}
				}
				if v, ok := meta.Labels[labelRunnerImageSource]; ok {
					t.Errorf("label %s = %q on a default Job, want absent", labelRunnerImageSource, v)
				}
			}
			plain := spec
			plain.RunnerImage, plain.RunnerSearchPath, plain.RunnerImageManifest = "", "", ""
			if got, want := marshal(t, buildJobForTest(t, cfg, spec)), marshal(t, buildJobForTest(t, cfg, plain)); got != want {
				t.Errorf("non-injected Job differs from the plain one:\n--- plain\n%s\n--- got\n%s", want, got)
			}
		})
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestCreateAdoptedJobReportsWhatRuns: a retry that adopts an existing Job
// reports that Job's image, not what the retry would have built — a kill
// switch flipped between the two must not relabel a running pod.
func TestCreateAdoptedJobReportsWhatRuns(t *testing.T) {
	cs := fake.NewClientset()
	first := New(cs, injectedConfig(), nil)
	name, ref, err := first.Create(context.Background(), injectedSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref.Source != v1alpha1.RunnerImageSourceRepository {
		t.Fatalf("first Create source = %q, want repository", ref.Source)
	}
	off := injectedConfig()
	off.AllowRepositoryImages = false
	again, ref2, err := New(cs, off, nil).Create(context.Background(), injectedSpec())
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if again != name {
		t.Fatalf("second Create adopted %q, want %q", again, name)
	}
	if ref2 != ref {
		t.Errorf("adopting Create returned %+v, want the existing Job's %+v", ref2, ref)
	}
}

func TestInjectedPrepareContainer(t *testing.T) {
	job := buildJobForTest(t, injectedConfig(), injectedSpec())
	prepare := job.Spec.Template.Spec.InitContainers[0]

	// The trusted image, not the repository's.
	if prepare.Image != injectedConfig().Runners["claude"].Image {
		t.Errorf("prepare image = %q, want the trusted runner image", prepare.Image)
	}
	script := prepare.Command[2]
	if !strings.HasPrefix(script, prepareScript) {
		t.Errorf("prepare script no longer starts with the default script:\n%s", script)
	}
	tail := strings.TrimPrefix(script, prepareScript)
	for _, want := range []string{
		`for bin in $PATCHY_INJECT; do`,
		`cp "/usr/local/bin/$bin" "/patchy/bin/$bin"`,
		"/usr/local/bin/agent-runner sandbox-probe\n",
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("inject tail lacks %q:\n%s", want, tail)
		}
	}
	// The probe runs after the fetch and the copy, last in the script.
	if !strings.HasSuffix(script, "sandbox-probe\n") {
		t.Errorf("prepare script does not end with the probe:\n%s", script)
	}
	if !strings.HasPrefix(script, "set -eu\n") {
		t.Error("prepare script lost set -e; a failing probe would not stop the init")
	}

	envs := envMap(prepare)
	if got := envs["PATCHY_INJECT"].Value; got != "agent-runner claude" {
		t.Errorf("PATCHY_INJECT = %q, want the runner's Inject list", got)
	}
	if got := envs[sandboxprobe.TimeoutEnv].Value; got != "20s" {
		t.Errorf("%s = %q, want the 20s default", sandboxprobe.TimeoutEnv, got)
	}
	var mounted bool
	for _, m := range prepare.VolumeMounts {
		if m.Name == volPatchyBin {
			mounted = true
			if m.ReadOnly || m.MountPath != patchyBinDir {
				t.Errorf("prepare patchy-bin mount = %+v, want read-write at %s", m, patchyBinDir)
			}
		}
	}
	if !mounted {
		t.Error("prepare container does not mount patchy-bin")
	}
	var vol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == volPatchyBin && v.EmptyDir != nil {
			vol = true
		}
	}
	if !vol {
		t.Error("pod has no patchy-bin emptyDir")
	}
}

func TestSandboxProbeTimeoutConfigurable(t *testing.T) {
	cfg := injectedConfig()
	cfg.SandboxProbeTimeout = 45 * time.Second
	prepare := buildJobForTest(t, cfg, injectedSpec()).Spec.Template.Spec.InitContainers[0]
	if got := envMap(prepare)[sandboxprobe.TimeoutEnv].Value; got != "45s" {
		t.Errorf("%s = %q, want 45s", sandboxprobe.TimeoutEnv, got)
	}
}

func TestInjectedAgentContainer(t *testing.T) {
	job := buildJobForTest(t, injectedConfig(), injectedSpec())
	agent := job.Spec.Template.Spec.Containers[0]

	if agent.Image != repoImage {
		t.Errorf("agent image = %q, want the repository's %q", agent.Image, repoImage)
	}
	if got := strings.Join(agent.Command, " "); got != "/patchy/bin/agent-runner" {
		t.Errorf("agent command = %q, want the absolute injected path", got)
	}
	var mounted bool
	for _, m := range agent.VolumeMounts {
		if m.Name == volPatchyBin {
			mounted = true
			if !m.ReadOnly || m.MountPath != patchyBinDir {
				t.Errorf("agent patchy-bin mount = %+v, want read-only at %s", m, patchyBinDir)
			}
		}
	}
	if !mounted {
		t.Error("agent container does not mount patchy-bin")
	}

	envs := envMap(agent)
	wantValues := map[string]string{
		"PATCHY_BIN_DIR":      patchyBinDir,
		"PATH":                "/patchy/bin:" + repoSearchPath,
		"GIT_CONFIG_NOSYSTEM": "1",
		"DISABLE_AUTOUPDATER": "1",
		// The usual env survives untouched.
		"PATCHY_REPO":              "octo/repo",
		"PATCHY_BROKER_TOKEN_FILE": brokerTokenPath,
		"ANTHROPIC_BASE_URL":       "http://patchy-egress-broker.patchy.svc.cluster.local:8080/anthropic",
	}
	for k, want := range wantValues {
		if got, ok := envs[k]; !ok || got.Value != want {
			t.Errorf("agent env %s = %+v, want value %q", k, got, want)
		}
	}
	assertPlaceholder(t, envs)
}

// TestInjectedAgentEnvScrub: every scrubbed name is explicitly empty, so an
// image ENV of that name is overridden rather than inherited; names the Job
// sets keep their values and are never blanked; nothing appears twice.
func TestInjectedAgentEnvScrub(t *testing.T) {
	agent := buildJobForTest(t, injectedConfig(), injectedSpec()).Spec.Template.Spec.Containers[0]
	envs := envMap(agent)
	set := map[string]bool{}
	for _, e := range agent.Env {
		if e.Value != "" || e.ValueFrom != nil {
			set[e.Name] = true
		}
	}
	for _, name := range slices.Concat(scrubEnv, provider.GatewayEnvNames, agentrun.ConfigEnvKeys()) {
		got, ok := envs[name]
		if !ok {
			t.Errorf("agent env lacks %s; an image ENV of that name would reach the harness", name)
			continue
		}
		if set[name] {
			continue
		}
		if got.Value != "" || got.ValueFrom != nil {
			t.Errorf("agent env %s = %+v, want an explicit empty value", name, got)
		}
	}
	for _, name := range []string{"PATCHY_REPO", "PATCHY_PHASE", "PATCHY_BROKER_TOKEN_FILE", "ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "PATCHY_INVESTIGATE_TIMEOUT"} {
		if !set[name] {
			t.Errorf("agent env %s was blanked although the Job sets it", name)
		}
	}
	for _, name := range []string{"LD_PRELOAD", "BASH_ENV", "HTTPS_PROXY", "https_proxy", "PATCHY_MODEL_MAP",
		"PATCHY_CHANGESET_MAX_BYTES", "CLAUDE_CODE_USE_BEDROCK", "GIT_EXEC_PATH"} {
		if got, ok := envs[name]; !ok || got.Value != "" {
			t.Errorf("agent env %s = %+v, want blanked", name, got)
		}
	}
	// Each name appears once: a duplicated env entry is undefined in
	// Kubernetes.
	seen := map[string]int{}
	for _, e := range agent.Env {
		seen[e.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("agent env %s appears %d times", name, n)
		}
	}
}

func TestPodPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"image path follows patchy's", repoSearchPath, "/patchy/bin:" + repoSearchPath},
		{"empty means the runc default", "", "/patchy/bin:" + runnerimage.DefaultPath},
		{"empty components are dropped", "/usr/bin::/bin:", "/patchy/bin:/usr/bin:/bin"},
		{"relative components are dropped", "bin:/usr/bin:.:/bin", "/patchy/bin:/usr/bin:/bin"},
		{"patchy's own directory is not repeated", "/patchy/bin:/usr/bin", "/patchy/bin:/usr/bin"},
		{"only garbage leaves patchy's alone", ":::", "/patchy/bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podPath(tt.in)
			if got != tt.want {
				t.Errorf("podPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.HasSuffix(got, ":") || strings.Contains(got, "::") || !strings.HasPrefix(got, "/patchy/bin") {
				t.Errorf("podPath(%q) = %q has an empty component or the wrong head", tt.in, got)
			}
		})
	}
}

func TestEphemeralStorage(t *testing.T) {
	cfg := testConfig()
	cfg.EphemeralStorage = "8Gi"
	job := buildJobForTest(t, cfg, testSpec())
	for _, ct := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		lists := map[string]corev1.ResourceList{"requests": ct.Resources.Requests, "limits": ct.Resources.Limits}
		for kind, rl := range lists {
			q, ok := rl[corev1.ResourceEphemeralStorage]
			if !ok || q.String() != "8Gi" {
				t.Errorf("%s %s ephemeral-storage = %v, want 8Gi", ct.Name, kind, q)
			}
		}
		if cpu := ct.Resources.Requests.Cpu().String(); cpu != "500m" {
			t.Errorf("%s: cpu request = %s, want the existing 500m beside it", ct.Name, cpu)
		}
	}

	cfg.EphemeralStorage = "lots"
	if _, err := New(fake.NewClientset(), cfg, nil).buildJob("j", testSpec()); err == nil ||
		!strings.Contains(err.Error(), "ephemeral-storage") {
		t.Errorf("buildJob with a bad ephemeral-storage quantity = %v, want a naming error", err)
	}

	// Ephemeral storage alone still renders a resource list.
	only := Config{Namespace: "n", Runners: testConfig().Runners, EphemeralStorage: "1Gi"}
	rr, err := only.resources()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rr.Limits[corev1.ResourceEphemeralStorage]; !ok || len(rr.Limits) != 1 {
		t.Errorf("limits = %v, want only ephemeral-storage", rr.Limits)
	}
}

func TestReservedEnvNames(t *testing.T) {
	names := ReservedEnvNames()
	if !slices.IsSorted(names) {
		t.Errorf("ReservedEnvNames() = %v, want sorted", names)
	}
	for _, want := range slices.Concat(
		[]string{"GITHUB_TOKEN", "ANTHROPIC_API_KEY", "PATCHY_REPO", "HOME"},
		provider.GatewayEnvNames, proxyEnv,
	) {
		if !slices.Contains(names, want) {
			t.Errorf("ReservedEnvNames() lacks %s", want)
		}
	}
	if len(names) != len(reservedEnv) {
		t.Errorf("ReservedEnvNames() has %d names, reservedEnv %d", len(names), len(reservedEnv))
	}
}

func TestProxyEnvReserved(t *testing.T) {
	cfg := testConfig()
	cfg.Env["HTTPS_PROXY"] = "http://evil:3128"
	cfg.Env["no_proxy"] = "broker"
	envs := envMap(buildJobForTest(t, cfg, testSpec()).Spec.Template.Spec.Containers[0])
	for _, name := range []string{"HTTPS_PROXY", "no_proxy"} {
		if got, ok := envs[name]; ok {
			t.Errorf("%s = %+v reached the pod from Config.Env, want reserved", name, got)
		}
	}
}

func TestExitSandboxUnenforced(t *testing.T) {
	if ExitSandboxUnenforced != 78 {
		t.Errorf("ExitSandboxUnenforced = %d, want 78", ExitSandboxUnenforced)
	}
	if ExitSandboxUnenforced != sandboxprobe.ExitUnenforced {
		t.Errorf("ExitSandboxUnenforced = %d, want the probe's %d", ExitSandboxUnenforced, sandboxprobe.ExitUnenforced)
	}
}

// TestStatusReadsPod: the collectors' view of a Job includes its pod's
// waiting reason, the prepare init's exit code and the image source.
func TestStatusReadsPod(t *testing.T) {
	newJob := func(ann map[string]string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "patchy-agents", Annotations: ann}}
	}
	waiting := func(reason, msg string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	}
	tests := []struct {
		name string
		job  *batchv1.Job
		pod  *corev1.Pod
		want Status
	}{
		{"no pod yet", newJob(nil), nil, Status{RunnerImageSource: "default"}},
		{
			"agent stuck pulling",
			newJob(map[string]string{annotationRunnerImageSource: "repository"}),
			jobPodInState("j", corev1.PodPending, waiting("ErrImagePull",
				"rpc error: manifest unknown")),
			Status{Waiting: "ErrImagePull", WaitingMessage: "rpc error: manifest unknown", RunnerImageSource: "repository"},
		},
		{
			"agent initializing behind the probe",
			newJob(map[string]string{annotationRunnerImageSource: "repository"}),
			jobPodInState("j", corev1.PodPending, waiting("PodInitializing", "")),
			Status{Waiting: "PodInitializing", RunnerImageSource: "repository"},
		},
		{
			"probe refused",
			newJob(map[string]string{annotationRunnerImageSource: "repository"}),
			withInit(jobPodInState("j", corev1.PodFailed, waiting("PodInitializing", "")), ExitSandboxUnenforced),
			Status{Waiting: "PodInitializing", InitExitCode: new(int32(78)), RunnerImageSource: "repository"},
		},
		{
			"default job running",
			newJob(nil),
			withInit(jobPodInState("j", corev1.PodRunning, corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{}}), 0),
			Status{InitExitCode: new(int32(0)), RunnerImageSource: "default"},
		},
		{
			"a foreign source value reads as default",
			newJob(map[string]string{annotationRunnerImageSource: "banana"}),
			nil,
			Status{RunnerImageSource: "default"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{tt.job}
			if tt.pod != nil {
				objs = append(objs, tt.pod)
			}
			c := New(fake.NewClientset(objs...), testConfig(), nil)
			got, err := c.Status(context.Background(), "j")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if (got.InitExitCode == nil) != (tt.want.InitExitCode == nil) ||
				(got.InitExitCode != nil && *got.InitExitCode != *tt.want.InitExitCode) {
				t.Errorf("InitExitCode = %v, want %v", deref32(got.InitExitCode), deref32(tt.want.InitExitCode))
			}
			got.InitExitCode, tt.want.InitExitCode = nil, nil
			if got != tt.want {
				t.Errorf("Status = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// withInit adds a terminated prepare init container status with the exit
// code to the pod.
func withInit(pod *corev1.Pod, code int32) *corev1.Pod {
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  initContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
	}}
	return pod
}

func deref32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestAgentEnvParsesToBinDir ties the two sides of the pod boundary: the
// env the Job hands the agent container, read by agent-runner's own
// parser, puts the injected directory in Config.BinDir on a repository
// image and nothing there on the default one, so preflight and the CLI pin
// switch on exactly when the pod was built for them.
func TestAgentEnvParsesToBinDir(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"repository image", injectedConfig(), patchyBinDir},
		{"default image", brokeredConfig(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := buildJobForTest(t, tt.cfg, injectedSpec()).Spec.Template.Spec.Containers[0]
			env := map[string]string{}
			for _, e := range agent.Env {
				env[e.Name] = e.Value
			}
			got, err := agentrun.FromEnv(func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("agentrun.FromEnv over the agent env: %v", err)
			}
			if got.BinDir != tt.want {
				t.Errorf("BinDir = %q, want %q", got.BinDir, tt.want)
			}
		})
	}
}

// TestInjectedEnvOwnsItsNames: on a repository image the names the
// injection owns — where the binaries are, the PATH, git's system-config
// switch, the self-updater switch and every scrubbed name — carry
// patchy's value exactly once, whatever the operator's Config.Env says.
// An operator value of the same name would otherwise sit beside patchy's
// as a duplicate, which Kubernetes leaves undefined.
func TestInjectedEnvOwnsItsNames(t *testing.T) {
	cfg := injectedConfig()
	operator := map[string]string{
		"PATH": "/opt/tools/bin", "PATCHY_BIN_DIR": "/opt/evil", "GIT_CONFIG_NOSYSTEM": "0",
		"DISABLE_AUTOUPDATER": "0", "LD_PRELOAD": "/opt/lib/hook.so", "NODE_OPTIONS": "--require /opt/hook.js",
		"BASH_ENV": "/opt/rc", "GIT_SSH_COMMAND": "ssh -o ProxyCommand=evil",
	}
	maps.Copy(cfg.Env, operator)
	agent := buildJobForTest(t, cfg, injectedSpec()).Spec.Template.Spec.Containers[0]

	want := map[string]string{
		"PATH": "/patchy/bin:" + repoSearchPath, "PATCHY_BIN_DIR": patchyBinDir, "GIT_CONFIG_NOSYSTEM": "1",
		"DISABLE_AUTOUPDATER": "1", "LD_PRELOAD": "", "NODE_OPTIONS": "", "BASH_ENV": "", "GIT_SSH_COMMAND": "",
		// An operator value for a name the injection does not own survives.
		"PATCHY_INVESTIGATE_TIMEOUT": "15m",
	}
	for name, value := range want {
		var got []string
		for _, e := range agent.Env {
			if e.Name == name {
				got = append(got, e.Value)
			}
		}
		if len(got) != 1 || got[0] != value {
			t.Errorf("agent env %s = %q, want exactly one entry %q", name, got, value)
		}
	}
}

// TestDefaultJobKeepsOperatorEnv: the takeover is the repository image's
// alone. A default Job passes the same operator values through as it
// always has — except PATCHY_BIN_DIR, which is per-Job (set only when the
// Job injects), since on the default image it would send agent-runner
// looking for an injected CLI that was never copied.
func TestDefaultJobKeepsOperatorEnv(t *testing.T) {
	cfg := testConfig()
	maps.Copy(cfg.Env, map[string]string{
		"PATH": "/opt/tools/bin", "LD_PRELOAD": "/opt/lib/jemalloc.so", "DISABLE_AUTOUPDATER": "1",
		"PATCHY_BIN_DIR": "/opt/evil",
	})
	envs := envMap(buildJobForTest(t, cfg, testSpec()).Spec.Template.Spec.Containers[0])
	for name, value := range map[string]string{
		"PATH": "/opt/tools/bin", "LD_PRELOAD": "/opt/lib/jemalloc.so", "DISABLE_AUTOUPDATER": "1",
	} {
		if got := envs[name]; got.Value != value {
			t.Errorf("default Job env %s = %+v, want the operator's %q", name, got, value)
		}
	}
	if got, ok := envs["PATCHY_BIN_DIR"]; ok {
		t.Errorf("PATCHY_BIN_DIR = %+v reached a default Job from Config.Env, want it reserved", got)
	}
}

// TestPropertyNoDuplicateEnv: whatever names the operator's Config.Env
// carries — owned, scrubbed, gateway, reserved, patchy's own or unrelated —
// and whatever the runner's gateway Env renders beside them, no container
// of any Job, injected or default, ever lists a name twice. Runner.Env is
// drawn from what provider.Env can render (the gateway names bar the
// broker token file, which the Job sets itself) plus the injection-owned
// and scrubbed names, since those must be taken over from it too. Seeded
// so the gate is deterministic.
func TestPropertyNoDuplicateEnv(t *testing.T) {
	rng := rand.New(rand.NewSource(20260923))
	owned := []string{"PATH", "PATCHY_BIN_DIR", "GIT_CONFIG_NOSYSTEM", "DISABLE_AUTOUPDATER"}
	operatorNames := slices.Concat(owned, []string{"HOME", "PATCHY_INVESTIGATE_TIMEOUT", "EDITOR", "LANG"},
		scrubEnv, provider.GatewayEnvNames, agentrun.ConfigEnvKeys(), ReservedEnvNames())
	runnerNames := slices.Concat(owned, scrubEnv, []string{"EDITOR"},
		slices.DeleteFunc(slices.Clone(provider.GatewayEnvNames), func(n string) bool {
			return n == "PATCHY_BROKER_TOKEN_FILE"
		}))
	for i := range 1000 {
		cfg := injectedConfig()
		cfg.AllowRepositoryImages = rng.Intn(3) != 0
		for range rng.Intn(12) {
			cfg.Env[operatorNames[rng.Intn(len(operatorNames))]] = "operator"
		}
		claude := cfg.Runners["claude"]
		claude.Env = maps.Clone(claude.Env)
		for range rng.Intn(4) {
			claude.Env[runnerNames[rng.Intn(len(runnerNames))]] = "runner"
		}
		cfg.Runners["claude"] = claude
		pod := buildJobForTest(t, cfg, injectedSpec()).Spec.Template.Spec
		for _, ct := range append(pod.InitContainers, pod.Containers...) {
			seen := map[string]bool{}
			for _, e := range ct.Env {
				if seen[e.Name] {
					t.Fatalf("iteration %d: %s env lists %s twice\n  config env: %v\n  runner env: %v",
						i, ct.Name, e.Name, cfg.Env, claude.Env)
				}
				seen[e.Name] = true
			}
		}
	}
}

// TestGitRedirectionsReservedNotBlanked: the variables that point git at a
// different repository, work tree, index or object store cannot be
// neutralised in the pod — Kubernetes can set a variable but never unset
// it, and git reads an empty GIT_DIR, GIT_WORK_TREE, GIT_OBJECT_DIRECTORY,
// GIT_COMMON_DIR or GIT_INDEX_FILE as a (broken) value, not as absent. So
// they are reserved instead: an image whose ENV sets one is refused at
// resolution (the list runnerimage.CheckEnv is handed), and the operator's
// Config.Env cannot set one either. Blanking them would break every git
// call agent-runner makes.
func TestGitRedirectionsReservedNotBlanked(t *testing.T) {
	reserved := map[string]bool{}
	for _, name := range ReservedEnvNames() {
		reserved[name] = true
	}
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_COMMON_DIR"} {
		if !reserved[name] {
			t.Errorf("ReservedEnvNames() lacks %s", name)
		}
		if err := runnerimage.CheckEnv([]string{name + "=/opt/elsewhere"}, reserved); err == nil {
			t.Errorf("an image ENV setting %s passes the resolver's check", name)
		}
		if slices.Contains(scrubEnv, name) {
			t.Errorf("scrubEnv blanks %s, which breaks git", name)
		}
	}
}

// TestScrubbedGitNamesNeutralWhenEmpty guards the other direction: every
// git variable the pod blanks must behave, empty, exactly as if unset, or
// the backstop breaks the agent's own git calls. Runs the real git binary.
func TestScrubbedGitNamesNeutralWhenEmpty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH:", err)
	}
	repo := t.TempDir()
	gitIn := func(extra ...string) (string, error) {
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), extra...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	init := exec.Command("git", "init", "-q")
	init.Dir = repo
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for _, name := range scrubEnv {
		if !strings.HasPrefix(name, "GIT_") {
			continue
		}
		if out, err := gitIn(name + "="); err != nil || out != "" {
			t.Errorf("git status with %s empty = (%q, %v), want a clean, silent success", name, out, err)
		}
	}
}
