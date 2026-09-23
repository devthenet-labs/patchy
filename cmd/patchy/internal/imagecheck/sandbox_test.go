// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
)

// fakeDocker stands in for the docker CLI: answer maps one invocation's
// arguments onto how it ended, and every invocation is recorded.
type fakeDocker struct {
	missing bool
	answer  func(args []string) Result
	calls   [][]string
}

func (f *fakeDocker) LookPath(file string) (string, error) {
	if f.missing {
		return "", errors.New("executable file not found in $PATH")
	}
	return "/usr/local/bin/" + file, nil
}

func (f *fakeDocker) Run(_ context.Context, name string, args ...string) (Result, error) {
	if name != "docker" {
		return Result{}, errors.New("unexpected command " + name)
	}
	f.calls = append(f.calls, args)
	return f.answer(args), nil
}

// runs returns the recorded docker run invocations.
func (f *fakeDocker) runs() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[0] == "run" {
			out = append(out, c)
		}
	}
	return out
}

// entrypoint is what a docker run invocation executes.
func entrypoint(args []string) string {
	if i := slices.Index(args, "--entrypoint"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// healthyDocker answers like a working arm64 docker host and a compatible
// image; override tweaks individual answers.
func healthyDocker(override func(args []string) (Result, bool)) *fakeDocker {
	return &fakeDocker{answer: func(args []string) Result {
		if override != nil {
			if res, ok := override(args); ok {
				return res
			}
		}
		switch args[0] {
		case "version":
			return Result{Stdout: "linux/arm64\n"}
		case "create":
			return Result{Stdout: "c0ffee\n"}
		case "cp", "rm":
			return Result{}
		case "image":
			return Result{Stdout: `["PATH=/opt/local/bin:/usr/bin"]` + "\n"}
		case "run":
			switch entrypoint(args) {
			case "/patchy/bin/agent-runner":
				return Result{Stdout: "preflight passed: `/patchy/bin/claude --version`, `git --version` and " +
					"`bash -c true` ran\n"}
			case "git":
				return Result{Stdout: "git version 2.51.0\n"}
			}
			return Result{}
		}
		return Result{ExitCode: 1, Stderr: "unexpected docker " + args[0]}
	}}
}

func sandboxConfig() SandboxConfig {
	return SandboxConfig{
		Image:       "ghcr.io/org/app@sha256:" + strings.Repeat("a", 64),
		SearchPath:  []string{"/usr/local/go/bin", "/usr/bin"},
		Platforms:   []string{"linux/amd64", "linux/arm64"},
		RunnerImage: RunnerImageRepository + ":v1.2.3",
		BinDir:      "/tmp/patchy-bin",
	}
}

func checkLine(checks []Check) string {
	return statuses(Report{Checks: checks})
}

// TestSandboxRunsThePodShape: the image runs under the agent container's
// security context and environment, with the runner's binaries mounted
// read-only, and each check reads its own command's result.
func TestSandboxRunsThePodShape(t *testing.T) {
	d := healthyDocker(nil)
	checks := Sandbox(context.Background(), d, sandboxConfig())
	if got := checkLine(checks); got != "runner=PASS preflight=PASS bash=PASS git=PASS" {
		t.Fatalf("checks = %s\n%+v", got, checks)
	}
	if !strings.Contains(checks[3].Reason, "git version 2.51.0") ||
		!strings.Contains(checks[1].Reason, "preflight passed") {
		t.Errorf("reasons = %+v, want each command's own output", checks)
	}

	// The binaries come out of the runner image for the host's platform,
	// and the container that held them is removed.
	wantCalls := [][]string{
		{"create", "--quiet", "--platform", "linux/arm64", RunnerImageRepository + ":v1.2.3"},
		{"cp", "c0ffee:/usr/local/bin/agent-runner", "/tmp/patchy-bin/agent-runner"},
		{"cp", "c0ffee:/usr/local/bin/claude", "/tmp/patchy-bin/claude"},
		{"rm", "--force", "c0ffee"},
	}
	for _, want := range wantCalls {
		if !slices.ContainsFunc(d.calls, func(c []string) bool { return slices.Equal(c, want) }) {
			t.Errorf("docker was never called with %v; calls %v", want, d.calls)
		}
	}

	runs := d.runs()
	if len(runs) != 3 {
		t.Fatalf("docker run invoked %d times, want preflight, bash and git", len(runs))
	}
	image := sandboxConfig().Image
	for i, want := range [][]string{
		{"/patchy/bin/agent-runner", image, agentrun.PreflightCommand},
		{"bash", image, "-c", "true"},
		{"git", image, "--version"},
	} {
		if tail := runs[i][len(runs[i])-len(want):]; !slices.Equal(tail, want) {
			t.Errorf("run %d ends %v, want %v", i, tail, want)
		}
	}
	joined := " " + strings.Join(runs[0], " ") + " "
	for _, want := range []string{
		" --rm ", " --platform linux/arm64 ", " --user 65532:65532 ", " --read-only ", " --network none ",
		" --cap-drop ALL ", " --security-opt no-new-privileges ", " --tmpfs /tmp:exec,mode=1777 ",
		" --tmpfs /workspace:exec,mode=1777 ",
		" --mount type=bind,source=/tmp/patchy-bin,target=/patchy/bin,readonly ",
		" --workdir /workspace ", " --env HOME=/workspace ",
		" --env PATH=/patchy/bin:/usr/local/go/bin:/usr/bin ",
		" --env " + agentrun.BinDirEnv + "=/patchy/bin ", " --env GIT_CONFIG_NOSYSTEM=1 ",
		// The pod's blanks reach the local run too.
		" --env BASH_ENV= ", " --env LD_PRELOAD= ", " --env HTTPS_PROXY= ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run lacks %q:\n%s", strings.TrimSpace(want), joined)
		}
	}
}

func TestSandboxOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		docker   *fakeDocker
		cfg      func(SandboxConfig) SandboxConfig
		want     string
		reason   string // substring of the first non-PASS check's reason
		platform string // the --platform the runs used, when they ran
	}{
		{name: "no docker CLI", docker: &fakeDocker{missing: true},
			want: "runner=SKIP preflight=SKIP bash=SKIP git=SKIP", reason: "docker CLI not found on PATH"},
		{name: "daemon down", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}, args[0] == "version"
		}), want: "runner=SKIP preflight=SKIP bash=SKIP git=SKIP", reason: "Cannot connect to the Docker daemon"},
		{name: "windows containers", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{Stdout: "windows/amd64\n"}, args[0] == "version"
		}), want: "runner=SKIP preflight=SKIP bash=SKIP git=SKIP", reason: "the agent pod is linux"},
		{name: "runner image unavailable", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: 1, Stderr: "manifest unknown"}, args[0] == "create"
		}), want: "runner=FAIL preflight=SKIP bash=SKIP git=SKIP", reason: "manifest unknown"},
		{name: "incompatible image", docker: healthyDocker(func(args []string) (Result, bool) {
			switch entrypoint(args) {
			case "/patchy/bin/agent-runner":
				return Result{ExitCode: agentrun.ExitPreflightFailed,
					Stdout: "preflight: `bash -c true` could not start: exec: \"bash\": not found\n"}, true
			case "bash":
				return Result{ExitCode: 127, Stderr: "docker: Error response from daemon: failed to create task " +
					"for container: OCI runtime create failed: exec: \"bash\": executable file not found in $PATH\n\n" +
					"Run 'docker run --help' for more information\n"}, true
			}
			return Result{}, false
		}), want: "runner=PASS preflight=FAIL bash=FAIL git=PASS",
			reason: "image_incompatible: preflight: `bash -c true` could not start", platform: "linux/arm64"},
		{name: "runner older than the preflight subcommand", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: 2, Stderr: `level=ERROR msg="invalid configuration" error="PATCHY_REPO is required"`},
				entrypoint(args) == "/patchy/bin/agent-runner"
		}), want: "runner=PASS preflight=FAIL bash=PASS git=PASS", reason: "reached no verdict (exit 2"},
		{name: "docker cannot start the image", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: 125, Stderr: "pull access denied for ghcr.io/org/app"}, args[0] == "run"
		}), want: "runner=PASS preflight=FAIL bash=SKIP git=SKIP", reason: "pull access denied"},
		{name: "image only for another architecture runs emulated", docker: healthyDocker(nil),
			cfg:  func(c SandboxConfig) SandboxConfig { c.Platforms = []string{"linux/amd64"}; return c },
			want: "runner=PASS preflight=PASS bash=PASS git=PASS", platform: "linux/amd64"},
		{name: "unresolved image found locally", docker: healthyDocker(nil),
			cfg: func(c SandboxConfig) SandboxConfig {
				c.Image, c.SearchPath, c.Platforms = "app:dev", nil, nil
				return c
			},
			want: "runner=PASS preflight=PASS bash=PASS git=PASS", platform: "linux/arm64"},
		{name: "unresolved image missing locally", docker: healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: 1, Stderr: "Error: No such image: app:dev"}, args[0] == "image"
		}), cfg: func(c SandboxConfig) SandboxConfig { c.Image, c.SearchPath = "app:dev", nil; return c },
			want: "runner=SKIP preflight=SKIP bash=SKIP git=SKIP", reason: "not in the local docker store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sandboxConfig()
			if tc.cfg != nil {
				cfg = tc.cfg(cfg)
			}
			checks := Sandbox(context.Background(), tc.docker, cfg)
			if got := checkLine(checks); got != tc.want {
				t.Fatalf("checks = %s, want %s\n%+v", got, tc.want, checks)
			}
			if tc.reason != "" {
				i := slices.IndexFunc(checks, func(c Check) bool { return c.Status != Pass })
				if i < 0 || !strings.Contains(checks[i].Reason, tc.reason) {
					t.Errorf("checks = %+v, want the first non-PASS reason to mention %q", checks, tc.reason)
				}
			}
			for _, run := range tc.docker.runs() {
				if i := slices.Index(run, "--platform"); tc.platform != "" && run[i+1] != tc.platform {
					t.Errorf("docker run --platform %s, want %s", run[i+1], tc.platform)
				}
			}
			if tc.platform != "" && len(tc.docker.runs()) == 0 {
				t.Error("docker run was never invoked")
			}
		})
	}
}

