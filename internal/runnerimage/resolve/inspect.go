// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// Report is how every check judged one pinned reference. Resolve builds it
// stopping at the first rejection and returns Err; Inspect builds it whole,
// for the workstation check (patchy check image), which shows every
// verdict at once so an owner fixes the image in one pass.
//
// Each verdict is nil when the check passed and a *runnerimage.Rejection
// when it failed, carrying exactly the message Resolve would return.
type Report struct {
	// Image is the digest-pinned reference, "<repository>@sha256:<digest>".
	Image string
	// Index is true when Image names an image index rather than a single
	// image manifest.
	Index bool
	// Runnable is the rejection of the index or manifest as a whole: no
	// linux/amd64 or linux/arm64 manifest, an unchecked fallback a node
	// could pick, a nested index, too many children. Manifests is empty
	// when it is set.
	Runnable error
	// Manifests are the manifests a linux/amd64 or linux/arm64 node could
	// run, in index order (the one manifest when Image is not an index).
	Manifests []Manifest
	// SignatureChecked is true when a signature was required (the resolver
	// was not configured to allow unsigned images) and looked for.
	SignatureChecked bool
	// Signature is the verification verdict when SignatureChecked.
	Signature error

	// name is the pinned reference as the registry client spells it, which
	// is how every rejection message names the image.
	name string
}

// Manifest is one runnable manifest and each check's verdict on it, in the
// order Resolve applies them: Size, Config, Arch, Volumes, Env, Path.
type Manifest struct {
	// Digest names the manifest.
	Digest string
	// Platform is os/arch[/variant]: the index entry's, or the config's for
	// a single manifest (empty when that config was not read).
	Platform string
	// LayerBytes is the manifest's total of compressed layer sizes.
	LayerBytes int64
	// ConfigRead is true when the config blob was fetched, so Arch,
	// Volumes, Env and Path were judged; Config says why it was not.
	ConfigRead bool
	// SearchPath is the sanitized image PATH (runnerimage.SanitizePath).
	SearchPath []string

	// Size: the compressed layers are over the size limit.
	Size error
	// Config: the config blob declares no size or one over the config
	// cap, so it was not read.
	Config error
	// Arch: the config is not for linux/amd64 or linux/arm64.
	Arch error
	// Volumes: the config declares VOLUME instructions.
	Volumes error
	// Env: the config ENV sets a reserved name.
	Env error
	// Path: the config PATH has no absolute entry.
	Path error
}

// Err returns the first failed check of m in Resolve's order, or nil.
func (m Manifest) Err() error {
	for _, err := range []error{m.Size, m.Config, m.Arch, m.Volumes, m.Env, m.Path} {
		if err != nil {
			return err
		}
	}
	return nil
}

// Err returns what Resolve returns for the same reference: the first
// failed check in the order Resolve applies them (the index as a whole,
// then each manifest in turn followed by its PATH agreeing with the one
// before it, then the signature), or nil when the image is accepted.
func (r Report) Err() error {
	if r.Runnable != nil {
		return r.Runnable
	}
	for i, m := range r.Manifests {
		if err := m.Err(); err != nil {
			return err
		}
		if i > 0 && !slices.Equal(m.SearchPath, r.Manifests[i-1].SearchPath) {
			return pathMismatch(r.name, r.Manifests[i-1].SearchPath, m.SearchPath)
		}
	}
	if r.SignatureChecked {
		return r.Signature
	}
	return nil
}

// PathMismatch reports, as the rejection Resolve would return, runnable
// manifests whose sanitized PATHs differ, comparing only the manifests
// whose PATH was judged and passed; nil when they agree.
func (r Report) PathMismatch() error {
	var first []string
	seen := false
	for _, m := range r.Manifests {
		if !m.ConfigRead || m.Path != nil {
			continue
		}
		if !seen {
			first, seen = m.SearchPath, true
			continue
		}
		if !slices.Equal(m.SearchPath, first) {
			return pathMismatch(r.name, first, m.SearchPath)
		}
	}
	return nil
}

// pathMismatch is the rejection of an index whose runnable manifests set
// different PATHs: the pod's search path must not depend on the node.
func pathMismatch(pinned string, a, b []string) error {
	return &runnerimage.Rejection{Reason: "PathMismatch",
		Message: fmt.Sprintf("image `%s` sets a different PATH per platform (`%s` vs `%s`); "+
			"the pod's search path must not depend on the node architecture",
			pinned, strings.Join(a, ":"), strings.Join(b, ":"))}
}

// Inspect pins ref exactly as Resolve does, then runs every check Resolve
// runs and reports each verdict instead of stopping at the first
// rejection. It never consults or fills the verdict cache. The error is
// what stopped the inspection before the checks could finish: the
// reference did not resolve or a manifest could not be fetched (a
// *runnerimage.Rejection for not found or access denied), or the registry
// failed (transient). A signature lookup that fails transiently is
// reported in Report.Signature rather than discarding the other verdicts.
func (r *Resolver) Inspect(ctx context.Context, ref imageref.Ref) (Report, error) {
	repo, hash, pinned, err := r.pin(ctx, ref)
	if err != nil {
		return Report{}, err
	}
	return r.inspect(ctx, repo, hash, pinned, true)
}

