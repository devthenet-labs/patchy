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
	// interrupt, when set, is consulted first: a non-nil error ends that
	// invocation as an interrupted one (the Commander's error).
	interrupt func(ctx context.Context, args []string) error
	// rmCtxErrs records, per `docker rm`, whether its context was already
	// done when it was issued.
	rmCtxErrs []error
}

func (f *fakeDocker) LookPath(file string) (string, error) {
	if f.missing {
		return "", errors.New("executable file not found in $PATH")
	}
	return "/usr/local/bin/" + file, nil
}

func (f *fakeDocker) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if name != "docker" {
		return Result{}, errors.New("unexpected command " + name)
	}
	f.calls = append(f.calls, args)
	if args[0] == "rm" {
		f.rmCtxErrs = append(f.rmCtxErrs, ctx.Err())
	}
	if f.interrupt != nil {
		if err := f.interrupt(ctx, args); err != nil {
			return Result{}, err
		}
	}
	return f.answer(args), nil
}

// flagValue is the value that follows flag in a docker invocation.
func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
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
		Image:      "ghcr.io/org/app@sha256:" + strings.Repeat("a", 64),
		SearchPath: []string{"/usr/local/go/bin", "/usr/bin"},
		// The docker host's own platform alone; the multi-platform tests
		// set their own.
		Platforms:   []string{"linux/arm64"},
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
		" --cap-drop ALL ", " --security-opt no-new-privileges ", " --tmpfs /tmp:exec,mode=1777,size=512m ",
		" --tmpfs /workspace:exec,mode=1777,size=512m ", " --pids-limit 512 ", " --memory 2g ", " --cpus 1 ",
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

// TestSandboxRemovesItsContainers: every container of the image under test
// is named and force-removed once its run ends, even when the run was
// interrupted (Ctrl-C, or the run timeout), because killing the docker
// client leaves the container running on the daemon.
func TestSandboxRemovesItsContainers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := healthyDocker(nil)
	d.interrupt = func(_ context.Context, args []string) error {
		if args[0] == "run" && entrypoint(args) == "git" {
			cancel() // the user pressed Ctrl-C while git hung
			return context.Canceled
		}
		return nil
	}
	checks := Sandbox(ctx, d, sandboxConfig())
	if got := checkLine(checks); got != "runner=PASS preflight=PASS bash=PASS git=FAIL" {
		t.Fatalf("checks = %s\n%+v", got, checks)
	}
	seen := map[string]bool{}
	for _, run := range d.runs() {
		name := flagValue(run, "--name")
		if name == "" || seen[name] {
			t.Fatalf("docker run of %s has no unique --name (%q)", entrypoint(run), name)
		}
		seen[name] = true
		if !slices.ContainsFunc(d.calls, func(c []string) bool { return slices.Equal(c, []string{"rm", "--force", name}) }) {
			t.Errorf("container %s was never removed; calls %v", name, d.calls)
		}
	}
	for i, err := range d.rmCtxErrs {
		if err != nil {
			t.Errorf("docker rm %d was issued on a context already done (%v), so it could never run", i, err)
		}
	}
}

// TestSandboxBoundsTheContainer: the image under test runs with bounded
// processes, memory, CPU and tmpfs, so a hostile bash or git cannot fork-
// bomb or exhaust the docker host.
func TestSandboxBoundsTheContainer(t *testing.T) {
	d := healthyDocker(nil)
	Sandbox(context.Background(), d, sandboxConfig())
	runs := d.runs()
	if len(runs) == 0 {
		t.Fatal("docker run was never invoked")
	}
	for _, run := range runs {
		for _, flag := range []string{"--pids-limit", "--memory", "--cpus"} {
			if flagValue(run, flag) == "" {
				t.Errorf("docker run of %s lacks %s", entrypoint(run), flag)
			}
		}
		tmpfs := 0
		for i, a := range run {
			if a == "--tmpfs" && i+1 < len(run) {
				tmpfs++
				if !strings.Contains(run[i+1], ",size=") {
					t.Errorf("docker run of %s: tmpfs %s has no size", entrypoint(run), run[i+1])
				}
			}
		}
		if tmpfs != 2 {
			t.Errorf("docker run of %s has %d tmpfs mounts, want /tmp and /workspace", entrypoint(run), tmpfs)
		}
	}
}

