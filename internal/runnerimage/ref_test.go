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
		// Grammar: every path component is [a-z0-9]+ joined by the
		// distribution separators, the tag is [A-Za-z0-9_][A-Za-z0-9._-]{0,127},
		// the host is DNS labels with an optional port.
		{"dot-dot path segment", "ghcr.io/org/../evil/app", imageref.Ref{},
			"image reference `ghcr.io/org/../evil/app` is not a valid OCI image reference"},
		{"dot path segment", "ghcr.io/org/./app", imageref.Ref{},
			"image reference `ghcr.io/org/./app` is not a valid OCI image reference"},
		{"leading separator in segment", "ghcr.io/org/-app", imageref.Ref{},
			"image reference `ghcr.io/org/-app` is not a valid OCI image reference"},
		{"trailing separator in segment", "ghcr.io/org/app.", imageref.Ref{},
			"image reference `ghcr.io/org/app.` is not a valid OCI image reference"},
		{"illegal character in path", "ghcr.io/org/a+pp", imageref.Ref{},
			"image reference `ghcr.io/org/a+pp` is not a valid OCI image reference"},
		{"illegal character in tag", "ghcr.io/org/app:v1+x", imageref.Ref{},
			"image reference `ghcr.io/org/app:v1+x` is not a valid OCI image reference"},
		{"tag starting with a dot", "ghcr.io/org/app:.v1", imageref.Ref{},
			"image reference `ghcr.io/org/app:.v1` is not a valid OCI image reference"},
		{"tag too long", "ghcr.io/org/app:" + strings.Repeat("a", 129), imageref.Ref{},
			"image reference `ghcr.io/org/app:" + strings.Repeat("a", 129) + "` is not a valid OCI image reference"},
		{"bad host label", "ghcr-.io/org/app", imageref.Ref{},
			"image reference `ghcr-.io/org/app` is not a valid OCI image reference"},
		{"bad port", "localhost:50x0/app", imageref.Ref{},
			"image reference `localhost:50x0/app` is not a valid OCI image reference"},
		{"repository too long", "ghcr.io/" + strings.Repeat("a", 250), imageref.Ref{},
			"image reference `ghcr.io/" + strings.Repeat("a", 250) + "` is not a valid OCI image reference"},
		{"max tag", "ghcr.io/org/app:_" + strings.Repeat("A", 127),
			imageref.Ref{Repository: "ghcr.io/org/app", Tag: "_" + strings.Repeat("A", 127)}, ""},
		{"distribution separators", "ghcr.io/org/a__b--c.d_e/app",
			imageref.Ref{Repository: "ghcr.io/org/a__b--c.d_e/app", Tag: "latest"}, ""},
		// Canonical form: lowercase host and path, collapsed and trimmed
		// slashes, index.docker.io folded, library/ shorthand expanded. The
		// tag keeps its case (tags are case-sensitive).
		{"uppercase host and path", "GHCR.IO/Org/App:V1", imageref.Ref{Repository: "ghcr.io/org/app", Tag: "V1"}, ""},
		{"doubled slashes", "ghcr.io//org///app", imageref.Ref{Repository: "ghcr.io/org/app", Tag: "latest"}, ""},
		{"trailing slash", "ghcr.io/org/app/", imageref.Ref{Repository: "ghcr.io/org/app", Tag: "latest"}, ""},
		{"index.docker.io", "index.docker.io/acme/app", imageref.Ref{Repository: "docker.io/acme/app", Tag: "latest"}, ""},
		{"index.docker.io library shorthand", "index.docker.io/alpine",
			imageref.Ref{Repository: "docker.io/library/alpine", Tag: "latest"}, ""},
		{"docker.io library shorthand", "docker.io/alpine:3",
			imageref.Ref{Repository: "docker.io/library/alpine", Tag: "3"}, ""},
		{"hub path shorthand", "Acme//App", imageref.Ref{Repository: "docker.io/acme/app", Tag: "latest"}, ""},
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

// canonicalHosts are registry hosts already in canonical form.
var canonicalHosts = []string{"ghcr.io", "docker.io", "localhost:5000",
	"123456789012.dkr.ecr.us-east-1.amazonaws.com", "us-docker.pkg.dev"}

