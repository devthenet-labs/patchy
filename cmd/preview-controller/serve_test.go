// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("preview-controller", "test", opts)
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

// validArgs is a settings surface preview.Settings.Validate accepts.
func validArgs(extra ...string) []string {
	return append([]string{
		"--namespace", "patchy",
		"--preview-image-prefix", "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/",
		"--preview-host-suffix", "previews.example.com",
		"--preview-node-pool", "previews", "--preview-node-class", "previews",
		"--preview-taint-key", "patchy.bitwisemedia.uk/preview",
	}, extra...)
}

func TestAuthSettings(t *testing.T) {
	const set = `{"patchy-preview-0":{"alb.ingress.kubernetes.io/auth-type":"oidc"}}`
	for _, tt := range []struct {
		name         string
		args         []string
		wantRequired bool
		wantCurrent  int
		wantPrevious int
		wantErr      string
	}{
		{name: "unset"},
		{
			name: "required with both generations",
			args: []string{"--preview-auth-required", "--preview-auth-annotations", set,
				"--preview-auth-previous-annotations", set},
			wantRequired: true, wantCurrent: 1, wantPrevious: 1,
		},
		{name: "bad current", args: []string{"--preview-auth-annotations", "{"}, wantErr: "preview auth annotations"},
		{
			name:    "trailing data",
			args:    []string{"--preview-auth-annotations", set + " {}"},
			wantErr: "trailing data",
		},
		{
			name:    "bad previous",
			args:    []string{"--preview-auth-annotations", set, "--preview-auth-previous-annotations", "[]"},
			wantErr: "previous generation:",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := authSettings(serveOpts(t, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("authSettings error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("authSettings: %v", err)
			}
			if got.Required != tt.wantRequired || len(got.Annotations) != tt.wantCurrent ||
				len(got.Previous) != tt.wantPrevious {
				t.Errorf("authSettings = %+v, want required=%v current=%d previous=%d",
					got, tt.wantRequired, tt.wantCurrent, tt.wantPrevious)
			}
		})
	}
}

// TestServeStartupErrors: serve refuses an invalid configuration before it
// builds a manager, and reaches the manager (here: a kubeconfig that does
// not exist) only once the settings are valid.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	missing := filepath.Join(t.TempDir(), "absent")
	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "bad annotations", args: validArgs("--preview-auth-annotations", "nope"),
			wantErr: "preview auth annotations"},
		{name: "no image prefix", args: []string{"--namespace", "patchy"}, wantErr: "image prefix"},
		{name: "no namespace", args: validArgs("--namespace", ""), wantErr: "invalid preview-controller settings"},
		{name: "too many retries", args: validArgs("--preview-max-retries", "4"),
			wantErr: "invalid preview-controller settings"},
		{name: "annotations without required", args: validArgs("--preview-auth-annotations",
			`{"patchy-preview-0":{}}`), wantErr: "preview auth is not required"},
		{name: "required without annotations", args: validArgs("--preview-auth-required"),
			wantErr: "no annotations are set"},
		{name: "valid settings reach the manager", args: validArgs("--kubeconfig", missing),
			wantErr: "kubeconfig"},
		{name: "namespace from the pod", env: "patchy", args: validArgs("--namespace", "", "--kubeconfig", missing),
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("POD_NAMESPACE", tt.env)
			}
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
