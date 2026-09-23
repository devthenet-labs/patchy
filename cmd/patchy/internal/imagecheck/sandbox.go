// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// The sandbox's fixed shape: where the Job's prepare init copies the
// binaries from and to, the uid every patchy container runs as, and the
// pod's two writable emptyDirs.
const (
	toolsBinDir  = "/usr/local/bin"
	patchyBinDir = "/patchy/bin"
	sandboxUser  = "65532:65532"
	workspaceDir = "/workspace"
)

// The sandbox's resource bounds. The pod's own are the operator's Job
// configuration, which a workstation cannot know, so these are the CLI's:
// ample for the three commands a check runs, and small enough that an image
// whose bash or git forks or allocates without end cannot take the docker
// host down with it.
const (
	sandboxPids      = "512"
	sandboxMemory    = "2g"
	sandboxCPUs      = "1"
	sandboxTmpfsSize = "512m"
)

// removeTimeout bounds the forced removal of a container the sandbox made.
const removeTimeout = 30 * time.Second

// injected are the binaries the claude runner contributes to a
// repository-image Job (runnercfg's Inject for claude).
var injected = []string{"agent-runner", "claude"}

// runTimeout bounds one docker invocation. The first run of an image pulls
// it, which for a toolchain image of a few GiB takes minutes; the checks
// themselves take seconds, and agent-runner bounds each preflight command.
const runTimeout = 15 * time.Minute

