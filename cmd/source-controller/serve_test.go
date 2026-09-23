// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"

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

// TestRunnerImagesOnReject pins --repository-image-on-reject: unset, a
// rejected declaration falls back to the default image rather than parking
// the finding (decision 5 of docs/design/repository-runner-images.md); both
// named policies pass through, from the flag or from the PATCHY_* variable
// the chart renders; anything else fails startup.
func TestRunnerImagesOnReject(t *testing.T) {
	on := []string{"--repository-images", "--repository-image-registries", "ghcr.io/org/",
		"--repository-image-allow-unsigned"}
	for _, tc := range []struct {
		name    string
		args    []string
		env     string
		want    string
		wantErr bool
	}{
		{name: "unset", want: source.OnRejectDefault},
		{name: "default", args: []string{"--repository-image-on-reject", "default"}, want: source.OnRejectDefault},
		{name: "handoff", args: []string{"--repository-image-on-reject", "handoff"}, want: source.OnRejectHandoff},
		{name: "env handoff", env: "handoff", want: source.OnRejectHandoff},
		{name: "unknown", args: []string{"--repository-image-on-reject", "park"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PATCHY_REPOSITORY_IMAGE_ON_REJECT", tc.env)
			}
			ri, err := runnerImages(serveOpts(t, slices.Concat(on, tc.args)...))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "repository-image-on-reject") {
					t.Fatalf("runnerImages = %v, %v; want a repository-image-on-reject error", ri, err)
				}
				return
			}
			if err != nil || ri == nil {
				t.Fatalf("runnerImages = %v, %v", ri, err)
			}
			if ri.OnReject != tc.want {
				t.Errorf("OnReject = %q, want %q", ri.OnReject, tc.want)
			}
		})
	}
}

// chartRegistryPattern reads the pattern the chart's values schema puts on
// each agent.repositoryImages.registries entry.
func chartRegistryPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	const schema = "../../charts/patchy/values.schema.json"
	raw, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("read chart schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Agent struct {
				Properties struct {
					RepositoryImages struct {
						Properties struct {
							Registries struct {
								Items struct {
									Pattern string `json:"pattern"`
								} `json:"items"`
							} `json:"registries"`
						} `json:"properties"`
					} `json:"repositoryImages"`
				} `json:"properties"`
			} `json:"agent"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse chart schema: %v", err)
	}
	pattern := doc.Properties.Agent.Properties.RepositoryImages.Properties.Registries.Items.Pattern
	if pattern == "" {
		t.Fatalf("%s: no registries item pattern found — did the schema shape change?", schema)
	}
	// helm validates schema patterns with Go's regexp (santhosh-tekuri
	// jsonschema's default engine), so this is the same dialect.
	return regexp.MustCompile(pattern)
}

// genRegistryEntry returns a candidate allowlist entry: a host and path
// segments drawn from valid spellings and the near misses NormalizeEntry, or
// the comma split the flag goes through, trips on (a bare host, an empty
// segment, globs, a tag, a digest, whitespace, a comma), or else a short run
// of those pieces in any order.
func genRegistryEntry(r *rand.Rand) string {
	hosts := []string{"ghcr.io", "GHCR.io", "localhost:5000", "us-docker.pkg.dev",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com", "", "localhost:", "h st", "h*st", "a,b", "h@st", "\u00e9"}
	segments := []string{"org", "Team", "app-1", "a.b_c", "x", "..", "", "a b", "o*", "o?", "[ab]", "app:1",
		"app@sha256:abc", "a,b", "\t", "\u00e9"}
	pick := func(pool []string) string {
		// Two in three picks come from the first six, mostly valid, spellings.
		if r.Intn(3) > 0 {
			return pool[r.Intn(6)]
		}
		return pool[r.Intn(len(pool))]
	}
	if r.Intn(4) == 0 {
		var b strings.Builder
		for n := 1 + r.Intn(6); n > 0; n-- {
			b.WriteString(pick(append(append([]string{"/", "/", "//", ","}, hosts...), segments...)))
		}
		return b.String()
	}
	e := pick(hosts)
	for n := r.Intn(4); n > 0; n-- {
		e += "/" + pick(segments)
	}
	if r.Intn(2) == 0 {
		e += "/"
	}
	return e
}

// TestChartRegistryPatternIsSound: the chart comma-joins
// agent.repositoryImages.registries into PATCHY_REPOSITORY_IMAGE_REGISTRIES,
// and runnerImages refuses an entry NormalizeEntry rejects — at startup,
// where under the chart's Recreate strategy it replaces a running
// source-controller with a crash-looping one. So every entry the schema's
// pattern admits, alone or joined with the others, must be one runnerImages
// accepts from the environment exactly as the chart delivers it; and the
// entries operators write must be admitted, or the pattern is merely strict.
func TestChartRegistryPatternIsSound(t *testing.T) {
	re := chartRegistryPattern(t)
	accept := func(registries string) error {
		t.Setenv("PATCHY_REPOSITORY_IMAGE_REGISTRIES", registries)
		_, err := runnerImages(serveOpts(t, "--repository-images", "--repository-image-allow-unsigned"))
		return err
	}

	for _, e := range []string{"ghcr.io/org", "ghcr.io/org/", "ghcr.io/org/team/", "GHCR.IO/Org/Team",
		"index.docker.io/library/", "localhost:5000/team", "123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/",
		"us-docker.pkg.dev/my-project/agent_images.v2/"} {
		if !re.MatchString(e) {
			t.Errorf("pattern %s refuses %q, an entry operators write", re, e)
		}
		if err := accept(e); err != nil {
			t.Errorf("runnerImages refuses %q: %v", e, err)
		}
	}

	var admitted []string
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20260923)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genRegistryEntry(r))
		},
	}
	sound := func(entry string) bool {
		if !re.MatchString(entry) {
			return true
		}
		admitted = append(admitted, entry)
		if err := accept(entry); err != nil {
			t.Logf("pattern admits %q, runnerImages refuses it: %v", entry, err)
			return false
		}
		return true
	}
	if err := quick.Check(sound, cfg); err != nil {
		t.Error(err)
		return
	}
	if len(admitted) < 100 {
		t.Fatalf("only %d generated entries matched the pattern; the property is near-vacuous", len(admitted))
	}
	// The chart's `join ","` of every admitted entry is one flag value.
	if err := accept(strings.Join(admitted, ",")); err != nil {
		t.Errorf("runnerImages refuses the joined admitted entries: %v", err)
	}
}
