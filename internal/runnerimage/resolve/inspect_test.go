// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bitwise-media-group/patchy/internal/runnerimage"
)

// errText is an error's message, "" for nil, so verdicts compare as text.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestInspectReportsEveryCheck: where Resolve stops at the first rejection,
// Inspect judges every check, so an image that is oversized, sets a
// reserved ENV, declares a VOLUME and has no absolute PATH entry shows all
// four at once, each with the message Resolve would give it.
func TestInspectReportsEveryCheck(t *testing.T) {
	repo := newRegistry(t, false, nil)
	d := push(t, repo.Tag("v1"), image(t, &v1.ConfigFile{Config: v1.Config{
		Env:     []string{"PATH=bin", "ANTHROPIC_BASE_URL=http://evil"},
		Volumes: map[string]struct{}{"/data": {}},
	}}, make([]byte, 1200)))
	r := newResolver(t, Config{MaxBytes: 1000})
	rep, err := r.Inspect(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if rep.Image != repo.String()+"@"+d.String() || rep.Index || rep.Runnable != nil || rep.SignatureChecked {
		t.Errorf("Report = %+v, want the pinned single manifest, no signature check", rep)
	}
	if len(rep.Manifests) != 1 {
		t.Fatalf("Manifests = %+v, want one", rep.Manifests)
	}
	m := rep.Manifests[0]
	if m.Digest != d.String() || m.Platform != "linux/amd64" || m.LayerBytes != 1200 || !m.ConfigRead {
		t.Errorf("Manifest = %+v, want linux/amd64, 1200 layer bytes, config read", m)
	}
	rejection(t, m.Size, "Oversized", "1200 bytes of compressed layers; the limit is 1000 bytes")
	rejection(t, m.Volumes, "Volume", "VOLUME `/data`")
	rejection(t, m.Env, "ReservedEnv", "`ANTHROPIC_BASE_URL`")
	rejection(t, m.Path, "EmptyPath", "no absolute entries")
	if m.Config != nil || m.Arch != nil {
		t.Errorf("Config = %v, Arch = %v, want both to pass", m.Config, m.Arch)
	}

	_, want := newResolver(t, Config{MaxBytes: 1000}).Resolve(context.Background(),
		declared(t, repo.String()+":v1"))
	if errText(rep.Err()) != errText(want) {
		t.Errorf("Report.Err() = %v, Resolve = %v", rep.Err(), want)
	}
}

// TestInspectAgreesWithResolve pins parity: for every fixture, the first
// failure a whole report lists is exactly what Resolve returns, so the
// workstation check and source-controller cannot disagree about an image.
func TestInspectAgreesWithResolve(t *testing.T) {
	priv, pub := testKeys(t)
	env := []string{"PATH=/usr/local/go/bin:/usr/bin"}
	amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: env}}, []byte("amd"))
	arm := image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: env}}, []byte("arm"))
	other := image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: []string{"PATH=/other"}}},
		[]byte("other"))
	armVolume := image(t, &v1.ConfigFile{Architecture: "arm64",
		Config: v1.Config{Env: env, Volumes: map[string]struct{}{"/x": {}}}}, []byte("vol"))
	win := image(t, &v1.ConfigFile{OS: "windows"}, []byte("win"))

	cases := []struct {
		name  string
		cfg   Config
		setup func(t *testing.T, repo name.Repository)
	}{
		{"accepted single manifest", Config{}, func(t *testing.T, repo name.Repository) {
			push(t, repo.Tag("v1"), amd)
		}},
		{"accepted index", Config{}, func(t *testing.T, repo name.Repository) {
			pushIndex(t, repo.Tag("v1"), indexChild{amd, linux("amd64")}, indexChild{arm, linux("arm64")})
		}},
		{"index children disagree on PATH", Config{}, func(t *testing.T, repo name.Repository) {
			pushIndex(t, repo.Tag("v1"), indexChild{amd, linux("amd64")}, indexChild{other, linux("arm64")})
		}},
		{"second child declares a VOLUME", Config{}, func(t *testing.T, repo name.Repository) {
			pushIndex(t, repo.Tag("v1"), indexChild{amd, linux("amd64")}, indexChild{armVolume, linux("arm64")})
		}},
		{"no runnable manifest", Config{}, func(t *testing.T, repo name.Repository) {
			pushIndex(t, repo.Tag("v1"), indexChild{win, v1.Platform{OS: "windows", Architecture: "amd64"}})
		}},
		{"built for another OS", Config{}, func(t *testing.T, repo name.Repository) {
			push(t, repo.Tag("v1"), win)
		}},
		{"oversized", Config{MaxBytes: 1}, func(t *testing.T, repo name.Repository) {
			push(t, repo.Tag("v1"), amd)
		}},
		{"unsigned under a key", Config{PublicKey: pub}, func(t *testing.T, repo name.Repository) {
			push(t, repo.Tag("v1"), amd)
		}},
		{"signed under a key", Config{PublicKey: pub}, func(t *testing.T, repo name.Repository) {
			signLegacy(t, priv, repo, push(t, repo.Tag("v1"), amd))
		}},
		{"reserved env and unsigned", Config{PublicKey: pub}, func(t *testing.T, repo name.Repository) {
			push(t, repo.Tag("v1"), image(t, &v1.ConfigFile{Config: v1.Config{Env: []string{"PATCHY_X=1"}}}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, true, nil)
			tc.setup(t, repo)
			ref := declared(t, repo.String()+":v1")
			resolved, want := newResolver(t, tc.cfg).Resolve(context.Background(), ref)
			rep, err := newResolver(t, tc.cfg).Inspect(context.Background(), ref)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if got := rep.Err(); errText(got) != errText(want) {
				t.Errorf("Report.Err() = %v, Resolve = %v", got, want)
			}
			if want == nil && (rep.Image != resolved.Image || rep.SignatureChecked != resolved.Verified) {
				t.Errorf("Report pins %s (signature checked %v), Resolve %s (verified %v)",
					rep.Image, rep.SignatureChecked, resolved.Image, resolved.Verified)
			}
		})
	}
}