// Result is how one command ended.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Commander runs the docker CLI. ExecCommander is the real one; tests fake
// it, so no unit test needs a docker.
type Commander interface {
	// LookPath reports where an executable is, or an error when it is not
	// on PATH.
	LookPath(file string) (string, error)
	// Run runs a command to completion. A non-zero exit is a Result, not an
	// error; the error is a command that could not start or was
	// interrupted.
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// SandboxConfig is what one sandbox run needs.
type SandboxConfig struct {
	// Image is what runs: the digest-pinned reference when it resolved,
	// otherwise the reference as given, so an image that exists only in the
	// local docker store still runs.
	Image string
	// SearchPath is the image's sanitized PATH. When empty it is derived
	// from the local image's config, as source-controller would.
	SearchPath []string
	// Platforms are the image's runnable platforms (os/arch[/variant]) as
	// the registry serves them. Each is run: the docker host's own natively
	// and first, the others under docker's emulation. An empty list means
	// the host's alone.
	Platforms []string
	// RunnerImage is the trusted image agent-runner and claude come from.
	RunnerImage string
	// BinDir is an empty host directory the binaries are copied into and
	// mounted read-only from; the caller creates and removes it.
	BinDir string
	// Progress narrates the slow steps; nil is silent.
	Progress func(string)
}

// sandboxChecks are the checks Sandbox reports, in order.
var sandboxChecks = []string{CheckRunner, CheckPreflight, CheckBash, CheckGit}

// SkipSandbox reports every sandbox check as skipped, for a run that was
// not asked for or cannot mean anything.
func SkipSandbox(reason string) []Check {
	var r Report
	r.skip(reason, sandboxChecks...)
	return r.Checks
}

// Sandbox runs the image under test the way a repository-image Job's agent
// container runs it, once for each platform it serves, since a pod may land
// on a node of any of them, and returns the runner, preflight, bash and git
// checks of each, labelled with that platform. A missing docker CLI or an
// unreachable daemon skips them all; a platform the docker host cannot
// emulate skips that platform's.
func Sandbox(ctx context.Context, cmd Commander, cfg SandboxConfig) []Check {
	progress := func(string) {}
	if cfg.Progress != nil {
		// A platform name is the index's own string; it is shown inert,
		// like every reason.
		progress = func(msg string) { cfg.Progress(printable(msg)) }
	}
	if _, err := cmd.LookPath("docker"); err != nil {
		return SkipSandbox("docker CLI not found on PATH; --run needs a local docker")
	}
	host, err := docker(ctx, cmd, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		return SkipSandbox("docker daemon unreachable: " + err.Error())
	}
	host = strings.TrimSpace(host)
	if !strings.HasPrefix(host, "linux/") {
		return SkipSandbox("the docker host runs " + host + " containers; the agent pod is linux")
	}

	searchPath := cfg.SearchPath
	if len(searchPath) == 0 {
		if searchPath, err = localSearchPath(ctx, cmd, cfg.Image); err != nil {
			return SkipSandbox(err.Error())
		}
	}

	var checks []Check
	for _, p := range runPlatforms(host, cfg.Platforms) {
		var platformChecks []Check
		if err := ctx.Err(); err != nil {
			platformChecks = SkipSandbox("the run was interrupted: " + err.Error())
		} else {
			platformChecks = sandboxAs(ctx, cmd, cfg, searchPath, p, progress)
		}
		for _, c := range platformChecks {
			c.Platform = printable(p.name)
			checks = append(checks, c)
		}
	}
	return checks
}

// sandboxAs runs the four sandbox checks as one platform.
func sandboxAs(ctx context.Context, cmd Commander, cfg SandboxConfig, searchPath []string, p runPlatform,
	progress func(string)) []Check {
	var r Report
	if p.emulated {
		if reason, ok := emulates(ctx, cmd, cfg.RunnerImage, p.name); !ok {
			r.skip(reason, sandboxChecks...)
			return r.Checks
		}
	}

	progress(fmt.Sprintf("copying %s out of %s (%s)", strings.Join(injected, " and "), cfg.RunnerImage, p.name))
	if err := extract(ctx, cmd, cfg.RunnerImage, p.name, cfg.BinDir); err != nil {
		r.add(CheckRunner, Fail, err.Error())
		r.skip("the runner binaries could not be copied", CheckPreflight, CheckBash, CheckGit)
		return r.Checks
	}
	reason := fmt.Sprintf("%s from %s", strings.Join(injected, " and "), cfg.RunnerImage)
	if p.emulated {
		reason += "; the docker host is not " + p.name + ", so it runs emulated"
	}
	r.add(CheckRunner, Pass, reason)

	run := func(entrypoint string, args ...string) (Result, error) {
		return runContainer(ctx, cmd, sandboxArgs(cfg.Image, p.name, cfg.BinDir, searchPath, entrypoint, args)...)
	}

	progress(fmt.Sprintf("running agent-runner %s in %s (%s)", agentrun.PreflightCommand, cfg.Image, p.name))
	res, err := run(patchyBinDir+"/agent-runner", agentrun.PreflightCommand)
	switch {
	case err != nil:
		r.add(CheckPreflight, Fail, "docker run: "+err.Error())
		r.skip("docker could not run the image", CheckBash, CheckGit)
		return r.Checks
	case res.ExitCode == dockerRunFailed:
		r.add(CheckPreflight, Fail, "docker could not run "+cfg.Image+": "+detail(res))
		r.skip("docker could not run the image", CheckBash, CheckGit)
		return r.Checks
	case res.ExitCode == dockerCannotInvoke || res.ExitCode == dockerNotFound:
		r.add(CheckPreflight, Fail, fmt.Sprintf("docker could not execute %s/agent-runner in the image (exit %d: %s)",
			patchyBinDir, res.ExitCode, detail(res)))
	case res.ExitCode == 0:
		r.add(CheckPreflight, Pass, lastLine(res.Stdout))
	case res.ExitCode == agentrun.ExitPreflightFailed:
		r.add(CheckPreflight, Fail, "image_incompatible: "+lastLine(res.Stdout))
	default:
		r.add(CheckPreflight, Fail, fmt.Sprintf("agent-runner %s reached no verdict (exit %d: %s); a runner "+
			"image older than this CLI has no `agent-runner %s`, so pass --runner-image with a current one",
			agentrun.PreflightCommand, res.ExitCode, detail(res), agentrun.PreflightCommand))
	}

	res, err = run("bash", "-c", "true")
	r.add(CheckBash, commandStatus(res, err), commandReason("bash -c true", res, err, "exited 0"))
	res, err = run("git", "--version")
	r.add(CheckGit, commandStatus(res, err), commandReason("git --version", res, err, lastLine(res.Stdout)))
	return r.Checks
}

// emulates asks whether the docker host can run binaries of platform at
// all, by running the runner image's own `true` as that platform: a trusted
// binary, so its failure says nothing about the image under test. Only a
// definite no (exec format error: no binfmt handler for the platform) is
// ok == false, with the reason to report; any other outcome lets the real
// checks run and speak for themselves.
func emulates(ctx context.Context, cmd Commander, runnerImage, platform string) (reason string, ok bool) {
	res, err := runContainer(ctx, cmd, "--platform", platform, "--network", "none", "--entrypoint", "true",
		runnerImage)
	if err != nil || res.ExitCode == 0 || !strings.Contains(res.Stderr+res.Stdout, "exec format error") {
		return "", true
	}
	return fmt.Sprintf("the docker host cannot run %s binaries (%s), so this platform went unchecked; "+
		"install emulation for it (binfmt_misc with QEMU) to check it", platform, detail(res)), false
}

// docker run's own exit statuses, as opposed to the container's command
// failing: 125 when it could not create or start the container at all (no
// such image, pull denied, bad flag), 126 when the command could not be
// invoked (permission denied) and 127 when it was not found.
const (
	dockerRunFailed    = 125
	dockerCannotInvoke = 126
	dockerNotFound     = 127
)

// docker runs one docker command and returns its stdout, or an error
// carrying the last line docker printed.
func docker(ctx context.Context, cmd Commander, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	res, err := cmd.Run(ctx, "docker", args...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("docker %s exited %d: %s", args[0], res.ExitCode, detail(res))
	}
	return res.Stdout, nil
}

// runPlatform is one platform the sandbox runs the image as.
type runPlatform struct {
	name     string
	emulated bool
}

// runPlatforms lists the platforms to run the image as: the docker host's
// own first, natively, when the image serves it (or nothing is known about
// the image), then each other platform the image serves, once per os/arch,
// which docker emulates.
func runPlatforms(host string, platforms []string) []runPlatform {
	var out []runPlatform
	if len(platforms) == 0 || slices.ContainsFunc(platforms, func(p string) bool { return osArch(p) == host }) {
		out = append(out, runPlatform{name: host})
	}
	seen := map[string]bool{host: true}
	for _, p := range platforms {
		if !seen[osArch(p)] {
			seen[osArch(p)] = true
			out = append(out, runPlatform{name: p, emulated: true})
		}
	}
	return out
}

// osArch drops a platform's variant: linux/arm64/v8 is linux/arm64.
func osArch(p string) string {
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return p
	}
	return parts[0] + "/" + parts[1]
}

