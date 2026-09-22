// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestNormalizeEntry(t *testing.T) {
	cases := []struct {
		in, want, wantErr string
	}{
		{"ghcr.io/org", "ghcr.io/org/", ""},
		{"ghcr.io/org/", "ghcr.io/org/", ""},
		{"ghcr.io/org/team/", "ghcr.io/org/team/", ""},
		{"GHCR.IO/org/", "ghcr.io/org/", ""},
		{"index.docker.io/library/", "docker.io/library/", ""},
		{"Index.Docker.IO/library", "docker.io/library/", ""},
		{"localhost:5000/team", "localhost:5000/team/", ""},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/",
			"123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/", ""},
		{"", "", "registry allowlist entry is empty"},
		{"ghcr.io", "", "registry allowlist entry `ghcr.io` must be `host/path/` with at least one path segment"},
		{"ghcr.io/", "", "registry allowlist entry `ghcr.io/` must be `host/path/` with at least one path segment"},
		{"/org/", "", "registry allowlist entry `/org/` must be `host/path/` with at least one path segment"},
		{"ghcr.io/org/*", "", "registry allowlist entry `ghcr.io/org/*` may not contain glob characters"},
		{"ghcr.io/org?/", "", "registry allowlist entry `ghcr.io/org?/` may not contain glob characters"},
		{"ghcr.io/[ab]/", "", "registry allowlist entry `ghcr.io/[ab]/` may not contain glob characters"},
		{"ghcr.io/org//x/", "", "registry allowlist entry `ghcr.io/org//x/` has an empty path segment"},
		{"ghcr.io/org//", "", "registry allowlist entry `ghcr.io/org//` has an empty path segment"},
		{"ghcr.io/org/app:1/", "", "registry allowlist entry `ghcr.io/org/app:1/` may not name a tag or digest"},
		{"ghcr.io/org/app@sha256:x", "", "registry allowlist entry `ghcr.io/org/app@sha256:x` may not name a tag or digest"},
		{"ghcr.io/org /", "", "registry allowlist entry `ghcr.io/org /` contains whitespace"},
		{"ghcr.io/org/\n", "", "registry allowlist entry `ghcr.io/org/\n` contains whitespace"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := NormalizeEntry(c.in)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("NormalizeEntry(%q) error = %v, want %q", c.in, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEntry(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("NormalizeEntry(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNewPolicy(t *testing.T) {
	if _, err := NewPolicy(nil); err == nil || err.Error() != "registry allowlist is empty" {
		t.Errorf("NewPolicy(nil) error = %v", err)
	}
	if _, err := NewPolicy([]string{"ghcr.io/org/", "ghcr.io"}); err == nil ||
		err.Error() != "registry allowlist entry `ghcr.io` must be `host/path/` with at least one path segment" {
		t.Errorf("NewPolicy with a bad entry error = %v", err)
	}
	p, err := NewPolicy([]string{"GHCR.io/org", "index.docker.io/library/"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Entries(), []string{"ghcr.io/org/", "docker.io/library/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Entries = %v, want %v", got, want)
	}
	p.Entries()[0] = "mutated/"
	if p.Entries()[0] != "ghcr.io/org/" {
		t.Error("Entries must return a copy")
	}
}

func TestPolicyAllow(t *testing.T) {
	p, err := NewPolicy([]string{"ghcr.io/org/", "index.docker.io/library/", "LOCALHOST:5000/team/"})
	if err != nil {
		t.Fatal(err)
	}
	const suffix = " is not under an allowlisted registry path (ghcr.io/org/, docker.io/library/, localhost:5000/team/)"
	cases := []struct {
		in      string
		wantErr string
	}{
		{"ghcr.io/org/app:1", ""},
		{"ghcr.io/org/app", ""},
		{"ghcr.io/org/team/app@sha256:" + hex64, ""},
		{"GHCR.IO/org/app", ""},
		{"alpine", ""},
		{"docker.io/library/alpine:3", ""},
		{"index.docker.io/library/alpine", ""},
		{"localhost:5000/team/app", ""},
		{"ghcr.io/org-evil/app", "image `ghcr.io/org-evil/app:latest`" + suffix},
		{"ghcr.io/orgx/app", "image `ghcr.io/orgx/app:latest`" + suffix},
		{"ghcr.io/org", ""}, // an entry admits the repository it names exactly
		{"ghcr.io/other/org/app", "image `ghcr.io/other/org/app:latest`" + suffix},
		{"ghcr.io/Org/app", "image `ghcr.io/Org/app:latest`" + suffix},
		{"docker.io/acme/app", "image `docker.io/acme/app:latest`" + suffix},
		{"acme/app", "image `docker.io/acme/app:latest`" + suffix},
		{"localhost:5001/team/app", "image `localhost:5001/team/app:latest`" + suffix},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			ref, err := ParseDeclared(c.in)
			if err != nil {
				t.Fatal(err)
			}
			err = p.Allow(ref)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("Allow(%q) = %v, want allowed", c.in, err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Errorf("Allow(%q) = %v, want %q", c.in, err, c.wantErr)
			}
			if !IsRejection(err) {
				t.Errorf("error is not a Rejection: %T", err)
			}
		})
	}
}

var hosts = []string{"ghcr.io", "GHCR.io", "index.docker.io", "Index.Docker.IO", "docker.io", "localhost:5000",
	"123456789012.dkr.ecr.us-east-1.amazonaws.com", "us-docker.pkg.dev"}

func genSegment(r *rand.Rand) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-_."
	n := 1 + r.Intn(8)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func genSegments(r *rand.Rand) []string {
	segs := make([]string, 1+r.Intn(3))
	for i := range segs {
		segs[i] = genSegment(r)
	}
	return segs
}

// genEntry returns a valid, un-normalized entry: mixed-case host, optional
// trailing slash.
func genEntry(r *rand.Rand) string {
	e := hosts[r.Intn(len(hosts))] + "/" + strings.Join(genSegments(r), "/")
	if r.Intn(2) == 0 {
		e += "/"
	}
	return e
}

func TestNormalizeEntryIdempotentProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(20260922)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genEntry(r))
		},
	}
	idempotent := func(entry string) bool {
		once, err := NormalizeEntry(entry)
		if err != nil {
			return false
		}
		twice, err := NormalizeEntry(once)
		return err == nil && once == twice && strings.HasSuffix(once, "/")
	}
	if err := quick.Check(idempotent, cfg); err != nil {
		t.Error(err)
	}
}

