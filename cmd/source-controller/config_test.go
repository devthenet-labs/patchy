// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/artifact"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInternalUploadToken(t *testing.T) {
	for _, tt := range []struct {
		name    string
		file    func(t *testing.T) string
		want    string
		wantErr string
	}{
		{name: "unset", file: func(*testing.T) string { return "" }},
		{name: "trimmed", file: func(t *testing.T) string { return writeFile(t, "tok", "  s3cret \n") }, want: "s3cret"},
		{
			name:    "missing file",
			file:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErr: "internal-upload-token-file:",
		},
		{
			name:    "blank file",
			file:    func(t *testing.T) string { return writeFile(t, "tok", " \n\t") },
			wantErr: "internal-upload-token-file: file is empty",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			if f := tt.file(t); f != "" {
				args = []string{"--internal-upload-token-file", f}
			}
			got, err := internalUploadToken(serveOpts(t, args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("internalUploadToken = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestArtifactBaseURL(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "default service address", want: "http://patchy-source-controller.patchy.svc.cluster.local:9790"},
		{
			name: "port follows the artifact address",
			args: []string{"--artifact-addr", "0.0.0.0:7000"},
			want: "http://patchy-source-controller.patchy.svc.cluster.local:7000",
		},
		{
			name: "configured wins",
			args: []string{"--artifact-base-url", "http://artifacts.test", "--artifact-addr", "nonsense"},
			want: "http://artifacts.test",
		},
		{name: "bad artifact address", args: []string{"--artifact-addr", "no-port"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := artifactBaseURL(serveOpts(t, tt.args...), "patchy")
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("artifactBaseURL = %q, %v; want %q (err %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestInternalServer: the upload endpoint is off unless an address is set,
// and when a token file is configured the handler refuses an upload
// without that bearer token.
func TestInternalServer(t *testing.T) {
	store, err := artifact.NewStore(t.TempDir(), "http://artifacts.test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := internalServer(serveOpts(t), store)
	if err != nil || srv != nil {
		t.Fatalf("disabled internalServer = %v, %v; want nil, nil", srv, err)
	}

	if _, err := internalServer(serveOpts(t, "--artifact-internal-addr", "127.0.0.1:0",
		"--internal-upload-token-file", filepath.Join(t.TempDir(), "absent")), store); err == nil {
		t.Fatal("internalServer with an unreadable token file succeeded")
	}

	tok := writeFile(t, "tok", "s3cret")
	srv, err = internalServer(serveOpts(t, "--artifact-internal-addr", "127.0.0.1:0",
		"--internal-upload-token-file", tok), store)
	if err != nil || srv == nil {
		t.Fatalf("internalServer = %v, %v", srv, err)
	}
	if srv.Addr != "127.0.0.1:0" || srv.ReadHeaderTimeout == 0 {
		t.Errorf("server addr=%q readHeaderTimeout=%v", srv.Addr, srv.ReadHeaderTimeout)
	}
	digest := strings.Repeat("a", 64)
	for _, tc := range []struct {
		auth string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer s3cret", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodHead, "/internal/blobs/"+digest, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("HEAD with %q = %d, want %d", tc.auth, rec.Code, tc.want)
		}
	}
}

func ecdsaPublicPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func ed25519PublicPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// TestRunnerImagesStartupValidation: the feature is off unless switched on,
// and each policy input that cannot be honoured fails startup naming its
// flag rather than admitting images.
func TestRunnerImagesStartupValidation(t *testing.T) {
	on := []string{"--repository-images", "--repository-image-registries", "ghcr.io/org/"}
	for _, tt := range []struct {
		name    string
		args    func(t *testing.T) []string
		wantNil bool
		wantErr string
	}{
		{name: "off", args: func(*testing.T) []string { return nil }, wantNil: true},
		{
			name:    "no registries",
			args:    func(*testing.T) []string { return []string{"--repository-images", "--repository-image-allow-unsigned"} },
			wantErr: "repository-image-registries:",
		},
		{
			name:    "unsigned without opt-out or key",
			args:    func(*testing.T) []string { return on },
			wantErr: "a cosign public key is required",
		},
		{
			name: "unreadable key file",
			args: func(t *testing.T) []string {
				return append(on, "--repository-image-cosign-key-file", filepath.Join(t.TempDir(), "absent"))
			},
			wantErr: "repository-image-cosign-key-file:",
		},
		{
			name: "not PEM",
			args: func(t *testing.T) []string {
				return append(on, "--repository-image-cosign-key-file", writeFile(t, "key", "garbage"))
			},
			wantErr: "repository-image-cosign-key-file: cosign public key: no PEM block",
		},
		{
			name: "not ECDSA",
			args: func(t *testing.T) []string {
				return append(on, "--repository-image-cosign-key-file", writeFile(t, "key", ed25519PublicPEM(t)))
			},
			wantErr: "is not an ECDSA key",
		},
		{
			name: "signed with a key",
			args: func(t *testing.T) []string {
				return append(on, "--repository-image-cosign-key-file", writeFile(t, "key", ecdsaPublicPEM(t)))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ri, err := runnerImages(serveOpts(t, tt.args(t)...))
			switch {
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("runnerImages error = %v, want %q", err, tt.wantErr)
				}
			case err != nil:
				t.Fatalf("runnerImages: %v", err)
			case tt.wantNil != (ri == nil):
				t.Fatalf("runnerImages = %v, want nil=%v", ri, tt.wantNil)
			case ri != nil && (ri.Resolver == nil || ri.OnReject == ""):
				t.Errorf("runnerImages = %+v, want a resolver and an on-reject policy", ri)
			}
		})
	}
}

// TestServeStartupErrors covers the configuration serve refuses before it
// builds a manager or binds a listener.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	notDir := writeFile(t, "file", "x")
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no namespace", wantErr: "namespace is required"},
		{name: "bad artifact address", args: []string{"--namespace", "p", "--artifact-addr", "no-port"},
			wantErr: "artifact-addr"},
		{name: "artifact dir is a file", args: []string{"--namespace", "p", "--artifact-dir",
			filepath.Join(notDir, "sub")}, wantErr: "artifact dir"},
		{name: "missing kubeconfig", args: []string{"--namespace", "p", "--artifact-dir", t.TempDir(),
			"--kubeconfig", filepath.Join(t.TempDir(), "absent")}, wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestServeNamespaceFromEnv: POD_NAMESPACE stands in for --namespace, so
// startup proceeds to the next check.
func TestServeNamespaceFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "patchy")
	err := serve(t.Context(), serveOpts(t, "--artifact-addr", "no-port"))
	if err == nil || !strings.Contains(err.Error(), "artifact-addr") {
		t.Fatalf("serve error = %v, want the artifact-addr error after the namespace resolved", err)
	}
}

func TestSweepBlobsStopsWithContext(t *testing.T) {
	store, err := artifact.NewStore(t.TempDir(), "http://artifacts.test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- sweepBlobs(ctx, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	if err := <-done; err != nil {
		t.Fatalf("sweepBlobs = %v, want nil on cancellation", err)
	}
}
