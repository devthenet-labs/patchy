// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package imagecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fakeRegistry stands in for the registry the runner image is chosen from:
// Tags answers tags (or tagsErr), Digest answers every reference with
// digestOf it (or digestErr), and every call is recorded.
type fakeRegistry struct {
	tags      []string
	tagsErr   error
	digestErr error
	calls     []string
}

func (f *fakeRegistry) Tags(_ context.Context, repository string) ([]string, error) {
	f.calls = append(f.calls, "tags "+repository)
	return f.tags, f.tagsErr
}

func (f *fakeRegistry) Digest(_ context.Context, reference string) (string, error) {
	f.calls = append(f.calls, "digest "+reference)
	if f.digestErr != nil {
		return "", f.digestErr
	}
	return digestOf(reference), nil
}

// digestOf is the digest fakeRegistry says a reference names.
func digestOf(reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// publishedTags is what the runner image repository holds: releases (one
// with a two-digit patch that sorts first as a string), latest, cosign's
// signature tags, a pre-release and tags that only look like releases.
var publishedTags = []string{
	"latest", "v0.11.3", "sha256-89ea4b28bc18469ad8b047104170baf3abafc424b64f37c4afce4198400064e0.sig",
	"v0.11.9", "v0.11.10", "v0.12.0-rc.1", "0.13.0", "v1.0", "v2", "v01.2.3", "sha-b4c563a", "main",
}

// TestDefaultRunnerImage: a release takes the runner image released with
// it; any other build takes the newest vX.Y.Z release in the registry,
// never latest, a pre-release or a tag that only looks like a release;
// either way the tag is pinned to the digest the registry has for it now,
// and when there is none to take, the error says to pass --runner-image.
func TestDefaultRunnerImage(t *testing.T) {
	newest := RunnerImageRepository + ":v0.11.10"
	offline := errors.New("dial tcp: lookup ghcr.io: no such host")
	cases := []struct {
		name     string
		version  string
		registry *fakeRegistry
		want     string   // the chosen tag reference; empty when none is
		errHas   []string // substrings of the error when none is chosen
		calls    []string // the registry calls, in order
	}{
		{name: "release", version: "0.11.6", registry: &fakeRegistry{tags: publishedTags},
			want: RunnerImageRepository + ":v0.11.6", calls: []string{"digest " + RunnerImageRepository + ":v0.11.6"}},
		{name: "release with its v", version: "v0.12.0", registry: &fakeRegistry{tags: publishedTags},
			want: RunnerImageRepository + ":v0.12.0", calls: []string{"digest " + RunnerImageRepository + ":v0.12.0"}},
		{name: "development build", version: "dev", registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "no version at all", version: "", registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "git describe of a commit past a release", version: "v0.11.7-2-gb4c563a-dirty",
			registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "git describe of an untagged history", version: "b4c563a",
			registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "goreleaser snapshot", version: "0.11.8-SNAPSHOT-b4c563a",
			registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "a pre-release version is not a release", version: "0.12.0-rc.1",
			registry: &fakeRegistry{tags: publishedTags}, want: newest,
			calls: []string{"tags " + RunnerImageRepository, "digest " + newest}},
		{name: "development build, registry unreachable", version: "dev",
			registry: &fakeRegistry{tagsErr: offline},
			errHas:   []string{"development build", "no such host", "--runner-image"},
			calls:    []string{"tags " + RunnerImageRepository}},
		{name: "development build, no release published", version: "dev",
			registry: &fakeRegistry{tags: []string{"latest", "v0.12.0-rc.1", "main", "0.13.0"}},
			errHas:   []string{"development build", "no release", "--runner-image"},
			calls:    []string{"tags " + RunnerImageRepository}},
		{name: "release, registry unreachable", version: "0.11.6",
			registry: &fakeRegistry{digestErr: offline},
			errHas:   []string{RunnerImageRepository + ":v0.11.6", "no such host", "--runner-image"},
			calls:    []string{"digest " + RunnerImageRepository + ":v0.11.6"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DefaultRunnerImage(context.Background(), tc.registry, tc.version)
			switch {
			case tc.want != "" && err != nil:
				t.Fatalf("DefaultRunnerImage(%q): %v", tc.version, err)
			case tc.want != "":
				if want := (RunnerImage{Reference: tc.want, Digest: digestOf(tc.want)}); got != want {
					t.Errorf("DefaultRunnerImage(%q) = %+v, want %+v", tc.version, got, want)
				}
			case err == nil:
				t.Fatalf("DefaultRunnerImage(%q) = %+v, want no runner image", tc.version, got)
			default:
				if got != (RunnerImage{}) {
					t.Errorf("DefaultRunnerImage(%q) = %+v alongside its error", tc.version, got)
				}
				for _, want := range tc.errHas {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q lacks %q", err, want)
					}
				}
			}
			if !slices.Equal(tc.registry.calls, tc.calls) {
				t.Errorf("registry calls = %q, want %q", tc.registry.calls, tc.calls)
			}
		})
	}
}

