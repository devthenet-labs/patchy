// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/dex"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/keydir"
	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/signer"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("preview-auth", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	signingKeyOnce sync.Once
	signingKeyPEM  string
)

// rsaKeyPEM is one 2048-bit RSA key per test process (generation is slow
// under -race).
func rsaKeyPEM(t *testing.T) string {
	t.Helper()
	signingKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, signer.MinRSABits)
		if err != nil {
			panic(err)
		}
		signingKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return signingKeyPEM
}

// keysDir writes the mounted keys Secret, signingKey replaced when given.
func keysDir(t *testing.T, signingKey string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, keydir.FileMaster, "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ01\n")
	writeFile(t, dir, keydir.FileGeneration, "3\n")
	if signingKey == "" {
		signingKey = rsaKeyPEM(t)
	}
	writeFile(t, dir, keydir.FileSigningKey, signingKey)
	return dir
}

// staticArgs is a flag set loadStatic accepts, extra appended (a later
// flag wins).
func staticArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	secret := writeFile(t, t.TempDir(), "client-secret", "dex-secret\n")
	return append([]string{
		"--namespace", "patchy",
		"--preview-auth-ledger-lease", "patchy-preview-auth-codes",
		"--preview-auth-issuer", "https://auth.previews.example.com",
		"--preview-auth-host-suffix", "previews.example.com",
		"--preview-auth-keys-dir", keysDir(t, ""),
		"--preview-auth-dex-issuer-url", "https://dex.example.com",
		"--preview-auth-dex-client-id", "preview-auth",
		"--preview-auth-dex-client-secret-file", secret,
		"--preview-auth-username-prefix", "oidc:",
		"--preview-auth-groups-prefix", "oidc:",
	}, extra...)
}