// TestSandboxAgentRunnerNotExecutable: when docker cannot execute the
// mounted agent-runner at all (exit 126: permission denied, as when uid
// 65532 cannot search the bind-mounted directory; exit 127: not found),
// the preflight fails with docker's own reason, not with advice to replace
// a runner image that is not at fault.
func TestSandboxAgentRunnerNotExecutable(t *testing.T) {
	for code, stderr := range map[int]string{
		126: "docker: Error response from daemon: failed to create task for container: failed to create shim " +
			"task: OCI runtime create failed: runc create failed: unable to start container process: error " +
			"during container init: exec: \"/patchy/bin/agent-runner\": stat /patchy/bin/agent-runner: " +
			"permission denied: unknown\n\nRun 'docker run --help' for more information\n",
		127: "docker: Error response from daemon: failed to create task for container: OCI runtime create " +
			"failed: exec: \"/patchy/bin/agent-runner\": stat /patchy/bin/agent-runner: no such file or " +
			"directory: unknown\n",
	} {
		d := healthyDocker(func(args []string) (Result, bool) {
			return Result{ExitCode: code, Stderr: stderr}, entrypoint(args) == "/patchy/bin/agent-runner"
		})
		checks := Sandbox(context.Background(), d, sandboxConfig())
		if got := checkLine(checks); got != "runner=PASS preflight=FAIL bash=PASS git=PASS" {
			t.Fatalf("exit %d: checks = %s\n%+v", code, got, checks)
		}
		reason := checks[1].Reason
		if strings.Contains(reason, "--runner-image") || strings.Contains(reason, "older") {
			t.Errorf("exit %d: reason blames the runner image's age: %s", code, reason)
		}
		if !strings.Contains(reason, `exec: "/patchy/bin/agent-runner": stat /patchy/bin/agent-runner`) ||
			!strings.Contains(reason, "could not execute") {
			t.Errorf("exit %d: reason = %s, want docker's own exec error", code, reason)
		}
	}
}

// platformLine is checkLine with each check's platform.
func platformLine(checks []Check) string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Platform+":"+c.Name+"="+string(c.Status))
	}
	return strings.Join(out, " ")
}

// TestSandboxRunsEveryPlatform: an index must satisfy the contract on every
// platform a node could pull, so each one it serves is run, the docker
// host's own first and natively, the others under docker's emulation, with
// the runner's binaries for that platform, and every line names its
// platform.
func TestSandboxRunsEveryPlatform(t *testing.T) {
	d := healthyDocker(nil)
	cfg := sandboxConfig()
	cfg.Platforms = []string{"linux/amd64", "linux/arm64"}
	checks := Sandbox(context.Background(), d, cfg)
	want := "linux/arm64:runner=PASS linux/arm64:preflight=PASS linux/arm64:bash=PASS linux/arm64:git=PASS " +
		"linux/amd64:runner=PASS linux/amd64:preflight=PASS linux/amd64:bash=PASS linux/amd64:git=PASS"
	if got := platformLine(checks); got != want {
		t.Fatalf("checks = %s\nwant     %s", got, want)
	}
	if strings.Contains(checks[0].Reason, "emulated") || !strings.Contains(checks[4].Reason, "emulated") {
		t.Errorf("runner reasons = %q, %q; want only linux/amd64 marked emulated", checks[0].Reason,
			checks[4].Reason)
	}
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		create := []string{"create", "--quiet", "--platform", platform, cfg.RunnerImage}
		if !slices.ContainsFunc(d.calls, func(c []string) bool { return slices.Equal(c, create) }) {
			t.Errorf("the runner binaries were never copied for %s", platform)
		}
		var entrypoints []string
		for _, run := range d.runs() {
			if flagValue(run, "--platform") == platform && flagValue(run, "--entrypoint") != "true" {
				entrypoints = append(entrypoints, entrypoint(run))
			}
		}
		if want := []string{"/patchy/bin/agent-runner", "bash", "git"}; !slices.Equal(entrypoints, want) {
			t.Errorf("%s ran %v, want %v", platform, entrypoints, want)
		}
	}
}

