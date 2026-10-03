// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/browser"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/ghapp"
)

// defaultSetupNamespace is the Secret's namespace when -n is not given: the
// release namespace the install guide uses.
const defaultSetupNamespace = "patchy"

// newSetupCmd is the `setup` verb: create what patchy needs outside the
// cluster. Cluster-free, like `dev`, `mirror` and `check image`.
func newSetupCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create what patchy needs outside the cluster",
		Long: "Create the things patchy needs before it is installed, from your workstation. Nothing\n" +
			"here talks to a cluster: the kubeconfig flags are inert, and -n only names the\n" +
			"namespace a written manifest carries.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSetupGitHubAppCmd(opts))
	return cmd
}

// setupGitHubAppFlags are `setup github-app`'s flags.
type setupGitHubAppFlags struct {
	org         string
	user        bool
	security    bool
	intents     bool
	checks      bool
	webhookURL  string
	name        string
	homepageURL string
	secretName  string
	output      string
	force       bool
	noBrowser   bool
	dryRun      bool
	timeout     time.Duration
}

// setupDeps is what `setup github-app` reaches beyond its flags: GitHub's
// web and API hosts, the browser, the terminal a pasted code is read from,
// and the HTTP client the code is exchanged with. Tests fake every one.
type setupDeps struct {
	webURL string
	apiURL string
	open   func(string) error
	stdin  io.Reader
	client *http.Client
	// tempDir holds the --no-browser start page; "" is the system's.
	tempDir string
	// terminal reports whether a stream is a terminal: -o - refuses one.
	terminal func(io.Writer) bool
}