func TestLifetimesAndClaims(t *testing.T) {
	opts := serveOpts(t, "--preview-auth-code-ttl", "30s", "--preview-auth-access-token-ttl", "5m",
		"--preview-auth-login-ttl", "2m", "--preview-auth-session-max-age", "8h",
		"--preview-auth-username-claim", "email", "--preview-auth-groups-claim", "roles",
		"--preview-auth-username-prefix", "u:", "--preview-auth-groups-prefix", "g:",
		"--preview-auth-require-verified-email")
	lt := lifetimes(opts)
	if lt != (previewauth.Lifetimes{Code: 30 * time.Second, Access: 5 * time.Minute, Login: 2 * time.Minute,
		SessionMaxAge: 8 * time.Hour}) {
		t.Errorf("lifetimes = %+v", lt)
	}
	c := claims(opts)
	if c.Username != "email" || c.Groups != "roles" || c.UsernamePrefix != "u:" || c.GroupsPrefix != "g:" ||
		!c.RequireVerifiedEmail {
		t.Errorf("claims = %+v", c)
	}
	if def := lifetimes(serveOpts(t)); def != previewauth.DefaultLifetimes() {
		t.Errorf("default lifetimes = %+v, want %+v", def, previewauth.DefaultLifetimes())
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name, path, want, wantErr string
	}{
		{name: "unset", wantErr: "--preview-auth-dex-client-secret-file is required"},
		{name: "missing", path: filepath.Join(dir, "absent"), wantErr: "dex client secret:"},
		{name: "blank", path: writeFile(t, dir, "blank", " \n"), wantErr: "dex client secret file is empty"},
		{name: "trimmed", path: writeFile(t, dir, "ok", "\ts3cret\n"), want: "s3cret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readSecretFile(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readSecretFile error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("readSecretFile = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// tlsCertPEM is the certificate of a TLS test server, as a Dex CA bundle.
func tlsCertPEM(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

func TestLoadStatic(t *testing.T) {
	st, err := loadStatic(serveOpts(t, staticArgs(t)...))
	if err != nil {
		t.Fatalf("loadStatic: %v", err)
	}
	if st.namespace != "patchy" || st.lease != "patchy-preview-auth-codes" ||
		st.issuer != "https://auth.previews.example.com" || st.callbacks.Suffix() != "previews.example.com" {
		t.Errorf("static = %+v", st)
	}
	if st.keys.Ring == nil || st.keys.Ring.Generation() != 3 || st.signer == nil || st.signer.KeyID() == "" ||
		st.upstream == nil {
		t.Errorf("keys/signer/upstream not loaded: %+v", st)
	}

	ca := writeFile(t, t.TempDir(), "ca.pem", tlsCertPEM(t))
	if _, err := loadStatic(serveOpts(t, staticArgs(t, "--preview-auth-dex-ca-file", ca)...)); err != nil {
		t.Errorf("loadStatic with a Dex CA bundle: %v", err)
	}

	t.Setenv("POD_NAMESPACE", "from-pod")
	st, err = loadStatic(serveOpts(t, staticArgs(t, "--namespace", "")...))
	if err != nil || st.namespace != "from-pod" {
		t.Errorf("loadStatic with POD_NAMESPACE = %q, %v; want from-pod", st.namespace, err)
	}
}

// TestLoadStaticRefuses: every input the relay cannot run safely with fails
// startup naming it, before the cluster is touched.
func TestLoadStaticRefuses(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	dir := t.TempDir()
	for _, tt := range []struct {
		name    string
		extra   []string
		wantErr string
	}{
		{name: "no namespace", extra: []string{"--namespace", ""}, wantErr: "--namespace (or POD_NAMESPACE) is required"},
		{name: "no lease", extra: []string{"--preview-auth-ledger-lease", ""},
			wantErr: "--preview-auth-ledger-lease is required"},
		{name: "issuer with a path", extra: []string{"--preview-auth-issuer", "https://auth.example.com/relay"},
			wantErr: "is not an https origin with no path"},
		{name: "http issuer", extra: []string{"--preview-auth-issuer", "http://auth.example.com"},
			wantErr: "is not an https origin"},
		{name: "single-label suffix", extra: []string{"--preview-auth-host-suffix", "localhost"},
			wantErr: "host suffix"},
		{name: "keys dir missing", extra: []string{"--preview-auth-keys-dir", filepath.Join(dir, "absent")},
			wantErr: "keys:"},
		{name: "signing key not PEM", extra: []string{"--preview-auth-keys-dir", keysDir(t, "garbage")},
			wantErr: "current signing key"},
		{name: "no client secret", extra: []string{"--preview-auth-dex-client-secret-file", ""},
			wantErr: "--preview-auth-dex-client-secret-file is required"},
		{name: "CA file missing", extra: []string{"--preview-auth-dex-ca-file", filepath.Join(dir, "absent")},
			wantErr: "dex CA bundle"},
		{name: "CA file without a certificate", extra: []string{"--preview-auth-dex-ca-file",
			writeFile(t, dir, "ca.pem", "not pem")}, wantErr: "holds no PEM certificate"},
		{name: "http Dex", extra: []string{"--preview-auth-dex-issuer-url", "http://dex.example.com"},
			wantErr: "is not an https URL"},
		{name: "no client id", extra: []string{"--preview-auth-dex-client-id", ""},
			wantErr: "dex client id and client secret are required"},
		{name: "no claim prefixes", extra: []string{"--preview-auth-username-prefix", ""},
			wantErr: "prefix"},
		{name: "unverified email username", extra: []string{"--preview-auth-username-claim", "email"},
			wantErr: "email"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadStatic(serveOpts(t, staticArgs(t, tt.extra...)...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadStatic error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestServeStartupErrors: a static configuration error fails serve before a
// manager is built; a valid one gets as far as the kubeconfig.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	if err := serve(t.Context(), serveOpts(t)); err == nil || !strings.Contains(err.Error(), "--namespace") {
		t.Errorf("serve with no flags = %v, want the namespace error", err)
	}
	args := staticArgs(t, "--kubeconfig", filepath.Join(t.TempDir(), "absent"))
	if err := serve(t.Context(), serveOpts(t, args...)); err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("serve with a missing kubeconfig = %v, want the kubeconfig error", err)
	}
}

func TestListener(t *testing.T) {
	l := listener{srv: &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}}
	if l.NeedLeaderElection() {
		t.Error("the relay listener waits for leader election; every replica must serve")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Start(ctx); err != nil {
		t.Errorf("Start after cancel = %v, want a clean shutdown", err)
	}

	bad := listener{srv: &http.Server{Addr: "127.0.0.1:not-a-port", ReadHeaderTimeout: time.Second}}
	if err := bad.Start(t.Context()); err == nil {
		t.Error("Start on an unbindable address succeeded")
	}
}

// TestSyncWatcher: the relay turns ready once the Preview cache syncs (and
// not before), and an unreachable Dex is a warning, not a failure.
func TestSyncWatcher(t *testing.T) {
	dexSrv := httptest.NewTLSServer(http.NotFoundHandler()) // its certificate is not trusted
	t.Cleanup(dexSrv.Close)
	up, err := dex.New(dex.Config{
		IssuerURL: dexSrv.URL, ClientID: "relay", ClientSecret: "s",
		RedirectURL: "https://auth.example.com" + dex.CallbackPath,
		Claims: claims(serveOpts(t, "--preview-auth-username-prefix", "u:",
			"--preview-auth-groups-prefix", "g:")),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, synced := range []bool{true, false} {
		var logs bytes.Buffer
		var ready atomic.Bool
		w := syncWatcher{
			wait:     func(context.Context) bool { return synced },
			synced:   &ready,
			upstream: up,
			log:      slog.New(slog.NewTextHandler(&logs, nil)),
		}
		if w.NeedLeaderElection() {
			t.Error("the sync watcher waits for leader election")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := w.Start(ctx); err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
		if ready.Load() != synced {
			t.Errorf("cache synced=%v: ready = %v", synced, ready.Load())
		}
		if !strings.Contains(logs.String(), "Dex discovery failed") {
			t.Errorf("logs = %q, want the Dex discovery warning", logs.String())
		}
	}
}

// TestServeInformerError: with a valid configuration the relay builds its
// manager, and an API server it cannot reach fails startup at the Preview
// informer rather than serving sign-ins it cannot check.
func TestServeInformerError(t *testing.T) {
	kc := writeFile(t, t.TempDir(), "kubeconfig", `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
users:
  - name: test
    user:
      token: t
`)
	err := serve(t.Context(), serveOpts(t, staticArgs(t, "--kubeconfig", kc, "--health-addr", "")...))
	if err == nil || !strings.Contains(err.Error(), "preview informer") {
		t.Fatalf("serve against an unreachable API server = %v, want the preview informer error", err)
	}
}