// TestChooseRunner: the choice is always reported as the runner-image
// check, naming the image and its digest and why it was chosen; an
// explicit --runner-image is used as given, without asking the registry;
// no runner image at all is a FAIL that says to pass one.
func TestChooseRunner(t *testing.T) {
	pinned := "ghcr.io/acme/runner:v9@sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name      string
		override  string
		version   string
		registry  *fakeRegistry
		want      *RunnerImage
		image     string // what docker is given
		status    Status
		reasonHas []string
		calls     int
	}{
		{name: "development build", version: "dev", registry: &fakeRegistry{tags: publishedTags},
			want: &RunnerImage{Reference: RunnerImageRepository + ":v0.11.10",
				Digest: digestOf(RunnerImageRepository + ":v0.11.10")},
			image:  RunnerImageRepository + "@" + digestOf(RunnerImageRepository+":v0.11.10"),
			status: Pass,
			reasonHas: []string{RunnerImageRepository + ":v0.11.10@" + digestOf(RunnerImageRepository+":v0.11.10"),
				"newest release", "development build"},
			calls: 2},
		{name: "release", version: "0.11.6", registry: &fakeRegistry{},
			want: &RunnerImage{Reference: RunnerImageRepository + ":v0.11.6",
				Digest: digestOf(RunnerImageRepository + ":v0.11.6")},
			image:  RunnerImageRepository + "@" + digestOf(RunnerImageRepository+":v0.11.6"),
			status: Pass,
			reasonHas: []string{RunnerImageRepository + ":v0.11.6@" + digestOf(RunnerImageRepository+":v0.11.6"),
				"released with this CLI"},
			calls: 1},
		{name: "--runner-image tag", override: "runner:test", version: "dev", registry: &fakeRegistry{},
			want: &RunnerImage{Reference: "runner:test"}, image: "runner:test", status: Pass,
			reasonHas: []string{"runner:test", "--runner-image"}},
		{name: "--runner-image digest", override: pinned, version: "0.11.6", registry: &fakeRegistry{},
			want:  &RunnerImage{Reference: pinned, Digest: "sha256:" + strings.Repeat("b", 64)},
			image: "ghcr.io/acme/runner@sha256:" + strings.Repeat("b", 64), status: Pass,
			reasonHas: []string{pinned, "--runner-image"}},
		{name: "none to be had", version: "dev", registry: &fakeRegistry{tagsErr: errors.New("connection refused")},
			status: Fail, reasonHas: []string{"connection refused", "pass --runner-image"}, calls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, check := ChooseRunner(context.Background(), tc.registry, tc.override, tc.version)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ChooseRunner = %+v, want %+v", got, tc.want)
			}
			if got != nil && got.Image() != tc.image {
				t.Errorf("Image() = %q, want %q", got.Image(), tc.image)
			}
			if check.Name != CheckRunnerImage || check.Status != tc.status || check.Platform != "" {
				t.Errorf("check = %+v, want an unlabelled %s %s line", check, tc.status, CheckRunnerImage)
			}
			for _, want := range tc.reasonHas {
				if !strings.Contains(check.Reason, want) {
					t.Errorf("reason %q lacks %q", check.Reason, want)
				}
			}
			if len(tc.registry.calls) != tc.calls {
				t.Errorf("registry calls = %q, want %d", tc.registry.calls, tc.calls)
			}
		})
	}
}

