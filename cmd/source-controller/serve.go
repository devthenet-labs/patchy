// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/bitwise-media-group/patchy/internal/artifact"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/source"
	"github.com/bitwise-media-group/patchy/internal/forge"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/runnerimage/resolve"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Forge and Repository reconcilers and the artifact server",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the patchy resources live in (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("artifact-addr", ":9790", "listen address of the artifact server")
	f.String("artifact-base-url", "",
		"base URL agent pods fetch artifacts from (default: the in-cluster service address)")
	f.String("artifact-dir", "/data/artifacts", "directory the artifact tarballs are stored in")
	f.Int("max-artifact-bytes", int(source.DefaultMaxArtifactBytes),
		"largest repository tarball stored; larger repositories stall")
	f.String("artifact-internal-addr", "",
		"listen address of the internal workspace-upload endpoint (empty disables it; deploy :9791)")
	f.String("internal-upload-token-file", "",
		"file holding the shared bearer token internal uploads must present (optional defense-in-depth)")
	f.Int("max-workspace-bytes", 64<<20, "largest workspace bundle accepted on the internal endpoint")
	f.Duration("workspace-retention", 7*24*time.Hour,
		"remove workspace bundles not accessed for this long (0 keeps forever)")
	f.Bool("repository-images", false,
		"resolve repository-declared agent runner images (.patchy/agent.yaml, devcontainer.json); the kill switch")
	f.String("repository-image-registries", "",
		"comma-separated host/path prefixes a declared image must sit under (required with --repository-images)")
	f.Int("repository-image-max-bytes", int(resolve.DefaultMaxBytes),
		"largest compressed layer total per platform of a declared image")
	f.String("repository-image-cosign-key-file", "",
		"PEM public key every declared image must be cosign-signed with (required unless --repository-image-allow-unsigned)")
	f.Bool("repository-image-allow-unsigned", false,
		"admit declared images without a signature (explicit opt-out; never the default)")
	f.String("repository-image-on-reject", source.OnRejectDefault,
		"what a rejected declaration does to the finding: default (run the default image) or handoff (park it for a human)")
	return cmd
}

// runnerImages builds the repository-declared runner image configuration
// from the flags, or nil when the feature is off. Every policy value is
// validated here so a bad allowlist entry or key fails startup rather than
// admitting an image.
func runnerImages(opts *cli.Options) (*source.RunnerImages, error) {
	if !opts.Bool("repository-images") {
		return nil, nil
	}
	policy, err := runnerimage.NewPolicy(opts.StringList("repository-image-registries"))
	if err != nil {
		return nil, fmt.Errorf("repository-image-registries: %w", err)
	}
	onReject := opts.String("repository-image-on-reject")
	if onReject != source.OnRejectHandoff && onReject != source.OnRejectDefault {
		return nil, fmt.Errorf("repository-image-on-reject: %q is not default or handoff", onReject)
	}
	cfg := resolve.Config{
		MaxBytes:      int64(opts.Int("repository-image-max-bytes")),
		AllowUnsigned: opts.Bool("repository-image-allow-unsigned"),
		// The Job builder's own reserved names, beyond the prefixes and
		// gateway names the resolver refuses on its own: one list, so a name
		// the Job starts reserving is refused at resolve time with no
		// second copy to keep in step.
		ReservedEnv: reservedEnvSet(jobs.ReservedEnvNames()),
		Keychain:    resolve.NewKeychain(),
	}
	if path := opts.String("repository-image-cosign-key-file"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("repository-image-cosign-key-file: %w", err)
		}
		if cfg.PublicKey, err = resolve.ParsePublicKey(raw); err != nil {
			return nil, fmt.Errorf("repository-image-cosign-key-file: %w", err)
		}
	}
	resolver, err := resolve.New(cfg)
	if err != nil {
		return nil, err
	}
	return &source.RunnerImages{Policy: policy, Resolver: resolver, OnReject: onReject}, nil
}