// newSetupGitHubAppCmd creates the GitHub App through the manifest flow.
func newSetupGitHubAppCmd(opts *Options) *cobra.Command {
	f := &setupGitHubAppFlags{}
	cmd := &cobra.Command{
		Use:   "github-app",
		Short: "Create the GitHub App patchy authenticates as, and write its Secret",
		Long: "Create the GitHub App patchy authenticates as through GitHub's App manifest flow,\n" +
			"and write its credentials as the Secret manifest the Forge and Integration\n" +
			"resources name in spec.secretRef.\n\n" +
			"The App asks for the least that the features you choose use, and nothing\n" +
			"else; choose at least one:\n" +
			"  --security  the findings pipeline: code scanning alerts, issues, contents and\n" +
			"              pull requests write, and the webhook events code_scanning_alert,\n" +
			"              issues, issue_comment and pull_request (needs --webhook-url, the\n" +
			"              integration-controller's https://<host>/github/webhooks).\n" +
			"  --intents   intent-driven development: issues, contents and pull requests\n" +
			"              write. intent-controller polls GitHub, so intents add no webhook.\n" +
			"  --checks    with --intents: checks, statuses and actions read, which a\n" +
			"              Project's check-fix rounds (spec.checks.fix) need.\n" +
			"Metadata read comes with every App. The intent permissions are the table\n" +
			"intent-controller proves before a Project is Ready.\n\n" +
			"The flow: patchy serves a page on a random 127.0.0.1 port and opens it in your\n" +
			"browser; the page posts the manifest to GitHub's \"create a GitHub App\" form for\n" +
			"--org (you need to be an owner of it) or, with --user, your own account. Check\n" +
			"the form and click \"Create GitHub App\": GitHub sends the browser back to the\n" +
			"local page with a one-time code, which patchy accepts only with the state it\n" +
			"started the flow with, exchanges for the App's credentials, and then stops\n" +
			"listening. With --no-browser nothing listens: patchy writes the page to a file\n" +
			"you open in any browser, GitHub sends you back to your GitHub Apps settings,\n" +
			"and you paste that page's address (or just its code) into the terminal. A code\n" +
			"works once, within an hour.\n\n" +
			"The Secret manifest (--secret-name, default patchy-github, in -n, default\n" +
			"patchy) holds appID, privateKey and, for an App with a webhook, webhookSecret.\n" +
			"It is written to -o, default <secret-name>.secret.yaml, with mode 0600 (not\n" +
			"enforced on Windows), and an existing file is never replaced without --force.\n" +
			"-o - writes it to stdout instead, to pipe into an encryption tool such as\n" +
			"sops, and refuses a stdout that is a terminal. The private key is printed\n" +
			"nowhere else and GitHub keeps no copy: apply or encrypt the file, then\n" +
			"delete it. Everything else, including the install link, goes to stderr.\n" +
			"Install the App on the repositories patchy works on (\"Only select\n" +
			"repositories\" is enough): for intents, the intent repository and every\n" +
			"application repository.\n\n" +
			"--dry-run prints the manifest as JSON and creates nothing. github.com only.",
		Example: "  patchy setup github-app --org acme --intents --checks\n" +
			"  patchy setup github-app --org acme --security --webhook-url https://patchy.acme.dev/github/webhooks\n" +
			"  patchy setup github-app --org acme --intents --dry-run\n" +
			"  patchy setup github-app --org acme --intents -o - | sops --encrypt --input-type yaml " +
			"--output-type yaml /dev/stdin > patchy-github.enc.yaml\n" +
			"  patchy setup github-app --user --intents --no-browser",
		Args:              cobra.NoArgs,
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps := setupDeps{
				webURL:   ghapp.DefaultWebURL,
				apiURL:   ghapp.DefaultAPIURL,
				open:     browser.Open,
				stdin:    cmd.InOrStdin(),
				client:   &http.Client{Timeout: opts.RequestTimeout},
				terminal: isTerminal,
			}
			if opts.setupDeps != nil {
				deps = *opts.setupDeps
			}
			return runSetupGitHubApp(cmd.Context(), opts, f, deps)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.org, "org", "", "organization to create the App under (you must be an owner)")
	fl.BoolVar(&f.user, "user", false, "create the App under your own account instead of an organization")
	fl.BoolVar(&f.security, "security", false, "the findings pipeline: alerts, tracking issues, remediation PRs")
	fl.BoolVar(&f.intents, "intents", false, "intent-driven development (no webhook)")
	fl.BoolVar(&f.checks, "checks", false, "with --intents: the reads check-fix rounds need")
	fl.StringVar(&f.webhookURL, "webhook-url", "",
		"with --security: the integration-controller's https://<host>/github/webhooks")
	fl.StringVar(&f.name, "name", "", "the App's name, at most 34 characters (default patchy-<org>, or patchy)")
	fl.StringVar(&f.homepageURL, "homepage-url", ghapp.DefaultHomepageURL, "the App's homepage")
	fl.StringVar(&f.secretName, "secret-name", ghapp.DefaultSecretName, "name of the Secret written")
	// -o names a file here, not an output format: this command's only output
	// is the Secret manifest. The local flag shadows the global one.
	fl.StringVarP(&f.output, "output", "o", "",
		"file to write the Secret manifest to, or - for stdout (default <secret-name>.secret.yaml)")
	fl.BoolVar(&f.force, "force", false, "replace an existing output file")
	fl.BoolVar(&f.noBrowser, "no-browser", false,
		"open no browser and listen on no port: write the page to a file and paste the code back")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the manifest as JSON; create nothing")
	fl.DurationVar(&f.timeout, "timeout", 15*time.Minute, "how long to wait for GitHub to send the browser back")
	_ = cmd.RegisterFlagCompletionFunc("org", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("webhook-url", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("name", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("homepage-url", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("secret-name", noFileCompletion)
	return cmd
}

// setupPlan is a validated `setup github-app` invocation.
type setupPlan struct {
	owner      ghapp.Owner
	config     ghapp.Config
	secretName string
	namespace  string
	output     string // "-" for stdout
}

// planSetup validates the flags into a plan; every refusal is a usage error.
func planSetup(opts *Options, f *setupGitHubAppFlags) (setupPlan, error) {
	var p setupPlan
	switch {
	case f.org != "" && f.user:
		return p, errUsage(errors.New("--org and --user are exclusive: the App has one owner"))
	case f.org == "" && !f.user:
		return p, errUsage(errors.New("name the App's owner: --org <organization>, or --user"))
	case f.org != "":
		if err := ghapp.ValidateOrg(f.org); err != nil {
			return p, errUsage(fmt.Errorf("--org: %w", err))
		}
	}
	if f.timeout <= 0 {
		return p, errUsage(errors.New("--timeout must be positive"))
	}
	if errs := validation.IsDNS1123Subdomain(f.secretName); len(errs) > 0 {
		return p, errUsage(fmt.Errorf("--secret-name %q: %s", f.secretName, strings.Join(errs, "; ")))
	}
	p.owner = ghapp.Owner{Org: f.org}
	p.secretName = f.secretName
	p.namespace = opts.Namespace
	if p.namespace == "" {
		p.namespace = defaultSetupNamespace
	}
	if errs := validation.IsDNS1123Label(p.namespace); len(errs) > 0 {
		return p, errUsage(fmt.Errorf("-n %q: %s", p.namespace, strings.Join(errs, "; ")))
	}
	p.output = f.output
	if p.output == "" {
		p.output = f.secretName + ".secret.yaml"
	}
	name := f.name
	if name == "" {
		name = ghapp.DefaultName(p.owner)
	}
	p.config = ghapp.Config{
		Features:    ghapp.Features{Security: f.security, Intents: f.intents, Checks: f.checks},
		Name:        name,
		HomepageURL: f.homepageURL,
		WebhookURL:  f.webhookURL,
	}
	if _, err := ghapp.Build(p.config); err != nil {
		return p, errUsage(err)
	}
	return p, nil
}

// runSetupGitHubApp creates the App and writes its Secret.
func runSetupGitHubApp(ctx context.Context, opts *Options, f *setupGitHubAppFlags, deps setupDeps) error {
	plan, err := planSetup(opts, f)
	if err != nil {
		return err
	}
	if f.dryRun {
		return printManifest(opts, plan, deps)
	}
	// Before anything exists on GitHub: an App whose credentials have nowhere
	// to go is an App to delete by hand.
	if plan.output == "-" {
		if deps.terminal(opts.Out) {
			return errUsage(errors.New("-o - writes the Secret, private key included, to stdout, which is a " +
				"terminal: pipe it into the tool that keeps it (sops, kubectl apply -f -), or name a file"))
		}
	} else if err := ghapp.CheckWritable(plan.output, f.force); err != nil {
		return err
	}
	state, err := ghapp.NewState()
	if err != nil {
		return err
	}
	createURL := ghapp.CreateURL(deps.webURL, plan.owner, state)
	var code string
	if f.noBrowser {
		code, err = pasteCode(ctx, opts, plan, deps, createURL, state)
	} else {
		code, err = browserCode(ctx, opts, plan, deps, createURL, state, f.timeout)
	}
	if errors.Is(err, ghapp.ErrNoCode) {
		// The App may exist although its code never arrived: a browser on
		// another machine cannot reach this one's loopback address.
		return fmt.Errorf("%w; if GitHub created the App, delete it at %s, or within the hour run again with "+
			"--no-browser and paste the code= value from the address the browser could not open",
			err, ghapp.AppsURL(deps.webURL, plan.owner))
	}
	if err != nil {
		return err
	}

	app, err := ghapp.Convert(ctx, deps.client, deps.apiURL, code)
	if err != nil {
		return fmt.Errorf("%w; if GitHub created the App, delete it or generate a new private key at %s",
			err, ghapp.AppsURL(deps.webURL, plan.owner))
	}
	return writeSecret(opts, plan, deps, app, f.force)
}

// printManifest is --dry-run: the manifest on stdout, where it would go on
// stderr.
func printManifest(opts *Options, plan setupPlan, deps setupDeps) error {
	m, err := ghapp.Build(plan.config)
	if err != nil {
		return errUsage(err)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(opts.Out, "%s\n", out)
	notef(opts.ErrOut, "patchy: dry run: nothing created. The flow posts this manifest to %s with a fresh "+
		"state, and redirect_url set to where GitHub sends the browser back.\n",
		ghapp.CreateURL(deps.webURL, plan.owner, ""))
	return err
}

// browserCode runs the loopback flow: serve the start page, open it, and
// wait for GitHub to send the browser back with the code.
func browserCode(ctx context.Context, opts *Options, plan setupPlan, deps setupDeps, createURL, state string,
	timeout time.Duration) (string, error) {
	cb, err := ghapp.Listen()
	if err != nil {
		return "", err
	}
	defer cb.Close()
	page, err := startPage(plan, cb.RedirectURL(), createURL)
	if err != nil {
		return "", err
	}
	cb.Serve(state, page)
	notef(opts.ErrOut, "patchy: opening %s in your browser: it sends the App manifest to GitHub.\n"+
		"patchy: check the form there and click \"Create GitHub App\"; waiting up to %s.\n", cb.StartURL(), timeout)
	if err := deps.open(cb.StartURL()); err != nil {
		notef(opts.ErrOut, "patchy: no browser opened (%v): open %s yourself.\n", err, cb.StartURL())
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return cb.Wait(wait)
}

// pasteCode runs the flow without a listener: the start page goes to a
// file, GitHub lands the browser on the owner's App settings, and the
// person pastes that address back.
func pasteCode(ctx context.Context, opts *Options, plan setupPlan, deps setupDeps, createURL,
	state string) (string, error) {
	page, err := startPage(plan, ghapp.AppsURL(deps.webURL, plan.owner), createURL)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(deps.tempDir, "patchy-github-app-*.html")
	if err != nil {
		return "", err
	}
	_, err = file.Write(page)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("write the start page: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	notef(opts.ErrOut, "patchy: open %s in a browser signed in to GitHub (copy it to that machine if need be).\n"+
		"patchy: it sends the App manifest to GitHub; check the form and click \"Create GitHub App\".\n"+
		"patchy: GitHub then opens your GitHub Apps settings. Paste that page's address here (or just its "+
		"code=... value):\n", file.Name())
	line, err := readLine(ctx, deps.stdin)
	if err != nil {
		return "", err
	}
	code, err := ghapp.ParseCode(line, state)
	if err != nil {
		return "", errUsage(err)
	}
	return code, nil
}

// startPage builds the manifest with redirectURL and renders the page that
// posts it to createURL.
func startPage(plan setupPlan, redirectURL, createURL string) ([]byte, error) {
	cfg := plan.config
	cfg.RedirectURL = redirectURL
	m, err := ghapp.Build(cfg)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return ghapp.StartPage(createURL, raw)
}

// readLine reads one line from r, or gives up when ctx ends: a read from a
// terminal cannot itself be cancelled.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
		if errors.Is(err, io.EOF) && line != "" {
			err = nil
		}
		got <- result{line, err}
	}()
	select {
	case res := <-got:
		if res.err != nil {
			return "", fmt.Errorf("read the pasted address: %w", res.err)
		}
		return res.line, nil
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

// writeSecret writes the Secret manifest and tells the person what is left.
func writeSecret(opts *Options, plan setupPlan, deps setupDeps, app *ghapp.App, force bool) error {
	notef(opts.ErrOut, "patchy: created GitHub App %q (ID %d) owned by %s: %s\n", app.Name, app.ID, app.Owner,
		app.HTMLURL)
	m, err := ghapp.Build(plan.config)
	if err != nil {
		return err
	}
	for _, d := range ghapp.Drift(m, app) {
		notef(opts.ErrOut, "patchy: warning: %s\n", d)
	}
	if m.HookAttributes != nil && !app.Credentials.HasWebhookSecret() {
		notef(opts.ErrOut, "patchy: warning: GitHub issued no webhook secret, and an Integration refuses a Secret "+
			"without %s: set one on the App's webhook at %s and add it to the Secret.\n", ghapp.KeyWebhookSecret,
			ghapp.SettingsURL(deps.webURL, plan.owner, app.Slug))
	}
	data, err := ghapp.SecretManifest(app, plan.secretName, plan.namespace)
	if err != nil {
		return err
	}
	lost := fmt.Sprintf("; the App exists, but its private key was not saved: generate a new one under "+
		"\"Private keys\" at %s", app.HTMLURL)
	keys := "appID, privateKey"
	if app.Credentials.HasWebhookSecret() {
		keys += ", webhookSecret"
	}
	if plan.output == "-" {
		if _, err := opts.Out.Write(data); err != nil {
			return fmt.Errorf("write the Secret to stdout: %w%s", err, lost)
		}
		notef(opts.ErrOut, "patchy: wrote Secret %s/%s (%s) to stdout\n", plan.namespace, plan.secretName, keys)
	} else {
		if err := ghapp.WriteFile(plan.output, data, force); err != nil {
			return fmt.Errorf("%w%s", err, lost)
		}
		notef(opts.ErrOut, "patchy: wrote Secret %s/%s (%s) to %s, mode 0600. It holds the only copy of the "+
			"private key: apply or encrypt it, then delete it.\n", plan.namespace, plan.secretName, keys, plan.output)
	}
	notef(opts.ErrOut, "patchy: install the App: %s\n", ghapp.InstallURL(deps.webURL, app.Slug))
	if plan.config.Features.Intents {
		notef(opts.ErrOut, "patchy: for intents, install it on the intent repository and every application "+
			"repository.\n")
	}
	if plan.secretName != ghapp.DefaultSecretName {
		notef(opts.ErrOut, "patchy: name %q in the Forge's (and Integration's) spec.secretRef and in the chart's "+
			"intentController.forgeSecrets.\n", plan.secretName)
	}
	return nil
}

// isTerminal reports whether w is a terminal, where a private key would
// land in the scrollback.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