// localSearchPath derives the search path from the local docker store's copy
// of the image, the way the resolver derives it from the registry's, for an
// image the registry could not provide.
func localSearchPath(ctx context.Context, cmd Commander, image string) ([]string, error) {
	out, err := docker(ctx, cmd, "image", "inspect", "--format", "{{json .Config.Env}}", image)
	if err != nil {
		return nil, fmt.Errorf("the image did not resolve and is not in the local docker store: %w", err)
	}
	var env []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		return nil, fmt.Errorf("docker image inspect: %w", err)
	}
	sp, err := runnerimage.SanitizePath(env)
	if err != nil {
		return nil, fmt.Errorf("the image's PATH is rejected: %w", err)
	}
	return sp, nil
}

// extract copies the injected binaries out of the runner image into binDir,
// the way the Job's prepare init copies them into the patchy-bin emptyDir,
// and always removes the container it made.
func extract(ctx context.Context, cmd Commander, runnerImage, platform, binDir string) error {
	id, err := docker(ctx, cmd, "create", "--quiet", "--platform", platform, runnerImage)
	if err != nil {
		return fmt.Errorf("could not create a container from %s: %w", runnerImage, err)
	}
	id = strings.TrimSpace(id)
	defer removeContainer(ctx, cmd, id)
	for _, bin := range injected {
		if _, err := docker(ctx, cmd, "cp", id+":"+toolsBinDir+"/"+bin, filepath.Join(binDir, bin)); err != nil {
			return fmt.Errorf("could not copy %s out of %s: %w", bin, runnerImage, err)
		}
	}
	return nil
}

