// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/access"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/dex"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/hostprobe"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/keydir"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/kubeview"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/ledger"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/signer"
	"github.com/bitwise-media-group/patchy/internal/previewauth/httpapi"
	"github.com/bitwise-media-group/patchy/internal/telemetry"
	"github.com/bitwise-media-group/patchy/internal/version"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
	"github.com/bitwise-media-group/patchy/internal/web/authz"
)

func newServeCmd(opts *cli.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the preview sign-in relay",
		RunE:  func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.String("namespace", "", "release namespace holding the Previews and the ledger Lease (default: POD_NAMESPACE)")
	f.String("kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	f.String("health-addr", ":8081", "healthz/readyz probe listen address")
	f.String("preview-auth-issuer", "", "the relay's issuer, https://<relay host> with no path: byte-equal in "+
		"discovery, ID tokens and the preview Ingress annotations")
	f.String("preview-auth-host-suffix", "", "the preview host suffix; codes go only to "+
		"https://<label>.<suffix>/oauth2/idpresponse")
	f.Int("preview-auth-slot-count", 2, "number of preview slots (one ALB client each)")
	f.String("preview-auth-keys-dir", "/etc/patchy/preview-auth/keys", "the mounted keys Secret: master, "+
		"generation, signingKey and, during a rotation, previousMaster, previousGeneration, previousSigningKey")
	f.String("preview-auth-dex-issuer-url", "", "Dex's issuer URL (https)")
	f.String("preview-auth-dex-client-id", "", "the relay's static client id at Dex")
	f.String("preview-auth-dex-client-secret-file", "", "mounted file holding the relay's Dex client secret")
	f.String("preview-auth-dex-ca-file", "", "PEM certificates trusted for Dex besides the system roots "+
		"(a Dex behind a private CA); empty trusts the system roots only")
	f.String("preview-auth-username-claim", "preferred_username", "ID-token claim access reviews run for")
	f.String("preview-auth-groups-claim", "groups", "ID-token claim holding the viewer's groups")
	f.String("preview-auth-username-prefix", "", "prefix for the username in access reviews (required)")
	f.String("preview-auth-groups-prefix", "", "prefix for every group in access reviews (required)")
	f.Bool("preview-auth-require-verified-email", false,
		"refuse an ID token whose email_verified is not true (required when the username claim is email)")
	lt := previewauth.DefaultLifetimes()
	f.Duration("preview-auth-code-ttl", lt.Code, "authorization code lifetime")
	f.Duration("preview-auth-access-token-ttl", lt.Access,
		"access and ID token lifetime, and every token response's expires_in")
	f.Duration("preview-auth-login-ttl", lt.Login, "how long a sign-in through Dex may take")
	f.Duration("preview-auth-session-max-age", lt.SessionMaxAge,
		"relay session lifetime, and so every refresh token's")
	f.Duration("preview-auth-review-ttl", 20*time.Second, "how long one access review's answer is cached")
	f.String("preview-auth-ledger-lease", "", "name of the Lease the single-use code ledger lives on")
	f.Float64("preview-auth-rate-per-second", 5, "per-source-address request rate; 0 disables the limit")
	f.Int("preview-auth-rate-burst", 30, "per-source-address burst")
	f.Int("preview-auth-forwarded-hops", 0, "trusted proxies appending to X-Forwarded-For in front of the relay "+
		"(1 behind an ALB); 0 uses the peer address")
	f.Duration("preview-auth-probe-interval", time.Minute,
		"how often every Ready preview host is probed without credentials; 0 disables the probe")
	f.Duration("preview-auth-probe-timeout", hostprobe.DefaultTimeout, "one host probe's timeout")
	return cmd
}

func lifetimes(opts *cli.Options) previewauth.Lifetimes {
	return previewauth.Lifetimes{
		Code: opts.Duration("preview-auth-code-ttl"), Access: opts.Duration("preview-auth-access-token-ttl"),
		Login: opts.Duration("preview-auth-login-ttl"), SessionMaxAge: opts.Duration("preview-auth-session-max-age"),
	}
}

func claims(opts *cli.Options) auth.ClaimsConfig {
	return auth.ClaimsConfig{
		Username: opts.String("preview-auth-username-claim"), Groups: opts.String("preview-auth-groups-claim"),
		UsernamePrefix:       opts.String("preview-auth-username-prefix"),
		GroupsPrefix:         opts.String("preview-auth-groups-prefix"),
		RequireVerifiedEmail: opts.Bool("preview-auth-require-verified-email"),
	}
}

func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("--preview-auth-dex-client-secret-file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("dex client secret: %w", err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", errors.New("dex client secret file is empty")
	}
	return s, nil
}