func TestPolicyAllowNoCrossHostOrSiblingProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20260922))
	for i := 0; i < 500; i++ {
		host := hosts[r.Intn(len(hosts))]
		segs := genSegments(r)
		entry, err := NormalizeEntry(host + "/" + strings.Join(segs, "/"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewPolicy([]string{entry})
		if err != nil {
			t.Fatal(err)
		}
		entryHost := entry[:strings.Index(entry, "/")]
		path := strings.Join(segs, "/")
		// The reference under the entry is allowed; everything derived from it
		// by moving host, widening a segment, or shortening the path is not.
		mustAllow := entryHost + "/" + path + "/" + genSegment(r)
		if ref, err := ParseDeclared(mustAllow); err != nil || p.Allow(ref) != nil {
			t.Fatalf("case %d: %q must be allowed under %q: %v", i, mustAllow, entry, err)
		}
		other := hosts[r.Intn(len(hosts))]
		otherHost := canonicalHost(other)
		sibling := append([]string(nil), segs...)
		sibling[len(sibling)-1] += "x"
		widened := append([]string(nil), segs...)
		widened[0] = "x" + widened[0]
		mustReject := []string{
			entryHost + "/" + strings.Join(sibling, "/"),
			entryHost + "/" + strings.Join(sibling, "/") + "/app",
			entryHost + "/" + strings.Join(widened, "/") + "/app",
			entryHost + "/" + strings.Join(segs[:len(segs)-1], "/"),
		}
		if otherHost != entryHost {
			mustReject = append(mustReject, other+"/"+path+"/app")
		}
		for _, in := range mustReject {
			if !strings.Contains(in, "/") || strings.HasSuffix(in, "/") {
				continue // a host-only or empty-path reference is not parseable
			}
			ref, err := ParseDeclared(in)
			if err != nil {
				continue
			}
			if err := p.Allow(ref); err == nil {
				t.Fatalf("case %d: %q must not be allowed under %q", i, in, entry)
			}
		}
	}
}
