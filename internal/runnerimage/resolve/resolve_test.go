// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// counter records the registry traffic a test wants to assert on.
type counter struct {
	mu        sync.Mutex
	heads     int            // HEAD manifests/<ref>
	gets      int            // GET manifests/<ref>
	blobs     map[string]int // GET blobs/<digest> by digest
	onHead    func()         // runs after the first HEAD is served
	headFired bool
}

var (
	manifestPath = regexp.MustCompile(`^/v2/.+/manifests/[^/]+$`)
	blobPath     = regexp.MustCompile(`^/v2/.+/blobs/(sha256:[0-9a-f]{64})$`)
)

func (c *counter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		c.mu.Lock()
		fire := false
		switch {
		case r.Method == http.MethodHead && manifestPath.MatchString(r.URL.Path):
			c.heads++
			fire = !c.headFired
			c.headFired = true
		case r.Method == http.MethodGet && manifestPath.MatchString(r.URL.Path):
			c.gets++
		case r.Method == http.MethodGet && blobPath.MatchString(r.URL.Path):
			if c.blobs == nil {
				c.blobs = map[string]int{}
			}
			c.blobs[blobPath.FindStringSubmatch(r.URL.Path)[1]]++
		}
		c.mu.Unlock()
		// Outside the lock and without a Once: the hook talks to this same
		// server, and its own HEADs must not wait on this one.
		if fire && c.onHead != nil {
			c.onHead()
		}
	})
}

// reset forgets the traffic so far (the pushes that set a test up also HEAD
// and GET manifests) and arms the HEAD hook.
func (c *counter) reset(onHead func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heads, c.gets, c.blobs = 0, 0, nil
	c.onHead, c.headFired = onHead, false
}

// headCount reads the HEAD count under the lock.
func (c *counter) headCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heads
}