// TestInspectIndex: every runnable child is listed with its own digest,
// platform and size, and the rest of the index is skipped.
func TestInspectIndex(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	amd := image(t, &v1.ConfigFile{Config: v1.Config{Env: env}}, []byte("amd64 layer"))
	arm := image(t, &v1.ConfigFile{Architecture: "arm64", Config: v1.Config{Env: env}}, []byte("arm"))
	win := image(t, &v1.ConfigFile{OS: "windows"}, []byte("win"))
	repo := newRegistry(t, false, nil)
	d := pushIndex(t, repo.Tag("v1"), indexChild{amd, linux("amd64")}, indexChild{arm, linux("arm64")},
		indexChild{win, v1.Platform{OS: "windows", Architecture: "amd64"}})

	rep, err := newResolver(t, Config{}).Inspect(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !rep.Index || rep.Image != repo.String()+"@"+d.String() || rep.Err() != nil || rep.PathMismatch() != nil {
		t.Fatalf("Report = %+v, want an accepted index pinned to %s", rep, d)
	}
	got := make([]string, 0, len(rep.Manifests))
	for _, m := range rep.Manifests {
		got = append(got, fmt.Sprintf("%s %d", m.Platform, m.LayerBytes))
	}
	if strings.Join(got, ", ") != "linux/amd64 11, linux/arm64 3" {
		t.Errorf("Manifests = %s, want the two linux children with their layer sizes", strings.Join(got, ", "))
	}
	for i, img := range []v1.Image{amd, arm} {
		want, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Manifests[i].Digest != want.String() {
			t.Errorf("Manifests[%d].Digest = %s, want %s", i, rep.Manifests[i].Digest, want)
		}
	}
}

// TestInspectPathMismatchAmongPassingManifests: PathMismatch compares only
// PATHs that were judged and passed, and reports the first disagreement.
func TestInspectPathMismatchAmongPassingManifests(t *testing.T) {
	rej := &runnerimage.Rejection{Message: "no absolute entries"}
	cases := []struct {
		name      string
		manifests []Manifest
		want      string
	}{
		{"agreeing", []Manifest{
			{ConfigRead: true, SearchPath: []string{"/bin"}},
			{ConfigRead: true, SearchPath: []string{"/bin"}},
		}, ""},
		{"disagreeing", []Manifest{
			{ConfigRead: true, SearchPath: []string{"/bin"}},
			{ConfigRead: true, SearchPath: []string{"/usr/bin"}},
		}, "(`/bin` vs `/usr/bin`)"},
		{"a failed PATH is not compared", []Manifest{
			{ConfigRead: true, Path: rej},
			{ConfigRead: true, SearchPath: []string{"/bin"}},
		}, ""},
		{"an unread config is not compared", []Manifest{
			{Config: rej},
			{ConfigRead: true, SearchPath: []string{"/bin"}},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Report{Manifests: tc.manifests, name: "r@sha256:0"}.PathMismatch()
			if tc.want == "" {
				if err != nil {
					t.Errorf("PathMismatch = %v, want nil", err)
				}
				return
			}
			rejection(t, err, "PathMismatch", tc.want)
		})
	}
}

// TestInspectNeverReadsAnOversizedConfig: the config cap holds whether or
// not the inspection stops early; the unread config leaves the checks that
// need it unjudged rather than passed.
func TestInspectNeverReadsAnOversizedConfig(t *testing.T) {
	c := &counter{}
	repo := newRegistry(t, false, c.wrap)
	huge := image(t, &v1.ConfigFile{Config: v1.Config{
		Labels: map[string]string{"pad": strings.Repeat("x", 5<<20)},
	}}, []byte("huge"))
	push(t, repo.Tag("v1"), huge)
	c.reset(nil)
	rep, err := newResolver(t, Config{}).Inspect(context.Background(), declared(t, repo.String()+":v1"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	m := rep.Manifests[0]
	rejection(t, m.Config, "Oversized", "-byte config; the limit is")
	if m.ConfigRead || m.Platform != "" {
		t.Errorf("Manifest = %+v, want the config unread and its platform unknown", m)
	}
	cn, err := huge.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	if c.blobs[cn.String()] != 0 {
		t.Error("fetched the oversized config")
	}
}

// TestInspectUnresolvable: a reference the registry does not have stops the
// inspection with Resolve's rejection; nothing is judged.
func TestInspectUnresolvable(t *testing.T) {
	repo := newRegistry(t, false, nil)
	_, err := newResolver(t, Config{}).Inspect(context.Background(), declared(t, repo.String()+":missing"))
	var rej *runnerimage.Rejection
	if !errors.As(err, &rej) || rej.Reason != "NotFound" {
		t.Errorf("Inspect error = %v, want the NotFound rejection", err)
	}
}
