// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

const (
	// DefaultMaxBytes caps the compressed layers of one platform child.
	DefaultMaxBytes = int64(4) << 30 // 4 GiB
	// DefaultCacheTTL bounds how long a checked digest's verdict is reused.
	DefaultCacheTTL = 10 * time.Minute
	// maxChildren bounds the distinct manifests of one index that are
	// checked. A real image lists one per architecture (a few per
	// architecture with variants); every check costs a manifest and a config
	// fetch on the single Repository worker.
	maxChildren = 8
	// maxConfigBytes caps an image config blob. ggcr reads a config whole,
	// bounded only by the size the manifest declares, and the checks run
	// before the signature, so an unsigned image from any pusher would
	// otherwise choose how much the controller allocates (JSON decoding
	// multiplies it several times over).
	maxConfigBytes = 4 << 20
)

// Config configures a Resolver.
type Config struct {
	// MaxBytes caps the compressed layers of each platform child; <= 0
	// means DefaultMaxBytes.
	MaxBytes int64
	// PublicKey is the operator's cosign key every image must be signed
	// with; required unless AllowUnsigned.
	PublicKey *ecdsa.PublicKey
	// AllowUnsigned skips signature verification; Resolved.Verified is then
	// always false.
	AllowUnsigned bool
	// ReservedEnv names variables an image ENV may not set beyond the
	// built-in list runnerimage.CheckEnv enforces: the Job builder's own
	// reserved names, which the wiring injects from jobs.ReservedEnvNames;
	// nil means the built-in list only.
	ReservedEnv map[string]bool
	// Keychain authenticates registry calls (NewKeychain); nil is anonymous.
	Keychain authn.Keychain
	// CacheTTL bounds the verdict cache; <= 0 means DefaultCacheTTL.
	CacheTTL time.Duration
	// Now is the clock seam for cache expiry; nil means time.Now.
	Now func() time.Time
}

// Resolver is the registry-backed runnerimage.Resolver.
type Resolver struct {
	cfg   Config
	cache *cache
	// backoff overrides the registry client's retry policy; tests shorten
	// it, production keeps the client default.
	backoff *remote.Backoff
}

var _ runnerimage.Resolver = (*Resolver)(nil)

// New validates cfg and builds a Resolver. A key is required unless
// unsigned images are explicitly allowed, so a misconfiguration fails at
// startup rather than admitting an unverified image.
func New(cfg Config) (*Resolver, error) {
	if cfg.PublicKey == nil && !cfg.AllowUnsigned {
		return nil, errors.New("runner image resolver: a cosign public key is required unless unsigned images are allowed")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Resolver{cfg: cfg, cache: newCache(cfg.CacheTTL, cfg.Now)}, nil
}

// options builds the per-call remote options.
func (r *Resolver) options(ctx context.Context) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx)}
	if r.cfg.Keychain != nil {
		opts = append(opts, remote.WithAuthFromKeychain(r.cfg.Keychain))
	}
	if r.backoff != nil {
		opts = append(opts, remote.WithRetryBackoff(*r.backoff))
	}
	return opts
}

// Resolve implements runnerimage.Resolver: one HEAD pins a tag to a digest
// (a digest pin skips it), then every check runs on, and the result names,
// "<repository>@<digest>".
func (r *Resolver) Resolve(ctx context.Context, ref imageref.Ref) (runnerimage.Resolved, error) {
	repo, err := name.NewRepository(ref.Repository)
	if err != nil {
		return runnerimage.Resolved{}, &runnerimage.Rejection{Reason: "InvalidReference",
			Message: fmt.Sprintf("image reference `%s` is invalid: %v", ref.String(), err)}
	}
	digest := ref.Digest
	if digest == "" {
		desc, err := remote.Head(repo.Tag(ref.Tag), r.options(ctx)...)
		if err != nil {
			return runnerimage.Resolved{}, classify(ref.String(), err)
		}
		digest = desc.Digest.String()
	}
	pinned, err := runnerimage.Pin(ref, digest)
	if err != nil {
		return runnerimage.Resolved{}, labeled("InvalidDigest", err)
	}
	if v, ok := r.cache.get(pinned); ok {
		return v.resolved, v.err
	}
	hash, err := v1.NewHash(digest)
	if err != nil {
		return runnerimage.Resolved{}, &runnerimage.Rejection{Reason: "InvalidDigest",
			Message: fmt.Sprintf("digest `%s` is invalid: %v", digest, err)}
	}
	resolved, err := r.check(ctx, repo, hash)
	resolved.Image = pinned
	if err != nil {
		resolved = runnerimage.Resolved{}
	}
	if err == nil || runnerimage.IsRejection(err) {
		r.cache.put(pinned, resolved, err)
	}
	return resolved, err
}

