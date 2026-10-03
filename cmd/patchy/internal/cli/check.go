// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/imagecheck"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/runnerimage/resolve"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// staticCheckTimeout bounds the registry side of `check image`: a few
// manifest and config fetches, a signature lookup. A wedged registry must
// not hang a shell.
const staticCheckTimeout = 2 * time.Minute

// newCheckCmd is the `check` verb: judge something the way the pipeline
// will, before it reaches the pipeline. `check image` is cluster-free like
// `dev` and `mirror` (the persistent kubeconfig flags are inert there);
// `check project` reads the cluster with the caller's own kubeconfig.
func newCheckCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check something the way patchy will judge it, before the pipeline does",
		Long: "Judge something you are handing to patchy the way the pipeline will judge it,\n" +
			"from your workstation, and report every verdict as a PASS, FAIL or SKIP line.\n\n" +
			"check image needs no cluster: it checks an agent image a repository means to\n" +
			"declare, and the kubeconfig flags are inert for it. check project reads the\n" +
			"cluster with your own kubeconfig, and GitHub and the registry with your own\n" +
			"credentials, to tell whether a Project is ready for its first intent.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newCheckImageCmd(opts), newCheckProjectCmd(opts))
	return cmd
}

// checkImageFlags are `check image`'s flags.
type checkImageFlags struct {
	allow       []string
	deny        []string
	cosignKey   string
	maxBytes    int64
	run         bool
	runnerImage string
}

