// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/internal/changeset"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/intent"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// The flags this binary alone reads carry an intent- prefix, so the shared
// kustomize ConfigMap's PATCHY_* keys for the other controllers can never
// set them by accident.
func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the project, intent, run-scheduler and TTL reconcilers",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the patchy resources live in (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")

	f.Duration("intent-poll-interval", intent.DefaultPollInterval,
		"how often each Project's intent repository and each active intent's issue are polled")
	f.Duration("intent-approval-poll-interval", intent.DefaultApprovalPollInterval,
		"how often an intent awaiting approval polls its issue for an approval")
	f.Duration("intent-pr-poll-interval", intent.DefaultPRPollInterval,
		"how often an intent in review polls its pull requests")
	f.Int("intent-max-concurrent-runs", 1, "intent agent Jobs running at once (a pool separate from remediation's)")
	f.Int("intent-rate-limit-floor", intent.DefaultRateLimitFloor,
		"pause intent polling while the installation has fewer core GitHub requests left than this (0 disables)")
	f.Duration("intent-ttl", intent.DefaultTTL, "retention of ended intents after completion; 0 keeps them forever")
	f.Duration("intent-job-deadline", 90*time.Minute,
		"activeDeadlineSeconds on an intent agent Job, at least both stage timeouts (the broker caller token is "+
			"minted for it plus 15m)")
	f.Int("intent-plan-max-turns", 40, "most agent turns a plan run may take (a Project may lower it)")
	f.Int("intent-plan-token-budget", 200000, "most output tokens a plan run may spend (a Project may lower it)")
	f.Duration("intent-plan-timeout", 20*time.Minute, "wall-clock limit of a plan run")
	f.Int("intent-build-max-turns", 150, "most agent turns a build run may take (a Project may lower it)")
	f.Int("intent-build-token-budget", 800000, "most output tokens a build run may spend (a Project may lower it)")
	f.Duration("intent-build-timeout", 60*time.Minute, "wall-clock limit of a build run")
	f.Int("intent-revise-max-turns", 80, "most agent turns a revise or check-fix run may take (a Project may lower it)")
	f.Int("intent-revise-token-budget", 400000, "most output tokens a revise or check-fix run may spend")
	f.Duration("intent-revise-timeout", 45*time.Minute, "wall-clock limit of a revise or check-fix run")
	f.String("intent-plan-model", "anthropic/claude-sonnet-5", "canonical model the plan stage runs")
	f.String("intent-build-model", "anthropic/claude-sonnet-5", "canonical model the build stage runs")

	f.String("agent-namespace", "patchy-agents", "namespace the agent Jobs run in")
	f.String("agent-service-account", "patchy-agent", "service account for the agent Jobs")
	f.Duration("job-ttl", time.Hour, "ttlSecondsAfterFinished for a finished agent Job")
	runnercfg.RegisterFlags(f)
	runnercfg.RegisterRepositoryImageFlags(f)
	f.Int("changeset-max-entries", changeset.DefaultMaxEntries,
		"most files (upserts plus deletes) a build's changeset may touch before it is rejected without any forge call")
	return cmd
}

// settings reads the shared reconciler settings from the flags.
func settings(opts *cli.Options, namespace, agentNS string) (intent.Settings, error) {
	s := intent.Settings{
		Namespace:            namespace,
		AgentNamespace:       agentNS,
		PollInterval:         opts.Duration("intent-poll-interval"),
		ApprovalPollInterval: opts.Duration("intent-approval-poll-interval"),
		PRPollInterval:       opts.Duration("intent-pr-poll-interval"),
		RateLimitFloor:       opts.Int("intent-rate-limit-floor"),
		MaxAttempts:          intent.DefaultMaxAttempts,
		Plan: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-plan-max-turns")),
			TokenBudget: int64(opts.Int("intent-plan-token-budget")),
			Timeout:     opts.Duration("intent-plan-timeout"),
		},
		Build: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-build-max-turns")),
			TokenBudget: int64(opts.Int("intent-build-token-budget")),
			Timeout:     opts.Duration("intent-build-timeout"),
		},
		Revise: intent.StageCeiling{
			MaxTurns:    int32(opts.Int("intent-revise-max-turns")),
			TokenBudget: int64(opts.Int("intent-revise-token-budget")),
			Timeout:     opts.Duration("intent-revise-timeout"),
		},
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"--intent-poll-interval", s.PollInterval > 0},
		{"--intent-approval-poll-interval", s.ApprovalPollInterval > 0},
		{"--intent-pr-poll-interval", s.PRPollInterval > 0},
		{"--intent-rate-limit-floor", s.RateLimitFloor >= 0},
		{"--intent-plan-max-turns", s.Plan.MaxTurns > 0},
		{"--intent-plan-token-budget", s.Plan.TokenBudget > 0},
		{"--intent-plan-timeout", s.Plan.Timeout > 0},
		{"--intent-build-max-turns", s.Build.MaxTurns > 0},
		{"--intent-build-token-budget", s.Build.TokenBudget > 0},
		{"--intent-build-timeout", s.Build.Timeout > 0},
		{"--intent-revise-max-turns", s.Revise.MaxTurns > 0},
		{"--intent-revise-token-budget", s.Revise.TokenBudget > 0},
		{"--intent-revise-timeout", s.Revise.Timeout > 0},
	} {
		if !c.ok {
			return intent.Settings{}, fmt.Errorf("%s must be positive", c.name)
		}
	}
	if deadline := opts.Duration("intent-job-deadline"); deadline < max(s.Plan.Timeout, s.Build.Timeout, s.Revise.Timeout) {
		return intent.Settings{}, fmt.Errorf("--intent-job-deadline %s is shorter than a stage's timeout", deadline)
	}
	return s, nil
}