// static is everything the relay loads before it touches the cluster.
type static struct {
	namespace, lease, issuer string
	callbacks                previewauth.Callbacks
	keys                     keydir.Keys
	signer                   *signer.Signer
	upstream                 *dex.Upstream
}

// loadStatic validates the flags and loads the keys and the Dex client.
func loadStatic(opts *cli.Options) (static, error) {
	st := static{namespace: opts.String("namespace"), lease: opts.String("preview-auth-ledger-lease"),
		issuer: opts.String("preview-auth-issuer")}
	if st.namespace == "" {
		st.namespace = os.Getenv("POD_NAMESPACE")
	}
	if st.namespace == "" {
		return st, errors.New("--namespace (or POD_NAMESPACE) is required")
	}
	if st.lease == "" {
		return st, errors.New("--preview-auth-ledger-lease is required")
	}
	if !previewauth.ValidIssuer(st.issuer) {
		return st, fmt.Errorf("--preview-auth-issuer %q is not an https origin with no path", st.issuer)
	}
	var err error
	if st.callbacks, err = previewauth.NewCallbacks(opts.String("preview-auth-host-suffix")); err != nil {
		return st, err
	}
	if st.keys, err = keydir.Load(opts.String("preview-auth-keys-dir")); err != nil {
		return st, err
	}
	if st.signer, err = signer.New(st.keys.SigningKey, st.keys.PreviousSigningKey); err != nil {
		return st, err
	}
	secret, err := readSecretFile(opts.String("preview-auth-dex-client-secret-file"))
	if err != nil {
		return st, err
	}
	var httpClient *http.Client
	if path := opts.String("preview-auth-dex-ca-file"); path != "" {
		caPEM, err := os.ReadFile(path)
		if err != nil {
			return st, fmt.Errorf("dex CA bundle: %w", err)
		}
		if httpClient, err = dex.ClientWithCA(caPEM); err != nil {
			return st, err
		}
	}
	st.upstream, err = dex.New(dex.Config{
		IssuerURL: opts.String("preview-auth-dex-issuer-url"), ClientID: opts.String("preview-auth-dex-client-id"),
		ClientSecret: secret, RedirectURL: st.issuer + dex.CallbackPath, Claims: claims(opts),
		HTTPClient: httpClient,
	})
	return st, err
}