// TestSandboxLocalSearchPath: an image the registry could not provide runs
// with the PATH its local copy declares, sanitized as the resolver would.
func TestSandboxLocalSearchPath(t *testing.T) {
	d := healthyDocker(nil)
	cfg := sandboxConfig()
	cfg.Image, cfg.SearchPath = "app:dev", nil
	Sandbox(context.Background(), d, cfg)
	runs := d.runs()
	if len(runs) == 0 || !slices.Contains(runs[0], "PATH=/patchy/bin:/opt/local/bin:/usr/bin") {
		t.Errorf("runs = %v, want the local image's PATH behind /patchy/bin", runs)
	}
}

func TestDefaultRunnerImage(t *testing.T) {
	for version, want := range map[string]string{
		"0.11.6":  RunnerImageRepository + ":v0.11.6",
		"v0.12.0": RunnerImageRepository + ":v0.12.0",
		"dev":     RunnerImageRepository + ":latest",
		"":        RunnerImageRepository + ":latest",
		"none.1":  RunnerImageRepository + ":latest",
	} {
		if got := DefaultRunnerImage(version); got != want {
			t.Errorf("DefaultRunnerImage(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestChoosePlatform(t *testing.T) {
	cases := []struct {
		host      string
		platforms []string
		want      string
		emulated  bool
	}{
		{"linux/arm64", nil, "linux/arm64", false},
		{"linux/arm64", []string{"linux/amd64", "linux/arm64/v8"}, "linux/arm64", false},
		{"linux/arm64", []string{"linux/amd64"}, "linux/amd64", true},
		{"linux/amd64", []string{"linux/amd64"}, "linux/amd64", false},
	}
	for _, tc := range cases {
		got, emulated := choosePlatform(tc.host, tc.platforms)
		if got != tc.want || emulated != tc.emulated {
			t.Errorf("choosePlatform(%s, %v) = %s, %v; want %s, %v", tc.host, tc.platforms, got, emulated, tc.want,
				tc.emulated)
		}
	}
}

func TestSkipSandbox(t *testing.T) {
	if got := checkLine(SkipSandbox("not asked")); got != "runner=SKIP preflight=SKIP bash=SKIP git=SKIP" {
		t.Errorf("SkipSandbox = %s", got)
	}
}

func TestDetail(t *testing.T) {
	cases := []struct {
		res  Result
		want string
	}{
		{Result{Stderr: "docker: Error response from daemon: failed to create task for container: runc create " +
			"failed: error during container init: exec: \"bash\": executable file not found in $PATH\n\n" +
			"Run 'docker run --help' for more information\n"}, `exec: "bash": executable file not found in $PATH`},
		{Result{Stderr: "fatal: bad config\n", Stdout: "ignored"}, "fatal: bad config"},
		{Result{Stdout: "only stdout\n"}, "only stdout"},
		{Result{}, "(no output)"},
	}
	for _, tc := range cases {
		if got := detail(tc.res); got != tc.want {
			t.Errorf("detail(%+v) = %q, want %q", tc.res, got, tc.want)
		}
	}
}