// check fetches the pinned manifest, enumerates what would run, checks each
// child and verifies the signature.
func (r *Resolver) check(ctx context.Context, repo name.Repository, digest v1.Hash) (runnerimage.Resolved, error) {
	pinned := repo.Digest(digest.String())
	desc, err := remote.Get(pinned, r.options(ctx)...)
	if err != nil {
		return runnerimage.Resolved{}, classify(pinned.String(), err)
	}
	children, err := runnable(pinned, desc)
	if err != nil {
		return runnerimage.Resolved{}, err
	}
	var searchPath []string
	for i, child := range children {
		sp, err := r.checkChild(ctx, repo, child)
		if err != nil {
			return runnerimage.Resolved{}, err
		}
		if i > 0 && !slices.Equal(sp, searchPath) {
			return runnerimage.Resolved{}, &runnerimage.Rejection{Reason: "PathMismatch",
				Message: fmt.Sprintf("image `%s` sets a different PATH per platform (`%s` vs `%s`); "+
					"the pod's search path must not depend on the node architecture",
					pinned, strings.Join(searchPath, ":"), strings.Join(sp, ":"))}
		}
		searchPath = sp
	}
	verified := false
	if !r.cfg.AllowUnsigned {
		if err := r.verifySignature(ctx, repo, digest); err != nil {
			return runnerimage.Resolved{}, err
		}
		verified = true
	}
	return runnerimage.Resolved{SearchPath: searchPath, Verified: verified}, nil
}

// child is one manifest to check: the pinned manifest itself, or an index
// entry named by its own digest with the platform the index claims for it.
type child struct {
	digest   v1.Hash
	platform *v1.Platform
}

// runnable lists the manifests a linux/amd64 or linux/arm64 node could run,
// judged the way containerd picks an index child (containerd/platforms Only
// and Normalize, images.Manifest), or the single manifest itself.
//
// Every entry whose normalized platform is linux/amd64 or linux/arm64 (any
// variant, and any spelling containerd folds into those: x86_64, aarch64,
// upper case, an empty OS) is enumerated by its own digest, once per digest,
// and checked. containerd also falls back to a linux/386 entry on an amd64
// node and a linux/arm/* entry on an arm64 node when no entry names the
// node's own architecture, and judges an entry with no platform by its
// config when no platform-tagged entry matches at all. Those are never
// checked, so an index that leaves one reachable is rejected; entries only
// other platforms run (windows, the unknown/unknown attestation manifests,
// s390x, ...) are skipped. An index with no runnable entry, a nested index
// where a node could reach it, or more than maxChildren distinct runnable
// children is rejected.
func runnable(pinned name.Digest, desc *remote.Descriptor) ([]child, error) {
	switch {
	case desc.MediaType.IsIndex():
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", pinned, err)
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", pinned, err)
		}
		// covered: an architecture with a baseline entry, which containerd
		// prefers over every fallback on a node of that architecture.
		covered := map[string]bool{}
		for _, m := range im.Manifests {
			if m.Platform == nil {
				continue
			}
			if p := normalizePlatform(*m.Platform); p.OS == "linux" && p.Variant == "" {
				covered[p.Architecture] = true
			}
		}
		var out []child
		seen := map[v1.Hash]bool{}
		for _, m := range im.Manifests {
			switch platformReach(m.Platform, covered) {
			case reachNever:
				continue
			case reachUnchecked:
				return nil, &runnerimage.Rejection{Reason: "UnsupportedPlatform",
					Message: fmt.Sprintf("image `%s` lists a manifest with %s, which a node falls back to when no "+
						"entry names its own architecture; only linux/amd64 and linux/arm64 manifests are checked, so "+
						"list both or drop it", pinned, describePlatform(m.Platform))}
			}
			if !m.MediaType.IsImage() {
				return nil, &runnerimage.Rejection{Reason: "Unsupported",
					Message: fmt.Sprintf("image `%s` nests a %s manifest for %s; only image manifests can run",
						pinned, m.MediaType, describePlatform(m.Platform))}
			}
			if seen[m.Digest] {
				continue
			}
			seen[m.Digest] = true
			out = append(out, child{digest: m.Digest, platform: m.Platform})
		}
		if len(out) == 0 {
			return nil, &runnerimage.Rejection{Reason: "UnsupportedPlatform",
				Message: fmt.Sprintf("image `%s` has no linux/amd64 or linux/arm64 manifest", pinned)}
		}
		if len(out) > maxChildren {
			return nil, &runnerimage.Rejection{Reason: "Unsupported",
				Message: fmt.Sprintf("image `%s` lists %d runnable manifests; at most %d are checked",
					pinned, len(out), maxChildren)}
		}
		return out, nil
	case desc.MediaType.IsImage():
		return []child{{digest: desc.Digest}}, nil
	default:
		return nil, &runnerimage.Rejection{Reason: "Unsupported",
			Message: fmt.Sprintf("image `%s` is a %s, not an image manifest or index", pinned, desc.MediaType)}
	}
}

