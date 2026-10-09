// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ocireg

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// anonymous is a hermetic client: an empty keychain resolves every
// registry to anonymous, never reading the workstation's docker config.
func anonymous() *Client { return New(authn.NewMultiKeychain()) }

func TestSaveTarballRoundTrips(t *testing.T) {
	host := newRegistry(t)
	ctx := context.Background()
	ref := host + "/scan/app:v1"
	want := pushRandom(t, ref)

	path := filepath.Join(t.TempDir(), "image.tar")
	if err := anonymous().SaveTarball(ctx, ref, path); err != nil {
		t.Fatalf("SaveTarball: %v", err)
	}
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		t.Fatalf("read back tarball: %v", err)
	}
	got, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Errorf("tarball image digest = %s, want %s", got, want)
	}
}

func TestExportStreamsFlattenedFilesystem(t *testing.T) {
	host := newRegistry(t)
	ctx := context.Background()
	img, err := crane.Image(map[string][]byte{
		"etc/os-release": []byte("ID=test\n"),
		"app/bin":        []byte("binary"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/fs/app:v1"
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := anonymous().Export(ctx, ref, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(b)
	}
	if files["etc/os-release"] != "ID=test\n" || files["app/bin"] != "binary" {
		t.Errorf("exported files = %v", files)
	}
}

func TestMissingReferenceErrors(t *testing.T) {
	host := newRegistry(t)
	ctx := context.Background()
	c := anonymous()
	missing := host + "/absent/app:v1"

	checks := []struct {
		name string
		call func() error
		want string
	}{
		{"Digest", func() error { _, err := c.Digest(ctx, missing); return err }, "resolve digest of " + missing},
		{"Tags", func() error { _, err := c.Tags(ctx, host+"/absent/app"); return err }, "list tags of"},
		{"Platforms", func() error { _, err := c.Platforms(ctx, missing); return err }, "fetch manifest of"},
		{"Created", func() error { _, _, err := c.Created(ctx, missing); return err }, "fetch config of"},
		{"ConfigMediaType", func() error { _, err := c.ConfigMediaType(ctx, missing); return err }, "fetch manifest of"},
		{"Copy", func() error { return c.Copy(ctx, missing, host+"/dst/app:v1") }, "copy " + missing},
		{"SaveTarball", func() error {
			return c.SaveTarball(ctx, missing, filepath.Join(t.TempDir(), "x.tar"))
		}, "pull " + missing},
		{"Export", func() error { return c.Export(ctx, missing, io.Discard) }, "pull " + missing},
		{"LayerBytes", func() error { _, err := c.LayerBytes(ctx, missing, "x"); return err }, "fetch " + missing},
		{"LayerBytes bad ref", func() error {
			_, err := c.LayerBytes(ctx, "Not A Ref!!", "x")
			return err
		}, "parse Not A Ref!!"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v does not contain %q", err, tc.want)
			}
		})
	}
}

func TestExistsNonNotFoundIsError(t *testing.T) {
	// A malformed reference is not a registry 404: it must surface as an
	// error rather than a silent "absent".
	d, ok, err := anonymous().Exists(context.Background(), "Bad Reference::")
	if err == nil || ok || d != "" {
		t.Errorf("Exists(bad) = (%q, %v, %v), want error", d, ok, err)
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"404", &transport.Error{StatusCode: http.StatusNotFound}, true},
		{"wrapped 404", errors.Join(errors.New("ctx"), &transport.Error{StatusCode: http.StatusNotFound}), true},
		{"401", &transport.Error{StatusCode: http.StatusUnauthorized}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("%s: isNotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlatformsSingleImageIsEmpty(t *testing.T) {
	host := newRegistry(t)
	ref := host + "/single/app:v1"
	pushRandom(t, ref)
	got, err := anonymous().Platforms(context.Background(), ref)
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("single-platform image Platforms = %v, want empty", got)
	}
}

func TestCreatedAbsent(t *testing.T) {
	host := newRegistry(t)
	ref := host + "/undated/app:v1"
	// empty.Image's config carries no creation time.
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("x"), types.DockerLayer))
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	ts, ok, err := anonymous().Created(context.Background(), ref)
	if err != nil || ok || !ts.IsZero() {
		t.Errorf("Created = (%v, %v, %v), want zero, false, nil", ts, ok, err)
	}
}

func TestLayerBytesRejectsAmbiguousLayers(t *testing.T) {
	host := newRegistry(t)
	const mt = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	img, err := mutate.AppendLayers(
		mutate.MediaType(empty.Image, types.OCIManifestSchema1),
		static.NewLayer([]byte("one"), types.MediaType(mt)),
		static.NewLayer([]byte("two"), types.MediaType(mt)),
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/charts/dup:1.0.0"
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	_, err = anonymous().LayerBytes(context.Background(), ref, mt)
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Errorf("LayerBytes = %v, want multiple-layers error", err)
	}
}
