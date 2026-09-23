// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("source-controller", "test", opts)
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

// TestRunnerImagesRejectJobReservedEnv resolves through the resolver the
// flags build: an image ENV may not set a name the agent Job reserves for
// itself (the GitHub tokens the no-GitHub-token invariant keeps out of the
// pod, the other harnesses' credential channels, HOME), not only the
// prefixes and gateway names the resolver knows on its own.
func TestRunnerImagesRejectJobReservedEnv(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ri, err := runnerImages(serveOpts(t, "--repository-images", "--repository-image-registries", u.Host+"/org/",
		"--repository-image-allow-unsigned"))
	if err != nil || ri == nil {
		t.Fatalf("runnerImages = %v, %v", ri, err)
	}
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN", "COPILOT_GITHUB_TOKEN", "OPENAI_API_KEY",
		"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HOME"} {
		t.Run(env, func(t *testing.T) {
			ref, err := name.ParseReference(u.Host + "/org/app:" + strings.ToLower(strings.ReplaceAll(env, "_", "-")))
			if err != nil {
				t.Fatal(err)
			}
			img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64",
				Config: v1.Config{Env: []string{env + "=x", "PATH=/usr/bin"}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := remote.Write(ref, img); err != nil {
				t.Fatal(err)
			}
			declared, err := runnerimage.ParseDeclared(ref.String())
			if err != nil {
				t.Fatal(err)
			}
			_, err = ri.Resolver.Resolve(t.Context(), declared)
			var rej *runnerimage.Rejection
			if !errors.As(err, &rej) || rej.Reason != "ReservedEnv" || !strings.Contains(rej.Message, "`"+env+"`") {
				t.Errorf("Resolve with ENV %s = %v, want a ReservedEnv rejection naming it", env, err)
			}
		})
	}
}
