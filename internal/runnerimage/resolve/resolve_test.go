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

	"github.com/google/go-containerregistry/pkg/authn"
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

// bareOS marks an indexChild listed with no platform at all.
const bareOS = "(no platform)"

// bare is an index entry with no platform.
func bare(img v1.Image) indexChild { return indexChild{img, v1.Platform{OS: bareOS}} }

func pushIndex(t *testing.T, ref name.Reference, children ...indexChild) v1.Hash {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, c := range children {
		desc := v1.Descriptor{}
		if c.platform.OS != bareOS {
			p := c.platform
			desc.Platform = &p
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: c.img, Descriptor: desc})
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
				arch := ch.platform.Architecture
				runnable := ch.platform.OS == "linux" && (arch == "amd64" || arch == "arm64")
				if fetched != runnable {
					t.Errorf("config %s (%s/%s) fetched = %v", cn, ch.platform.OS, ch.platform.Architecture, fetched)
				}
			}
		})
	}
}

// TestResolveIndexChildRejections: every runnable child of an index is
// judged, not only the first, so a clean amd64 child cannot carry an
// oversized, reserved-ENV or VOLUME arm64 child past the checks. Each case
// runs with the children in both orders, and the message names the child's
// platform so the owner knows which image to fix.
func TestResolveIndexChildRejections(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: env}}, []byte("amd"))
	cases := []struct {
		name   string
		arm    v1.Image
		reason string
		detail string
	}{
		{"oversized", image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: env}}, make([]byte, 2000)),
			"Oversized", "2000 bytes of compressed layers"},
		{"reserved env", image(t, &v1.ConfigFile{Architecture: "arm64",
			Config: v1.Config{Env: []string{"PATH=/usr/bin", "ANTHROPIC_BASE_URL=http://evil"}}}, []byte("env")),
			"ReservedEnv", "`ANTHROPIC_BASE_URL`"},
		{"volume", image(t, &v1.ConfigFile{Architecture: "arm64",
			Config: v1.Config{Env: env, Volumes: map[string]struct{}{"/x": {}}}}, []byte("vol")),
			"Volume", "VOLUME `/x`"},
		{"empty path", image(t, &v1.ConfigFile{Architecture: "arm64",
			Config: v1.Config{Env: []string{"PATH=bin"}}}, []byte("path")),
			"EmptyPath", "no absolute entries"},
	}
	for _, tc := range cases {
		for _, armFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/arm64 first %v", tc.name, armFirst), func(t *testing.T) {
				repo := newRegistry(t, false, nil)
				children := []indexChild{{amd, linux("amd64")}, {tc.arm, linux("arm64")}}
				if armFirst {
					children[0], children[1] = children[1], children[0]
				}
				pushIndex(t, repo.Tag("v1"), children...)
				r := newResolver(t, Config{MaxBytes: 1000})
				_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
				rejection(t, err, tc.reason, "linux/arm64")
				rejection(t, err, tc.reason, tc.detail)
			})
		}
	}
}

// poolEntry is one index entry the property can include, with its oracle
// written out by hand from containerd's platform matching (containerd/
// platforms Only + Normalize, images.Manifest), never computed by the code
// under test: checked entries are the ones a linux/amd64 or linux/arm64
// node could run as its own architecture; covers names the architecture
// whose baseline entry containerd prefers over every fallback on that node;
// fallback names the node architecture that falls back to the entry when
// nothing covers it ("any" for an entry with no platform, which a node
// judges by its config when no platform-tagged entry matches).
type poolEntry struct {
	platform v1.Platform
	bare     bool
	checked  bool
	covers   string
	fallback string
}

var indexPool = []poolEntry{
	{platform: linux("amd64"), checked: true, covers: "amd64"},
	{platform: linux("arm64"), checked: true, covers: "arm64"},
	{platform: linux("386"), fallback: "amd64"},
	{platform: v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, fallback: "arm64"},
	{platform: v1.Platform{OS: "windows", Architecture: "amd64"}},
	{platform: v1.Platform{OS: "unknown", Architecture: "unknown"}},
	{platform: v1.Platform{OS: "darwin", Architecture: "arm64"}},
	{platform: linux("s390x")},
	{platform: v1.Platform{OS: "", Architecture: "x86_64"}, checked: true, covers: "amd64"},
	{platform: v1.Platform{OS: "Linux", Architecture: "aarch64"}, checked: true, covers: "arm64"},
	{platform: v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}, checked: true},
	{bare: true, fallback: "any"},
}