// runContainer is one `docker run` of args in a container of its own,
// bounded by runTimeout (quiet, so a pull's progress never becomes the line
// a failure is explained by). Interrupting a run (Ctrl-C, the timeout)
// kills the docker client, not the container, and --rm removes only a
// container that exits, so each is named and force-removed by name however
// the run ends.
func runContainer(ctx context.Context, cmd Commander, args ...string) (Result, error) {
	name := "patchy-check-" + strings.ToLower(rand.Text())
	defer removeContainer(ctx, cmd, name)
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	return cmd.Run(ctx, "docker", append([]string{"run", "--rm", "--quiet", "--name", name}, args...)...)
}

// removeContainer force-removes a container the sandbox made, on a context
// of its own: the run it follows may have been interrupted, and the
// container must go anyway.
func removeContainer(ctx context.Context, cmd Commander, container string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	_, _ = cmd.Run(ctx, "docker", "rm", "--force", container)
}

// sandboxArgs is the runContainer arguments of one command in the emulated
// agent container: the pod's security context (uid 65532, read-only root,
// all capabilities dropped, no privilege escalation; docker's default
// seccomp profile stands in for RuntimeDefault), bounded processes, memory
// and CPU, no network at all (the pod reaches only DNS, the artifact server
// and the broker, none of which a check needs), the two emptyDirs as sized
// executable tmpfs mounts, the binaries read-only at /patchy/bin, the
// image's ENTRYPOINT replaced as the Job's Command replaces it, and the
// agent container's environment.
func sandboxArgs(image, platform, binDir string, searchPath []string, entrypoint string, args []string) []string {
	tmpfs := ":exec,mode=1777,size=" + sandboxTmpfsSize
	out := []string{
		"--platform", platform,
		"--user", sandboxUser, "--read-only", "--network", "none",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", sandboxPids, "--memory", sandboxMemory, "--cpus", sandboxCPUs,
		"--tmpfs", "/tmp" + tmpfs,
		"--tmpfs", workspaceDir + tmpfs,
		"--mount", "type=bind,source=" + binDir + ",target=" + patchyBinDir + ",readonly",
		"--workdir", workspaceDir,
		"--env", "HOME=" + workspaceDir,
	}
	for _, e := range jobs.InjectedEnv(strings.Join(searchPath, ":")) {
		out = append(out, "--env", e.Name+"="+e.Value)
	}
	out = append(out, "--entrypoint", entrypoint, image)
	return append(out, args...)
}

// commandStatus is Pass for a command that exited 0.
func commandStatus(res Result, err error) Status {
	if err == nil && res.ExitCode == 0 {
		return Pass
	}
	return Fail
}

// commandReason explains how a command ended: pass when it exited 0,
// otherwise its exit status and the last line it (or docker) printed.
func commandReason(name string, res Result, err error, pass string) string {
	switch {
	case err != nil:
		return "`" + name + "` could not run: " + err.Error()
	case res.ExitCode == 0:
		return pass
	}
	return "`" + name + "` exited " + strconv.Itoa(res.ExitCode) + ": " + detail(res)
}

// detail is the line worth showing from a failed command: the last one it
// wrote to stderr, or to stdout when stderr is empty, without docker's
// usage hint, and cut to the exec error when docker could not start the
// command at all (the runtime's error chain before it is noise).
func detail(res Result) string {
	out := res.Stderr
	if strings.TrimSpace(out) == "" {
		out = res.Stdout
	}
	var kept []string
	for l := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "Run 'docker ") {
			kept = append(kept, l)
		}
	}
	line := lastLine(strings.Join(kept, "\n"))
	if i := strings.LastIndex(line, "exec: "); i >= 0 && strings.HasPrefix(line, "docker: ") {
		line = strings.TrimSuffix(line[i:], ": unknown")
	}
	return line
}

// lastLine is the last non-blank line of s, which is where docker and the
// commands it runs put the line worth reading.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "(no output)"
}
