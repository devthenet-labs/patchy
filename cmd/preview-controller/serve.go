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
	f.String("preview-image-prefix", "", "exact <registry>/patchy/previews/ prefix")
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
	reconciler := &preview.Reconciler{Client: direct, Settings: settings, Log: prov.Logger}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.Add(&preview.Sweeper{Client: direct, Settings: settings, Log: prov.Logger}); err != nil {
		return err
	}
	prov.Logger.LogAttrs(ctx, slog.LevelInfo, "preview-controller starting",
		slog.Int("slot_count", settings.SlotCount), slog.String("namespace", namespace))
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
