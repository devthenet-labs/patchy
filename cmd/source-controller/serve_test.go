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
	"github.com/bitwise-media-group/patchy/internal/controller/source"
	"github.com/bitwise-media-group/patchy/internal/jobs"
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
// flags build: an image ENV may not set ANY name the agent Job reserves for
// itself (jobs.ReservedEnvNames: credential channels, PATCHY_* handoff,
// gateway and proxy names, git's repository redirections, HOME). It
// iterates the Job builder's live list, so a newly reserved name is covered
// without editing this test.
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
	names := jobs.ReservedEnvNames()
	if len(names) == 0 {
		t.Fatal("jobs.ReservedEnvNames is empty")
	}
	for _, env := range names {
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

// TestRunnerImagesOnRejectFlag: a rejected declaration falls back to the
// default image unless the operator asks for a hand-off, and a policy that
// is neither fails startup rather than silently picking one.
func TestRunnerImagesOnRejectFlag(t *testing.T) {
	base := []string{"--repository-images", "--repository-image-registries", "ghcr.io/acme/",
		"--repository-image-allow-unsigned"}
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"unset means default", nil, source.OnRejectDefault, false},
		{"explicit default", []string{"--repository-image-on-reject", "default"}, source.OnRejectDefault, false},
		{"explicit handoff", []string{"--repository-image-on-reject", "handoff"}, source.OnRejectHandoff, false},
		{"unknown policy", []string{"--repository-image-on-reject", "park"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ri, err := runnerImages(serveOpts(t, append(append([]string{}, base...), tc.args...)...))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "repository-image-on-reject") {
					t.Fatalf("runnerImages = %+v, %v; want a repository-image-on-reject error", ri, err)
				}
				return
			}
			if err != nil || ri == nil {
				t.Fatalf("runnerImages = %+v, %v", ri, err)
			}
			if ri.OnReject != tc.want {
				t.Errorf("OnReject = %q, want %q", ri.OnReject, tc.want)
			}
		})
	}
}