// reach is whether a linux/amd64 or linux/arm64 node could run an index
// entry.
type reach int

const (
	// reachNever: no such node runs the entry.
	reachNever reach = iota
	// reachChecked: the entry names the node's own architecture; it is
	// enumerated and checked.
	reachChecked
	// reachUnchecked: a node could fall back to the entry, which is never
	// checked.
	reachUnchecked
)

// platformReach classifies one index entry given the architectures that
// have a baseline entry (covered).
func platformReach(p *v1.Platform, covered map[string]bool) reach {
	if p == nil {
		// Judged by its config on a node whose architecture nothing names.
		if covered["amd64"] && covered["arm64"] {
			return reachNever
		}
		return reachUnchecked
	}
	n := normalizePlatform(*p)
	if n.OS != "linux" {
		return reachNever
	}
	switch n.Architecture {
	case "amd64", "arm64":
		return reachChecked
	case "386":
		if !covered["amd64"] {
			return reachUnchecked
		}
	case "arm":
		if !covered["arm64"] {
			return reachUnchecked
		}
	}
	return reachNever
}

// normalizePlatform folds a platform the way containerd does before matching
// (containerd/platforms Normalize): OS, architecture and variant are
// case-insensitive, an empty OS is the node's own (linux), x86_64, x86-64,
// aarch64, i386, armhf and armel are spellings of amd64, arm64, 386 and arm,
// and the baseline variants (amd64 v1, arm64 v8) fold to empty.
func normalizePlatform(p v1.Platform) v1.Platform {
	os, arch, variant := strings.ToLower(p.OS), strings.ToLower(p.Architecture), strings.ToLower(p.Variant)
	if os == "" {
		os = "linux"
	}
	switch arch {
	case "x86_64", "x86-64", "amd64":
		arch = "amd64"
		if variant == "v1" {
			variant = ""
		}
	case "aarch64", "arm64":
		arch = "arm64"
		if variant == "8" || variant == "v8" {
			variant = ""
		}
	case "i386":
		arch, variant = "386", ""
	case "armhf":
		arch, variant = "arm", "v7"
	case "armel":
		arch, variant = "arm", "v6"
	}
	return v1.Platform{OS: os, Architecture: arch, Variant: variant}
}

// runnableConfig reports whether an image config's os/arch is one the
// agent pod could run, normalized as containerd does.
func runnableConfig(cf *v1.ConfigFile) bool {
	n := normalizePlatform(v1.Platform{OS: cf.OS, Architecture: cf.Architecture})
	return n.OS == "linux" && (n.Architecture == "amd64" || n.Architecture == "arm64")
}

// describePlatform names an index entry's platform in a message.
func describePlatform(p *v1.Platform) string {
	if p == nil {
		return "no platform"
	}
	return "platform " + platformName(*p)
}