// newRegistry starts an in-memory registry, optionally wrapped, and returns
// a repository in it.
func newRegistry(t *testing.T, referrers bool, wrap func(http.Handler) http.Handler) name.Repository {
	t.Helper()
	h := registry.New(registry.WithReferrersSupport(referrers),
		registry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
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

// image builds an OCI image with the config and one static layer per
// payload; OS/arch default to linux/amd64.
func image(t *testing.T, cf *v1.ConfigFile, layers ...[]byte) v1.Image {
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
	if cf == nil {
		cf = &v1.ConfigFile{}
	}
	cf = cf.DeepCopy()
	cf.RootFS = base.RootFS
	if cf.OS == "" {
		cf.OS = "linux"
	}
	if cf.Architecture == "" {
		cf.Architecture = "amd64"
	}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return img
}

func push(t *testing.T, ref name.Reference, img v1.Image) v1.Hash {
	t.Helper()
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// indexChild is one entry of a pushed index.
type indexChild struct {
	img      v1.Image
	platform v1.Platform
}

func pushIndex(t *testing.T, ref name.Reference, children ...indexChild) v1.Hash {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, c := range children {
		p := c.platform
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: c.img, Descriptor: v1.Descriptor{Platform: &p}})
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("push index %s: %v", ref, err)
	}
	d, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func linux(arch string) v1.Platform { return v1.Platform{OS: "linux", Architecture: arch} }

// declared parses a reference the way the reconciler does.
func declared(t *testing.T, s string) imageref.Ref {
	t.Helper()
	ref, err := runnerimage.ParseDeclared(s)
	if err != nil {
		t.Fatalf("ParseDeclared(%s): %v", s, err)
	}
	return ref
}

func newResolver(t *testing.T, cfg Config) *Resolver {
	t.Helper()
	if cfg.PublicKey == nil {
		cfg.AllowUnsigned = true
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// One attempt: the transient-failure tests must not wait out ggcr's
	// exponential backoff.
	r.backoff = &remote.Backoff{Steps: 1}
	return r
}

// rejection asserts err is a Rejection with the reason and a message
// containing want.
func rejection(t *testing.T, err error, reason, want string) {
	t.Helper()
	var rej *runnerimage.Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("error = %v, want a *Rejection with reason %s", err, reason)
	}
	if rej.Reason != reason || !strings.Contains(rej.Message, want) {
		t.Errorf("rejection = %s %q, want reason %s containing %q", rej.Reason, rej.Message, reason, want)
	}
}

func TestNewRequiresKeyOrAllowUnsigned(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with neither key nor AllowUnsigned: want error")
	}
	if _, err := New(Config{AllowUnsigned: true}); err != nil {
		t.Errorf("New with AllowUnsigned: %v", err)
	}
}

// TestResolvePinsTagBeforeItMoves flips the tag between the HEAD that
// resolves it and the manifest fetch: what is recorded and checked must be
// the object the HEAD saw.
func TestResolvePinsTagBeforeItMoves(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	a := image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/a/bin"}}}, []byte("a"))
	b := image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/b/bin"}}}, []byte("b"))
	digestA := push(t, repo.Tag("v1"), a)
	pushHeads := 0 // the HEADs the moving push itself makes
	c.reset(func() {
		// The tag moves the instant it has been resolved.
		before := c.headCount()
		if err := remote.Write(repo.Tag("v1"), b); err != nil {
			panic(err)
		}
		pushHeads = c.headCount() - before
	})

	r := newResolver(t, Config{})
	got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := repo.String() + "@" + digestA.String(); got.Image != want {
		t.Errorf("Image = %s, want the pre-move object %s", got.Image, want)
	}
	if strings.Join(got.SearchPath, ":") != "/a/bin" {
		t.Errorf("SearchPath = %v, want the pre-move config's /a/bin", got.SearchPath)
	}
	if got := c.headCount() - pushHeads; got != 1 {
		t.Errorf("resolver HEAD count = %d, want exactly 1", got)
	}
	if moved, err := remote.Head(repo.Tag("v1")); err != nil || moved.Digest == digestA {
		t.Fatalf("test setup: tag did not move (%v, %v)", moved, err)
	}
}

func TestResolveDigestPinSkipsHead(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	d := push(t, repo.Tag("v1"), image(t, nil, []byte("a")))
	c.reset(nil)

	r := newResolver(t, Config{})
	got, err := r.Resolve(context.Background(), declared(t, repo.String()+"@"+d.String()))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Image != repo.String()+"@"+d.String() || c.heads != 0 {
		t.Errorf("Image = %s, HEADs = %d; want the pin and no HEAD", got.Image, c.heads)
	}
	if strings.Join(got.SearchPath, ":") != runnerimage.DefaultPath {
		t.Errorf("SearchPath = %v, want the runc default for a config without PATH", got.SearchPath)
	}
	if got.Verified {
		t.Error("Verified = true with AllowUnsigned")
	}
}

func TestResolveIndex(t *testing.T) {
	env := []string{"PATH=/usr/local/go/bin:/usr/bin"}
	amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: env}}, []byte("amd"))
	arm := image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: env}}, []byte("arm"))
	other := image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: []string{"PATH=/other"}}}, []byte("o"))
	win := image(t, &v1.ConfigFile{OS: "windows", Config: v1.Config{Env: []string{"PATH=C:\\x"}}}, []byte("w"))
	att := image(t, &v1.ConfigFile{OS: "unknown", Architecture: "unknown"}, []byte("attestation"))
	windows := v1.Platform{OS: "windows", Architecture: "amd64"}
	unknown := v1.Platform{OS: "unknown", Architecture: "unknown"}

	cases := []struct {
		name     string
		children []indexChild
		reason   string
		want     string
	}{
		{"both runnable platforms pass with attestation and windows ignored",
			[]indexChild{{amd, linux("amd64")}, {arm, linux("arm64")}, {att, unknown}, {win, windows}}, "", ""},
		{"one runnable platform is enough", []indexChild{{amd, linux("amd64")}, {win, windows}}, "", ""},
		{"children must agree on PATH", []indexChild{{amd, linux("amd64")}, {other, linux("arm64")}},
			"PathMismatch", "different PATH per platform"},
		{"no runnable platform", []indexChild{{win, windows}, {att, unknown}},
			"UnsupportedPlatform", "no linux/amd64 or linux/arm64 manifest"},
		{"index child whose config disagrees with the index", []indexChild{{win, linux("amd64")}},
			"UnsupportedPlatform", "built for windows/amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &counter{}
			repo := newRegistry(t, false, c.wrap)
			d := pushIndex(t, repo.Tag("v1"), tc.children...)
			c.reset(nil)
			r := newResolver(t, Config{})
			got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if tc.reason != "" {
				rejection(t, err, tc.reason, tc.want)
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Image != repo.String()+"@"+d.String() {
				t.Errorf("Image = %s, want the index digest %s", got.Image, d)
			}
			if strings.Join(got.SearchPath, ":") != "/usr/local/go/bin:/usr/bin" {
				t.Errorf("SearchPath = %v", got.SearchPath)
			}
			// Only runnable children's configs were read.
			for _, ch := range tc.children {
				cn, err := ch.img.ConfigName()
				if err != nil {
					t.Fatal(err)
				}
				fetched := c.blobs[cn.String()] > 0
				if fetched != runnablePlatform(&ch.platform) {
					t.Errorf("config %s (%s/%s) fetched = %v", cn, ch.platform.OS, ch.platform.Architecture, fetched)
				}
			}
		})
	}
}