// indexPoolProperty is the property behind index enumeration: for the mix
// of indexPool entries mask selects, an index is accepted exactly when it
// has a checked entry and no unchecked entry a node could fall back to, and
// an accepted index had exactly its checked entries' configs read.
func indexPoolProperty(t *testing.T) func(mask uint16) bool {
	return func(mask uint16) bool {
		c := &counter{}
		repo := newRegistry(t, false, c.wrap)
		children, want, accept := poolIndex(t, mask)
		if len(children) == 0 {
			return true
		}
		pushIndex(t, repo.Tag("v1"), children...)
		c.reset(nil)
		r := newResolver(t, Config{})
		_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
		if (err == nil) != accept {
			t.Logf("mask %012b: err = %v, want accepted %v", mask, err, accept)
			return false
		}
		if err != nil && !runnerimage.IsRejection(err) {
			t.Logf("mask %012b: err = %v, want a Rejection", mask, err)
			return false
		}
		got := map[string]bool{}
		for d := range c.blobs {
			got[d] = true
		}
		if accept && !maps.Equal(got, want) {
			t.Logf("mask %012b: fetched %v, want %v", mask, got, want)
			return false
		}
		return true
	}
}

// poolIndex builds the index entries mask selects from indexPool, the
// configs the resolver must read for them, and whether the index must be
// accepted, all from the pool's hand-written oracle columns.
func poolIndex(t *testing.T, mask uint16) (children []indexChild, want map[string]bool, accept bool) {
	t.Helper()
	want = map[string]bool{}
	covered := map[string]bool{}
	var fallbacks []string
	for i, e := range indexPool {
		if mask&(1<<i) == 0 {
			continue
		}
		cfOS, cfArch := e.platform.OS, e.platform.Architecture
		if e.bare {
			cfOS, cfArch = "linux", "amd64"
		}
		img := image(t, &v1.ConfigFile{OS: cfOS, Architecture: cfArch, Config: v1.Config{
			Env: []string{"PATH=/usr/bin"}, Labels: map[string]string{"child": fmt.Sprint(i)},
		}}, []byte(fmt.Sprint(i)))
		child := indexChild{img, e.platform}
		if e.bare {
			child = bare(img)
		}
		children = append(children, child)
		if e.checked {
			cn, err := img.ConfigName()
			if err != nil {
				t.Fatal(err)
			}
			want[cn.String()] = true
		}
		if e.covers != "" {
			covered[e.covers] = true
		}
		if e.fallback != "" {
			fallbacks = append(fallbacks, e.fallback)
		}
	}
	reachable := false
	for _, f := range fallbacks {
		if f == "any" {
			reachable = reachable || !covered["amd64"] || !covered["arm64"]
		} else {
			reachable = reachable || !covered[f]
		}
	}
	return children, want, len(want) > 0 && !reachable
}

// TestCheckedConfigsEqualRunnableChildren runs indexPoolProperty seeded.
func TestCheckedConfigsEqualRunnableChildren(t *testing.T) {
	cfg := &quick.Config{MaxCount: 96, Rand: mrand.New(mrand.NewSource(20260922))}
	if err := quick.Check(indexPoolProperty(t), cfg); err != nil {
		t.Fatal(err)
	}
}

// TestCheckedConfigsCounterexamples pins the masks the property failed on,
// so they survive a seed change: 0xcf04 (linux/386, x86_64, Linux/aarch64,
// amd64/v3 and an entry with no platform) was accepted with only part of
// what containerd could run checked.
func TestCheckedConfigsCounterexamples(t *testing.T) {
	for _, mask := range []uint16{0xcf04} {
		if !indexPoolProperty(t)(mask) {
			t.Errorf("mask %#04x fails the index property", mask)
		}
	}
}

