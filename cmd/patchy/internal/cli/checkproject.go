// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/projectcheck"
	"github.com/bitwise-media-group/patchy/internal/runnerimage/resolve"
)

// checkProjectTimeout bounds a whole `check project` run: a handful of
// cluster reads, a few GitHub requests and registry fetches per
// repository, one DNS lookup and one TLS handshake.
const checkProjectTimeout = 3 * time.Minute

// newCheckProjectCmd checks that a Project is ready for its first intent.
func newCheckProjectCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project <name>",
		Short: "Check that a Project is ready for its first intent",
		Long: "Check a Project in the current namespace (-n) from your workstation, before its\n" +
			"first intent: what an intent and its preview need, one line per check, PASS,\n" +
			"FAIL or SKIP, then the repository it is about and the reason.\n\n" +
			"What the controllers already prove is read from the cluster, never proven again:\n" +
			"intent-controller's Ready verdict on the Project (it mints the App's scoped\n" +
			"tokens on every repository and ensures the labels in-cluster), the\n" +
			"IntentNameConflict condition, and the Ready verdict of each covering Forge. The\n" +
			"check never reads a Secret, so it never handles the App's private key: its\n" +
			"cluster reads are Projects, Forges, ConfigMaps and the preview placeholder\n" +
			"Ingress, with your kubeconfig.\n\n" +
			"The rest is checked with your own identity, and each reason says so: every\n" +
			"repository resolves to exactly one Forge; each app repository's agent image,\n" +
			"as .patchy/agent.yaml (or .devcontainer/devcontainer.json) declares it at the\n" +
			"default-branch head, passes source-controller's live policy, read from its\n" +
			"ConfigMap; and, for a previewed Project, previews are on, each previewed\n" +
			"repository's image sits under preview-controller's prefix with\n" +
			"sha-<default-branch head> published, <project>-0.<host suffix> resolves to\n" +
			"the preview load balancer, and it serves a certificate trusted for that name.\n\n" +
			"GitHub is read with GH_TOKEN, else GITHUB_TOKEN, else anonymously (public\n" +
			"repositories only). Registries are read with your cloud and docker\n" +
			"credentials: an ECR repository through the AWS SDK's default chain\n" +
			"(AWS_PROFILE), Artifact Registry through Application Default Credentials,\n" +
			"any other through your docker config. What this cannot prove is that the\n" +
			"cluster's own credentials work; a Repository's status.runnerImage and a\n" +
			"Preview's status are the evidence for those. The preview load balancer admits\n" +
			"only the chart's preview.inboundCIDRs, so from any other address the TLS check\n" +
			"times out and is a SKIP, not a FAIL.\n\n" +
			"-o json or -o yaml prints the whole report as data. The exit status is 1 when\n" +
			"any check fails, 3 when the Project does not exist and 4 when you may not\n" +
			"read it.",
		Example: "  patchy check project shop -n patchy\n" +
			"  GH_TOKEN=$(gh auth token) AWS_PROFILE=prod patchy check project shop -n patchy\n" +
			"  patchy check project shop -o json | jq '.checks[] | select(.status == \"FAIL\")'",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			token, source := githubToken()
			return runCheckProject(cmd.Context(), opts, args[0], checkProjectDeps{
				github:   &projectcheck.HTTPGitHub{Token: token, TokenSource: source},
				keychain: resolve.NewKeychain(),
				resolver: net.DefaultResolver,
				dialTLS:  projectcheck.DialTLS,
			})
		},
	}
	return cmd
}

// githubToken is the caller's GitHub token from the environment, as the gh
// CLI reads it (GH_TOKEN before GITHUB_TOKEN), with the variable it came
// from; empty when neither is set.
func githubToken() (token, source string) {
	for _, env := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			return v, env
		}
	}
	return "", ""
}

// checkProjectDeps is what `check project` reaches beyond the cluster: the
// caller's GitHub, registries, DNS and the preview host. Tests fake all of
// them.
type checkProjectDeps struct {
	github   projectcheck.GitHub
	keychain authn.Keychain
	resolver projectcheck.Resolver
	dialTLS  projectcheck.TLSDialer
}

// runCheckProject runs the checks and renders the report; any failed check
// makes the command fail.
func runCheckProject(ctx context.Context, opts *Options, name string, deps checkProjectDeps) error {
	format, err := printer.ParseFormat(opts.Output)
	if err != nil {
		return errUsage(err)
	}
	if opts.AllNamespaces {
		return errUsage(errors.New("check project reads one Project: name its namespace with -n, not -A"))
	}
	env, err := opts.Connect()
	if err != nil {
		return err
	}
	if env.Namespace == "" {
		return errUsage(errors.New("check project needs the Project's namespace: pass -n"))
	}
	ctx, cancel := context.WithTimeout(ctx, checkProjectTimeout)
	defer cancel()
	opts.debugf("checking project %s in namespace %s", name, env.Namespace)
	report, err := projectcheck.Run(ctx, projectcheck.Config{
		Reader: env.Client, Namespace: env.Namespace, Project: name,
		GitHub: deps.github, Keychain: deps.keychain, Resolver: deps.resolver, DialTLS: deps.dialTLS,
	})
	if err != nil {
		return fmt.Errorf("project %s in namespace %s: %w", name, env.Namespace, err)
	}
	if err := checkreport.Render(opts.Out, format, report, report.Lines()); err != nil {
		return err
	}
	if n := report.Failed(); n > 0 {
		return fmt.Errorf("%d check%s failed", n, plural(n, "", "s"))
	}
	return nil
}
