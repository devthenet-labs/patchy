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
	// reserved names, injected by the wiring; nil means the built-in list.
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

// runnable lists the manifests that would actually run: the linux/amd64 and
// linux/arm64 entries of an index, or the single manifest itself. Entries of
// other platforms (and the unknown/unknown attestation manifests) never run
// and are not checked; an index with no runnable entry is rejected.
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
		var out []child
		for _, m := range im.Manifests {
			if !runnablePlatform(m.Platform) {
				continue
			}
			if !m.MediaType.IsImage() {
				return nil, &runnerimage.Rejection{Reason: "Unsupported",
					Message: fmt.Sprintf("image `%s` nests a %s manifest for %s/%s; only image manifests can run",
						pinned, m.MediaType, m.Platform.OS, m.Platform.Architecture)}
			}
			out = append(out, child{digest: m.Digest, platform: m.Platform})
		}
		if len(out) == 0 {
			return nil, &runnerimage.Rejection{Reason: "UnsupportedPlatform",
				Message: fmt.Sprintf("image `%s` has no linux/amd64 or linux/arm64 manifest", pinned)}
		}
		return out, nil
	case desc.MediaType.IsImage():
		return []child{{digest: desc.Digest}}, nil
	default:
		return nil, &runnerimage.Rejection{Reason: "Unsupported",
			Message: fmt.Sprintf("image `%s` is a %s, not an image manifest or index", pinned, desc.MediaType)}
	}
}

// runnablePlatform reports whether an index entry's platform is one the
// agent pod could be scheduled on.
func runnablePlatform(p *v1.Platform) bool {
	return p != nil && p.OS == "linux" && (p.Architecture == "amd64" || p.Architecture == "arm64")
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
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, classify(ref.String(), err)
	}
	if !runnablePlatform(&v1.Platform{OS: cf.OS, Architecture: cf.Architecture}) {
		return nil, &runnerimage.Rejection{Reason: "UnsupportedPlatform",
			Message: fmt.Sprintf("image `%s` is built for %s/%s; only linux/amd64 and linux/arm64 can run",
				ref, cf.OS, cf.Architecture)}
	}
	volumes := make([]string, 0, len(cf.Config.Volumes))
	for path := range cf.Config.Volumes {
		volumes = append(volumes, path)
	}
	if err := runnerimage.CheckVolumes(volumes); err != nil {
		return nil, labeled("Volume", err)
	}
	if err := runnerimage.CheckEnv(cf.Config.Env, r.cfg.ReservedEnv); err != nil {
		return nil, labeled("ReservedEnv", err)
	}
	searchPath, err := runnerimage.SanitizePath(cf.Config.Env)
	if err != nil {
		return nil, labeled("EmptyPath", err)
	}
	return searchPath, nil
}

// platformSuffix names an index child's platform in a message.
func platformSuffix(p *v1.Platform) string {
	if p == nil {
		return ""
	}
	return " (" + p.OS + "/" + p.Architecture + ")"
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
