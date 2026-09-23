// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

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

// newCheckCmd is the `check` verb: judge an artifact the way the pipeline
// will, before it reaches the pipeline. Cluster-free like `dev` and
// `mirror`; the persistent kubeconfig flags are inert here.
func newCheckCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check an artifact the way patchy will judge it, without a cluster",
		Long: "Judge something you are about to hand to patchy the way the pipeline will judge\n" +
			"it, from your workstation and with no cluster access. The kubeconfig flags are\n" +
			"inert here.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newCheckImageCmd(opts))
	return cmd
}

// checkImageFlags are `check image`'s flags.
type checkImageFlags struct {
	allow       []string
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
			"--repository-image-registries) when given, pinned to a digest, and every\n" +
			"linux/amd64 and linux/arm64 manifest is judged for platform, compressed size,\n" +
			"VOLUME, reserved ENV and PATH; with --cosign-key the signature is verified as\n" +
			"well. Registry credentials are your local docker credentials\n" +
			"(~/.docker/config.json and its credential helpers), so an image you can pull\n" +
			"is an image this can check. Every check is reported, not just the first\n" +
			"failure.\n\n" +
			"With --run, the image is also run the way the agent pod runs it, on your local\n" +
			"docker: agent-runner and the claude CLI are copied out of the claude runner\n" +
			"image released with this CLI (--runner-image to override), and the image runs\n" +
			"as uid 65532 with a read-only root filesystem, no network, no capabilities, no\n" +
			"privilege escalation, executable tmpfs mounts at /tmp and /workspace, the two\n" +
			"binaries read-only at /patchy/bin, PATH=/patchy/bin:<the image's PATH> and the\n" +
			"rest of the pod's environment. In it, agent-runner's own preflight (the check a\n" +
			"stage runs before its first model call: claude --version, git --version and\n" +
			"bash -c true) runs, then bash -c true and git --version on their own. Without a\n" +
			"docker CLI the run is skipped, not failed. An image that exists only in your\n" +
			"local docker store fails the registry checks but still runs; to check both\n" +
			"before publishing, push it to a scratch tag or a local registry.\n\n" +
			"Each check prints one line: PASS, FAIL or SKIP, the check, and the reason.\n" +
			"-o json or -o yaml prints the whole report as data instead. The exit status is\n" +
			"non-zero when any check fails.",
		Example: "  patchy check image ghcr.io/acme/shop-agent:1\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --allow ghcr.io/acme/ --cosign-key cosign.pub\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --run\n" +
			"  patchy check image ghcr.io/acme/shop-agent:1 --run -o json | jq '.checks[] | select(.status == \"FAIL\")'",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckImage(cmd.Context(), opts, f, args[0], imagecheck.ExecCommander{})
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.allow, "allow", nil,
		"registry path prefix the image must sit under, as the operator's --repository-image-registries "+
			"entries (repeatable)")
	fl.StringVar(&f.cosignKey, "cosign-key", "",
		"operator's PEM cosign public key; verify the image's signature with it")
	fl.Int64Var(&f.maxBytes, "max-bytes", resolve.DefaultMaxBytes,
		"largest compressed layer total per platform, as the operator's --repository-image-max-bytes")
	fl.BoolVar(&f.run, "run", false, "also run the image the way the agent pod does, on the local docker")
	fl.StringVar(&f.runnerImage, "runner-image", "",
		"claude runner image to take agent-runner and claude from with --run "+
			"(default: the one released with this CLI)")
	_ = cmd.RegisterFlagCompletionFunc("allow", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("runner-image", noFileCompletion)
	return cmd
}

// runCheckImage runs the checks and renders the report; any failed check
// makes the command fail.
func runCheckImage(ctx context.Context, opts *Options, f *checkImageFlags, reference string,
	docker imagecheck.Commander) error {
	format, err := printer.ParseFormat(opts.Output)
	if err != nil {
		return errUsage(err)
	}
	if f.maxBytes <= 0 {
		return errUsage(errors.New("--max-bytes must be positive"))
	}
	cfg := imagecheck.StaticConfig{Reference: reference, MaxBytes: f.maxBytes, Keychain: authn.DefaultKeychain}
	if len(f.allow) > 0 {
		policy, err := runnerimage.NewPolicy(f.allow)
		if err != nil {
			return errUsage(fmt.Errorf("--allow: %w", err))
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
		report.RunnerImage = f.runnerImage
		if report.RunnerImage == "" {
			report.RunnerImage = imagecheck.DefaultRunnerImage(version.Version)
		}
		report.Checks = append(report.Checks, sandbox(ctx, opts, report, docker)...)
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
// already say the pod could never run it.
func sandbox(ctx context.Context, opts *Options, report imagecheck.Report,
	docker imagecheck.Commander) []imagecheck.Check {
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
	var platforms []string
	for _, p := range report.Platforms {
		platforms = append(platforms, p.Platform)
	}
	dir, err := sandboxDir()
	if err != nil {
		return imagecheck.SkipSandbox("no directory for the runner binaries: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return imagecheck.Sandbox(ctx, docker, imagecheck.SandboxConfig{
		Image:       image,
		SearchPath:  report.SearchPath,
		Platforms:   platforms,
		RunnerImage: report.RunnerImage,
		BinDir:      dir,
		Progress:    func(msg string) { notef(opts.ErrOut, "patchy: %s\n", msg) },
	})
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
// line per check otherwise.
func renderCheckImage(opts *Options, report imagecheck.Report, format printer.Format) error {
	switch format {
	case printer.FormatJSON:
		enc := json.NewEncoder(opts.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case printer.FormatYAML:
		out, err := yaml.Marshal(report)
		if err != nil {
			return err
		}
		_, err = opts.Out.Write(out)
		return err
	}
	w := tabwriter.NewWriter(opts.Out, 0, 8, 2, ' ', 0)
	for _, c := range report.Checks {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", c.Status, c.Name, oneLine(c.Reason)); err != nil {
			return err
		}
	}
	return w.Flush()
}

// oneLine keeps a reason on its check's line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