// TestResolveIndexChildrenContainerdCouldRun pins the entries containerd
// would pick that an exact linux/amd64|arm64 match misses: other spellings
// of the architectures (checked like any child), and the fallbacks a node
// takes when nothing names its own architecture (rejected, since they are
// never checked). The unchecked child carries a VOLUME and a reserved ENV,
// so accepting the index would run it unchecked.
func TestResolveIndexChildrenContainerdCouldRun(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	clean := func(arch string) v1.Image {
		return image(t, &v1.ConfigFile{Architecture: arch, Config: v1.Config{Env: env}}, []byte("clean-"+arch))
	}
	evil := image(t, &v1.ConfigFile{Config: v1.Config{
		Env:     []string{"PATH=/usr/bin", "ANTHROPIC_BASE_URL=http://evil"},
		Volumes: map[string]struct{}{"/var/lib/evil": {}},
	}}, []byte("evil"))
	amd, arm := clean("amd64"), clean("arm64")
	cases := []struct {
		name     string
		children []indexChild
		reason   string
		want     string
	}{
		{"x86_64 is amd64", []indexChild{{arm, linux("arm64")}, {evil, v1.Platform{OS: "linux", Architecture: "x86_64"}}},
			"Volume", "/var/lib/evil"},
		{"an empty OS is linux", []indexChild{{arm, linux("arm64")}, {evil, v1.Platform{Architecture: "amd64"}}},
			"Volume", "/var/lib/evil"},
		{"the OS is case-insensitive",
			[]indexChild{{arm, linux("arm64")}, {evil, v1.Platform{OS: "Linux", Architecture: "amd64"}}},
			"Volume", "/var/lib/evil"},
		{"aarch64 is arm64", []indexChild{{amd, linux("amd64")}, {evil, v1.Platform{OS: "linux", Architecture: "aarch64"}}},
			"Volume", "/var/lib/evil"},
		{"386 is an amd64 node's fallback", []indexChild{{arm, linux("arm64")}, {evil, linux("386")}},
			"UnsupportedPlatform", "linux/386"},
		{"arm/v7 is an arm64 node's fallback", []indexChild{{amd, linux("amd64")},
			{evil, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}}},
			"UnsupportedPlatform", "linux/arm/v7"},
		{"a variant-only amd64 entry does not cover the fallback",
			[]indexChild{{amd, v1.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"}}, {arm, linux("arm64")},
				{evil, linux("386")}},
			"UnsupportedPlatform", "linux/386"},
		{"an entry without a platform is judged by its config", []indexChild{{amd, linux("amd64")}, bare(evil)},
			"UnsupportedPlatform", "no platform"},
		{"fallbacks behind both architectures never run",
			[]indexChild{{amd, linux("amd64")}, {arm, linux("arm64")}, {evil, linux("386")},
				{evil, v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}}, bare(evil)},
			"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &counter{}
			repo := newRegistry(t, false, c.wrap)
			pushIndex(t, repo.Tag("v1"), tc.children...)
			c.reset(nil)
			r := newResolver(t, Config{})
			_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			evilConfig, cerr := evil.ConfigName()
			if cerr != nil {
				t.Fatal(cerr)
			}
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if c.blobs[evilConfig.String()] != 0 {
					t.Error("read the config of an entry no node would run")
				}
				return
			}
			rejection(t, err, tc.reason, tc.want)
		})
	}
}

