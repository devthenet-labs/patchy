// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// newRepo starts an in-memory registry and returns a repository in it.
func newRepo(t *testing.T) name.Repository {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := name.NewRepository(u.Host + "/org/app")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// testImage builds a linux image (amd64 unless cf says otherwise) with the
// config and one layer per payload.
func testImage(t *testing.T, cf v1.ConfigFile, layers ...[]byte) v1.Image {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	for _, l := range layers {
		var err error
		if img, err = mutate.AppendLayers(img, static.NewLayer(l, types.OCILayer)); err != nil {
			t.Fatal(err)
		}
	}
	base, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf.RootFS = base.RootFS
	if cf.OS == "" {
		cf.OS = "linux"
	}
	if cf.Architecture == "" {
		cf.Architecture = "amd64"
	}
	if img, err = mutate.ConfigFile(img, &cf); err != nil {
		t.Fatal(err)
	}
	return img
}

// pushed pushes img as repo:v1 and returns the reference a repository would
// declare.
func pushed(t *testing.T, repo name.Repository, img v1.Image) string {
	t.Helper()
	if err := remote.Write(repo.Tag("v1"), img); err != nil {
		t.Fatal(err)
	}
	return repo.String() + ":v1"
}

// statuses renders a report's checks as "name=STATUS" in order.
func statuses(r Report) string {
	out := make([]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		out = append(out, c.Name+"="+string(c.Status))
	}
	return strings.Join(out, " ")
}

// reasonOf is the reason of the named check.
func reasonOf(t *testing.T, r Report, check string) string {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == check {
			return c.Reason
		}
	}
	t.Fatalf("report has no %s check: %+v", check, r.Checks)
	return ""
}

