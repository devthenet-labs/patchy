// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/preview"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use: "serve", Short: "Run the Preview slot controller and orphan sweep",
		RunE: func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "release namespace (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.Int("preview-slot-count", 2, "number of chart-created preview slots")
	f.String("preview-image-prefix", "", "exact <registry>/<path>/ prefix every preview image repository sits "+
		"directly under, one DNS-label leaf deep (the chart's <preview.imageRegistry>/<preview.imagePathPrefix>/)")
	f.String("preview-host-suffix", "", "DNS suffix after <project>-<issue>")
	f.String("preview-node-pool", "", "dedicated DefaultDeny NodePool")
	f.String("preview-node-class", "", "dedicated DefaultDeny NodeClass")
	f.String("preview-taint-key", "", "dedicated NoExecute preview-only taint key")
	f.Duration("preview-rollout-timeout", 10*time.Minute, "one runtime rollout attempt deadline")
	f.Duration("preview-poll-interval", 15*time.Second, "readiness/queue poll and orphan-sweep interval")
	f.Int("preview-max-retries", 3, "max timed-out or rejected rollout attempts per PR head")
	f.Bool("preview-target-health", false, "mark a Preview Ready only once each component's load balancer "+
		"target is healthy (its Pods' readiness gates, injected in slot namespaces labelled for it): the Ingress "+
		"precedes the Pods and stays across redeploys")
	f.Bool("preview-auth-required", false, "put every preview Ingress behind the sign-in relay: render each "+
		"slot's pinned auth annotations (--preview-auth-annotations) and have the sweeper report, then after a "+
		"grace period delete, a slot Ingress without them")
	f.String("preview-auth-annotations", "", "JSON object of slot namespace to its pinned auth annotations, "+
		"the current key generation's, exactly as the chart renders it for the slot admission policy")
	f.String("preview-auth-previous-annotations", "", "the previous key generation's pinned auth annotations "+
		"during a rotation's overlap (same JSON shape); an Ingress still on them is not unauthenticated")
	return cmd
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir: os.Getenv("PATCHY_TELEMETRY_DIR"), Level: opts.LogLevel,
		ServiceName: "preview-controller", ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	namespace := opts.String("namespace")
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	settings := preview.Settings{
		Namespace: namespace, SlotCount: opts.Int("preview-slot-count"),
		ImagePrefix: opts.String("preview-image-prefix"), HostSuffix: opts.String("preview-host-suffix"),
		NodePool: opts.String("preview-node-pool"), NodeClass: opts.String("preview-node-class"),
		TaintKey:       opts.String("preview-taint-key"),
		RolloutTimeout: opts.Duration("preview-rollout-timeout"), PollInterval: opts.Duration("preview-poll-interval"),
		MaxRetries:   int32(opts.Int("preview-max-retries")),
		TargetHealth: opts.Bool("preview-target-health"),
	}
	auth, err := authSettings(opts)
	if err != nil {
		return err
	}
	settings.Auth = auth
	if err := settings.Validate(); err != nil {
		return err
	}
	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig: opts.String("kubeconfig"), LeaderElectionID: "patchy-preview-controller-leader",
		LeaderElectionNamespace: namespace, Namespaces: []string{namespace},
		HealthAddr: opts.String("health-addr"), Log: prov.Logger,
	})
	if err != nil {
		return err
	}
	// The manager's cache is release-namespace-only. Slot reads use a direct
	// client, so the lease and cleanup see the current API server state and
	// no cluster-scoped informer or slot-wide cache is needed.
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return fmt.Errorf("preview direct client: %w", err)
	}
	recorder := mgr.GetEventRecorder("patchy-preview-controller")
	reconciler := &preview.Reconciler{Client: direct, Settings: settings, Log: prov.Logger, Events: recorder}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.Add(&preview.Sweeper{Client: direct, Settings: settings, Log: prov.Logger,
		Events: recorder}); err != nil {
		return err
	}
	prov.Logger.LogAttrs(ctx, slog.LevelInfo, "preview-controller starting",
		slog.Int("slot_count", settings.SlotCount), slog.String("namespace", namespace),
		slog.Bool("auth_required", settings.Auth.Required))
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// authSettings reads the sign-in relay's pinned annotations, which the chart
// renders once for both this controller and the slot admission policy.
func authSettings(opts *cli.Options) (preview.AuthSettings, error) {
	current, err := preview.ParseAuthAnnotations(opts.String("preview-auth-annotations"))
	if err != nil {
		return preview.AuthSettings{}, err
	}
	previous, err := preview.ParseAuthAnnotations(opts.String("preview-auth-previous-annotations"))
	if err != nil {
		return preview.AuthSettings{}, fmt.Errorf("previous generation: %w", err)
	}
	return preview.AuthSettings{Required: opts.Bool("preview-auth-required"), Annotations: current,
		Previous: previous}, nil
}