// TestResolveIndexBoundsChildren: every checked child costs a manifest and a
// config fetch on the single Repository worker, so an index repeating one
// child is checked once, and one listing more distinct runnable children
// than any real image needs is refused before any child is fetched.
func TestResolveIndexBoundsChildren(t *testing.T) {
	t.Run("a repeated child is checked once", func(t *testing.T) {
		c := &counter{}
		repo := newRegistry(t, false, c.wrap)
		amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/usr/bin"}}}, []byte("amd"))
		children := make([]indexChild, 200)
		for i := range children {
			children[i] = indexChild{img: amd, platform: linux("amd64")}
		}
		pushIndex(t, repo.Tag("v1"), children...)
		c.reset(nil)
		r := newResolver(t, Config{})
		if _, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		cn, err := amd.ConfigName()
		if err != nil {
			t.Fatal(err)
		}
		if got := c.blobs[cn.String()]; got != 1 {
			t.Errorf("config fetched %d times, want once", got)
		}
	})
	t.Run("too many distinct children", func(t *testing.T) {
		c := &counter{}
		repo := newRegistry(t, false, c.wrap)
		children := make([]indexChild, 0, 20)
		for i := range 20 {
			img := image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/usr/bin"}}}, []byte(fmt.Sprint(i)))
			children = append(children, indexChild{img: img, platform: linux("amd64")})
		}
		pushIndex(t, repo.Tag("v1"), children...)
		c.reset(nil)
		r := newResolver(t, Config{})
		_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
		rejection(t, err, "Unsupported", "20 runnable manifests")
		if len(c.blobs) != 0 {
			t.Errorf("fetched %d blobs before refusing the index", len(c.blobs))
		}
	})
}

