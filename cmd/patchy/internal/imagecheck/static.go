// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/runnerimage"
	"github.com/bitwise-media-group/patchy/internal/runnerimage/resolve"
)

// UnsignedReason is the signature line when no key was given: whether the
// image is signed is not known, and source-controller admits an image it
// cannot verify only when the operator opted out of signatures.
const UnsignedReason = "unsigned (allowed only with --repository-image-allow-unsigned); " +
	"pass --cosign-key with the operator's public key to verify a signature"

// StaticConfig is what the static checks run with.
type StaticConfig struct {
	// Reference is the image as a repository would declare it.
	Reference string
	// Policy is the registry allowlist to check against, standing in for
	// source-controller's --repository-image-registries; nil skips the
	// check.
	Policy *runnerimage.Policy
	// MaxBytes caps the compressed layers of each platform, as
	// --repository-image-max-bytes does; <= 0 is resolve.DefaultMaxBytes.
	MaxBytes int64
	// PublicKey is the operator's cosign key; nil skips verification.
	PublicKey *ecdsa.PublicKey
	// KeyName names where PublicKey came from, for the report.
	KeyName string
	// Keychain authenticates registry calls; nil is anonymous. The CLI
	// passes the local docker credentials.
	Keychain authn.Keychain
}

// Static runs the checks source-controller applies to a declared image and
// returns a report of every verdict: reference, allowlist, resolve,
// platform, size, volume, env, path and signature, in that order. The
// error is a resolver that could not be built, never a failed check.
func Static(ctx context.Context, cfg StaticConfig) (Report, error) {
	r := Report{Reference: cfg.Reference}
	after := []string{CheckResolve, CheckPlatform, CheckSize, CheckVolume, CheckEnv, CheckPath, CheckSignature}

	ref, err := runnerimage.ParseDeclared(cfg.Reference)
	if err != nil {
		r.add(CheckReference, Fail, err.Error())
		r.skip("the reference is invalid", append([]string{CheckAllowlist}, after...)...)
		return r, nil
	}
	r.Canonical = ref.String()
	r.add(CheckReference, Pass, r.Canonical)

	switch cfg.Policy {
	case nil:
		r.add(CheckAllowlist, Skip, "no --allow given; source-controller admits only images under its "+
			"--repository-image-registries")
	default:
		if err := cfg.Policy.Allow(ref); err != nil {
			r.add(CheckAllowlist, Fail, err.Error())
		} else {
			r.add(CheckAllowlist, Pass, "under "+strings.Join(cfg.Policy.Entries(), ", "))
		}
	}

	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = resolve.DefaultMaxBytes
	}
	resolver, err := resolve.New(resolve.Config{
		MaxBytes:      maxBytes,
		PublicKey:     cfg.PublicKey,
		AllowUnsigned: cfg.PublicKey == nil,
		ReservedEnv:   reservedEnv(),
		Keychain:      cfg.Keychain,
	})
	if err != nil {
		return r, err
	}
	rep, err := resolver.Inspect(ctx, ref)
	if err != nil {
		r.add(CheckResolve, Fail, err.Error())
		r.skip("the image did not resolve", after[1:]...)
		return r, nil
	}
	r.Image, r.Index = rep.Image, rep.Index
	kind := "image manifest"
	if rep.Index {
		kind = "image index"
	}
	r.add(CheckResolve, Pass, rep.Image+" ("+kind+")")
	if rep.Runnable != nil {
		r.add(CheckPlatform, Fail, rep.Runnable.Error())
		r.skip("no manifest a linux/amd64 or linux/arm64 node could run", CheckSize, CheckVolume, CheckEnv,
			CheckPath, CheckSignature)
		return r, nil
	}
	r.judge(rep, maxBytes)
	switch {
	case !rep.SignatureChecked:
		r.add(CheckSignature, Skip, UnsignedReason)
	case rep.Signature != nil:
		r.add(CheckSignature, Fail, rep.Signature.Error())
	default:
		r.add(CheckSignature, Pass, "verified with "+cfg.KeyName)
	}
	return r, nil
}

// judge adds the per-manifest checks: platform, size, volume, env and path.
func (r *Report) judge(rep resolve.Report, maxBytes int64) {
	var platforms, sizes []string
	var platformErrs, sizeErrs, volumeErrs, envErrs, pathErrs []error
	read := false
	for _, m := range rep.Manifests {
		r.Platforms = append(r.Platforms, Platform{Platform: m.Platform, Digest: m.Digest,
			CompressedBytes: m.LayerBytes})
		label := m.Platform
		if label == "" {
			label = "unknown platform"
		}
		platforms = append(platforms, label)
		sizes = append(sizes, fmt.Sprintf("%s %s", label, humanBytes(m.LayerBytes)))
		platformErrs = append(platformErrs, m.Arch)
		sizeErrs = append(sizeErrs, m.Size)
		if oversizedConfig(m.Config) {
			sizeErrs = append(sizeErrs, m.Config)
		} else {
			platformErrs = append(platformErrs, m.Config)
		}
		if !m.ConfigRead {
			continue
		}
		read = true
		volumeErrs = append(volumeErrs, m.Volumes)
		envErrs = append(envErrs, m.Env)
		pathErrs = append(pathErrs, m.Path)
		if m.Path == nil && r.SearchPath == nil {
			r.SearchPath = m.SearchPath
		}
	}
	r.verdict(CheckPlatform, platformErrs, strings.Join(platforms, ", "))
	r.verdict(CheckSize, sizeErrs, fmt.Sprintf("%s of compressed layers; the limit is %s",
		strings.Join(sizes, ", "), humanBytes(maxBytes)))
	if !read {
		r.skip("no image config was read", CheckVolume, CheckEnv, CheckPath)
		return
	}
	r.verdict(CheckVolume, volumeErrs, "no VOLUME")
	r.verdict(CheckEnv, envErrs, "no reserved variable in ENV")
	if mismatch := rep.PathMismatch(); mismatch != nil {
		pathErrs = append(pathErrs, mismatch)
	}
	if r.verdict(CheckPath, pathErrs, "/patchy/bin:"+strings.Join(r.SearchPath, ":")) == Fail {
		r.SearchPath = nil
	}
}

// verdict adds one check from the verdicts it collected across manifests:
// Fail with every failure's message, Pass with the pass reason otherwise.
func (r *Report) verdict(name string, errs []error, pass string) Status {
	var msgs []string
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		r.add(name, Fail, strings.Join(msgs, "; "))
		return Fail
	}
	r.add(name, Pass, pass)
	return Pass
}

// oversizedConfig reports a config verdict about its size, which belongs on
// the size line; the other config verdict (no size at all) is the image
// being malformed, which belongs on the platform line.
func oversizedConfig(err error) bool {
	var rej *runnerimage.Rejection
	return errors.As(err, &rej) && rej.Reason == "Oversized"
}

// reservedEnv is the Job builder's reserved names as the set the resolver
// takes, exactly as source-controller wires it.
func reservedEnv() map[string]bool {
	names := jobs.ReservedEnvNames()
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// humanBytes renders a byte count in binary units, one decimal place.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
