// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/mirror/imageref"
)

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseDeclared(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    imageref.Ref
		wantErr string
	}{
		{"bare name normalizes to the hub", "alpine",
			imageref.Ref{Repository: "docker.io/library/alpine", Tag: "latest"}, ""},
		{"tagged", "ghcr.io/org/app:1.26", imageref.Ref{Repository: "ghcr.io/org/app", Tag: "1.26"}, ""},
		{"digest pin", "ghcr.io/org/app@sha256:" + hex64,
			imageref.Ref{Repository: "ghcr.io/org/app", Digest: "sha256:" + hex64}, ""},
		{"tag and digest", "ghcr.io/org/app:1@sha256:" + hex64,
			imageref.Ref{Repository: "ghcr.io/org/app", Tag: "1", Digest: "sha256:" + hex64}, ""},
		{"registry port", "localhost:5000/app", imageref.Ref{Repository: "localhost:5000/app", Tag: "latest"}, ""},
		{"empty", "", imageref.Ref{}, "image reference is empty"},
		{"inner space", "ghcr.io/org/app :1", imageref.Ref{}, "image reference `ghcr.io/org/app :1` contains whitespace"},
		{"trailing newline", "ghcr.io/org/app\n", imageref.Ref{}, "image reference `ghcr.io/org/app\n` contains whitespace"},
		{"short digest", "ghcr.io/org/app@sha256:abc", imageref.Ref{},
			"image `ghcr.io/org/app@sha256:abc` pins digest `sha256:abc`, which is not `sha256:` followed by 64 hex characters"},
		{"uppercase digest", "ghcr.io/org/app@sha256:" + strings.ToUpper(hex64), imageref.Ref{},
			"image `ghcr.io/org/app@sha256:" + strings.ToUpper(hex64) + "` pins digest `sha256:" +
				strings.ToUpper(hex64) + "`, which is not `sha256:` followed by 64 hex characters"},
		{"sha512 digest", "ghcr.io/org/app@sha512:" + hex64 + hex64, imageref.Ref{},
			"image `ghcr.io/org/app@sha512:" + hex64 + hex64 + "` pins digest `sha512:" + hex64 + hex64 +
				"`, which is not `sha256:` followed by 64 hex characters"},
		{"empty digest", "ghcr.io/org/app@", imageref.Ref{},
			"image reference `ghcr.io/org/app@` is invalid: image reference \"ghcr.io/org/app@\" has an empty digest"},
		{"empty tag", "ghcr.io/org/app:", imageref.Ref{},
			"image reference `ghcr.io/org/app:` is invalid: image reference \"ghcr.io/org/app:\" has an empty tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseDeclared(c.in)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("ParseDeclared(%q) error = %v, want %q", c.in, err, c.wantErr)
				}
				if !IsRejection(err) {
					t.Errorf("error is not a Rejection: %T", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDeclared(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ParseDeclared(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestValidateDigest(t *testing.T) {
	cases := map[string]bool{
		"sha256:" + hex64:                  true,
		"sha256:" + strings.ToUpper(hex64): false,
		"sha256:" + hex64[:63]:             false,
		"sha256:" + hex64 + "0":            false,
		"sha512:" + hex64 + hex64:          false,
		hex64:                              false,
		"":                                 false,
		"sha256:" + hex64[:63] + "g":       false,
	}
	for in, ok := range cases {
		err := ValidateDigest(in)
		if (err == nil) != ok {
			t.Errorf("ValidateDigest(%q) = %v, want ok=%v", in, err, ok)
		}
		if err != nil && err.Error() != "digest `"+in+"` is not `sha256:` followed by 64 hex characters" {
			t.Errorf("ValidateDigest(%q) message = %q", in, err.Error())
		}
	}
}

func TestPin(t *testing.T) {
	ref := imageref.Ref{Repository: "ghcr.io/org/app", Tag: "1.26"}
	got, err := Pin(ref, "sha256:"+hex64)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ghcr.io/org/app@sha256:" + hex64; got != want {
		t.Errorf("Pin = %q, want %q (the tag must not survive)", got, want)
	}
	if _, err := Pin(ref, "sha256:short"); err == nil || !IsRejection(err) {
		t.Errorf("Pin with a bad digest = %v, want a Rejection", err)
	}
}

// hexDigest generates a random 64-hex digest body.
func hexDigest(r *rand.Rand) string {
	const alphabet = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

var repositories = []string{
	"ghcr.io/org/app", "docker.io/library/alpine", "localhost:5000/team/app",
	"123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/go-agent-env", "us-docker.pkg.dev/proj/repo/img",
}

func TestDigestPinRoundTripProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 300,
		Rand:     rand.New(rand.NewSource(20260922)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(repositories[r.Intn(len(repositories))])
			args[1] = reflect.ValueOf("sha256:" + hexDigest(r))
		},
	}
	roundTrips := func(repo, digest string) bool {
		pinned, err := Pin(imageref.Ref{Repository: repo, Tag: "v1"}, digest)
		if err != nil {
			return false
		}
		back, err := ParseDeclared(pinned)
		if err != nil {
			return false
		}
		return back.Repository == repo && back.Digest == digest && back.Tag == "" && back.String() == pinned
	}
	if err := quick.Check(roundTrips, cfg); err != nil {
		t.Error(err)
	}
}