func policy(t *testing.T, entries ...string) *runnerimage.Policy {
	t.Helper()
	p, err := runnerimage.NewPolicy(entries)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestStatic(t *testing.T) {
	good := v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/usr/local/go/bin:/usr/bin:/bin"}}}
	cases := []struct {
		name   string
		cfg    func(t *testing.T, repo name.Repository) StaticConfig
		want   string            // every check's status, in order
		reason map[string]string // check -> substring its reason must carry
	}{
		{
			name: "good image",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				return StaticConfig{Reference: pushed(t, repo, testImage(t, good, []byte("go"))),
					Policy: policy(t, repo.RegistryStr()+"/org/")}
			},
			want: "reference=PASS allowlist=PASS resolve=PASS platform=PASS size=PASS volume=PASS env=PASS " +
				"path=PASS signature=SKIP",
			reason: map[string]string{
				CheckPlatform:  "linux/amd64",
				CheckSize:      "linux/amd64 2 B of compressed layers; the limit is 4.0 GiB",
				CheckPath:      "/patchy/bin:/usr/local/go/bin:/usr/bin:/bin",
				CheckSignature: "allowed only with --repository-image-allow-unsigned",
				CheckAllowlist: "/org/",
			},
		},
		{
			name: "reserved ENV, including a name only the Job reserves",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				img := testImage(t, v1.ConfigFile{Config: v1.Config{
					Env: []string{"PATH=/usr/bin", "ANTHROPIC_BASE_URL=http://evil", "HOME=/root"}}})
				return StaticConfig{Reference: pushed(t, repo, img)}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=PASS size=PASS volume=PASS env=FAIL " +
				"path=PASS signature=SKIP",
			reason: map[string]string{CheckEnv: "`ANTHROPIC_BASE_URL`, `HOME`", CheckAllowlist: "no --allow"},
		},
		{
			name: "VOLUME",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				img := testImage(t, v1.ConfigFile{Config: v1.Config{Volumes: map[string]struct{}{"/data": {}}}})
				return StaticConfig{Reference: pushed(t, repo, img)}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=PASS size=PASS volume=FAIL env=PASS " +
				"path=PASS signature=SKIP",
			reason: map[string]string{CheckVolume: "VOLUME `/data`",
				// No PATH at all gets the runc default, as the kubelet's runtime would.
				CheckPath: "/patchy/bin:" + runnerimage.DefaultPath},
		},
		{
			name: "missing PATH",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				img := testImage(t, v1.ConfigFile{Config: v1.Config{Env: []string{"PATH="}}})
				return StaticConfig{Reference: pushed(t, repo, img)}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=PASS size=PASS volume=PASS env=PASS " +
				"path=FAIL signature=SKIP",
			reason: map[string]string{CheckPath: "has no absolute entries"},
		},
		{
			name: "disallowed registry",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				return StaticConfig{Reference: pushed(t, repo, testImage(t, good)),
					Policy: policy(t, repo.RegistryStr()+"/other/", "ghcr.io/org/")}
			},
			want: "reference=PASS allowlist=FAIL resolve=PASS platform=PASS size=PASS volume=PASS env=PASS " +
				"path=PASS signature=SKIP",
			reason: map[string]string{CheckAllowlist: "is not under an allowlisted registry path"},
		},
		{
			name: "every failure at once",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				img := testImage(t, v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=bin", "PATCHY_X=1"},
					Volumes: map[string]struct{}{"/v": {}}}}, make([]byte, 64))
				return StaticConfig{Reference: pushed(t, repo, img), MaxBytes: 10}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=PASS size=FAIL volume=FAIL env=FAIL " +
				"path=FAIL signature=SKIP",
			reason: map[string]string{CheckSize: "64 bytes of compressed layers; the limit is 10 bytes"},
		},
		{
			name: "not linux",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				img := testImage(t, v1.ConfigFile{OS: "windows", Config: good.Config})
				return StaticConfig{Reference: pushed(t, repo, img)}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=FAIL size=PASS volume=PASS env=PASS " +
				"path=PASS signature=SKIP",
			reason: map[string]string{CheckPlatform: "built for windows/amd64"},
		},
		{
			name: "invalid reference",
			cfg: func(*testing.T, name.Repository) StaticConfig {
				return StaticConfig{Reference: "ghcr.io/Org/app:bad tag"}
			},
			want: "reference=FAIL allowlist=SKIP resolve=SKIP platform=SKIP size=SKIP volume=SKIP env=SKIP " +
				"path=SKIP signature=SKIP",
			reason: map[string]string{CheckReference: "contains whitespace", CheckEnv: "the reference is invalid"},
		},
		{
			name: "not in the registry",
			cfg: func(_ *testing.T, repo name.Repository) StaticConfig {
				return StaticConfig{Reference: repo.String() + ":missing"}
			},
			want: "reference=PASS allowlist=SKIP resolve=FAIL platform=SKIP size=SKIP volume=SKIP env=SKIP " +
				"path=SKIP signature=SKIP",
			reason: map[string]string{CheckResolve: "was not found in the registry"},
		},
		{
			name: "unsigned under the operator's key",
			cfg: func(t *testing.T, repo name.Repository) StaticConfig {
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				return StaticConfig{Reference: pushed(t, repo, testImage(t, good)), PublicKey: &key.PublicKey,
					KeyName: "cosign.pub"}
			},
			want: "reference=PASS allowlist=SKIP resolve=PASS platform=PASS size=PASS volume=PASS env=PASS " +
				"path=PASS signature=FAIL",
			reason: map[string]string{CheckSignature: "carries no signature by the operator's cosign key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			r, err := Static(context.Background(), tc.cfg(t, repo))
			if err != nil {
				t.Fatalf("Static: %v", err)
			}
			if got := statuses(r); got != tc.want {
				t.Errorf("checks = %s\nwant     %s\n%+v", got, tc.want, r.Checks)
			}
			for check, want := range tc.reason {
				if got := reasonOf(t, r, check); !strings.Contains(got, want) {
					t.Errorf("%s reason = %q, want it to mention %q", check, got, want)
				}
			}
			if strings.Contains(tc.want, "resolve=PASS") != (r.Image != "") {
				t.Errorf("Image = %q, want it set exactly when the reference resolved", r.Image)
			}
		})
	}
}

// TestStaticIndex: an index reports each runnable platform with its own
// digest and size, and the pod's search path the platforms agree on.
func TestStaticIndex(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin"}
	amd := testImage(t, v1.ConfigFile{Config: v1.Config{Env: env}}, make([]byte, 3000))
	arm := testImage(t, v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: env}}, make([]byte, 2048))
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, c := range []struct {
		img  v1.Image
		arch string
	}{{amd, "amd64"}, {arm, "arm64"}} {
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: c.img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: c.arch}}})
	}
	repo := newRepo(t)
	if err := remote.WriteIndex(repo.Tag("v1"), idx); err != nil {
		t.Fatal(err)
	}
	d, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}

	r, err := Static(context.Background(), StaticConfig{Reference: repo.String() + ":v1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed() != 0 || !r.Index || r.Image != repo.String()+"@"+d.String() {
		t.Fatalf("report = %+v, want an accepted index pinned to %s", r, d)
	}
	if got := reasonOf(t, r, CheckSize); got != "linux/amd64 2.9 KiB, linux/arm64 2.0 KiB of compressed layers; "+
		"the limit is 4.0 GiB" {
		t.Errorf("size reason = %q", got)
	}
	if len(r.Platforms) != 2 || r.Platforms[0].Platform != "linux/amd64" || r.Platforms[1].CompressedBytes != 2048 {
		t.Errorf("Platforms = %+v", r.Platforms)
	}
	if strings.Join(r.SearchPath, ":") != "/usr/bin:/bin" {
		t.Errorf("SearchPath = %v", r.SearchPath)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 4 << 30: "4.0 GiB", 5 << 40: "5.0 TiB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