// TestResolveNestedIndexUnderAnotherSpelling: a nested index is refused
// wherever containerd could reach it, not only under an exact platform.
func TestResolveNestedIndexUnderAnotherSpelling(t *testing.T) {
	repo := newRegistry(t, false, nil)
	inner := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	inner = mutate.AppendManifests(inner, mutate.IndexAddendum{Add: image(t, nil, []byte("x")),
		Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	outer := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	outer = mutate.AppendManifests(outer,
		mutate.IndexAddendum{Add: image(t, &v1.ConfigFile{Architecture: "arm64"}, []byte("a")),
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: inner,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "x86_64"}}})
	if err := remote.WriteIndex(repo.Tag("v1"), outer); err != nil {
		t.Fatal(err)
	}
	r := newResolver(t, Config{})
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Unsupported", "nests a")
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

// TestResolveConfigSizeBounded: ggcr reads a config blob whole, bounded only
// by the size the manifest declares, and the config is read before the
// signature is checked, so any pusher chooses what the controller
// allocates. An oversized config is refused before it is fetched.
func TestResolveConfigSizeBounded(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	huge := image(t, &v1.ConfigFile{Config: v1.Config{
		Labels: map[string]string{"pad": strings.Repeat("x", 5<<20)},
	}}, []byte("huge"))
	push(t, repo.Tag("v1"), huge)
	c.reset(nil)
	r := newResolver(t, Config{})
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Oversized", "-byte config; the limit is")
	cn, cerr := huge.ConfigName()
	if cerr != nil {
		t.Fatal(cerr)
	}
	if c.blobs[cn.String()] != 0 {
		t.Error("fetched the oversized config before refusing it")
	}
}

// TestResolveConfigSizeUnknown: a config descriptor without a size would
// lift ggcr's only bound on the read.
func TestResolveConfigSizeUnknown(t *testing.T) {
	repo := newRegistry(t, false, nil)
	cfg := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	if err := remote.WriteLayer(repo, static.NewLayer(cfg, types.OCIConfigJSON)); err != nil {
		t.Fatal(err)
	}
	h, _, err := v1.SHA256(strings.NewReader(string(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"size":-1,"digest":%q},"layers":[]}`,
		types.OCIManifestSchema1, types.OCIConfigJSON, h)
	if err := remote.Put(repo.Tag("v1"), rawManifest{raw, types.OCIManifestSchema1}); err != nil {
		t.Fatal(err)
	}
	r := newResolver(t, Config{})
	_, err = r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "Unsupported", "no config size")
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

// staticKeychain answers every host with one authenticator.
type staticKeychain struct{ auth authn.Authenticator }

func (k staticKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return k.auth, nil }

// TestResolveUsesKeychain: the resolver presents the keychain's credential
// to a registry that demands one, and without it is denied, which is what
// ECR and pullSecret-backed registries depend on.
func TestResolveUsesKeychain(t *testing.T) {
	open := true
	basic := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !open && (!ok || u != "patchy" || p != "s3cret") {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	repo := newRegistry(t, false, basic)
	push(t, repo.Tag("v1"), image(t, nil, []byte("private")))
	open = false

	r := newResolver(t, Config{Keychain: staticKeychain{&authn.Basic{Username: "patchy", Password: "s3cret"}}})
	if _, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1")); err != nil {
		t.Errorf("Resolve with the keychain: %v", err)
	}
	r = newResolver(t, Config{})
	_, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
	rejection(t, err, "AccessDenied", "registry denied access")
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

// TestResolveCacheTagMoved: the verdict is cached by the digest the tag
// resolved to, and the HEAD is never cached, so a tag moved to a rejected
// image inside the TTL is resolved and judged afresh.
func TestResolveCacheTagMoved(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	push(t, repo.Tag("v1"), image(t, nil, []byte("a")))
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r := newResolver(t, Config{CacheTTL: 10 * time.Minute, Now: func() time.Time { return now }})
	ref := declared(t, repo.String()+":v1")
	if _, err := r.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve before the move: %v", err)
	}

	push(t, repo.Tag("v1"), image(t, &v1.ConfigFile{Config: v1.Config{Volumes: map[string]struct{}{"/moved": {}}}}))
	c.reset(nil)
	now = now.Add(time.Minute)
	_, err := r.Resolve(context.Background(), ref)
	rejection(t, err, "Volume", "/moved")
	if c.headCount() != 1 {
		t.Errorf("HEADs after the move = %d, want 1: the tag is resolved every time", c.headCount())
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

// pushReferrer attaches blob to subject as an OCI artifact of artifactType
// and returns the referrer's digest.
func pushReferrer(t *testing.T, repo name.Repository, subject v1.Descriptor, artifactType string, blob []byte) v1.Hash {
	t.Helper()
	return pushReferrerLayers(t, repo, subject, artifactType, blob, 1)
}

// pushReferrerLayers attaches an OCI artifact of artifactType to subject
// whose manifest lists blob n times as a layer, and returns its digest.
func pushReferrerLayers(t *testing.T, repo name.Repository, subject v1.Descriptor, artifactType string,
	blob []byte, n int,
) v1.Hash {
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
	layers := make([]v1.Descriptor, n)
	for i := range layers {
		layers[i] = desc(blob, artifactType)
	}
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"artifactType":  artifactType,
		"config":        desc(emptyBlob, "application/vnd.oci.empty.v1+json"),
		"layers":        layers,
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
	return h
}

// signLegacy writes the sha256-<digest>.sig tag the way cosign v2 did: one
// simple-signing payload layer, its ECDSA signature in the layer annotation.
func signLegacy(t *testing.T, priv *ecdsa.PrivateKey, repo name.Repository, digest v1.Hash) {
	t.Helper()
	signLegacyNaming(t, priv, repo, digest, digest)
}

// signLegacyNaming writes the .sig tag of digest with a payload that names
// payloadDigest: what copying another image's signature under this tag
// looks like.
func signLegacyNaming(t *testing.T, priv *ecdsa.PrivateKey, repo name.Repository, digest, payloadDigest v1.Hash) {
	t.Helper()
	payload := fmt.Appendf(nil, `{"critical":{"identity":{"docker-reference":"%s"},`+
		`"image":{"docker-manifest-digest":"%s"},"type":"cosign container image signature"},"optional":null}`,
		repo, payloadDigest)
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

// TestResolveSignedIndex: for an index the signature must name the index
// digest, the object the kubelet pulls and cosign signs, not one of its
// children (which `cosign sign --recursive` also signs, and which an
// attacker-built index could reuse beside an unchecked child).
func TestResolveSignedIndex(t *testing.T) {
	priv, pub := testKeys(t)
	cases := []struct {
		name  string
		sign  func(t *testing.T, repo name.Repository, index, child v1.Hash)
		wantR string
	}{
		{"the index digest is signed", func(t *testing.T, repo name.Repository, index, _ v1.Hash) {
			signLegacy(t, priv, repo, index)
		}, ""},
		{"only the child is signed", func(t *testing.T, repo name.Repository, _, child v1.Hash) {
			signLegacy(t, priv, repo, child)
		}, "Unsigned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, true, nil)
			amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATH=/usr/bin"}}}, []byte("amd"))
			index := pushIndex(t, repo.Tag("v1"), indexChild{amd, linux("amd64")})
			child, err := amd.Digest()
			if err != nil {
				t.Fatal(err)
			}
			tc.sign(t, repo, index, child)
			r := newResolver(t, Config{PublicKey: pub})
			got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if tc.wantR != "" {
				rejection(t, err, tc.wantR, "carries no signature")
				return
			}
			if err != nil || !got.Verified || got.Image != repo.String()+"@"+index.String() {
				t.Errorf("Resolve = %+v, %v; want Verified on the index digest %s", got, err, index)
			}
		})
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
		// A signature the operator made for another image, copied under
		// this image's .sig tag: the key verifies, the digest must not.
		{"legacy by the operator key naming another digest", func(t *testing.T, repo name.Repository) v1.Hash {
			signed := push(t, repo.Tag("signed"), image(t, nil, []byte("old")))
			d := push(t, repo.Tag("v1"), image(t, nil, []byte("new")))
			signLegacyNaming(t, priv, repo, d, signed)
			return d
		}, pub, "SignatureInvalid", "carries 1 signature(s)"},
		// The right message digest, signed by someone else: the digest
		// check passes, so only the signature check can refuse it.
		{"message signature over the image digest by another key", func(t *testing.T, repo name.Repository) v1.Hash {
			d := push(t, repo.Tag("v1"), image(t, nil, []byte("k")))
			pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, messageSignatureBundle(t, other, d))
			return d
		}, pub, "SignatureInvalid", "none of which verifies"},
		{"message signature with a corrupted signature", func(t *testing.T, repo name.Repository) v1.Hash {
			d := push(t, repo.Tag("v1"), image(t, nil, []byte("c")))
			var b map[string]any
			if err := json.Unmarshal(messageSignatureBundle(t, priv, d), &b); err != nil {
				t.Fatal(err)
			}
			ms := b["messageSignature"].(map[string]any)
			sig, err := base64.StdEncoding.DecodeString(ms["signature"].(string))
			if err != nil {
				t.Fatal(err)
			}
			sig[len(sig)-1] ^= 0xff
			ms["signature"] = sig
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, raw)
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

// bundleType is the artifact type cosign v3 gives a bundle referrer.
const bundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// TestResolveSignatureSurvivesBrokenReferrers: a referrer is anything anyone
// with push access attaches, so one that cannot be fetched or read is a
// candidate that does not verify, never a verdict on the image. Each case
// carries a valid legacy signature by the operator key (or a valid bundle
// behind the junk) and must verify.
func TestResolveSignatureSurvivesBrokenReferrers(t *testing.T) {
	priv, pub := testKeys(t)
	cases := []struct {
		name      string
		referrers bool
		setup     func(t *testing.T, repo name.Repository, digest v1.Hash)
	}{
		{"a dangling entry in the fallback index", false, func(t *testing.T, repo name.Repository, digest v1.Hash) {
			ref := pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, []byte(`{"junk":true}`))
			if err := remote.Delete(repo.Digest(ref.String())); err != nil {
				t.Fatal(err)
			}
			signLegacy(t, priv, repo, digest)
		}},
		{"a bundle blob over the size cap", true, func(t *testing.T, repo name.Repository, digest v1.Hash) {
			pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, make([]byte, 2<<20))
			signLegacy(t, priv, repo, digest)
		}},
		{"an unparseable bundle", true, func(t *testing.T, repo name.Repository, digest v1.Hash) {
			pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, []byte("not json"))
			signLegacy(t, priv, repo, digest)
		}},
		{"64 non-bundle referrers ahead of a valid bundle", false, func(t *testing.T, repo name.Repository, digest v1.Hash) {
			subject := subjectOf(t, repo.Tag("v1"))
			for i := range maxReferrers {
				pushReferrer(t, repo, subject, "application/vnd.example.sbom+json", fmt.Appendf(nil, `{"sbom":%d}`, i))
			}
			pushReferrer(t, repo, subject, bundleType, messageSignatureBundle(t, priv, digest))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, tc.referrers, nil)
			digest := push(t, repo.Tag("v1"), image(t, nil, []byte("signed")))
			tc.setup(t, repo, digest)
			r := newResolver(t, Config{PublicKey: pub})
			got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !got.Verified {
				t.Error("Verified = false for an image signed by the operator key")
			}
		})
	}
}

// TestResolveSignatureBlobsBounded: a referrer may list thousands of
// bundle-typed layers, and a legacy .sig as many signature layers; each is
// read (up to 1 MiB) before it is judged, so both are capped per image.
func TestResolveSignatureBlobsBounded(t *testing.T) {
	priv, pub := testKeys(t)
	junk := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","junk":true}`)
	junkDigest, _, err := v1.SHA256(strings.NewReader(string(junk)))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("bundle layers of one referrer", func(t *testing.T) {
		c := &counter{}
		repo := newRegistry(t, true, c.wrap)
		digest := push(t, repo.Tag("v1"), image(t, nil, []byte("b")))
		pushReferrerLayers(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, junk, 300)
		signLegacy(t, priv, repo, digest)
		c.reset(nil)
		r := newResolver(t, Config{PublicKey: pub})
		got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
		if err != nil || !got.Verified {
			t.Fatalf("Resolve = %+v, %v; want Verified by the legacy signature", got, err)
		}
		if n := c.blobs[junkDigest.String()]; n > maxBundleLayers {
			t.Errorf("read the junk bundle %d times, want at most %d", n, maxBundleLayers)
		}
	})
	t.Run("legacy signature layers", func(t *testing.T) {
		c := &counter{}
		repo := newRegistry(t, true, c.wrap)
		digest := push(t, repo.Tag("v1"), image(t, nil, []byte("l")))
		payload := []byte(`{"critical":{"type":"junk"}}`)
		payloadDigest, _, err := v1.SHA256(strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
		img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
		for range 300 {
			if img, err = mutate.Append(img, mutate.Addendum{
				Layer:       static.NewLayer(payload, "application/vnd.dev.cosign.simplesigning.v1+json"),
				Annotations: map[string]string{legacySignatureAnnotation: "AAAA"},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := remote.Write(repo.Tag("sha256-"+digest.Hex+".sig"), img); err != nil {
			t.Fatal(err)
		}
		c.reset(nil)
		r := newResolver(t, Config{PublicKey: pub})
		_, err = r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
		rejection(t, err, "SignatureInvalid", "none of which verifies")
		if n := c.blobs[payloadDigest.String()]; n > maxLegacySignatures {
			t.Errorf("read the junk payload %d times, want at most %d", n, maxLegacySignatures)
		}
	})
}

// TestResolveSignatureTransientCandidate: a candidate the registry could
// not serve (503) might have been the valid signature, so when nothing else
// verifies the result is transient, never a rejection; when the legacy
// signature verifies, the image is accepted.
func TestResolveSignatureTransientCandidate(t *testing.T) {
	priv, pub := testKeys(t)
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy signature %v", legacy), func(t *testing.T) {
			var flaky string
			wrap := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if flaky != "" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifests/"+flaky) {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
			repo := newRegistry(t, true, wrap)
			digest := push(t, repo.Tag("v1"), image(t, nil, []byte("t")))
			ref := pushReferrer(t, repo, subjectOf(t, repo.Tag("v1")), bundleType, messageSignatureBundle(t, priv, digest))
			if legacy {
				signLegacy(t, priv, repo, digest)
			}
			flaky = ref.String()
			r := newResolver(t, Config{PublicKey: pub})
			got, err := r.Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if legacy {
				if err != nil || !got.Verified {
					t.Fatalf("Resolve = %+v, %v; want Verified by the legacy signature", got, err)
				}
				return
			}
			if err == nil || runnerimage.IsRejection(err) {
				t.Errorf("Resolve = %v, want a transient (non-Rejection) error", err)
			}
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