// spellRandomCase upper-cases a random subset of s's letters.
func spellRandomCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' && r.Intn(2) == 0 {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// genCanonicalRepository returns a repository already in canonical form.
func genCanonicalRepository(r *rand.Rand) string {
	host := canonicalHosts[r.Intn(len(canonicalHosts))]
	segs := genSegments(r)
	if host == "docker.io" && len(segs) == 1 {
		segs = append([]string{"library"}, segs...)
	}
	return host + "/" + strings.Join(segs, "/")
}

// genSpelling returns a random non-canonical spelling of a canonical
// repository: mixed case, doubled slashes, a trailing slash, and for the hub
// the index.docker.io alias, a dropped host and a dropped library/.
func genSpelling(r *rand.Rand, repo string) string {
	host, path, _ := strings.Cut(repo, "/")
	if host == "docker.io" {
		if short, ok := strings.CutPrefix(path, "library/"); ok && r.Intn(2) == 0 {
			path = short
		}
		first, _, _ := strings.Cut(path, "/")
		switch {
		case r.Intn(3) == 0 && !strings.ContainsAny(first, ".:") && first != "localhost":
			host = ""
		case r.Intn(2) == 0:
			host = "index.docker.io"
		}
	}
	segs := strings.Split(path, "/")
	var b strings.Builder
	if host != "" {
		b.WriteString(spellRandomCase(r, host))
		b.WriteString(strings.Repeat("/", 1+r.Intn(3)))
	}
	for i, seg := range segs {
		if i > 0 {
			b.WriteString(strings.Repeat("/", 1+r.Intn(3)))
		}
		b.WriteString(spellRandomCase(r, seg))
	}
	if r.Intn(2) == 0 {
		b.WriteString("/")
	}
	return b.String()
}

// genSuffix returns an optional ":tag" and optional "@digest".
func genSuffix(r *rand.Rand) string {
	var s string
	if r.Intn(2) == 0 {
		s += ":" + []string{"1.26", "V1", "latest", "_x-Y.z"}[r.Intn(4)]
	}
	if r.Intn(3) == 0 {
		s += "@sha256:" + hexDigest(r)
	}
	return s
}

func TestParseDeclaredCanonicalIdempotentProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(20260922)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genSpelling(r, genCanonicalRepository(r)) + genSuffix(r))
		},
	}
	idempotent := func(declared string) bool {
		once, err := ParseDeclared(declared)
		if err != nil {
			t.Logf("ParseDeclared(%q): %v", declared, err)
			return false
		}
		twice, err := ParseDeclared(once.String())
		return err == nil && twice == once && twice.String() == once.String()
	}
	if err := quick.Check(idempotent, cfg); err != nil {
		t.Error(err)
	}
}

func TestParseDeclaredNonCanonicalSpellingProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20260922))
	for i := 0; i < 500; i++ {
		repo := genCanonicalRepository(r)
		suffix := genSuffix(r)
		canonical, err := ParseDeclared(repo + suffix)
		if err != nil {
			t.Fatalf("case %d: canonical %q: %v", i, repo+suffix, err)
		}
		if canonical.Repository != repo {
			t.Fatalf("case %d: canonical %q parsed to repository %q", i, repo, canonical.Repository)
		}
		// An entry that is a prefix of the canonical repository, and one on a
		// random other path: a spelling is allowed exactly when its canonical
		// form is.
		host, path, _ := strings.Cut(repo, "/")
		segs := strings.Split(path, "/")
		prefix := host + "/" + strings.Join(segs[:1+r.Intn(len(segs))], "/")
		other := genCanonicalRepository(r)
		for j := 0; j < 5; j++ {
			spelling := genSpelling(r, repo) + suffix
			got, err := ParseDeclared(spelling)
			if err != nil {
				t.Fatalf("case %d: spelling %q of %q: %v", i, spelling, repo, err)
			}
			if got != canonical {
				t.Fatalf("case %d: spelling %q parsed to %+v, canonical %+v", i, spelling, got, canonical)
			}
			for _, entry := range []string{prefix, other} {
				p, err := NewPolicy([]string{entry})
				if err != nil {
					t.Fatal(err)
				}
				if (p.Allow(got) == nil) != (p.Allow(canonical) == nil) {
					t.Fatalf("case %d: %q and its canonical %q disagree under %q", i, spelling, repo, entry)
				}
			}
		}
	}
}