// TestCheckedConfigsEqualRunnableChildren is the seeded property behind index
// enumeration: for any mix of platforms in an index, the configs the
// resolver reads are exactly those of the linux/amd64 and linux/arm64
// children, and the index is accepted exactly when there is at least one.
func TestCheckedConfigsEqualRunnableChildren(t *testing.T) {
	pool := []v1.Platform{
		linux("amd64"), linux("arm64"), linux("386"), {OS: "linux", Architecture: "arm", Variant: "v7"},
		{OS: "windows", Architecture: "amd64"}, {OS: "unknown", Architecture: "unknown"},
		{OS: "darwin", Architecture: "arm64"}, linux("s390x"),
	}
	property := func(mask uint8) bool {
		c := &counter{}
		repo := newRegistry(t, false, c.wrap)
		var children []indexChild
		want := map[string]bool{}
		for i, p := range pool {
			if mask&(1<<i) == 0 {
				continue
			}
			img := image(t, &v1.ConfigFile{OS: p.OS, Architecture: p.Architecture, Config: v1.Config{
				Env: []string{"PATH=/usr/bin"}, Labels: map[string]string{"child": fmt.Sprint(i)},
			}}, []byte(fmt.Sprint(i)))
			children = append(children, indexChild{img, p})
			if runnablePlatform(&p) {
				cn, err := img.ConfigName()
				if err != nil {
					t.Fatal(err)
				}
				want[cn.String()] = true
			}
		}
		if len(children) == 0 {
			return true
		}
		pushIndex(t, repo.Tag("v1"), children...)
		c.reset(nil)
		r := newResolver(t, Config{})
		_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
		got := map[string]bool{}
		for d := range c.blobs {
			got[d] = true
		}
		if !maps.Equal(got, want) {
			t.Logf("mask %08b: fetched %v, want %v", mask, got, want)
			return false
		}
		if (err == nil) != (len(want) > 0) {
			t.Logf("mask %08b: err = %v with %d runnable children", mask, err, len(want))
			return false
		}
		return true
	}
	cfg := &quick.Config{MaxCount: 48, Rand: mrand.New(mrand.NewSource(20260922))}
	if err := quick.Check(property, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestResolveNestedIndexRejected(t *testing.T) {
	repo := newRegistry(t, false, nil)
	inner := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	inner = mutate.AppendManifests(inner, mutate.IndexAddendum{Add: image(t, nil, []byte("x")),
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	outer := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	outer = mutate.AppendManifests(outer, mutate.IndexAddendum{Add: inner,
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	if err := remote.WriteIndex(repo.Tag("v1"), outer); err != nil {
		t.Fatal(err)
	}
	r := newResolver(t, Config{})
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Unsupported", "nests a")
}

func TestResolveRejections(t *testing.T) {
	big := make([]byte, 600)
	cases := []struct {
		name   string
		cfg    Config
		image  func(t *testing.T) v1.Image
		reason string
		want   string
	}{
		{"oversized", Config{MaxBytes: 1000},
			func(t *testing.T) v1.Image { return image(t, nil, big, big) },
			"Oversized", "1200 bytes of compressed layers; the limit is 1000 bytes"},
		{"reserved env", Config{},
			func(t *testing.T) v1.Image {
				return image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"ANTHROPIC_API_KEY=x", "PATH=/bin"}}})
			},
			"ReservedEnv", "`ANTHROPIC_API_KEY`"},
		{"injected reserved env", Config{ReservedEnv: map[string]bool{"PATCHY_BIN_DIR": true, "TOOLS_IMAGE": true}},
			func(t *testing.T) v1.Image {
				return image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"TOOLS_IMAGE=x", "PATH=/bin"}}})
			},
			"ReservedEnv", "`TOOLS_IMAGE`"},
		{"volume", Config{},
			func(t *testing.T) v1.Image {
				return image(t, &v1.ConfigFile{Config: v1.Config{Volumes: map[string]struct{}{"/data": {}}}})
			},
			"Volume", "VOLUME `/data`"},
		{"relative-only PATH", Config{},
			func(t *testing.T) v1.Image {
				return image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=bin:."}}})
			},
			"EmptyPath", "no absolute entries"},
		{"non-linux single manifest", Config{},
			func(t *testing.T) v1.Image { return image(t, &v1.ConfigFile{OS: "darwin", Architecture: "arm64"}) },
			"UnsupportedPlatform", "built for darwin/arm64"},
		{"unsupported architecture", Config{},
			func(t *testing.T) v1.Image { return image(t, &v1.ConfigFile{Architecture: "s390x"}) },
			"UnsupportedPlatform", "built for linux/s390x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, false, nil)
			push(t, repo.Tag("v1"), tc.image(t))
			r := newResolver(t, tc.cfg)
			_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			rejection(t, err, tc.reason, tc.want)
		})
	}
}