// harness resolves the one harness both intent stages run on: intents run
// on brokered claude only (no other harness honours the sandbox postures),
// or on the fake harness in dev and tests.
func harness(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, agentNS string,
	runners map[string]jobs.Runner) (string, error) {
	planModel, buildModel := opts.String("intent-plan-model"), opts.String("intent-build-model")
	enabled, err := runnercfg.ResolveWithoutBrokerProbe(ctx, opts, cs, agentNS, runners, runnercfg.Restrict(opts),
		[]string{planModel, buildModel}, planModel, buildModel)
	if err != nil {
		return "", err
	}
	plan, _, err := runnercfg.ResolveHarness(planModel, enabled)
	if err != nil {
		return "", err
	}
	build, _, err := runnercfg.ResolveHarness(buildModel, enabled)
	if err != nil {
		return "", err
	}
	if plan != build {
		return "", fmt.Errorf("the plan model runs on %s and the build model on %s; intents run both on one harness",
			plan, build)
	}
	if !slices.Contains([]string{model.HarnessClaude, model.HarnessFake}, plan) {
		return "", fmt.Errorf("intents run on brokered claude only, and the intent models resolve to %s", plan)
	}
	if r := runners[plan]; plan == model.HarnessClaude && !r.Brokered {
		return "", errors.New("intents run on brokered claude only, and the claude runner is not brokered")
	}
	return plan, nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("PATCHY_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "intent-controller",
		ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return errors.New("namespace is required (--namespace or POD_NAMESPACE)")
	}
	agentNS := opts.String("agent-namespace")
	set, err := settings(opts, namespace, agentNS)
	if err != nil {
		return err
	}
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		return err
	}
	repositoryImages, ephemeralStorage, err := runnercfg.RepositoryImages(opts)
	if err != nil {
		return err
	}
	if opts.Int("changeset-max-entries") <= 0 {
		return errors.New("--changeset-max-entries must be positive")
	}

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig:              opts.String("kubeconfig"),
		LeaderElectionID:        "patchy-intent-controller-leader",
		LeaderElectionNamespace: namespace,
		Namespaces:              []string{namespace},
		AgentNamespace:          agentNS,
		HealthAddr:              opts.String("health-addr"),
		Log:                     log,
		// Only intent-labelled ConfigMaps: never the Finding transcripts
		// beside them in the release namespace.
		ConfigMapSelector:  intent.ConfigMapSelector(),
		RepositorySelector: intent.ConfigMapSelector(),
	})
	if err != nil {
		return err
	}
	cfg, err := kube.RestConfig(opts.String("kubeconfig"))
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes clientset: %w", err)
	}
	harnessID, err := harness(ctx, opts, cs, agentNS, runners)
	if err != nil {
		return err
	}

	runner := intent.NewJobRunner(cs, jobs.Config{
		Namespace:      agentNS,
		ServiceAccount: opts.String("agent-service-account"),
		Deadline:       opts.Duration("intent-job-deadline"),
		TTL:            opts.Duration("job-ttl"),
		Runners:        runners,
		BrokerAudience: opts.String("broker-token-audience"),

		EphemeralStorage:      ephemeralStorage,
		AllowRepositoryImages: repositoryImages,
	}, log)
	// Forges come from the cache; their Secrets are never cached, so the
	// manager's client reads each one live.
	gh := intent.NewForgeGitHub(forge.NewStore(mgr.GetClient()), namespace)
	images := runnerguard.Guard{
		Enabled: repositoryImages,
		Breaker: runnerguard.NewBreaker("intent-controller", log),
	}
	nudger := intent.NewNudger()

	if err := (&intent.ProjectReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), GitHub: gh, Settings: set, Nudger: nudger, Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.IntentReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), GitHub: gh, Settings: set, Images: images,
		Nudger: nudger, Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.RunReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Jobs: runner, GitHub: gh, Settings: set,
		MaxConcurrent: opts.Int("intent-max-concurrent-runs"), Harness: harnessID,
		PlanModel: opts.String("intent-plan-model"), BuildModel: opts.String("intent-build-model"),
		Images: images, MaxChangesetEntries: opts.Int("changeset-max-entries"), Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&intent.TTLReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), TTL: opts.Duration("intent-ttl"), Log: log,
	}).SetupWithManager(mgr); err != nil {
		return err
	}

	log.LogAttrs(ctx, slog.LevelInfo, "intent-controller starting",
		slog.String("namespace", namespace),
		slog.String("agent_namespace", agentNS),
		slog.String("harness", harnessID),
		slog.Int("max_concurrent_runs", opts.Int("intent-max-concurrent-runs")),
		slog.Bool("repository_images", repositoryImages))

	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