// TestSandboxSkipsPlatformsItCannotEmulate: a docker host with no emulation
// for a platform (no binfmt handler: the runner image's own binary fails
// with exec format error) cannot say anything about it, so that platform's
// lines are SKIP with the reason, never FAIL and never silently absent.
func TestSandboxSkipsPlatformsItCannotEmulate(t *testing.T) {
	d := healthyDocker(func(args []string) (Result, bool) {
		return Result{ExitCode: 255, Stderr: "exec /usr/bin/true: exec format error\n"},
			args[0] == "run" && flagValue(args, "--platform") == "linux/amd64"
	})
	cfg := sandboxConfig()
	cfg.Platforms = []string{"linux/amd64", "linux/arm64"}
	checks := Sandbox(context.Background(), d, cfg)
	want := "linux/arm64:runner=PASS linux/arm64:preflight=PASS linux/arm64:bash=PASS linux/arm64:git=PASS " +
		"linux/amd64:runner=SKIP linux/amd64:preflight=SKIP linux/amd64:bash=SKIP linux/amd64:git=SKIP"
	if got := platformLine(checks); got != want {
		t.Fatalf("checks = %s\nwant     %s", got, want)
	}
	if !strings.Contains(checks[4].Reason, "exec format error") || !strings.Contains(checks[4].Reason, "emulat") {
		t.Errorf("reason = %q, want the host's missing emulation named", checks[4].Reason)
	}
	for _, run := range d.runs() {
		if flagValue(run, "--platform") == "linux/amd64" && strings.Contains(strings.Join(run, " "), "@sha256:") {
			t.Errorf("the image under test ran as linux/amd64 after the probe failed: %v", entrypoint(run))
		}
	}
}

// TestSandboxStopsWhenInterrupted: once the run is interrupted, the
// platforms not yet run are skipped as interrupted rather than failed one
// docker call at a time.
func TestSandboxStopsWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := healthyDocker(nil)
	d.interrupt = func(_ context.Context, args []string) error {
		if args[0] == "run" && entrypoint(args) == "git" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	cfg := sandboxConfig()
	cfg.Platforms = []string{"linux/amd64", "linux/arm64"}
	checks := Sandbox(ctx, d, cfg)
	want := "linux/arm64:runner=PASS linux/arm64:preflight=PASS linux/arm64:bash=PASS linux/arm64:git=FAIL " +
		"linux/amd64:runner=SKIP linux/amd64:preflight=SKIP linux/amd64:bash=SKIP linux/amd64:git=SKIP"
	if got := platformLine(checks); got != want {
		t.Fatalf("checks = %s\nwant     %s", got, want)
	}
	if !strings.Contains(checks[4].Reason, "interrupted") {
		t.Errorf("reason = %q, want the interruption named", checks[4].Reason)
	}
	for _, c := range d.calls {
		if flagValue(c, "--platform") == "linux/amd64" {
			t.Errorf("docker was still called for linux/amd64 after the interruption: %v", c[:2])
		}
	}
}

// TestSandboxPlatformIsPrintable: a platform name is the index's own
// string, so the label and the progress narrating it are shown inert.
func TestSandboxPlatformIsPrintable(t *testing.T) {
	var progress []string
	cfg := sandboxConfig()
	cfg.Platforms = []string{"linux/arm64", "linux/amd64/\x1b[2K"}
	cfg.Progress = func(msg string) { progress = append(progress, msg) }
	checks := Sandbox(context.Background(), healthyDocker(nil), cfg)
	if len(checks) != 8 || checks[4].Platform != `linux/amd64/\x1b[2K` {
		t.Fatalf("checks = %+v, want the second platform labelled with its escape shown", checks)
	}
	for _, msg := range progress {
		if strings.ContainsRune(msg, 0x1b) {
			t.Errorf("progress %q carries a raw escape", msg)
		}
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

func TestRunPlatforms(t *testing.T) {
	cases := []struct {
		host      string
		platforms []string
		want      []runPlatform
	}{
		{"linux/arm64", nil, []runPlatform{{name: "linux/arm64"}}},
		{"linux/arm64", []string{"linux/amd64", "linux/arm64/v8"},
			[]runPlatform{{name: "linux/arm64"}, {name: "linux/amd64", emulated: true}}},
		{"linux/arm64", []string{"linux/amd64"}, []runPlatform{{name: "linux/amd64", emulated: true}}},
		{"linux/amd64", []string{"linux/amd64"}, []runPlatform{{name: "linux/amd64"}}},
		{"linux/amd64", []string{"linux/arm64/v8", "linux/amd64", "linux/arm64"},
			[]runPlatform{{name: "linux/amd64"}, {name: "linux/arm64/v8", emulated: true}}},
	}
	for _, tc := range cases {
		if got := runPlatforms(tc.host, tc.platforms); !slices.Equal(got, tc.want) {
			t.Errorf("runPlatforms(%s, %v) = %+v, want %+v", tc.host, tc.platforms, got, tc.want)
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
