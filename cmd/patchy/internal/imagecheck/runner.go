// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// RunnerImageRepository is where the release publishes the claude runner
// image, the trusted donor of agent-runner and the claude CLI.
const RunnerImageRepository = "ghcr.io/devthenet-labs/patchy/claude-agent-runner"

// passRunnerImage ends every reason no runner image could be chosen for.
const passRunnerImage = "pass --runner-image to take agent-runner and claude from a runner image of your choice " +
	"(it is used as given)"

// RunnerImage is the trusted image a sandbox run takes agent-runner and the
// claude CLI from.
type RunnerImage struct {
	// Reference is the image as chosen: a release tag of
	// RunnerImageRepository, or --runner-image as given.
	Reference string `json:"reference"`
	// Digest is the manifest digest the image runs at: the one the release
	// tag named in the registry when it was chosen, or the one a
	// --runner-image reference carries. It is empty for a --runner-image
	// tag, which runs as the local docker store has it.
	Digest string `json:"digest,omitempty"`
}

// Image is the reference docker is given: the repository pinned to Digest,
// so no local copy of a tag, however old, stands in for the image chosen,
// and Reference itself when there is no digest.
func (r RunnerImage) Image() string {
	if r.Digest == "" {
		return r.Reference
	}
	ref, err := name.ParseReference(r.Reference)
	if err != nil {
		return r.Reference
	}
	return ref.Context().Name() + "@" + r.Digest
}

// String is the image as a report line names it: Reference, followed by
// Digest when Reference does not already carry it.
func (r RunnerImage) String() string {
	if r.Digest == "" || strings.HasSuffix(r.Reference, "@"+r.Digest) {
		return r.Reference
	}
	return r.Reference + "@" + r.Digest
}

// Registry is the registry access choosing the runner image needs.
// RemoteRegistry is the real one; tests fake it.
type Registry interface {
	// Tags lists a repository's tags.
	Tags(ctx context.Context, repository string) ([]string, error)
	// Digest is the digest of the manifest a tag reference names now.
	Digest(ctx context.Context, reference string) (string, error)
}

// RemoteRegistry is the Registry that asks the registry itself.
type RemoteRegistry struct {
	// Keychain authenticates registry calls; nil is anonymous. The CLI
	// passes the local docker credentials.
	Keychain authn.Keychain
}

var _ Registry = RemoteRegistry{}

// options builds the per-call remote options.
func (r RemoteRegistry) options(ctx context.Context) []remote.Option {
	opts := []remote.Option{remote.WithContext(ctx)}
	if r.Keychain != nil {
		opts = append(opts, remote.WithAuthFromKeychain(r.Keychain))
	}
	return opts
}

// Tags implements Registry.
func (r RemoteRegistry) Tags(ctx context.Context, repository string) ([]string, error) {
	repo, err := name.NewRepository(repository)
	if err != nil {
		return nil, err
	}
	return remote.List(repo, r.options(ctx)...)
}

// Digest implements Registry: one HEAD, as source-controller pins a tag.
func (r RemoteRegistry) Digest(ctx context.Context, reference string) (string, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(ref, r.options(ctx)...)
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// DefaultRunnerImage chooses the runner image for a CLI of the given version
// when no --runner-image is given. A release (exactly X.Y.Z, with or without
// its v) takes the runner image released with it, whose agent-runner speaks
// every subcommand this CLI drives. Any other build (a plain go build's
// "dev", hack/build.sh's git describe, a goreleaser snapshot) has no runner
// image of its own and takes the newest release: the highest vX.Y.Z tag in
// the registry, never latest or a pre-release, since what a local docker
// store holds under latest can be any age. Either way the tag is pinned to
// the digest it names in the registry now, so a stale local copy of it
// never runs. The error, when there is no such image or the registry cannot
// say, is written for the report and says to pass --runner-image.
func DefaultRunnerImage(ctx context.Context, reg Registry, version string) (RunnerImage, error) {
	tag, isRelease := releaseTag(version)
	if !isRelease {
		dev := fmt.Sprintf("this CLI is a development build (version %q) with no runner image of its own", version)
		tags, err := reg.Tags(ctx, RunnerImageRepository)
		if err != nil {
			return RunnerImage{}, fmt.Errorf("%s, and the newest release could not be found, since the tags of %s "+
				"could not be listed: %w; %s", dev, RunnerImageRepository, err, passRunnerImage)
		}
		if tag = newestRelease(tags); tag == "" {
			return RunnerImage{}, fmt.Errorf("%s, and %s has no release (a vX.Y.Z tag); %s", dev,
				RunnerImageRepository, passRunnerImage)
		}
	}
	ref := RunnerImageRepository + ":" + tag
	digest, err := reg.Digest(ctx, ref)
	if err != nil {
		return RunnerImage{}, fmt.Errorf("%s did not resolve in the registry: %w; %s", ref, err, passRunnerImage)
	}
	return RunnerImage{Reference: ref, Digest: digest}, nil
}

// ChooseRunner picks the runner image for a sandbox run and reports the
// pick as the runner-image check, so a report always says which image its
// agent-runner came from: override (--runner-image) as given, without
// asking the registry, otherwise DefaultRunnerImage for the CLI's version.
// A nil image is a run that cannot happen, and the check is then a FAIL
// saying why.
func ChooseRunner(ctx context.Context, reg Registry, override, version string) (*RunnerImage, Check) {
	var r Report
	if override != "" {
		img := RunnerImage{Reference: override}
		reason := img.String() + ", as given with --runner-image"
		if d, err := name.NewDigest(override); err == nil {
			img.Digest = d.DigestStr()
		} else {
			reason += "; docker runs its local copy of a tag, when it has one"
		}
		r.add(CheckRunnerImage, Pass, reason)
		return &img, r.Checks[0]
	}
	img, err := DefaultRunnerImage(ctx, reg, version)
	if err != nil {
		r.add(CheckRunnerImage, Fail, err.Error())
		return nil, r.Checks[0]
	}
	why := fmt.Sprintf("the newest release, as this CLI is a development build (version %q)", version)
	if _, isRelease := releaseTag(version); isRelease {
		why = "the one released with this CLI"
	}
	r.add(CheckRunnerImage, Pass, img.String()+", "+why)
	return &img, r.Checks[0]
}

// releaseTag is the runner image tag of a release version, "vX.Y.Z", and
// whether version is one: exactly X.Y.Z, with or without its v. Anything
// else (a pre-release, build metadata, a git describe string) is not.
func releaseTag(version string) (string, bool) {
	v, ok := parseRelease(strings.TrimPrefix(version, "v"))
	if !ok {
		return "", false
	}
	return "v" + v.String(), true
}

// newestRelease is the highest release tag, vX.Y.Z exactly, among tags, or
// "" when there is none.
func newestRelease(tags []string) string {
	var newest *semver.Version
	best := ""
	for _, tag := range tags {
		plain, ok := strings.CutPrefix(tag, "v")
		if !ok {
			continue
		}
		if v, ok := parseRelease(plain); ok && (newest == nil || v.GreaterThan(newest)) {
			newest, best = v, tag
		}
	}
	return best
}

// parseRelease parses X.Y.Z, strictly: three numbers without leading zeros,
// and no pre-release or build metadata.
func parseRelease(s string) (*semver.Version, bool) {
	v, err := semver.StrictNewVersion(s)
	if err != nil || v.Prerelease() != "" || v.Metadata() != "" {
		return nil, false
	}
	return v, true
}