// strictRelease matches the tags newestRelease may pick, independently of
// how it parses them.
var strictRelease = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// releaseKey is a strict release tag's numbers, for comparing.
func releaseKey(tag string) [3]uint64 {
	var k [3]uint64
	for i, s := range strictRelease.FindStringSubmatch(tag)[1:] {
		k[i], _ = strconv.ParseUint(s, 10, 64)
	}
	return k
}

// TestNewestReleaseProperty: for any tag list, newestRelease picks a strict
// vX.Y.Z tag from it that no other is newer than, or nothing when there is
// none, and the order the registry lists them in does not matter.
func TestNewestReleaseProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20260923)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			num := func() string { return strconv.Itoa([]int{0, 1, 2, 9, 10, 11, 100}[r.Intn(7)]) }
			shapes := []func() string{
				func() string { return "v" + num() + "." + num() + "." + num() },
				func() string { return num() + "." + num() + "." + num() },
				func() string { return "v" + num() + "." + num() + "." + num() + "-rc." + num() },
				func() string { return "v" + num() + "." + num() },
				func() string { return "v0" + num() + ".1.2" },
				func() string { return "latest" },
				func() string { return "sha256-" + strings.Repeat("a", 64) + ".sig" },
			}
			tags := make([]string, r.Intn(8))
			for i := range tags {
				tags[i] = shapes[r.Intn(len(shapes))]()
			}
			args[0] = reflect.ValueOf(tags)
		},
	}
	newestHolds := func(tags []string) bool {
		got := newestRelease(tags)
		var releases []string
		for _, tag := range tags {
			if strictRelease.MatchString(tag) {
				releases = append(releases, tag)
			}
		}
		if len(releases) == 0 {
			return got == ""
		}
		if !slices.Contains(releases, got) {
			return false
		}
		for _, tag := range releases {
			if k, g := releaseKey(tag), releaseKey(got); slices.Compare(k[:], g[:]) > 0 {
				return false
			}
		}
		reversed := slices.Clone(tags)
		slices.Reverse(reversed)
		return newestRelease(reversed) == got
	}
	if err := quick.Check(newestHolds, cfg); err != nil {
		t.Error(err)
	}
}

// TestRemoteRegistry: the real Registry lists a repository's tags and
// resolves a tag to the digest the registry has for it, against a registry
// rather than a local store.
func TestRemoteRegistry(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repo := u.Host + "/org/claude-agent-runner"
	digests := map[string]string{}
	for i, tag := range []string{"v0.11.9", "v0.11.10", "latest"} {
		img, err := mutate.ConfigFile(mutate.MediaType(empty.Image, types.OCIManifestSchema1), &v1.ConfigFile{
			OS: "linux", Architecture: "amd64", Config: v1.Config{Labels: map[string]string{"n": fmt.Sprint(i)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ref, err := name.NewTag(repo + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests[tag] = d.String()
	}

	reg := RemoteRegistry{}
	tags, err := reg.Tags(context.Background(), repo)
	slices.Sort(tags)
	if err != nil || !slices.Equal(tags, []string{"latest", "v0.11.10", "v0.11.9"}) {
		t.Errorf("Tags = %q, %v", tags, err)
	}
	if got, err := reg.Digest(context.Background(), repo+":v0.11.10"); err != nil || got != digests["v0.11.10"] {
		t.Errorf("Digest(v0.11.10) = %q, %v; want %q", got, err, digests["v0.11.10"])
	}
	if got, err := reg.Digest(context.Background(), repo+":v0.9.0"); err == nil {
		t.Errorf("Digest of a tag the registry lacks = %q, want an error", got)
	}
	if _, err := reg.Tags(context.Background(), u.Host+"/org/nothing"); err == nil {
		t.Error("Tags of a repository the registry lacks succeeded")
	}
}
