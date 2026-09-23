// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build ignore

// gen regenerates the bundle-form signature fixture beside it from a real
// cosign run: it starts an in-process registry, pushes the fixture image,
// signs it with the committed test key (key.pem, imported into cosign's own
// format in a temporary directory) and snapshots the image manifest, the
// referrer manifest cosign attached and every blob into signed/. The
// legacy .sig form is produced by the test package itself, because cosign
// v3 no longer writes it; cosign verify still reads it, which is how that
// form is cross-checked.
//
//	go run ./internal/runnerimage/resolve/testdata/gen.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := "internal/runnerimage/resolve/testdata"
	if _, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}
	keyDir, err := os.MkdirTemp("", "patchy-cosign-key")
	if err != nil {
		return err
	}
	defer os.RemoveAll(keyDir)
	imp := exec.Command("cosign", "import-key-pair", "--key", filepath.Join(dir, "key.pem"),
		"--output-key-prefix", filepath.Join(keyDir, "cosign"))
	imp.Env = append(os.Environ(), "COSIGN_PASSWORD=")
	imp.Stderr = os.Stderr
	if err := imp.Run(); err != nil {
		return fmt.Errorf("cosign import-key-pair: %w", err)
	}
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true),
		registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		return err
	}
	repo, err := name.NewRepository(u.Host + "/patchy/fixture")
	if err != nil {
		return err
	}

	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err = mutate.AppendLayers(img, static.NewLayer([]byte("patchy signature fixture layer\n"), types.OCILayer))
	if err != nil {
		return err
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: "amd64",
		OS:           "linux",
		Config:       v1.Config{Env: []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"}},
		RootFS:       mustRootFS(img),
	})
	if err != nil {
		return err
	}
	tag := repo.Tag("v1")
	if err := remote.Write(tag, img); err != nil {
		return err
	}
	digest, err := img.Digest()
	if err != nil {
		return err
	}
	pinned := repo.Digest(digest.String())
	fmt.Println("image:", pinned)

	cmd := exec.Command("cosign", "sign", "--key", filepath.Join(keyDir, "cosign.key"),
		"--tlog-upload=false", "--use-signing-config=false", "--yes",
		"--allow-http-registry", "--allow-insecure-registry", pinned.String())
	cmd.Env = append(os.Environ(), "COSIGN_PASSWORD=")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cosign sign: %w", err)
	}

	out := filepath.Join(dir, "signed")
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "blobs"), 0o755); err != nil {
		return err
	}
	// Image manifest and its blobs.
	if err := snapshot(repo, pinned, filepath.Join(out, "image.json"), out); err != nil {
		return err
	}
	// Referrers: the bundle artifact(s).
	idx, err := remote.Referrers(pinned)
	if err != nil {
		return fmt.Errorf("referrers: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	for i, d := range im.Manifests {
		fmt.Printf("referrer %d: %s artifactType=%s\n", i, d.Digest, d.ArtifactType)
		path := filepath.Join(out, fmt.Sprintf("referrer-%d.json", i))
		if err := snapshot(repo, repo.Digest(d.Digest.String()), path, out); err != nil {
			return err
		}
	}
	// Every tag the registry now holds, for the record.
	tags, err := remote.List(repo)
	if err != nil {
		return err
	}
	fmt.Println("tags:", tags)
	return nil
}

func mustRootFS(img v1.Image) v1.RootFS {
	cf, err := img.ConfigFile()
	if err != nil {
		panic(err)
	}
	return cf.RootFS
}

// snapshot writes ref's raw manifest to path and every blob it references
// (config and layers) under blobs/<hex>.
func snapshot(repo name.Repository, ref name.Reference, path, out string) error {
	desc, err := remote.Get(ref)
	if err != nil {
		return fmt.Errorf("get %s: %w", ref, err)
	}
	if err := os.WriteFile(path, desc.Manifest, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %s %s (%d bytes)\n", filepath.Base(path), desc.Digest, desc.MediaType, len(desc.Manifest))
	var m struct {
		Config v1.Descriptor   `json:"config"`
		Layers []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return err
	}
	for _, d := range append([]v1.Descriptor{m.Config}, m.Layers...) {
		if d.Digest.Hex == "" {
			continue
		}
		l, err := remote.Layer(repo.Digest(d.Digest.String()))
		if err != nil {
			return err
		}
		rc, err := l.Compressed()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "blobs", d.Digest.Hex), data, 0o644); err != nil {
			return err
		}
		fmt.Printf("  blob %s %s (%d bytes)\n", d.Digest, d.MediaType, len(data))
	}
	return nil
}