func serve(ctx context.Context, opts *cli.Options) error {
	prov, shutdown, err := telemetry.Init(ctx, telemetry.Config{
		Dir: os.Getenv("PATCHY_TELEMETRY_DIR"), Level: opts.LogLevel,
		ServiceName: "preview-auth", ServiceVersion: version.Version,
	})
	if err != nil {
		prov.Logger.LogAttrs(ctx, slog.LevelWarn, "telemetry disabled", slog.Any("error", err))
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	log := prov.Logger

	st, err := loadStatic(opts)
	if err != nil {
		return err
	}
	namespace, leaseName, issuer, cb, keys, sg, upstream := st.namespace, st.lease, st.issuer, st.callbacks,
		st.keys, st.signer, st.upstream

	mgr, err := kube.NewManager(kube.Options{
		Kubeconfig: opts.String("kubeconfig"), Namespaces: []string{namespace},
		HealthAddr: opts.String("health-addr"), Log: log,
	})
	if err != nil {
		return err
	}
	// The Lease and the access reviews go to the API server directly: the
	// ledger's compare-and-swap needs the current resourceVersion, and the
	// relay may not list or watch Leases.
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return fmt.Errorf("direct client: %w", err)
	}
	// Start the Preview informer now, so readiness can wait for it.
	if _, err := mgr.GetCache().GetInformer(ctx, &v1alpha1.Preview{}); err != nil {
		return fmt.Errorf("preview informer: %w", err)
	}
	var synced atomic.Bool
	if err := mgr.AddReadyzCheck("previews-synced", func(*http.Request) error {
		if !synced.Load() {
			return errors.New("preview cache not synced")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("readyz check: %w", err)
	}

	lookup := kubeview.Lookup{Reader: mgr.GetClient(), Namespace: namespace}
	srv, err := httpapi.New(httpapi.Config{
		Issuer: issuer, SlotCount: opts.Int("preview-auth-slot-count"), Callbacks: cb, Lifetimes: lifetimes(opts),
		Ring: keys.Ring, Lookup: lookup,
		Authorizer: access.Reviewer{
			Projects: authz.NewProjectReviewer(direct, namespace, opts.Duration("preview-auth-review-ttl")),
			Record:   httpapi.CountSAR,
		},
		Ledger:   &ledger.Ledger{Client: direct, Namespace: namespace, Name: leaseName, Record: httpapi.CountLedger},
		Upstream: upstream, Signer: sg,
		RateLimit: httpapi.RateLimit{
			PerSecond: opts.Float("preview-auth-rate-per-second"), Burst: opts.Int("preview-auth-rate-burst"),
			ForwardedHops: opts.Int("preview-auth-forwarded-hops"),
		},
		Log: log,
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(&hostprobe.Prober{
		Lister: lookup, Callbacks: cb, Issuer: issuer, Interval: opts.Duration("preview-auth-probe-interval"),
		Timeout: opts.Duration("preview-auth-probe-timeout"), Log: log,
	}); err != nil {
		return fmt.Errorf("host probe: %w", err)
	}
	httpSrv := &http.Server{
		Addr: opts.ListenAddr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
	}
	if err := mgr.Add(listener{srv: httpSrv}); err != nil {
		return fmt.Errorf("listener: %w", err)
	}
	if err := mgr.Add(syncWatcher{mgr.GetCache().WaitForCacheSync, &synced, upstream, log}); err != nil {
		return fmt.Errorf("sync watcher: %w", err)
	}

	log.LogAttrs(ctx, slog.LevelInfo, "preview-auth starting",
		slog.String("issuer", issuer), slog.String("host_suffix", cb.Suffix()),
		slog.Int("slot_count", opts.Int("preview-auth-slot-count")), slog.Int("key_generation", keys.Ring.Generation()),
		slog.String("signing_kid", sg.KeyID()), slog.String("listen_addr", httpSrv.Addr))
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// listener serves the relay until the manager stops.
type listener struct{ srv *http.Server }

func (l listener) NeedLeaderElection() bool { return false }

func (l listener) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- l.srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return l.srv.Shutdown(sctx)
	}
}

// syncWatcher marks the relay ready once the Preview cache has synced, and
// tries Dex discovery once so a misconfigured issuer shows in the log at
// start rather than at the first sign-in. Dex being down does not keep the
// relay unready: refreshes and userinfo never need it.
type syncWatcher struct {
	wait     func(context.Context) bool
	synced   *atomic.Bool
	upstream *dex.Upstream
	log      *slog.Logger
}

func (w syncWatcher) NeedLeaderElection() bool { return false }

func (w syncWatcher) Start(ctx context.Context) error {
	if w.wait(ctx) {
		w.synced.Store(true)
	}
	if err := w.upstream.Ready(ctx); err != nil {
		w.log.LogAttrs(ctx, slog.LevelWarn, "Dex discovery failed; sign-ins will retry it", slog.Any("error", err))
	}
	<-ctx.Done()
	return nil
}