// newCheckImageCmd checks a repository-declared agent image.
func newCheckImageCmd(opts *Options) *cobra.Command {
	f := &checkImageFlags{}
	cmd := &cobra.Command{
		Use:   "image <reference>",
		Short: "Check an agent image a repository means to declare",
		Long: "Check an image before declaring it in .patchy/agent.yaml or as the image of\n" +
			".devcontainer/devcontainer.json, with the checks patchy itself runs.\n\n" +
			"Without --run, the checks are source-controller's own, run by the same code: the\n" +
			"reference is canonicalised, checked against --allow (the operator's\n" +
			"--repository-image-registries) when given, less any --deny (the operator's\n" +
			"--repository-image-denied-registries, which the chart sets to the preview image\n" +
			"prefix), pinned to a digest, and every linux/amd64 and linux/arm64 manifest is\n" +
			"judged for platform, compressed size, VOLUME, reserved ENV and PATH; with\n" +
			"--cosign-key the signature is verified as well. Registry credentials are your\n" +
			"local docker credentials (~/.docker/config.json and its credential helpers), so\n" +
			"an image you can pull is an image this can check. Every check is reported, not\n" +
			"just the first failure.\n\n" +
			"With --run, the image is also run the way the agent pod runs it, on your local\n" +
			"docker: agent-runner and the claude CLI are copied out of the claude runner\n" +
			"image released with this CLI or, for a development build, which has none, the\n" +
			"newest release in the registry (the highest vX.Y.Z tag, never latest), pinned\n" +
			"to the digest its tag names there now, so no stale local copy of the tag stands\n" +
			"in for it. Which registry that is was stamped into this CLI by the build that\n" +
			"made it: a release names the registry it published its images to, and a plain\n" +
			"go build names none. --runner-image overrides the choice and is used as given\n" +
			"(a tag as your local docker has it). The runner-image line names the image and\n" +
			"digest used; when none can be chosen (the registry is unreachable, or the CLI\n" +
			"knows none) it fails, the rest of the run is skipped, and --runner-image is the\n" +
			"way on. The image runs as uid 65532 with a read-only root filesystem, no\n" +
			"network, no capabilities, no privilege escalation, bounded processes, memory\n" +
			"and CPU, sized executable tmpfs mounts at /tmp and /workspace, the two binaries\n" +
			"read-only at /patchy/bin, PATH=/patchy/bin:<the image's PATH> and the rest of\n" +
			"the pod's environment; each container is removed when its run ends, even an\n" +
			"interrupted one. In it, agent-runner's own preflight (the check a stage runs\n" +
			"before its first model call: claude --version, git --version and bash -c true)\n" +
			"runs, then bash -c true and git --version on their own. A pod may land on a\n" +
			"node of any platform the image serves, so all of that runs once per platform:\n" +
			"the docker host's own natively and first, any other under docker's emulation\n" +
			"(Docker Desktop has it; on Linux, binfmt_misc with QEMU). A platform the docker\n" +
			"host cannot emulate is reported as SKIP. Without a docker CLI the run is\n" +
			"skipped, not failed. An image that exists only in your local docker store fails\n" +
			"the registry checks but still runs; to check both before publishing, push it\n" +
			"to a scratch tag or a local registry.\n\n" +
			"Each check prints one line: PASS, FAIL or SKIP, the check, the platform for a\n" +
			"--run check, and the reason.\n" +
			"-o json or -o yaml prints the whole report as data instead. The exit status is\n" +
			"non-zero when any check fails.",
		Example: "  patchy check image ghcr.io/acme/shop-agent:1\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --cosign-key cosign.pub\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --deny ghcr.io/acme/previews/\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --run\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --run -o json | jq '.checks[] | select(.status == \"FAIL\")'",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckImage(cmd.Context(), opts, f, args[0], defaultCheckImageDeps())
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.allow, "allow", nil,
		"registry path prefix the image must sit under, as the operator's --repository-image-registries "+
			"entries (repeatable)")
	fl.StringArrayVar(&f.deny, "deny", nil,
		"registry path prefix the image may never sit under, even inside an --allow entry, as the operator's "+
			"--repository-image-denied-registries entries: the chart's preview image prefix (repeatable; needs --allow)")
	fl.StringVar(&f.cosignKey, "cosign-key", "",
		"operator's PEM cosign public key; verify the image's signature with it")
	fl.Int64Var(&f.maxBytes, "max-bytes", resolve.DefaultMaxBytes,
		"largest compressed layer total per platform, as the operator's --repository-image-max-bytes")
	fl.BoolVar(&f.run, "run", false, "also run the image the way the agent pod does, on the local docker")
	fl.StringVar(&f.runnerImage, "runner-image", "",
		"claude runner image to take agent-runner and claude from with --run, used as given "+
			"(default: the one released with this CLI, or the newest release for a development build, "+
			"in the release registry this CLI was built with, pinned to its digest)")
	_ = cmd.RegisterFlagCompletionFunc("allow", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("deny", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("runner-image", noFileCompletion)
	return cmd
}

// checkImageDeps is what `check image` reaches beyond its arguments: the
// docker CLI, the registry the default runner image is chosen from, the
// runner image repository it is chosen in and the CLI's own version, which
// decides that choice. Tests fake all four.
type checkImageDeps struct {
	docker   imagecheck.Commander
	registry imagecheck.Registry
	// runnerRepository is the release registry's runner image repository
	// the CLI was built with; empty for a build without one.
	runnerRepository string
	version          string
}

// defaultCheckImageDeps are the real dependencies: the docker CLI, the
// registry reached with the local docker credentials, and the runner image
// repository and version stamped into this build.
func defaultCheckImageDeps() checkImageDeps {
	return checkImageDeps{
		docker:           imagecheck.ExecCommander{},
		registry:         imagecheck.RemoteRegistry{Keychain: authn.DefaultKeychain},
		runnerRepository: version.RunnerImageRepository,
		version:          version.Version,
	}
}

// runCheckImage runs the checks and renders the report; any failed check
// makes the command fail.
func runCheckImage(ctx context.Context, opts *Options, f *checkImageFlags, reference string,
	deps checkImageDeps) error {
	format, err := printer.ParseFormat(opts.Output)
	if err != nil {
		return errUsage(err)
	}
	if f.maxBytes <= 0 {
		return errUsage(errors.New("--max-bytes must be positive"))
	}
	cfg := imagecheck.StaticConfig{Reference: reference, MaxBytes: f.maxBytes, Keychain: authn.DefaultKeychain}
	if len(f.deny) > 0 && len(f.allow) == 0 {
		// source-controller denies paths only inside its allowlist, which it
		// always has; a denial with nothing allowed would judge nothing.
		return errUsage(errors.New("--deny needs --allow: the denied paths are carved out of the allowlist"))
	}
	if len(f.allow) > 0 {
		policy, err := runnerimage.NewPolicy(f.allow)
		if err != nil {
			return errUsage(fmt.Errorf("--allow: %w", err))
		}
		// The same carve-out source-controller makes with
		// --repository-image-denied-registries: without it an image under the
		// preview prefix would PASS here and be refused there.
		if policy, err = policy.Deny(f.deny); err != nil {
			return errUsage(fmt.Errorf("--deny: %w", err))
		}
		cfg.Policy = &policy
	}
	if f.cosignKey != "" {
		raw, err := os.ReadFile(f.cosignKey)
		if err != nil {
			return errUsage(fmt.Errorf("--cosign-key: %w", err))
		}
		if cfg.PublicKey, err = resolve.ParsePublicKey(raw); err != nil {
			return errUsage(fmt.Errorf("--cosign-key: %w", err))
		}
		cfg.KeyName = f.cosignKey
	}

	staticCtx, cancel := context.WithTimeout(ctx, staticCheckTimeout)
	report, err := imagecheck.Static(staticCtx, cfg)
	cancel()
	if err != nil {
		return err
	}
	if f.run {
		checks := sandbox(ctx, opts, &report, f.runnerImage, deps)
		report.Checks = append(report.Checks, checks...)
	}

	if err := renderCheckImage(opts, report, format); err != nil {
		return err
	}
	if n := report.Failed(); n > 0 {
		return fmt.Errorf("%d check%s failed", n, plural(n, "", "s"))
	}
	return nil
}

// sandbox runs the image on the local docker, unless the static checks
// already say the pod could never run it: first the runner-image check,
// choosing the image agent-runner and claude come from (recorded on the
// report), then the per-platform checks, skipped when there is none.
func sandbox(ctx context.Context, opts *Options, report *imagecheck.Report, runnerImage string,
	deps checkImageDeps) []imagecheck.Check {
	image := report.Image
	for _, c := range report.Checks {
		switch {
		case c.Name == imagecheck.CheckReference && c.Status == imagecheck.Fail:
			return imagecheck.SkipSandbox("the reference is invalid")
		case c.Name == imagecheck.CheckPath && c.Status == imagecheck.Fail:
			return imagecheck.SkipSandbox("the image's PATH is rejected, so no pod would run it")
		}
	}
	if image == "" {
		// Not in the registry (or not reachable): the local docker store may
		// still have it, which is how an image is tried before it is pushed.
		image = report.Reference
	}
	chooseCtx, cancel := context.WithTimeout(ctx, staticCheckTimeout)
	runner, chosen := imagecheck.ChooseRunner(chooseCtx, deps.registry, runnerImage, deps.runnerRepository,
		deps.version)
	cancel()
	report.RunnerImage = runner
	checks := []imagecheck.Check{chosen}
	if runner == nil {
		return append(checks, imagecheck.SkipSandbox("there is no runner image to take agent-runner and claude from")...)
	}
	var platforms []string
	for _, p := range report.Platforms {
		platforms = append(platforms, p.Platform)
	}
	dir, err := sandboxDir()
	if err != nil {
		return append(checks, imagecheck.SkipSandbox("no directory for the runner binaries: "+err.Error())...)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return append(checks, imagecheck.Sandbox(ctx, deps.docker, imagecheck.SandboxConfig{
		Image:       image,
		SearchPath:  report.SearchPath,
		Platforms:   platforms,
		RunnerImage: runner.Image(),
		BinDir:      dir,
		Progress:    func(msg string) { notef(opts.ErrOut, "patchy: %s\n", msg) },
	})...)
}

// sandboxDir makes the directory the runner binaries are copied into and
// bind-mounted from. It sits under the user cache directory rather than
// the system temp directory because a docker VM (Docker Desktop, colima)
// shares the home directory with its containers by default, and not
// always the system temp directory. It is 0755, not MkdirTemp's 0700: on
// native Linux docker a bind mount keeps the host inode's owner and mode,
// and the container runs as uid 65532, which must reach agent-runner
// through it. What sits in it is the trusted runner's binaries, so there
// is nothing to hide from other local users.
func sandboxDir() (string, error) {
	parent := ""
	if cache, err := os.UserCacheDir(); err == nil {
		parent = filepath.Join(cache, "patchy")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			parent = ""
		}
	}
	dir, err := os.MkdirTemp(parent, "check-image-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// renderCheckImage writes the report: structured under -o json/yaml, one
// line per check otherwise, with the platform after the check's name for
// the sandbox checks, which run once per platform.
func renderCheckImage(opts *Options, report imagecheck.Report, format printer.Format) error {
	lines := make([]checkreport.Line, 0, len(report.Checks))
	for _, c := range report.Checks {
		lines = append(lines, checkreport.Line{Status: c.Status, Cells: []string{c.Name, c.Platform}, Reason: c.Reason})
	}
	return checkreport.Render(opts.Out, format, report, lines)
}