// deny answers every manifest request under the repository path with the
// status, leaving the ping alone, so the resolver sees what a registry that
// refuses the pull returns.
func deny(status int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if manifestPath.MatchString(r.URL.Path) {
				w.WriteHeader(status)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func TestResolveRegistryStatus(t *testing.T) {
	cases := []struct {
		name   string
		wrap   func(http.Handler) http.Handler
		ref    string
		reason string
		want   string
	}{
		{"401 is a deterministic access rejection", deny(http.StatusUnauthorized), ":v1", "AccessDenied",
			"registry denied access to `"},
		{"403 is a deterministic access rejection", deny(http.StatusForbidden), ":v1", "AccessDenied",
			"configure pullSecret or a cloud credential"},
		{"missing tag", nil, ":missing", "NotFound", "was not found in the registry"},
		{"missing digest pin", nil, "@sha256:" + strings.Repeat("0", 64), "NotFound", "was not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, false, tc.wrap)
			r := newResolver(t, Config{})
			_, err := r.Resolve(context.Background(), declared(t, repo.String()+tc.ref))
			rejection(t, err, tc.reason, tc.want)
		})
	}
}

func TestResolveTransientErrorIsNotRejected(t *testing.T) {
	repo := newRegistry(t, false, deny(http.StatusBadGateway))
	r := newResolver(t, Config{})
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	if err == nil || runnerimage.IsRejection(err) {
		t.Errorf("Resolve on 502 = %v, want a transient (non-Rejection) error", err)
	}
}

func TestResolveCache(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	push(t, repo.Tag("v1"), image(t, nil, []byte("a")))
	c.reset(nil)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r := newResolver(t, Config{CacheTTL: 10 * time.Minute, Now: func() time.Time { return now }})
	ref := declared(t, repo.String()+":v1")

	first, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	gets := c.gets
	second, err := r.Resolve(context.Background(), ref)
	if err != nil || second.Image != first.Image {
		t.Fatalf("second Resolve = %+v, %v", second, err)
	}
	if c.heads != 2 {
		t.Errorf("HEADs = %d, want 2: the tag is resolved every time", c.heads)
	}
	if c.gets != gets {
		t.Errorf("manifest GETs grew from %d to %d on a cached digest", gets, c.gets)
	}

	now = now.Add(11 * time.Minute)
	if _, err := r.Resolve(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if c.gets == gets {
		t.Error("manifest GETs did not grow after the cache TTL elapsed")
	}
}

func TestResolveCachesRejectionsNotTransients(t *testing.T) {
	var fail bool
	flaky := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail && r.Method == http.MethodGet && manifestPath.MatchString(r.URL.Path) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	repo := newRegistry(t, false, flaky)
	push(t, repo.Tag("v1"), image(t, &v1.ConfigFile{Config: v1.Config{Volumes: map[string]struct{}{"/v": {}}}}))
	push(t, repo.Tag("ok"), image(t, nil, []byte("ok")))
	r := newResolver(t, Config{})

	// A transient failure is retried on the next call.
	fail = true
	if _, err := r.Resolve(context.Background(), declared(t, repo.String()+":ok")); err == nil {
		t.Fatal("Resolve under 503: want error")
	}
	fail = false
	if _, err := r.Resolve(context.Background(), declared(t, repo.String()+":ok")); err != nil {
		t.Errorf("Resolve after the outage: %v, want success (transient verdicts are not cached)", err)
	}
	// A rejection is served from the cache even when the registry is down.
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Volume", "/v")
	fail = true
	_, err = r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Volume", "/v")
}

// --- signatures -----------------------------------------------------------

// testKeys loads the committed pair: key.pem (PKCS#8, the private half the
// legacy fixture is signed with at test time) and cosign.pub.
func testKeys(t *testing.T) (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("key.pem is %T", key)
	}
	pubPEM, err := os.ReadFile(filepath.Join("testdata", "cosign.pub"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.PublicKey.Equal(pub) {
		t.Fatal("key.pem and cosign.pub are not a pair")
	}
	return priv, pub
}

// rawManifest uploads manifest bytes verbatim, so a fixture keeps its digest.
type rawManifest struct {
	raw []byte
	mt  types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error)        { return m.raw, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return m.mt, nil }

// pushFixture replays testdata/signed (the image and the referrer cosign v3
// attached to it) into repo and returns the image digest.
func pushFixture(t *testing.T, repo name.Repository) v1.Hash {
	t.Helper()
	dir := filepath.Join("testdata", "signed")
	blobs, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blobs {
		data, err := os.ReadFile(filepath.Join(dir, "blobs", b.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.WriteLayer(repo, static.NewLayer(data, types.OCILayer)); err != nil {
			t.Fatalf("upload blob %s: %v", b.Name(), err)
		}
	}
	imgRaw, err := os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := v1.SHA256(strings.NewReader(string(imgRaw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []name.Reference{repo.Digest(digest.String()), repo.Tag("v1")} {
		if err := remote.Put(ref, rawManifest{imgRaw, types.OCIManifestSchema1}); err != nil {
			t.Fatalf("put image at %s: %v", ref, err)
		}
	}
	refRaw, err := os.ReadFile(filepath.Join(dir, "referrer-0.json"))
	if err != nil {
		t.Fatal(err)
	}
	refDigest, _, err := v1.SHA256(strings.NewReader(string(refRaw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(repo.Digest(refDigest.String()), rawManifest{refRaw, types.OCIManifestSchema1}); err != nil {
		t.Fatalf("put referrer: %v", err)
	}
	return digest
}

// subjectOf reads the descriptor a referrer must name.
func subjectOf(t *testing.T, ref name.Reference) v1.Descriptor {
	t.Helper()
	desc, err := remote.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	return desc.Descriptor
}

// pushReferrer attaches blob to subject as an OCI artifact of artifactType.
func pushReferrer(t *testing.T, repo name.Repository, subject v1.Descriptor, artifactType string, blob []byte) {
	t.Helper()
	emptyBlob := []byte("{}")
	for _, b := range [][]byte{emptyBlob, blob} {
		if err := remote.WriteLayer(repo, static.NewLayer(b, types.OCILayer)); err != nil {
			t.Fatal(err)
		}
	}
	desc := func(data []byte, mt string) v1.Descriptor {
		h, _, err := v1.SHA256(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		return v1.Descriptor{MediaType: types.MediaType(mt), Size: int64(len(data)), Digest: h}
	}
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"artifactType":  artifactType,
		"config":        desc(emptyBlob, "application/vnd.oci.empty.v1+json"),
		"layers":        []v1.Descriptor{desc(blob, artifactType)},
		"subject":       subject,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := v1.SHA256(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(repo.Digest(h.String()), rawManifest{raw, types.OCIManifestSchema1}); err != nil {
		t.Fatalf("put referrer: %v", err)
	}
}

// signLegacy writes the sha256-<digest>.sig tag the way cosign v2 did: one
// simple-signing payload layer, its ECDSA signature in the layer annotation.
func signLegacy(t *testing.T, priv *ecdsa.PrivateKey, repo name.Repository, digest v1.Hash) {
	t.Helper()
	payload := fmt.Appendf(nil, `{"critical":{"identity":{"docker-reference":"%s"},`+
		`"image":{"docker-manifest-digest":"%s"},"type":"cosign container image signature"},"optional":null}`,
		repo, digest)
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err = mutate.Append(img, mutate.Addendum{
		Layer:       static.NewLayer(payload, "application/vnd.dev.cosign.simplesigning.v1+json"),
		Annotations: map[string]string{legacySignatureAnnotation: base64.StdEncoding.EncodeToString(sig)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Tag("sha256-"+digest.Hex+".sig"), img); err != nil {
		t.Fatal(err)
	}
}

// messageSignatureBundle builds the bundle v0.3 message-signature form over
// the digest.
func messageSignatureBundle(t *testing.T, priv *ecdsa.PrivateKey, digest v1.Hash) []byte {
	t.Helper()
	raw, err := hex.DecodeString(digest.Hex)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := ecdsa.SignASN1(rand.Reader, priv, raw)
	if err != nil {
		t.Fatal(err)
	}
	b := map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": "test"}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": raw},
			"signature":     sig,
		},
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestResolveVerifiesCosignBundle(t *testing.T) {
	_, pub := testKeys(t)
	for _, referrers := range []bool{true, false} {
		t.Run(fmt.Sprintf("referrers API %v", referrers), func(t *testing.T) {
			repo := newRegistry(t, referrers, nil)
			digest := pushFixture(t, repo)
			r := newResolver(t, Config{PublicKey: pub})
			got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !got.Verified || got.Image != repo.String()+"@"+digest.String() {
				t.Errorf("Resolved = %+v, want Verified on %s", got, digest)
			}
			if strings.Join(got.SearchPath, ":") != "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin" {
				t.Errorf("SearchPath = %v", got.SearchPath)
			}
		})
	}
}

func TestResolveVerifiesLegacySignature(t *testing.T) {
	priv, pub := testKeys(t)
	repo := newRegistry(t, true, nil)
	digest := push(t, repo.Tag("v1"), image(t, nil, []byte("legacy")))
	signLegacy(t, priv, repo, digest)

	r := newResolver(t, Config{PublicKey: pub})
	got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Verified {
		t.Errorf("Verified = false for a legacy .sig by the operator key")
	}
}

func TestResolveVerifiesMessageSignatureBundle(t *testing.T) {
	priv, pub := testKeys(t)
	repo := newRegistry(t, true, nil)
	digest := push(t, repo.Tag("v1"), image(t, nil, []byte("ms")))
	pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), "application/vnd.dev.sigstore.bundle.v0.3+json",
		messageSignatureBundle(t, priv, digest))

	r := newResolver(t, Config{PublicKey: pub})
	got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Verified {
		t.Error("Verified = false for a message-signature bundle by the operator key")
	}
}

func TestResolveSignatureRejections(t *testing.T) {
	priv, pub := testKeys(t)
	other, err := ecdsa.GenerateKey(pub.Curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		setup  func(t *testing.T, repo name.Repository) v1.Hash
		key    *ecdsa.PublicKey
		reason string
		want   string
	}{
		{"unsigned", func(t *testing.T, repo name.Repository) v1.Hash {
			return push(t, repo.Tag("v1"), image(t, nil, []byte("u")))
		}, pub, "Unsigned", "carries no signature"},
		{"bundle by another key", func(t *testing.T, repo name.Repository) v1.Hash {
			return pushFixture(t, repo)
		}, &other.PublicKey, "SignatureInvalid", "none of which verifies"},
		{"legacy by another key", func(t *testing.T, repo name.Repository) v1.Hash {
			d := push(t, repo.Tag("v1"), image(t, nil, []byte("l")))
			signLegacy(t, other, repo, d)
			return d
		}, pub, "SignatureInvalid", "carries 1 signature(s)"},
		{"bundle over a different digest", func(t *testing.T, repo name.Repository) v1.Hash {
			d := push(t, repo.Tag("v1"), image(t, nil, []byte("m")))
			wrong, err := v1.NewHash("sha256:" + strings.Repeat("ab", 32))
			if err != nil {
				t.Fatal(err)
			}
			pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), "application/vnd.dev.sigstore.bundle.v0.3+json",
				messageSignatureBundle(t, priv, wrong))
			return d
		}, pub, "SignatureInvalid", "none of which verifies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, true, nil)
			tc.setup(t, repo)
			r := newResolver(t, Config{PublicKey: tc.key})
			_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			rejection(t, err, tc.reason, tc.want)
		})
	}
}

// TestVerifyBundleUnits pins the bundle parser on the cosign v3 fixture
// without a registry: the DSSE envelope, its subject and its predicate.
func TestVerifyBundleUnits(t *testing.T) {
	_, pub := testKeys(t)
	imgRaw, err := os.ReadFile(filepath.Join("testdata", "signed", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := v1.SHA256(strings.NewReader(string(imgRaw)))
	if err != nil {
		t.Fatal(err)
	}
	var fixture []byte
	blobs, _ := os.ReadDir(filepath.Join("testdata", "signed", "blobs"))
	for _, b := range blobs {
		data, err := os.ReadFile(filepath.Join("testdata", "signed", "blobs", b.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "dsseEnvelope") {
			fixture = data
		}
	}
	if fixture == nil {
		t.Fatal("no bundle blob in testdata/signed/blobs")
	}
	if err := verifyBundle(fixture, pub, digest); err != nil {
		t.Errorf("fixture bundle: %v", err)
	}
	other, err := v1.NewHash("sha256:" + strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBundle(fixture, pub, other); err == nil {
		t.Error("fixture bundle against another digest: want error")
	}
	// An attestation (any other predicate) signed by the key is not an image
	// signature.
	var b map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &b); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload     []byte `json:"payload"`
		PayloadType string `json:"payloadType"`
	}
	if err := json.Unmarshal(b["dsseEnvelope"], &env); err != nil {
		t.Fatal(err)
	}
	if err := statementNames(env.Payload, digest); err != nil {
		t.Errorf("fixture statement: %v", err)
	}
	attestation := strings.Replace(string(env.Payload), cosignSignPredicate, "https://slsa.dev/provenance/v1", 1)
	if err := statementNames([]byte(attestation), digest); err == nil {
		t.Error("attestation predicate accepted as an image signature")
	}
	if err := verifyBundle([]byte(`{"mediaType":"application/json"}`), pub, digest); err == nil {
		t.Error("non-bundle media type accepted")
	}
}

// TestLegacySignatureVerifiesWithCosign cross-checks the legacy form the
// tests build against the real cosign binary, which still reads it.
func TestLegacySignatureVerifiesWithCosign(t *testing.T) {
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		t.Skip("cosign not on PATH")
	}
	priv, _ := testKeys(t)
	repo := newRegistry(t, true, nil)
	digest := push(t, repo.Tag("v1"), image(t, nil, []byte("legacy")))
	signLegacy(t, priv, repo, digest)

	cmd := exec.CommandContext(t.Context(), cosign, "verify", "--key", filepath.Join("testdata", "cosign.pub"),
		"--insecure-ignore-tlog=true", "--allow-http-registry", "--allow-insecure-registry",
		repo.String()+"@"+digest.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cosign verify: %v\n%s", err, out)
	}
}