// internalUploadToken reads the optional shared secret internal uploads must
// present; an empty flag means NetworkPolicy is the only gate.
func internalUploadToken(opts *cli.Options) (string, error) {
	path := opts.String("internal-upload-token-file")
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("internal-upload-token-file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("internal-upload-token-file: file is empty")
	}
	return token, nil
}

// artifactBaseURL is the URL minted into Repository statuses; unless
// configured it is the controller's in-cluster Service address on the
// artifact port.
func artifactBaseURL(opts *cli.Options, namespace string) (string, error) {
	if u := opts.String("artifact-base-url"); u != "" {
		return u, nil
	}
	_, port, err := net.SplitHostPort(opts.String("artifact-addr"))
	if err != nil {
		return "", fmt.Errorf("artifact-addr: %w", err)
	}
	return fmt.Sprintf("http://patchy-source-controller.%s.svc.cluster.local:%s", namespace, port), nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("PATCHY_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "source-controller",
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
	baseURL, err := artifactBaseURL(opts, namespace)
	if err != nil {
		return err
	}
	store, err := artifact.NewStore(opts.String("artifact-dir"), baseURL)
	if err != nil {
		return err
	}

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig:              opts.String("kubeconfig"),
		LeaderElectionID:        "patchy-source-controller-leader",
		LeaderElectionNamespace: namespace,
		Namespaces:              []string{namespace},
		HealthAddr:              opts.String("health-addr"),
		Log:                     log,
	})
	if err != nil {
		return err
	}

	forges := forge.NewStore(mgr.GetAPIReader())
	fc := &source.ForgeReconciler{Client: mgr.GetClient(), Forges: forges, Log: log}
	if err := fc.SetupWithManager(mgr); err != nil {
		return err
	}
	images, err := runnerImages(opts)
	if err != nil {
		return err
	}
	rc := &source.RepositoryReconciler{
		Client:           mgr.GetClient(),
		Forges:           forges,
		Artifacts:        store,
		MaxArtifactBytes: int64(opts.Int("max-artifact-bytes")),
		Images:           images,
		Log:              log,
	}
	if err := rc.SetupWithManager(mgr); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              opts.String("artifact-addr"),
		Handler:           store.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	internalSrv, err := internalServer(opts, store)
	if err != nil {
		return err
	}
	internalAddr := ""
	if internalSrv != nil {
		internalAddr = internalSrv.Addr
	}

	log.LogAttrs(ctx, slog.LevelInfo, "source-controller starting",
		slog.String("namespace", namespace),
		slog.String("artifact_addr", srv.Addr),
		slog.String("artifact_internal_addr", internalAddr),
		slog.String("artifact_base_url", baseURL),
		slog.Bool("repository_images", images != nil))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return mgr.Start(ctx) })
	g.Go(func() error {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	if internalSrv != nil {
		g.Go(func() error {
			if err := internalSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return internalSrv.Shutdown(shutdownCtx)
		})
	}
	if retention := opts.Duration("workspace-retention"); retention > 0 {
		g.Go(func() error { return sweepBlobs(ctx, store, retention, log) })
	}
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// internalServer builds the optional workspace-upload listener; nil when the
// endpoint is disabled.
func internalServer(opts *cli.Options, store *artifact.Store) (*http.Server, error) {
	addr := opts.String("artifact-internal-addr")
	if addr == "" {
		return nil, nil
	}
	token, err := internalUploadToken(opts)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           store.InternalHandler(token, int64(opts.Int("max-workspace-bytes"))),
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

// sweepBlobs runs the hourly last-access retention sweep until ctx ends.
func sweepBlobs(ctx context.Context, store *artifact.Store, retention time.Duration, log *slog.Logger) error {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if removed := store.SweepBlobs(time.Now().Add(-retention)); removed > 0 {
				log.LogAttrs(ctx, slog.LevelInfo, "workspace blobs swept",
					slog.Int("removed", removed))
			}
		}
	}
}

// reservedEnvSet turns the Job builder's sorted reserved names into the set
// resolve.Config takes.
func reservedEnvSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
