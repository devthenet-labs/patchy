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
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
	"github.com/bitwise-media-group/patchy/internal/web"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the status page server",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "namespace the patchy resources live in (default: POD_NAMESPACE)")
	f.String("agent-namespace", "patchy-agents",
		"namespace the agent Jobs run in; live transcripts are followed from their pod logs")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("auth-config", "",
		"path to the mounted authentication config; absent means rollup statistics only, no findings access")
	f.Bool("intents-enabled", false,
		"serve the intents views (board, timeline, run panel) with per-Project access reviews; needs auth mode "+
			"oidc with usernamePrefix and groupsPrefix, and turns on the hardened browser envelope")
	f.Bool("intents-dev-insecure", false,
		"development only: let the intents views run in auth mode none, which shows every Project to every "+
			"visitor; refused unless --listen-addr is a loopback address")
	return cmd
}

// intentsWiring is what --intents-enabled adds to the server, decided before
// anything starts.
type intentsWiring struct {
	on bool
	// projects is the tier source the auth gate chose; nil means the
	// access-review granter, built once the manager's client exists.
	projects web.ProjectGranter
}

func newIntentsWiring(opts *cli.Options, authCfg *auth.Config) (intentsWiring, error) {
	if !opts.Bool("intents-enabled") {
		return intentsWiring{}, nil
	}
	projects, err := intentsAuthGate(authCfg, opts.Bool("intents-dev-insecure"), opts.ListenAddr)
	if err != nil {
		return intentsWiring{}, err
	}
	return intentsWiring{on: true, projects: projects}, nil
}

// registerIndexes installs the intents indexes. The intents kinds are cached
// only with the views on: their informers need the intents RBAC, which the
// chart grants only then.
func (iw intentsWiring) registerIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if !iw.on {
		return nil
	}
	return web.RegisterIntentIndexes(ctx, idx)
}

// apply turns the intents views on in srv; with them off it returns srv
// unchanged.
func (iw intentsWiring) apply(srv *web.Server, c client.Client, namespace string, tailer web.Tailer) *web.Server {
	if !iw.on {
		return srv
	}
	projects := iw.projects
	if projects == nil {
		projects = authz.NewProjectReviewer(c, namespace, 0)
	}
	o := web.IntentsOptions{Projects: projects}
	if t, ok := tailer.(*jobs.Tailer); ok && t != nil {
		o.Facts = t
	}
	return srv.WithIntents(o)
}

// intentsAuthGate checks the auth posture may serve the intents views. Mode
// oidc must pass RequireForIntents; mode none is accepted only behind the
// explicit development flag and a loopback listen address. It returns the
// tier source the server uses.
func intentsAuthGate(cfg *auth.Config, devInsecure bool, listen string) (web.ProjectGranter, error) {
	if cfg != nil && cfg.Mode == auth.ModeNone {
		if !devInsecure {
			return nil, errors.New("--intents-enabled refuses auth mode none, which shows every Project to " +
				"every visitor; use mode oidc (or --intents-dev-insecure on a loopback address for development)")
		}
		if !loopback(listen) {
			return nil, fmt.Errorf("--intents-dev-insecure needs a loopback --listen-addr, not %q", listen)
		}
		return authz.FullProjects{}, nil
	}
	if err := cfg.RequireForIntents(); err != nil {
		return nil, err
	}
	return nil, nil
}

// loopback reports whether a listen address binds only the loopback
// interface: an explicit 127.0.0.0/8 or ::1 host, or localhost. An empty
// host (":8080") binds every interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// agentTailer builds the read-only pod-log reader for the agent namespace.
// It is a plain clientset rather than the manager's client: pod logs are a
// subresource stream, not a cacheable object, and the manager's cache is
// scoped to the patchy namespace anyway.
func agentTailer(kubeconfig, agentNamespace string) (web.Tailer, error) {
	if agentNamespace == "" {
		return nil, errors.New("no agent namespace configured")
	}
	cfg, err := kube.RestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset: %w", err)
	}
	return jobs.NewTailer(cs, agentNamespace), nil
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir:            os.Getenv("PATCHY_TELEMETRY_DIR"),
		Level:          opts.LogLevel,
		ServiceName:    "status-server",
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

	authCfg, err := auth.LoadConfig(opts.String("auth-config"))
	if err != nil {
		return err
	}
	if authCfg == nil {
		log.LogAttrs(ctx, slog.LevelWarn,
			"authentication not configured; serving rollup statistics only — findings and actions are unavailable")
	}
	// Checked before anything starts: a posture that cannot serve the
	// intents views safely must not come up serving them.
	intents, err := newIntentsWiring(opts, authCfg)
	if err != nil {
		return err
	}
	authn, err := auth.New(ctx, authCfg, log)
	if err != nil {
		return err
	}

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig: opts.String("kubeconfig"),
		Namespaces: []string{namespace},
		HealthAddr: opts.String("health-addr"),
		Log:        log,
	})
	if err != nil {
		return err
	}

	// The finding-detail projection looks child runs up by owning finding;
	// the index must exist before the cache starts serving.
	if err := web.RegisterIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	if err := intents.registerIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}

	// Mode none bypasses access reviews entirely; every other posture asks
	// the cluster per identity.
	var granter web.Granter = authz.NewReviewer(mgr.GetClient(), namespace, 0)
	if authCfg != nil && authCfg.Mode == auth.ModeNone {
		granter = authz.Full{}
	}

	srv := web.NewServer(mgr.GetClient(), namespace, authn, granter, log)
	// Transcripts read through the uncached API reader (caching every
	// ConfigMap in the namespace to serve objects read once per page view is
	// not worth an informer) and follow live runs from the agent namespace's
	// pod logs, via the API server — the status server never dials an agent pod.
	agentNS := opts.String("agent-namespace")
	tailer, err := agentTailer(opts.String("kubeconfig"), agentNS)
	if err != nil {
		// Live following is a nicety; persisted transcripts still serve.
		log.LogAttrs(ctx, slog.LevelWarn, "live transcripts unavailable",
			slog.String("agent_namespace", agentNS), slog.Any("error", err))
	}
	srv = intents.apply(srv.WithTranscripts(mgr.GetAPIReader(), tailer), mgr.GetClient(), namespace, tailer)

	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return srv.StartWatch(ctx, mgr.GetCache())
	}))
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              opts.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	mode := "unconfigured"
	if authCfg != nil {
		mode = authCfg.Mode
	}
	log.LogAttrs(ctx, slog.LevelInfo, "status-server starting",
		slog.String("namespace", namespace),
		slog.String("listen_addr", httpSrv.Addr),
		slog.String("auth_mode", mode),
		slog.Bool("intents", intents.on))

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return mgr.Start(ctx) })
	g.Go(func() error {
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	})
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
