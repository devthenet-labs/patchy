// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/imagecheck"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/scaffold"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// agentBaseTimeout bounds pinning the agent base: one HEAD.
const agentBaseTimeout = time.Minute

// newInitCmd is the `init` verb: write the files a repository needs to work
// with patchy. Cluster-free like `check`; the kubeconfig flags are inert.
func newInitCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold what a repository needs to work with patchy, without a cluster",
		Long: "Write the files a repository needs to work with patchy, from templates built into\n" +
			"this CLI. Nothing is sent to a cluster; the kubeconfig flags are inert here.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newInitAppCmd(opts))
	return cmd
}

// initAppFlags are `init app`'s flags.
type initAppFlags struct {
	repo        string
	lang        string
	imageName   string
	registry    string
	agentPrefix string
	agentBase   string
	branch      string
	existing    bool
	force       bool
}

// initAppDeps is what `init app` reaches beyond its arguments: the registry
// the agent base is pinned from and the CLI's version, which picks its
// release. Tests fake both.
type initAppDeps struct {
	registry imagecheck.Registry
	version  string
}

// newInitAppCmd scaffolds an application repository for intents and
// previews.
func newInitAppCmd(opts *Options) *cobra.Command {
	f := &initAppFlags{}
	cmd := &cobra.Command{
		Use:   "app [dir]",
		Short: "Scaffold an application repository for patchy's intents and previews",
		Long: "Write the files an application repository needs before patchy can work on it\n" +
			"from intents and preview its pull requests, into dir (default: the current\n" +
			"directory):\n\n" +
			"  .patchy/agent.yaml       the agent image the repository declares, an immutable\n" +
			"                           toolchain-v1 tag under --registry/--agent-prefix\n" +
			"  .patchy/Dockerfile       that image: patchy's agent base, pinned by digest,\n" +
			"                           plus the toolchain and the dependencies, offline\n" +
			"  .github/workflows/       uncredentialed builds of the runtime and agent images,\n" +
			"                           and the trusted publishers that push them to ECR\n" +
			"  .github/actions/publish/ the publishers' guard scripts, their tests and a README\n" +
			"                           listing the repository variables and trusted workflows\n" +
			"  Dockerfile, .dockerignore, .gitignore, README.md and a small service, for a new\n" +
			"  application only\n\n" +
			"The publishers push the runtime image of every open same-repository PR head and\n" +
			"default-branch commit (sha-<commit>) and the agent image from the default branch,\n" +
			"each gated on its own repository variable (PREVIEW_PUBLISH_ENABLED,\n" +
			"AGENT_PUBLISH_ENABLED). No generated file names an account ID, role or repository\n" +
			"ID: the publishers read them from repository variables, and the next steps\n" +
			"printed afterwards say which to set. Only .patchy/agent.yaml carries the registry,\n" +
			"because patchy reads the image from it.\n\n" +
			"--existing is for an application that already has its source and runtime\n" +
			"Dockerfile: it writes only .patchy/ and the CI publishers, builds the runtime\n" +
			"image in a workflow of its own (runtime-image.yml) so the application's CI is\n" +
			"untouched, and prints what the application must be adapted to.\n\n" +
			"The repository defaults to the git checkout's origin remote and the default\n" +
			"branch to the one origin's HEAD names (else main); both are read from .git, so no\n" +
			"git binary is needed. The image name, which both registry repositories end in,\n" +
			"defaults to the repository name made image-safe (Hello.Web becomes hello-web).\n" +
			"The agent base is the one released with this CLI, pinned to the digest its tag\n" +
			"names in the registry now. --agent-base overrides it: a reference pinned by\n" +
			"digest is used as given, and a tag is pinned the same way. A development build\n" +
			"has no agent base of its own, so it needs --agent-base, as does a registry that\n" +
			"cannot be reached; pass a reference pinned by digest (...@sha256:<64 hex>) then.\n" +
			"Nothing else is fetched: no GitHub call is made.\n\n" +
			"An existing file is never overwritten without --force, and a symbolic link or\n" +
			"other non-regular file never is: every path is checked before any is written.\n" +
			"Nor does --force rewrite the agent toolchain: an existing .patchy/agent.yaml or\n" +
			".patchy/Dockerfile is the repository's own, bumped with every toolchain change,\n" +
			"so it is kept as it is, and a kept .patchy/agent.yaml must declare a\n" +
			"toolchain-v<N> tag of the agent repository the options publish to. Remove both\n" +
			"to generate them again.\n" +
			"The written paths are printed on stdout, the next steps on stderr.",
		Example: "  patchy init app --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com\n" +
			"  patchy init app hello-web --repo acme/Hello.Web --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com\n" +
			"  patchy init app --existing --registry 123456789012.dkr.ecr.us-east-1.amazonaws.com --image-name shop",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: dirCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runInitApp(cmd.Context(), opts, f, dir, initAppDeps{
				registry: imagecheck.RemoteRegistry{Keychain: authn.DefaultKeychain},
				version:  version.Version,
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.repo, "repo", "", "GitHub repository as owner/name (default: the checkout's origin remote)")
	fl.StringVar(&f.lang, "lang", string(scaffold.LangGo), "application language: "+strings.Join(scaffold.Langs(), ", "))
	fl.StringVar(&f.imageName, "image-name", "",
		"image name both registry repositories end in (default: the repository name made image-safe)")
	fl.StringVar(&f.registry, "registry", "", "ECR registry host, <account>.dkr.ecr.<region>.amazonaws.com (required)")
	fl.StringVar(&f.agentPrefix, "agent-prefix", scaffold.DefaultAgentPrefix,
		"registry path the agent image sits under, as the operator's --repository-image-registries allows")
	fl.StringVar(&f.agentBase, "agent-base", "",
		"agent base image to build FROM, pinned by digest or a tag to pin (default: the one released with this CLI)")
	fl.StringVar(&f.branch, "default-branch", "",
		"the repository's default branch (default: the one origin's HEAD names, else main)")
	fl.BoolVar(&f.existing, "existing", false,
		"write only .patchy/ and the CI publishers, for an application that already has its source")
	fl.BoolVar(&f.force, "force", false,
		"overwrite files that already exist, but keep an existing .patchy/agent.yaml and .patchy/Dockerfile")
	_ = cmd.MarkFlagRequired("registry")
	_ = cmd.RegisterFlagCompletionFunc("lang", fixedCompletion(scaffold.Langs()))
	for _, name := range []string{"repo", "image-name", "registry", "agent-prefix", "agent-base", "default-branch"} {
		_ = cmd.RegisterFlagCompletionFunc(name, noFileCompletion)
	}
	return cmd
}

// dirCompletion completes init app's one argument, a directory.
func dirCompletion(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// runInitApp resolves the options, renders the files, writes them and
// prints what was written and what to do next.
func runInitApp(ctx context.Context, opts *Options, f *initAppFlags, dir string, deps initAppDeps) error {
	o, err := initAppOptions(f, dir)
	if err != nil {
		return err
	}
	if err := o.ValidateTarget(); err != nil {
		return errUsage(err)
	}
	switch info, err := os.Stat(dir); {
	case err == nil && !info.IsDir():
		return errUsage(fmt.Errorf("%s is not a directory", dir))
	case errors.Is(err, os.ErrNotExist) && f.existing:
		return errUsage(fmt.Errorf("%s does not exist; --existing is for an application already there", dir))
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	pinCtx, cancel := context.WithTimeout(ctx, agentBaseTimeout)
	o.Images.AgentBase, err = resolveAgentBase(pinCtx, deps.registry, deps.version, f.agentBase)
	cancel()
	if err != nil {
		return err
	}
	opts.debugf("agent base %s", o.Images.AgentBase)

	files, err := scaffold.Plan(o)
	if err != nil {
		return errUsage(err)
	}
	// The agent toolchain is the repository's own once written: --force
	// overwrites the rest, never it.
	var kept scaffold.Kept
	if f.force {
		if files, kept, err = scaffold.KeepToolchain(dir, files, o); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := scaffold.Write(dir, files, f.force); err != nil {
		var conflict *scaffold.ConflictError
		if errors.As(err, &conflict) {
			return conflictHint(conflict, f)
		}
		return err
	}
	for _, file := range files {
		if _, err := fmt.Fprintln(opts.Out, file.Path); err != nil {
			return err
		}
	}
	notef(opts.ErrOut, "\npatchy: wrote %d files for %s into %s (agent base %s).\n", len(files), o.Repo, dir,
		o.Images.AgentBase)
	if len(kept.Paths) > 0 {
		notef(opts.ErrOut, "patchy: kept %s as %s: --force never rewrites the agent toolchain%s. Remove both "+
			"to generate them again.\n", strings.Join(kept.Paths, " and "), keptAs(len(kept.Paths)),
			keptDeclares(kept.Tag))
	}
	notef(opts.ErrOut, "\n%s", scaffold.NextSteps(o, scaffold.Present{
		Dockerfile: fileExists(filepath.Join(dir, "Dockerfile")),
		GoMod:      fileExists(filepath.Join(dir, "go.mod")),
		Toolchain:  kept.Tag,
	}))
	return nil
}

// keptAs is "it is" or "they are", for n kept files.
func keptAs(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}

// keptDeclares names the tag a kept .patchy/agent.yaml declares, when one
// was kept.
func keptDeclares(tag string) string {
	if tag == "" {
		return ""
	}
	return ", and .patchy/agent.yaml still declares " + tag
}

// initAppOptions resolves the flags into scaffold options, reading the
// repository and default branch from dir's .git where the flags leave them
// out. The agent base is resolved separately, since that needs the
// registry.
func initAppOptions(f *initAppFlags, dir string) (scaffold.Options, error) {
	o := scaffold.Options{
		Lang:        scaffold.Lang(f.lang),
		Registry:    f.registry,
		AgentPrefix: f.agentPrefix,
		Branch:      f.branch,
		Existing:    f.existing,
		Images:      scaffold.Images{Go: scaffold.GoImage, Runtime: scaffold.RuntimeImage},
	}
	if !slices.Contains(scaffold.Langs(), f.lang) {
		return o, errUsage(fmt.Errorf("--lang %q has no templates; choose one of %s", f.lang,
			strings.Join(scaffold.Langs(), ", ")))
	}
	var err error
	if f.repo != "" {
		if o.Repo, err = scaffold.ParseRepo(f.repo); err != nil {
			return o, errUsage(fmt.Errorf("--repo: %w", err))
		}
	} else if o.Repo, err = scaffold.OriginRepo(dir); err != nil {
		return o, errUsage(fmt.Errorf("pass --repo owner/name: the repository could not be read from %s: %w", dir, err))
	}
	if f.imageName != "" {
		if err := scaffold.ValidateSlug(f.imageName); err != nil {
			return o, errUsage(fmt.Errorf("--image-name: %w", err))
		}
		o.ImageName = f.imageName
	} else if o.ImageName, err = scaffold.Sanitize(o.Repo.Name); err != nil {
		return o, errUsage(err)
	}
	if o.Branch == "" {
		o.Branch = scaffold.DefaultBranch
		if branch, ok := scaffold.OriginBranch(dir); ok {
			o.Branch = branch
		}
	}
	return o, nil
}

// agentBaseRepository is where a patchy release publishes its agent base,
// tagged v<version>: the FROM of a scaffolded .patchy/Dockerfile.
const agentBaseRepository = "ghcr.io/devthenet-labs/patchy/agent-base"

// resolveAgentBase is the one place `init app` chooses the agent base, and
// it always returns a reference pinned by digest, since the scaffold engine
// takes nothing else. override (--agent-base) is used as given when it
// carries a digest (scaffold.Plan checks it), and a tag is pinned to the
// digest it names in the registry now. Without an override, a release CLI
// (exactly X.Y.Z, with or without its v) takes the agent base released with
// it, pinned the same way. Anything else cannot be resolved, and the error
// says to pass a digest-pinned --agent-base.
func resolveAgentBase(ctx context.Context, reg imagecheck.Registry, cliVersion, override string) (string, error) {
	const pass = "pass --agent-base <image>@sha256:<digest>"
	ref := override
	if ref == "" {
		v, err := semver.StrictNewVersion(strings.TrimPrefix(cliVersion, "v"))
		if err != nil || v.Prerelease() != "" || v.Metadata() != "" {
			return "", fmt.Errorf("this CLI is a development build (version %q) with no agent base released "+
				"with it; %s", cliVersion, pass)
		}
		ref = agentBaseRepository + ":v" + v.String()
	} else if strings.Contains(ref, "@") {
		return ref, nil
	}
	digest, err := reg.Digest(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("agent base %s did not resolve in the registry: %w; %s", ref, err, pass)
	}
	return ref + "@" + digest, nil
}

// conflictHint explains a refusal to overwrite: --force for files that may
// be replaced, --existing when what is in the way is the application's own
// source, and that --force keeps the agent toolchain when it is in the way.
func conflictHint(c *scaffold.ConflictError, f *initAppFlags) error {
	hint := "nothing was written"
	if len(c.Paths) > 0 {
		hint += "; pass --force to overwrite"
		if !f.existing && slices.ContainsFunc(c.Paths, func(p string) bool {
			return !strings.HasPrefix(p, ".patchy/") && !strings.HasPrefix(p, ".github/")
		}) {
			hint += ", or --existing to leave the application's own files alone"
		}
		var toolchain []string
		for _, p := range scaffold.ToolchainPaths {
			if slices.Contains(c.Paths, p) {
				toolchain = append(toolchain, p)
			}
		}
		if len(toolchain) > 0 {
			hint += "; --force keeps " + strings.Join(toolchain, " and ") + " as " + keptAs(len(toolchain))
		}
	}
	return fmt.Errorf("%w; %s", c, hint)
}

// fileExists reports whether p is an existing regular file.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