// inspect fetches the pinned manifest, enumerates what would run, judges
// each runnable manifest and verifies the signature. With all false it
// stops at the first rejection, fetching nothing after it, which is the
// registry traffic Resolve has always made; with all true it judges every
// check it can reach.
func (r *Resolver) inspect(ctx context.Context, repo name.Repository, digest v1.Hash, image string,
	all bool) (Report, error) {
	pinned := repo.Digest(digest.String())
	rep := Report{Image: image, name: pinned.String()}
	desc, err := remote.Get(pinned, r.options(ctx)...)
	if err != nil {
		return rep, classify(pinned.String(), err)
	}
	rep.Index = desc.MediaType.IsIndex()
	children, err := runnable(pinned, desc)
	if err != nil {
		if !runnerimage.IsRejection(err) {
			return rep, err
		}
		rep.Runnable = err
		return rep, nil
	}
	for _, c := range children {
		m, err := r.inspectChild(ctx, repo, c, all)
		if err != nil {
			return rep, err
		}
		rep.Manifests = append(rep.Manifests, m)
		if !all && rep.Err() != nil {
			return rep, nil
		}
	}
	if r.cfg.AllowUnsigned {
		return rep, nil
	}
	rep.SignatureChecked = true
	if err := r.verifySignature(ctx, repo, digest); err != nil {
		if !all && !runnerimage.IsRejection(err) {
			return rep, err
		}
		rep.Signature = err
	}
	return rep, nil
}

// inspectChild fetches one manifest by digest and judges its size,
// platform, VOLUME, ENV and PATH. With all false it returns at the first
// failed check (so an oversized image's config is never fetched); with all
// true it judges every check it can. An oversized or unsized config is
// never fetched in either mode. The error is a fetch failure, classified.
func (r *Resolver) inspectChild(ctx context.Context, repo name.Repository, c child, all bool) (Manifest, error) {
	ref := repo.Digest(c.digest.String())
	out := Manifest{Digest: c.digest.String()}
	if c.platform != nil {
		out.Platform = platformName(*c.platform)
	}
	img, err := remote.Image(ref, r.options(ctx)...)
	if err != nil {
		return out, classify(ref.String(), err)
	}
	m, err := img.Manifest()
	if err != nil {
		return out, classify(ref.String(), err)
	}
	for _, l := range m.Layers {
		out.LayerBytes += l.Size
	}
	if out.LayerBytes > r.cfg.MaxBytes {
		out.Size = &runnerimage.Rejection{Reason: "Oversized",
			Message: fmt.Sprintf("image `%s`%s has %d bytes of compressed layers; the limit is %d bytes",
				ref, platformSuffix(c.platform), out.LayerBytes, r.cfg.MaxBytes)}
		if !all {
			return out, nil
		}
	}
	switch {
	case m.Config.Size <= 0:
		out.Config = &runnerimage.Rejection{Reason: "Unsupported",
			Message: fmt.Sprintf("image `%s`%s declares no config size", ref, platformSuffix(c.platform))}
		return out, nil
	case m.Config.Size > maxConfigBytes:
		out.Config = &runnerimage.Rejection{Reason: "Oversized",
			Message: fmt.Sprintf("image `%s`%s has a %d-byte config; the limit is %d bytes",
				ref, platformSuffix(c.platform), m.Config.Size, maxConfigBytes)}
		return out, nil
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return out, classify(ref.String(), err)
	}
	out.ConfigRead = true
	if c.platform == nil {
		out.Platform = platformName(v1.Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant})
	}
	if !runnableConfig(cf) {
		out.Arch = &runnerimage.Rejection{Reason: "UnsupportedPlatform",
			Message: fmt.Sprintf("image `%s` is built for %s/%s; only linux/amd64 and linux/arm64 can run",
				ref, cf.OS, cf.Architecture)}
		if !all {
			return out, nil
		}
	}
	volumes := make([]string, 0, len(cf.Config.Volumes))
	for path := range cf.Config.Volumes {
		volumes = append(volumes, path)
	}
	out.Volumes = inChild(labeled("Volume", runnerimage.CheckVolumes(volumes)), c)
	if out.Volumes != nil && !all {
		return out, nil
	}
	out.Env = inChild(labeled("ReservedEnv", runnerimage.CheckEnv(cf.Config.Env, r.cfg.ReservedEnv)), c)
	if out.Env != nil && !all {
		return out, nil
	}
	out.SearchPath, err = runnerimage.SanitizePath(cf.Config.Env)
	out.Path = inChild(labeled("EmptyPath", err), c)
	return out, nil
}
