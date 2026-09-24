// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// runnerImageRepository is where the application repository's runner image
// is published in the e2e registry; source-controller's allowlist is
// runnerImageOrg, its parent.
const (
	runnerImageOrg        = "org"
	runnerImageRepository = runnerImageOrg + "/app"
)

// publishRunnerImage starts an in-memory OCI registry (the one the
// resolver's own tests use) and publishes an application repository's
// runner image in it: one linux/amd64 image with a PATH and nothing the
// resolver refuses. It returns the registry's host and the image's tag
// reference, which the repository's .patchy/agent.yaml declares, so
// source-controller resolves, checks and pins it exactly as it would a real
// one. The registry is on loopback, which go-containerregistry reaches over
// plain http.
func publishRunnerImage(t *testing.T) (host, tagRef string) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host = u.Host
	tagRef = host + "/" + runnerImageRepository + ":v1"

	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	if img, err = mutate.AppendLayers(img, static.NewLayer([]byte("the app's toolchain"), types.OCILayer)); err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	cf.Config = v1.Config{Env: []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(tagRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("publish the runner image: %v", err)
	}
	return host, tagRef
}