// checkChild fetches one manifest by digest and applies the size, platform,
// VOLUME, ENV and PATH checks to it, returning the sanitized search path.
func (r *Resolver) checkChild(ctx context.Context, repo name.Repository, c child) ([]string, error) {
	ref := repo.Digest(c.digest.String())
	img, err := remote.Image(ref, r.options(ctx)...)
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	var size int64
	for _, l := range m.Layers {
		size += l.Size
	}
	if size > r.cfg.MaxBytes {
		return nil, &runnerimage.Rejection{Reason: "Oversized",
			Message: fmt.Sprintf("image `%s`%s has %d bytes of compressed layers; the limit is %d bytes",
				ref, platformSuffix(c.platform), size, r.cfg.MaxBytes)}
	}
	switch {
	case m.Config.Size <= 0:
		return nil, &runnerimage.Rejection{Reason: "Unsupported",
			Message: fmt.Sprintf("image `%s`%s declares no config size", ref, platformSuffix(c.platform))}
	case m.Config.Size > maxConfigBytes:
		return nil, &runnerimage.Rejection{Reason: "Oversized",
			Message: fmt.Sprintf("image `%s`%s has a %d-byte config; the limit is %d bytes",
				ref, platformSuffix(c.platform), m.Config.Size, maxConfigBytes)}
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	if !runnableConfig(cf) {
		return nil, &runnerimage.Rejection{Reason: "UnsupportedPlatform",
			Message: fmt.Sprintf("image `%s` is built for %s/%s; only linux/amd64 and linux/arm64 can run",
				ref, cf.OS, cf.Architecture)}
	}
	volumes := make([]string, 0, len(cf.Config.Volumes))
	for path := range cf.Config.Volumes {
		volumes = append(volumes, path)
	}
	if err := runnerimage.CheckVolumes(volumes); err != nil {
		return nil, inChild(labeled("Volume", err), c)
	}
	if err := runnerimage.CheckEnv(cf.Config.Env, r.cfg.ReservedEnv); err != nil {
		return nil, inChild(labeled("ReservedEnv", err), c)
	}
	searchPath, err := runnerimage.SanitizePath(cf.Config.Env)
	if err != nil {
		return nil, inChild(labeled("EmptyPath", err), c)
	}
	return searchPath, nil
}

// inChild names the index child a pure check's rejection came from, so the
// owner knows which platform's image to fix; a single manifest's rejection
// passes through.
func inChild(err error, c child) error {
	var rej *runnerimage.Rejection
	if c.platform == nil || !errors.As(err, &rej) {
		return err
	}
	return &runnerimage.Rejection{Reason: rej.Reason,
		Message: rej.Message + " (in the " + platformName(*c.platform) + " manifest)"}
}

// platformSuffix names an index child's platform in a message.
func platformSuffix(p *v1.Platform) string {
	if p == nil {
		return ""
	}
	return " (" + platformName(*p) + ")"
}

// platformName spells a platform as os/arch[/variant].
func platformName(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// labeled stamps reason onto a pure check's Rejection; any other error
// passes through.
func labeled(reason string, err error) error {
	var rej *runnerimage.Rejection
	if errors.As(err, &rej) && rej.Reason == "" {
		return &runnerimage.Rejection{Reason: reason, Message: rej.Message}
	}
	return err
}

// classify maps a registry error onto the contract: 401 and 403 are a
// deterministic access rejection (the fix is a credential, not a retry),
// 404 a deterministic not-found, and everything else transient.
func classify(ref string, err error) error {
	switch {
	case isStatus(err, http.StatusUnauthorized), isStatus(err, http.StatusForbidden):
		return &runnerimage.Rejection{Reason: "AccessDenied",
			Message: fmt.Sprintf("registry denied access to `%s`; configure pullSecret or a cloud credential", ref)}
	case isStatus(err, http.StatusNotFound):
		return &runnerimage.Rejection{Reason: "NotFound",
			Message: fmt.Sprintf("image `%s` was not found in the registry", ref)}
	}
	return fmt.Errorf("resolve %s: %w", ref, err)
}

// isStatus reports whether err is a registry response with the status.
func isStatus(err error, status int) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == status
}
